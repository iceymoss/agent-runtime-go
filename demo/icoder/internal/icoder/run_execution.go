package icoder

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
	"github.com/iceymoss/agent-runtime-go/permission"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

// runLeaseDuration bounds how long one attempt may hold a run before another
// worker is allowed to take it over. It is generous because a coding step can
// legitimately spend minutes inside a test command.
const runLeaseDuration = 15 * time.Minute

// agentKey names the runtime that owns these durable runs. It is part of the
// execution key, so changing it deliberately separates old records from new ones.
const agentKey = "icoder"

// runInvocation is everything one durable attempt needs. A fresh task and a
// resumed run differ only in how these fields are obtained, which is why both
// paths share one execution function.
type runInvocation struct {
	sessionID string
	snapshot  SessionSnapshot
	requestID string
	runKey    string
	// instruction is the user text this run is answering. It is empty on resume,
	// where the instruction was already committed to the durable checkpoint.
	instruction  string
	messages     []agent.Message
	inputDigest  string
	configDigest string
	// observeFence receives the durable fence once the lease is granted. The
	// queue needs it to prove the outcome it merges came from the attempt it
	// leased, and only the runtime knows it.
	observeFence func(uint64)
	toolResume   *agent.ToolSuspension
	observe      func(agent.Observation)
	approve      ApprovalFunc
	// delegationResumes counts how many times this run has already been resumed
	// after parking on a child agent, so a child that never finishes cannot spin
	// the parent forever.
	delegationResumes int
}

// maxDelegationResumes bounds how many times one run may park on delegated work
// and be continued. Each delegate call parks once, so the limit is really a cap
// on how many delegations a single run may chain.
const maxDelegationResumes = 8

// executeAttempt runs one durable attempt end to end: it claims a lease, runs the
// model and tool loop against the durable checkpoint store, and then finalizes.
//
// The three outcomes are deliberately different:
//   - completed: the session turn and the durable terminal state commit together.
//   - suspended awaiting approval: nothing is committed, because the assistant's
//     tool calls have no results yet and a half-finished exchange would corrupt
//     the conversation history.
//   - failed: the failure is persisted as the run's terminal state so a retry
//     starts a new run instead of resurrecting a broken one.
func (a *App) executeAttempt(ctx context.Context, invocation runInvocation) (*agent.RunResult, error) {
	attemptNonce, err := NewSessionID()
	if err != nil {
		return nil, err
	}
	attemptKey := invocation.runKey + ":attempt:" + attemptNonce
	// Admission is what makes a shutting-down process refuse new work instead of
	// starting it and killing it a moment later.
	ctx, finish, admitted := a.runs.admit(ctx, invocation.runKey)
	if !admitted {
		return nil, fmt.Errorf("icoder is shutting down and is not accepting new runs")
	}
	defer finish()
	// The fence is only known after the store grants the lease, so the run
	// context holds a cell the AttemptAcquired hook fills in before any tool runs.
	fence := new(atomic.Uint64)
	ctx = withRunContext(ctx, runContext{
		id: invocation.runKey, attempt: attemptKey, fence: fence, session: invocation.sessionID, approve: invocation.approve,
		record: func(eventCtx context.Context, suffix, eventType string, payload any) error {
			return a.store.AppendRunEvent(eventCtx, invocation.sessionID, attemptKey+":"+suffix, eventType, payload)
		},
	})
	adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
		Store: a.durable, Ledger: a.durable, AttemptKey: durable.AttemptKey(attemptKey),
	})
	if err != nil {
		return nil, err
	}
	if err := a.store.AppendRunEvent(ctx, invocation.sessionID, attemptKey+":started", "agent.run.started", map[string]any{
		"revision": invocation.snapshot.Revision, "request_id": invocation.requestID, "run_key": invocation.runKey,
		"input_digest": invocation.inputDigest, "tool_generation": a.catalog.GenerationDigest(),
		"resumed": invocation.toolResume != nil,
	}); err != nil {
		return nil, err
	}
	// Lossless: these observations are the answer a person is watching appear,
	// not telemetry. The lossy default drops deltas whenever the terminal or the
	// TUI falls behind the model, which would truncate what the user reads while
	// the committed transcript stayed complete.
	emitter := agent.NewObservationEmitterWith(agent.ObservationOptions{QueueSize: 64, Lossless: true}, invocation.observe)
	result, runErr := a.runner.Run(ctx, agent.RunRequest{
		Messages:           invocation.messages,
		ObservationEmitter: emitter,
		DurableRun: &agent.DurableRunConfig{
			// The durable request id is the run key: the store enforces that one
			// session/request pair identifies exactly one run, and a retry after a
			// permanently failed run must be a new run rather than a conflict.
			Identity:        agent.RunIdentity{RunKey: invocation.runKey, AgentKey: agentKey, SessionID: invocation.sessionID, RequestID: invocation.runKey},
			CheckpointStore: adapter,
			LeaseOwner:      attemptKey,
			LeaseDuration:   runLeaseDuration,
			InputDigest:     invocation.inputDigest,
			ConfigDigest:    invocation.configDigest,
			PromptVersion:   a.prompt.Version(),
			PolicyVersion:   string(policyVersion),
			ToolResume:      invocation.toolResume,
			AttemptAcquired: func(_ agent.RunIdentity, token uint64) error {
				fence.Store(token)
				if invocation.observeFence != nil {
					invocation.observeFence(token)
				}
				return nil
			},
		},
	})
	emitter.Close()
	if runErr != nil {
		return result, a.finalizeFailure(ctx, invocation, attemptKey, adapter, result, runErr)
	}
	if suspension := toolSuspension(result); suspension != nil {
		if suspension.Kind != agent.ToolSuspensionExternal {
			// An approval needs a human, so the run stays parked until one answers.
			return result, a.recordSuspension(ctx, invocation, attemptKey, result)
		}
		return a.continueDelegation(ctx, invocation, attemptKey, result, suspension)
	}
	if err := a.store.CommitTurn(ctx, invocation.snapshot, invocation.requestID, attemptKey,
		digest([]byte(invocation.instruction)), agent.NewUserMessage(invocation.instruction), *result, result.DurableCompletion); err != nil {
		return result, err
	}
	return result, nil
}

