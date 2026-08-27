package durable

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// EffectReplayer is an optional ExecutionLedger extension. A ledger implements
// it when it can re-arm an effect that a revoked lease left in EffectUnknown so
// that a tool whose ReplayPolicy proves the retry is safe can run again.
//
// Ledgers that do not implement it keep the conservative default: an unknown
// effect is never replayed automatically, because an unknown effect may already
// have changed the outside world. CheckpointAdapter only ever calls ReplayEffect
// after the runtime has told it that the tool opted into idempotent replay.
type EffectReplayer interface {
	ReplayEffect(context.Context, Guard, ExecutionKey, time.Time) (EffectRecord, error)
}

// CheckpointAdapterOptions configures one attempt's view of the durable store.
//
// A CheckpointAdapter is bound to a single attempt because AttemptKey is part of
// the effect ledger's immutable identity. Applications construct one per attempt
// and discard it when the attempt ends.
type CheckpointAdapterOptions struct {
	// Store is the durable run authority. Required.
	Store Store
	// Ledger records tool effects. Required; it is usually the same value as
	// Store because MemoryStore and most adapters implement both ports.
	Ledger ExecutionLedger
	// AttemptKey identifies the worker attempt that prepares effects. It is
	// recorded once per execution key and is not compared on later resumes.
	AttemptKey AttemptKey
	// Now supplies ledger timestamps. It defaults to time.Now.
	Now func() time.Time
}

// CheckpointAdapter exposes a durable Store and its effect ledger through the
// root agent.CheckpointStore port, so Agent.Run can execute durably against any
// durable adapter without the application re-deriving the phase state machine.
//
// The mapping is deliberately narrow:
//
//   - Run lifecycle mutations become exactly one Store.Save with the status and
//     phase the root runtime's next boundary requires.
//   - Tool lifecycle mutations become effect ledger commands keyed by
//     ToolExecutionKey, which is byte-identical to the runtime's
//     ToolExecution.IdempotencyKey, so both sides dedupe on the same anchor.
//   - Store errors are wrapped so callers can still test the root sentinels
//     (agent.ErrCheckpointConflict, agent.ErrToolExecutionUnknown,
//     agent.ErrInvalidRunTransition) as well as the durable sentinels.
//
// Like every CheckpointStore, it does not promise exactly-once side effects. It
// promises that an effect whose result was never committed is never silently
// replayed unless the tool proved that replaying it is safe.
type CheckpointAdapter struct {
	store   Store
	ledger  ExecutionLedger
	replay  EffectReplayer
	attempt AttemptKey
	now     func() time.Time
}