// toolSuspension returns the handle a run parked on, or nil if it did not park
// on a tool at all.
func toolSuspension(result *agent.RunResult) *agent.ToolSuspension {
	if result == nil || result.Outcome != agent.OutcomeSuspended || result.StopReason != agent.StopReasonToolSuspended {
		return nil
	}
	if result.Suspension == nil {
		return nil
	}
	return result.Suspension.Tool
}

// continueDelegation drives the child a run parked on and then resumes the run.
//
// The parking is real even though the terminal never sees it: the parent's whole
// state is checkpointed before the child starts, so a process that dies here
// leaves a resumable run rather than losing the delegation and the tokens
// already spent on it. Resuming is a fresh attempt, which is what lets a
// different worker pick it up after a crash.
func (a *App) continueDelegation(ctx context.Context, invocation runInvocation, attemptKey string, parked *agent.RunResult, suspension *agent.ToolSuspension) (*agent.RunResult, error) {
	if invocation.delegationResumes >= maxDelegationResumes {
		return parked, errorsJoin(
			fmt.Errorf("run %q parked on delegated work %d times without finishing", invocation.runKey, invocation.delegationResumes),
			a.recordSuspension(ctx, invocation, attemptKey, parked),
		)
	}
	if err := a.store.AppendRunEvent(ctx, invocation.sessionID, attemptKey+":delegated", "agent.subagent.awaited", map[string]any{
		"run_key": invocation.runKey, "relationship_key": suspension.RequestRef,
	}); err != nil {
		return parked, err
	}
	if err := a.driveDelegations(ctx); err != nil {
		return parked, err
	}
	resumed := invocation
	resumed.toolResume = suspension
	resumed.delegationResumes++
	return a.executeAttempt(ctx, resumed)
}

// driveDelegations runs the queued children and delivers their wake facts.
//
// It claims until the queue is empty rather than once, because one delegate call
// can be part of a batch of tool calls the model made in the same step.
func (a *App) driveDelegations(ctx context.Context) error {
	if a.delegations == nil {
		return nil
	}
	for range maxDelegationResumes {
		_, claimed, err := a.delegations.RunNext(ctx, tenantKey)
		if err != nil {
			return err
		}
		if !claimed {
			break
		}
	}
	// Reconciliation is what turns a committed child terminal into the wake fact
	// the parent's event stream records.
	_, err := a.delegations.Reconcile(ctx, subagent.ReconcileRequest{TenantKey: tenantKey, Limit: maxDelegationResumes})
	return err
}

// finalizeFailure persists the terminal failure and its event on a context that
// survives cancellation, because a canceled run still has to leave a record.
func (a *App) finalizeFailure(ctx context.Context, invocation runInvocation, attemptKey string, adapter *durable.CheckpointAdapter, result *agent.RunResult, runErr error) error {
	eventType := "agent.run.failed"
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
		eventType = "agent.run.canceled"
	}
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	payload := map[string]any{"error": runErr.Error(), "run_key": invocation.runKey}
	if result != nil {
		payload["outcome"], payload["stop_reason"], payload["usage"] = result.Outcome, result.StopReason, result.Usage
		if result.DurableFailure != nil {
			if _, err := adapter.Fail(cleanup, result.DurableFailure.Guard, result.DurableFailure.Checkpoint, result.DurableFailure.Failure); err != nil {
				payload["finalize_error"] = err.Error()
			}
		}
	}
	if eventErr := a.store.AppendRunEvent(cleanup, invocation.sessionID, attemptKey+":terminal", eventType, payload); eventErr != nil {
		return errorsJoin(runErr, eventErr)
	}
	return runErr
}

// recordSuspension publishes the fact that a run is parked on an approval, with
// the run key a human needs to resume it.
func (a *App) recordSuspension(ctx context.Context, invocation runInvocation, attemptKey string, result *agent.RunResult) error {
	payload := map[string]any{"run_key": invocation.runKey, "stop_reason": result.StopReason, "usage": result.Usage}
	if result.Suspension != nil && result.Suspension.Tool != nil {
		payload["execution_key"] = result.Suspension.Tool.ExecutionKey
		payload["request_ref"] = result.Suspension.Tool.RequestRef
	}
	return a.store.AppendRunEvent(ctx, invocation.sessionID, attemptKey+":suspended", "agent.run.suspended", payload)
}

// PendingRun describes a durable run that is not finished, so a human can decide
// what to do with it.
type PendingRun struct {
	RunKey            string
	SessionID         string
	Status            string
	Phase             string
	AwaitingApproval  bool
	ApprovalRequestID string
	// AwaitingDelegation is set when the run parked on a child agent rather than
	// on a human. It needs work driven, not a decision, so offering it an
	// approval prompt would be an invitation to answer a question nobody asked.
	AwaitingDelegation bool
	UpdatedRevision    uint64
}

// PendingRuns lists durable runs that still hold state: suspended runs waiting
// for an approval, and runs whose worker stopped without finishing.
//
// Runs whose session turn was already committed are excluded, because from the
// conversation's point of view they are done even though the recovery record
// remains for auditing.
func (a *App) PendingRuns(ctx context.Context) ([]PendingRun, error) {
	page, err := a.durable.Scan(ctx, durable.ScanRequest{
		Limit:    100,
		Statuses: []durable.Status{durable.StatusSuspended, durable.StatusRunning, durable.StatusClaimed},
	})
	if err != nil {
		return nil, err
	}
	runs := make([]PendingRun, 0, len(page.Snapshots))
	for _, snapshot := range page.Snapshots {
		pending := PendingRun{
			RunKey: snapshot.Identity.RunKey, SessionID: snapshot.Identity.SessionID,
			Status: string(snapshot.Status), Phase: string(snapshot.Phase), UpdatedRevision: snapshot.Revision,
		}
		if suspension := snapshot.Checkpoint.Outcome.Suspension; suspension != nil && suspension.Tool != nil {
			switch suspension.Tool.Kind {
			case agent.ToolSuspensionExternal:
				pending.AwaitingDelegation = true
			default:
				pending.AwaitingApproval = true
			}
			pending.ApprovalRequestID = suspension.Tool.RequestRef
		}
		runs = append(runs, pending)
	}
	return runs, nil
}