// NewCheckpointAdapter validates the ports an attempt needs before any run can
// begin, so a misconfigured composition fails at assembly instead of mid-run.
func NewCheckpointAdapter(options CheckpointAdapterOptions) (*CheckpointAdapter, error) {
	if options.Store == nil || options.Ledger == nil {
		return nil, durableError(ErrSnapshotIncoherent, "new checkpoint adapter", "", "store and ledger are required")
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	adapter := &CheckpointAdapter{store: options.Store, ledger: options.Ledger, attempt: options.AttemptKey, now: now}
	if replayer, ok := options.Ledger.(EffectReplayer); ok {
		adapter.replay = replayer
	}
	return adapter, nil
}

var _ agent.CheckpointStore = (*CheckpointAdapter)(nil)

// Begin is create-if-absent by run key and returns the existing snapshot when
// the immutable identity and digests match.
func (a *CheckpointAdapter) Begin(ctx context.Context, identity agent.RunIdentity, inputDigest, configDigest string, checkpoint agent.Checkpoint) (agent.RunSnapshot, error) {
	snapshot, _, err := a.store.Begin(ctx, BeginRequest{
		Identity:    Identity{RunKey: identity.RunKey, AgentKey: identity.AgentKey, SessionID: identity.SessionID, RequestID: identity.RequestID},
		InputDigest: inputDigest, ConfigDigest: configDigest, Checkpoint: checkpoint,
	})
	if err != nil {
		return agent.RunSnapshot{}, mapDurableError("begin", err)
	}
	return runSnapshot(snapshot), nil
}

// Acquire takes over an expired or suspended lease and returns the new fence.
// Suspended runs are acquirable because approval and sub-agent suspensions are
// resumed by a later attempt; a lease that is still live is refused.
func (a *CheckpointAdapter) Acquire(ctx context.Context, runKey, leaseOwner string, leaseUntil time.Time) (agent.RunSnapshot, error) {
	snapshot, err := a.store.Acquire(ctx, AcquireRequest{
		RunKey: RunKey(runKey), Owner: leaseOwner, Now: a.now(), LeaseUntil: leaseUntil, AllowSuspended: true,
	})
	if err != nil {
		return agent.RunSnapshot{}, mapDurableError("acquire", err)
	}
	return runSnapshot(snapshot), nil
}

// ModelInflight records the intent to call the model. It proves nothing about
// whether the provider observed the request, which is why recovery repeats it.
func (a *CheckpointAdapter) ModelInflight(ctx context.Context, guard agent.MutationGuard, checkpoint agent.Checkpoint) (agent.RunSnapshot, error) {
	return a.save(ctx, "model inflight", guard, StatusRunning, PhaseModelInflight, checkpoint, nil)
}

// CommitModelResponse durably records the response that is already folded into
// the checkpoint. The response argument carries no state the checkpoint lacks,
// so it is not persisted twice.
func (a *CheckpointAdapter) CommitModelResponse(ctx context.Context, guard agent.MutationGuard, _ agent.Response, checkpoint agent.Checkpoint) (agent.RunSnapshot, error) {
	return a.save(ctx, "commit model response", guard, StatusRunning, phaseAfterModel(checkpoint), checkpoint, nil)
}

// PrepareTools creates every effect record before any tool runs, so a crash
// during the batch always finds a prepared row to reconcile against.
func (a *CheckpointAdapter) PrepareTools(ctx context.Context, guard agent.MutationGuard, executions []agent.ToolExecution, checkpoint agent.Checkpoint) (agent.RunSnapshot, error) {
	snapshot, err := a.save(ctx, "prepare tools", guard, StatusRunning, PhaseToolsReady, checkpoint, nil)
	if err != nil {
		return agent.RunSnapshot{}, err
	}
	prepared := toDurableGuard(snapshot.Guard())
	preparedAt := a.now()
	for _, execution := range executions {
		// PrepareEffect is idempotent by execution key, so a resumed attempt
		// re-preparing an open batch adopts the existing records rather than
		// creating or conflicting with them.
		if _, _, err := a.ledger.PrepareEffect(ctx, PrepareEffectRequest{
			Guard: prepared, AttemptKey: a.attempt, StepNumber: execution.StepNumber,
			Ordinal: execution.Ordinal, ToolCall: execution.ToolCall, PreparedAt: preparedAt,
		}); err != nil {
			return agent.RunSnapshot{}, mapDurableError("prepare tools", err)
		}
	}
	return snapshot, nil
}

// BeginTool marks exactly one effect running before its external call.
//
// Three cases matter, and conflating any two of them either repeats an effect or
// strands a run:
//
//   - The effect already carries a durable result. It is returned as a completed
//     execution so the runtime reuses that result instead of running the tool
//     again. This is the case a crash between committing an effect and
//     committing its checkpoint leaves behind.
//   - The effect is prepared, or already running under this fence. It is armed
//     normally.
//   - A revoked lease left the effect unknown. It is refused unless the runtime
//     proved the tool is safe to replay and the ledger implements EffectReplayer.
func (a *CheckpointAdapter) BeginTool(ctx context.Context, guard agent.MutationGuard, idempotencyKey string, safeReplay bool) (agent.RunSnapshot, agent.ToolExecution, error) {
	snapshot, err := a.store.Load(ctx, RunKey(guard.RunKey))
	if err != nil {
		return agent.RunSnapshot{}, agent.ToolExecution{}, mapDurableError("begin tool", err)
	}
	key := ExecutionKey(idempotencyKey)
	if settled, ok, err := a.settledEffect(ctx, key); err != nil {
		return agent.RunSnapshot{}, agent.ToolExecution{}, err
	} else if ok {
		return runSnapshot(snapshot), toolExecution(guard.RunKey, settled), nil
	}
	record, err := a.ledger.BeginEffect(ctx, toDurableGuard(guard), key, a.now())
	if errors.Is(err, ErrToolEffectUnknown) && safeReplay && a.replay != nil {
		record, err = a.replay.ReplayEffect(ctx, toDurableGuard(guard), key, a.now())
	}
	if err != nil {
		return agent.RunSnapshot{}, agent.ToolExecution{}, mapDurableError("begin tool", err)
	}
	return runSnapshot(snapshot), toolExecution(guard.RunKey, record), nil
}

// settledEffect reports an effect whose outcome is already durably recorded.
// Only a record that actually carries a result counts: a failure with no result
// proves nothing about what the outside world saw, so it stays on the unknown
// path rather than being replayed as an answer.
func (a *CheckpointAdapter) settledEffect(ctx context.Context, key ExecutionKey) (EffectRecord, bool, error) {
	record, err := a.ledger.LoadEffect(ctx, key)
	if errors.Is(err, ErrEffectNotFound) {
		return EffectRecord{}, false, nil
	}
	if err != nil {
		return EffectRecord{}, false, mapDurableError("begin tool", err)
	}
	settled := record.Status == EffectSucceeded || record.Status == EffectFailed
	return record, settled && record.Result != nil, nil
}

// CommitTool stores one complete tool result and advances the checkpoint in the
// order a crash can tolerate: the effect result first, then the run phase.
func (a *CheckpointAdapter) CommitTool(ctx context.Context, guard agent.MutationGuard, execution agent.ToolExecution, checkpoint agent.Checkpoint) (agent.RunSnapshot, error) {
	if execution.Status == agent.ToolExecutionCompleted && execution.Result != nil {
		if err := a.completeEffect(ctx, guard, ExecutionKey(execution.IdempotencyKey), *execution.Result); err != nil {
			return agent.RunSnapshot{}, err
		}
	}
	return a.save(ctx, "commit tool", guard, StatusRunning, phaseAfterTool(checkpoint), checkpoint, nil)
}

// completeEffect records a finished tool call, arming the effect first when the
// runtime never called BeginTool.
//
// That happens for a tool that owns its own execution lifecycle - an advanced
// tool.Executor keeps its own ledger and the runtime steps aside. The durable
// effect record still has to reach a terminal state, or reconciliation would
// later see a prepared effect for work that is provably finished.
func (a *CheckpointAdapter) completeEffect(ctx context.Context, guard agent.MutationGuard, key ExecutionKey, result agent.ToolResult) error {
	durableGuard := toDurableGuard(guard)
	record, err := a.ledger.LoadEffect(ctx, key)
	if errors.Is(err, ErrEffectNotFound) {
		return nil
	}
	if err != nil {
		return mapDurableError("commit tool", err)
	}
	if record.Status == EffectPrepared {
		if _, err := a.ledger.BeginEffect(ctx, durableGuard, key, a.now()); err != nil {
			return mapDurableError("commit tool", err)
		}
	}
	stored := result
	if _, err := a.ledger.CompleteEffect(ctx, CompleteEffectRequest{
		Guard: durableGuard, ExecutionKey: key, Result: &stored, FinishedAt: a.now(),
	}); err != nil && !errors.Is(err, ErrEffectNotFound) {
		return mapDurableError("commit tool", err)
	}
	return nil
}

// Complete finalizes a successful run. The store rejects it unless the previous
// boundary already moved the run into the finalizing phase.
func (a *CheckpointAdapter) Complete(ctx context.Context, guard agent.MutationGuard, checkpoint agent.Checkpoint) (agent.RunSnapshot, error) {
	return a.save(ctx, "complete", guard, StatusCompleted, PhaseTerminal, checkpoint, nil)
}

// Suspend persists the resumable checkpoint, including any tool suspension, and
// releases the lease. It uses Save rather than Release because a suspended run
// must keep the blocker that a later attempt resumes from.
func (a *CheckpointAdapter) Suspend(ctx context.Context, guard agent.MutationGuard, checkpoint agent.Checkpoint) (agent.RunSnapshot, error) {
	return a.save(ctx, "suspend", guard, StatusSuspended, suspendPhase(checkpoint), checkpoint, nil)
}

// Fail persists a permanent terminal failure. Error values are process-local,
// so only the serializable RunFailure crosses the boundary.
func (a *CheckpointAdapter) Fail(ctx context.Context, guard agent.MutationGuard, checkpoint agent.Checkpoint, failure agent.RunFailure) (agent.RunSnapshot, error) {
	stored := failure
	return a.save(ctx, "fail", guard, StatusFailed, PhaseTerminal, checkpoint, &stored)
}

// Load is read-only: it neither acquires a lease nor authorizes a later write.
func (a *CheckpointAdapter) Load(ctx context.Context, runKey string) (agent.RunSnapshot, error) {
	snapshot, err := a.store.Load(ctx, RunKey(runKey))
	if err != nil {
		return agent.RunSnapshot{}, mapDurableError("load", err)
	}
	return runSnapshot(snapshot), nil
}

func (a *CheckpointAdapter) save(ctx context.Context, operation string, guard agent.MutationGuard, status Status, phase Phase, checkpoint agent.Checkpoint, failure *Failure) (agent.RunSnapshot, error) {
	snapshot, err := a.store.Save(ctx, SaveRequest{
		Guard: toDurableGuard(guard), Status: status, Phase: phase, Checkpoint: checkpoint, Failure: failure,
	})
	if err != nil {
		return agent.RunSnapshot{}, mapDurableError(operation, err)
	}
	return runSnapshot(snapshot), nil
}

// phaseAfterModel picks the boundary the runtime will reach next: pending calls
// mean tools run, a completed outcome means the domain owner finalizes, and
// anything else means another model step.
func phaseAfterModel(checkpoint agent.Checkpoint) Phase {
	if len(checkpoint.PendingToolCalls) > 0 {
		return PhaseToolsReady
	}
	if checkpoint.Outcome.Outcome == agent.OutcomeCompleted {
		return PhaseFinalizing
	}
	return PhaseModelReady
}

// phaseAfterTool keeps the run in the tools phase while the batch is still open.
// The runtime clears PendingToolCalls only when it commits the last call.
func phaseAfterTool(checkpoint agent.Checkpoint) Phase {
	if len(checkpoint.PendingToolCalls) > 0 {
		return PhaseToolsReady
	}
	if checkpoint.Outcome.Outcome == agent.OutcomeCompleted {
		return PhaseFinalizing
	}
	return PhaseModelReady
}

// suspendPhase reports the only two phases a suspended v1 snapshot may hold.
func suspendPhase(checkpoint agent.Checkpoint) Phase {
	if len(checkpoint.PendingToolCalls) > 0 {
		return PhaseToolsReady
	}
	return PhaseModelReady
}

func toDurableGuard(guard agent.MutationGuard) Guard {
	return Guard{RunKey: RunKey(guard.RunKey), LeaseOwner: guard.LeaseOwner, Revision: guard.Revision, FenceToken: guard.FenceToken}
}

func runSnapshot(snapshot Snapshot) agent.RunSnapshot {
	return agent.RunSnapshot{
		SchemaVersion: snapshot.SchemaVersion,
		Identity: agent.RunIdentity{
			RunKey: snapshot.Identity.RunKey, AgentKey: snapshot.Identity.AgentKey,
			SessionID: snapshot.Identity.SessionID, RequestID: snapshot.Identity.RequestID,
		},
		InputDigest: snapshot.InputDigest, ConfigDigest: snapshot.ConfigDigest,
		Status: agent.RunStatus(snapshot.Status), Phase: agent.RunPhase(snapshot.Phase),
		Revision: snapshot.Revision, FenceToken: snapshot.FenceToken,
		LeaseOwner: snapshot.LeaseOwner, LeaseUntil: snapshot.LeaseUntil,
		Checkpoint: snapshot.Checkpoint, Failure: snapshot.Failure,
	}
}

func toolExecution(runKey string, record EffectRecord) agent.ToolExecution {
	execution := agent.ToolExecution{
		RunKey: runKey, StepNumber: record.StepNumber, Ordinal: record.Ordinal,
		ToolCall: record.ToolCall, IdempotencyKey: string(record.ExecutionKey),
		InputHash: record.Digest, Status: toolExecutionStatus(record.Status), Result: record.Result,
	}
	return execution
}

func toolExecutionStatus(status EffectStatus) agent.ToolExecutionStatus {
	switch status {
	case EffectRunning:
		return agent.ToolExecutionExecuting
	case EffectSucceeded, EffectFailed:
		return agent.ToolExecutionCompleted
	case EffectUnknown:
		return agent.ToolExecutionUnknown
	default:
		return agent.ToolExecutionPrepared
	}
}

// mapDurableError keeps the durable sentinel testable while also satisfying the
// root sentinel the runtime branches on, so neither layer has to know the
// other's error vocabulary.
func mapDurableError(operation string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrToolEffectUnknown):
		return fmt.Errorf("durable %s: %w: %w", operation, agent.ErrToolExecutionUnknown, err)
	case errors.Is(err, ErrLeaseLost), errors.Is(err, ErrLeaseHeld), errors.Is(err, ErrRunConflict), errors.Is(err, ErrEffectConflict), errors.Is(err, ErrUsageConflict):
		return fmt.Errorf("durable %s: %w: %w", operation, agent.ErrCheckpointConflict, err)
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrTerminal):
		return fmt.Errorf("durable %s: %w: %w", operation, agent.ErrInvalidRunTransition, err)
	case errors.Is(err, ErrSnapshotSchema):
		return fmt.Errorf("durable %s: %w: %w", operation, agent.ErrUnsupportedRunSnapshotSchema, err)
	default:
		return fmt.Errorf("durable %s: %w", operation, err)
	}
}