// ResumeRun continues a suspended durable run. approve answers the pending
// approval; passing nil resumes without a decision, which only makes sense when
// the policy has changed in the meantime.
//
// The stored input and config digests are replayed verbatim so the resumed
// attempt is provably the same run rather than a similar one.
func (a *App) ResumeRun(ctx context.Context, runKey string, observe func(agent.Observation), approve ApprovalFunc) (*agent.RunResult, error) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	snapshot, err := a.durable.Load(ctx, durable.RunKey(runKey))
	if err != nil {
		return nil, err
	}
	if snapshot.Terminal() {
		return nil, fmt.Errorf("run %q is already %s", runKey, snapshot.Status)
	}
	suspension := snapshot.Checkpoint.Outcome.Suspension
	if suspension == nil || suspension.Tool == nil {
		return nil, fmt.Errorf("run %q is not waiting on a suspended tool", runKey)
	}
	if suspension.Tool.Kind == agent.ToolSuspensionExternal {
		// A run parked on a child agent is waiting for work, not for an answer.
		// The child may still be queued from before the restart, so drive it here
		// rather than resuming into a tool that would only park again.
		if err := a.driveDelegations(ctx); err != nil {
			return nil, err
		}
	}
	sessionSnapshot, _, err := a.store.Load(ctx, snapshot.Identity.SessionID)
	if err != nil {
		return nil, err
	}
	tool := *suspension.Tool
	input := runInputMessages(snapshot.Checkpoint)
	instruction, ok := lastUserInstruction(input)
	if !ok {
		return nil, fmt.Errorf("run %q has no recoverable user instruction", runKey)
	}
	return a.executeAttempt(ctx, runInvocation{
		sessionID: snapshot.Identity.SessionID, snapshot: sessionSnapshot,
		requestID: snapshot.Identity.RequestID, runKey: runKey, instruction: instruction,
		messages: input,
		// The stored digests are replayed verbatim rather than recomputed, so the
		// resumed attempt is provably the same run even if prompt rendering or
		// project instructions have drifted since it started.
		inputDigest: snapshot.InputDigest, configDigest: snapshot.ConfigDigest,
		toolResume: &tool, observe: observe, approve: approve,
	})
}

// runInputMessages recovers a run's immutable input from its checkpoint. History
// is the input followed by everything the run produced, and NewMessages is
// exactly that produced tail, so removing the tail leaves the original request.
func runInputMessages(checkpoint durable.Checkpoint) []agent.Message {
	boundary := len(checkpoint.History) - len(checkpoint.NewMessages)
	if boundary < 0 || boundary > len(checkpoint.History) {
		return checkpoint.History
	}
	return checkpoint.History[:boundary]
}

// lastUserInstruction recovers the task a suspended run was answering. User
// messages only ever enter a run through its immutable input, so the last one is
// the instruction this turn must be committed under.
func lastUserInstruction(input []agent.Message) (string, bool) {
	for i := len(input) - 1; i >= 0; i-- {
		if input[i].Role == agent.RoleUser {
			return input[i].Text(), true
		}
	}
	return "", false
}

// ResolveRunApproval records a human decision for the approval a suspended run
// is waiting on.
//
// It is deliberately separate from resuming. The decision is a durable fact that
// must exist before any attempt revalidates it, and a decision made here stays
// valid even if the resume that follows never happens.
func (a *App) ResolveRunApproval(ctx context.Context, runKey string, approved bool) error {
	snapshot, err := a.durable.Load(ctx, durable.RunKey(runKey))
	if err != nil {
		return err
	}
	suspension := snapshot.Checkpoint.Outcome.Suspension
	if suspension == nil || suspension.Tool == nil || suspension.Tool.RequestRef == "" {
		return fmt.Errorf("run %q is not waiting on a tool approval", runKey)
	}
	kind, reason := permission.ResolutionDeny, "cli-rejected"
	if approved {
		kind, reason = permission.ResolutionApprove, "cli-approved"
	}
	command := permission.ResolveCommand{
		TenantKey: tenantKey, RequestKey: permission.RequestKey(suspension.Tool.RequestRef),
		CommandKey: suspension.Tool.RequestRef + ":" + string(kind), ApproverKey: principalKey,
		ExpectedRevision: suspension.Tool.Revision, Kind: kind, ReasonCode: reason,
	}
	command.DecisionKey = permission.DecisionKey(command.CommandKey)
	_, _, err = a.permissions.Service().Resolve(ctx, command)
	return err
}

// AbandonRun ends a run nobody intends to finish. The lease is acquired first so
// the terminal write is authorized the same way every other durable write is.
func (a *App) AbandonRun(ctx context.Context, runKey string) error {
	now := time.Now().UTC()
	snapshot, err := a.durable.Acquire(ctx, durable.AcquireRequest{
		RunKey: durable.RunKey(runKey), Owner: "icoder-cli", Now: now, LeaseUntil: now.Add(time.Minute), AllowSuspended: true,
	})
	if err != nil {
		return err
	}
	_, err = a.durable.Save(ctx, durable.SaveRequest{
		Guard: snapshot.Guard(), Status: durable.StatusAbandoned, Phase: durable.PhaseTerminal, Checkpoint: snapshot.Checkpoint,
	})
	return err
}

// RunEffects reports what a run's tool calls actually did, which is the record an
// operator needs before deciding whether an interrupted run is safe to resume.
func (a *App) RunEffects(ctx context.Context, runKey string) ([]durable.EffectRecord, error) {
	return a.durable.ListEffects(ctx, durable.RunKey(runKey))
}
