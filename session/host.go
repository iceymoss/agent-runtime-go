package session

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type hostState uint8

const (
	hostAccepting hostState = iota
	hostDraining
	hostStopped
)

type SessionAgent struct {
	options Options
	clock   func() time.Time

	mu           sync.Mutex
	state        hostState
	runs         map[RunKey]struct{}
	active       map[RunKey]context.CancelFunc
	workerCancel context.CancelFunc
	workers      sync.WaitGroup
	drainDone    chan struct{}
	drainResult  DrainResult
}

func New(options Options) (*SessionAgent, error) {
	if err := validateHostOptions(options); err != nil {
		return nil, err
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	if options.CleanupTimeout == 0 {
		options.CleanupTimeout = 5 * time.Second
	}
	return &SessionAgent{options: options, clock: clock, runs: make(map[RunKey]struct{}), active: make(map[RunKey]context.CancelFunc)}, nil
}

func (a *SessionAgent) Run(ctx context.Context, request RunRequest) (RunReceipt, error) {
	if err := validateRunRequest(ctx, request); err != nil {
		return RunReceipt{}, err
	}
	selectors := append([]SelectorValue(nil), request.Selectors...)
	execution, err := a.options.Definitions.Resolve(ctx, DefinitionRequest{TenantKey: request.TenantKey, AgentKey: request.AgentKey, Selectors: selectors})
	if err != nil {
		return RunReceipt{}, err
	}
	if execution.Definition == nil || execution.DefinitionDigest == "" {
		return RunReceipt{}, fmt.Errorf("%w: incomplete resolved definition", ErrGenerationUnavailable)
	}
	execution, err = cloneExecution(execution)
	if err != nil {
		return RunReceipt{}, err
	}
	messages := cloneMessages(request.Messages)
	policy := cloneStepPolicy(request.StepPolicy)
	policyDigest := ""
	if len(policy.Steps) > 0 || policy.Schema != "" {
		policyDigest, err = policy.Digest()
		if err != nil {
			return RunReceipt{}, err
		}
	}
	inputDigest, err := agent.CanonicalDigest(messages)
	if err != nil {
		return RunReceipt{}, fmt.Errorf("digest run input: %w", err)
	}
	configDigest, err := agent.CanonicalDigest(struct {
		AgentKey  string
		Selectors []SelectorValue
		Merge     MergeStrategy
		Policy    string
	}{request.AgentKey, selectors, normalizedMerge(request.Merge), policyDigest})
	if err != nil {
		return RunReceipt{}, fmt.Errorf("digest run config: %w", err)
	}
	runKey := stableRunKey(request.TenantKey, request.SessionKey, request.RequestID)
	branchKey := makeBranchKey(request.TenantKey, runKey)
	planDigest, err := agent.CanonicalDigest(struct {
		Input      string
		Definition string
	}{inputDigest, execution.DefinitionDigest})
	if err != nil {
		return RunReceipt{}, fmt.Errorf("digest context plan: %w", err)
	}
	now := a.clock().UTC()
	admission := Admission{
		TenantKey: request.TenantKey, SessionKey: request.SessionKey, BranchKey: branchKey, RunKey: runKey,
		RequestID: request.RequestID, AgentKey: request.AgentKey, BaseRevision: cloneUint64(request.BaseRevision),
		DefinitionDigest: execution.DefinitionDigest, ContextPlanKey: "plan_" + planDigest[:32], ContextPlanDigest: planDigest,
		InputDigest: inputDigest, ConfigDigest: configDigest, StepPolicy: policy, StepPolicyDigest: policyDigest, Messages: messages, Merge: normalizedMerge(request.Merge), Priority: request.Priority, CreatedAt: now,
	}

	// StopAdmission and the admission commit share this lock, making their race
	// linearizable without holding it across definition resolution.
	a.mu.Lock()
	if a.state != hostAccepting {
		a.mu.Unlock()
		return RunReceipt{}, ErrAdmissionClosed
	}
	receipt, _, err := a.options.Store.AdmitAndBegin(ctx, AdmitAndBeginCommand{Admission: admission, Execution: execution, Limits: a.options.Limits})
	if err == nil {
		a.runs[receipt.RunKey] = struct{}{}
	}
	a.mu.Unlock()
	if err != nil {
		return RunReceipt{}, err
	}
	if a.options.Events != nil {
		notifyCtx, cancel := context.WithTimeout(context.Background(), a.options.CleanupTimeout)
		err = a.options.Events.PublishAfterCommit(notifyCtx, receipt.RunKey)
		cancel()
		if err != nil {
			return receipt, fmt.Errorf("publish accepted run: %w", err)
		}
	}
	return receipt, nil
}

func (a *SessionAgent) Await(ctx context.Context, runKey RunKey) (RunResult, error) {
	if ctx == nil {
		return RunResult{}, fmt.Errorf("%w: nil context", ErrInvalidCommand)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		result, err := a.Get(ctx, runKey)
		if err != nil {
			return RunResult{}, err
		}
		if settled(result.Receipt) {
			return result, nil
		}
		select {
		case <-ctx.Done():
			return RunResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *SessionAgent) Get(ctx context.Context, runKey RunKey) (RunResult, error) {
	result, err := a.options.Store.Get(ctx, runKey)
	if err != nil {
		return RunResult{}, err
	}
	return result, nil
}

func (a *SessionAgent) Merge(ctx context.Context, request MergeRequest) (MergeResult, error) {
	if ctx == nil || !request.TenantKey.Valid() || !request.RunKey.Valid() {
		return MergeResult{}, fmt.Errorf("%w: invalid merge request", ErrInvalidCommand)
	}
	if request.Strategy == "" {
		request.Strategy = MergeFastForward
	}
	stored, err := a.options.Store.Get(ctx, request.RunKey)
	if err != nil {
		return MergeResult{}, err
	}
	if stored.CoreResult == nil {
		return MergeResult{}, ErrInvalidTransition
	}
	result, err := a.options.Store.MergeAndFinalize(ctx, MergeAndFinalizeCommand{Request: request, DurableResult: *cloneCoreResult(stored.CoreResult)})
	a.publish(ctx, request.RunKey, err)
	return result, err
}

func (a *SessionAgent) Cancel(ctx context.Context, request CancelRequest) (CancelResult, error) {
	result, err := a.options.Store.RequestCancel(ctx, request)
	if err != nil {
		return CancelResult{}, err
	}
	if result.Requested && !result.AlreadyFinal {
		interruptErr := a.options.Attempts.Interrupt(ctx, request.RunKey, request.Mode)
		if interruptErr != nil && !errors.Is(interruptErr, ErrRunNotFound) {
			return result, interruptErr
		}
		a.mu.Lock()
		cancel := a.active[request.RunKey]
		a.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	a.publish(ctx, request.RunKey, nil)
	return result, nil
}

// Resume explicitly returns retryable suspended work to the durable queue.
// Suspended work is otherwise inert and is never resumed by reconciliation.
func (a *SessionAgent) Resume(ctx context.Context, request ResumeRequest) (ResumeResult, error) {
	if ctx == nil || !request.TenantKey.Valid() || !request.RunKey.Valid() {
		return ResumeResult{}, fmt.Errorf("%w: invalid resume request", ErrInvalidCommand)
	}
	result, err := a.options.Store.Resume(ctx, request, a.options.Limits)
	if err != nil {
		return ResumeResult{}, err
	}
	if result.Resumed {
		a.mu.Lock()
		a.runs[request.RunKey] = struct{}{}
		a.mu.Unlock()
		a.publish(ctx, request.RunKey, nil)
	}
	return result, nil
}

func (a *SessionAgent) StopAdmission() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != hostAccepting {
		return false
	}
	a.state = hostDraining
	return true
}

// RunNext claims and executes at most one durable admission.
func (a *SessionAgent) RunNext(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, fmt.Errorf("%w: nil context", ErrInvalidCommand)
	}
	now := a.clock().UTC()
	if err := a.options.Store.Reconcile(ctx, now); err != nil {
		return false, err
	}
	claim, ok, err := a.options.Store.ClaimNext(ctx, a.options.WorkerID, now.Add(a.options.LeaseDuration), a.options.Limits)
	if err != nil || !ok {
		return ok, err
	}
	return true, a.executeClaim(ctx, claim)
}

// RunWorker runs a claim loop until ctx is canceled. No worker starts in New.
func (a *SessionAgent) RunWorker(ctx context.Context) error {
	for {
		didWork, err := a.RunNext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if didWork {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (a *SessionAgent) StartWorkers(ctx context.Context, count int) error {
	if ctx == nil || count <= 0 {
		return fmt.Errorf("%w: worker context and positive count required", ErrInvalidCommand)
	}
	a.mu.Lock()
	if a.workerCancel != nil || a.state == hostStopped {
		a.mu.Unlock()
		return ErrInvalidTransition
	}
	workerCtx, cancel := context.WithCancel(ctx)
	a.workerCancel = cancel
	for i := 0; i < count; i++ {
		a.workers.Add(1)
		go func() {
			defer a.workers.Done()
			if err := a.RunWorker(workerCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				return
			}
		}()
	}
	a.mu.Unlock()
	return nil
}

func (a *SessionAgent) executeClaim(workerCtx context.Context, claim Claim) error {
	result, err := a.options.Store.Get(workerCtx, claim.RunKey)
	if err != nil {
		return err
	}
	if result.Receipt.ExecutionState == ExecutionMergePending {
		return a.resumeFinalization(workerCtx, claim, result)
	}
	execution, err := a.options.Definitions.ResolveGeneration(workerCtx, claim.TenantKey, result.Receipt.DefinitionDigest)
	if err != nil {
		failure := Failure{Code: "generation_unavailable", Message: err.Error(), Retryable: true, Cause: ErrGenerationUnavailable}
		err = a.options.Store.MarkSuspended(context.WithoutCancel(workerCtx), claim, failure)
		a.publish(context.WithoutCancel(workerCtx), claim.RunKey, err)
		return err
	}
	if execution.Definition == nil || execution.DefinitionDigest != result.Receipt.DefinitionDigest {
		failure := Failure{Code: "generation_unavailable", Message: "resolved generation does not match admission", Retryable: true, Cause: ErrGenerationUnavailable}
		err = a.options.Store.MarkSuspended(context.WithoutCancel(workerCtx), claim, failure)
		a.publish(context.WithoutCancel(workerCtx), claim.RunKey, err)
		return err
	}
	input, err := a.options.Store.LoadBranchInput(workerCtx, claim.RunKey)
	if err != nil {
		return err
	}
	policy, policyDigest, err := a.options.Store.LoadStepPolicy(workerCtx, claim.RunKey)
	if err != nil {
		return err
	}
	execution, err = cloneExecution(execution)
	if err != nil {
		return err
	}
	attemptCtx, cancel := context.WithCancel(workerCtx)
	a.mu.Lock()
	a.active[claim.RunKey] = cancel
	a.mu.Unlock()
	attempt, attemptErr := a.options.Attempts.Resume(attemptCtx, AttemptRequest{TenantKey: claim.TenantKey, RunKey: claim.RunKey, BranchKey: claim.BranchKey, ContextPlanKey: result.Receipt.ContextPlanKey, ContextPlanDigest: result.Receipt.ContextPlanDigest, Execution: execution, Input: input, StepPolicy: policy, StepPolicyDigest: policyDigest})
	cancel()
	a.mu.Lock()
	delete(a.active, claim.RunKey)
	a.mu.Unlock()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), a.options.CleanupTimeout)
	defer cleanupCancel()
	if attemptErr != nil {
		failure := Failure{Code: "attempt_failed", Message: attemptErr.Error(), Retryable: false, Cause: attemptErr}
		if attempt.Failure != nil {
			failure = *attempt.Failure
		}
		if attempt.Result != nil && (attempt.Result.DurableFailure != nil || attempt.Result.DurableSuspension != nil) {
			if err = a.options.Store.MarkRunning(cleanupCtx, claim, attempt.Fence); err != nil {
				return err
			}
			err = a.options.Store.FinalizeFailure(cleanupCtx, FinalizeFailureCommand{Claim: claim, DurableFence: attempt.Fence, DurableResult: *attempt.Result, Failure: failure})
			if err != nil {
				_ = a.options.Store.Release(cleanupCtx, claim)
			}
			a.publish(cleanupCtx, claim.RunKey, err)
			return err
		}
		if errors.Is(attemptErr, context.Canceled) || errors.Is(attemptErr, context.DeadlineExceeded) {
			err = a.options.Store.MarkSuspended(cleanupCtx, claim, failure)
			if errors.Is(err, ErrCanceled) {
				err = nil
			}
			a.publish(cleanupCtx, claim.RunKey, err)
			return err
		}
		err = a.options.Store.MarkFailed(cleanupCtx, claim, failure)
		if errors.Is(err, ErrCanceled) {
			err = nil
		}
		a.publish(cleanupCtx, claim.RunKey, err)
		return err
	}
	if err := a.options.Store.MarkRunning(cleanupCtx, claim, attempt.Fence); err != nil {
		if errors.Is(err, ErrCanceled) {
			a.publish(cleanupCtx, claim.RunKey, nil)
			return nil
		}
		return err
	}
	if attempt.Outcome == agent.OutcomeSuspended {
		failure := Failure{Code: "generation_suspended", Message: "generation suspended", Retryable: false}
		if attempt.Failure != nil {
			failure = *attempt.Failure
		}
		return a.options.Store.MarkSuspended(cleanupCtx, claim, failure)
	}
	if attemptErr != nil || attempt.Outcome == agent.OutcomeFailed || attempt.Result == nil {
		failure := Failure{Code: "failed", Message: "attempt failed"}
		if attempt.Failure != nil {
			failure = *attempt.Failure
		}
		return a.options.Store.MarkFailed(cleanupCtx, claim, failure)
	}
	if err := a.options.Store.MarkMergePending(cleanupCtx, claim, *attempt.Result); err != nil {
		return err
	}
	if result.Receipt.ExecutionState == ExecutionQueued {
		// The immutable merge policy is not projected in RunReceipt. Reference
		// stores expose it through the narrow composition reader below.
	}
	strategy := claim.Merge
	if strategy == MergeNone {
		if err := a.options.Store.Release(cleanupCtx, claim); err != nil {
			return err
		}
		a.publish(cleanupCtx, claim.RunKey, nil)
		return nil
	}
	_, err = a.options.Store.MergeAndFinalize(cleanupCtx, MergeAndFinalizeCommand{Request: MergeRequest{TenantKey: claim.TenantKey, RunKey: claim.RunKey, Strategy: strategy, ExpectedBase: result.Receipt.BaseRevision}, Claim: claim, DurableFence: attempt.Fence, DurableResult: *attempt.Result})
	a.publish(cleanupCtx, claim.RunKey, err)
	if errors.Is(err, ErrMergeConflict) || errors.Is(err, ErrCanceled) {
		return nil
	}
	if err != nil {
		_ = a.options.Store.Release(cleanupCtx, claim)
	}
	return err
}

func (a *SessionAgent) resumeFinalization(workerCtx context.Context, claim Claim, stored RunResult) error {
	if stored.CoreResult == nil {
		return &TerminalStateError{RunKey: claim.RunKey, State: ExecutionMergePending, Reason: "persisted result is missing"}
	}
	execution, err := a.options.Definitions.ResolveGeneration(workerCtx, claim.TenantKey, stored.Receipt.DefinitionDigest)
	if err != nil {
		return err
	}
	input, err := a.options.Store.LoadBranchInput(workerCtx, claim.RunKey)
	if err != nil {
		return err
	}
	policy, policyDigest, err := a.options.Store.LoadStepPolicy(workerCtx, claim.RunKey)
	if err != nil {
		return err
	}
	resumer, ok := a.options.Attempts.(FinalizationResumer)
	if !ok {
		return &TerminalStateError{RunKey: claim.RunKey, State: ExecutionMergePending, Reason: "attempt runner cannot resume finalization"}
	}
	core, err := resumer.ResumeFinalization(workerCtx, AttemptRequest{TenantKey: claim.TenantKey, RunKey: claim.RunKey, BranchKey: claim.BranchKey, ContextPlanKey: stored.Receipt.ContextPlanKey, ContextPlanDigest: stored.Receipt.ContextPlanDigest, Execution: execution, Input: input, StepPolicy: policy, StepPolicyDigest: policyDigest}, *stored.CoreResult)
	if err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), a.options.CleanupTimeout)
	defer cancel()
	if core.DurableFailure != nil || core.DurableSuspension != nil {
		failure := Failure{Code: "attempt_failed", Message: "attempt failed"}
		if stored.Failure != nil {
			failure = *stored.Failure
		}
		err = a.options.Store.FinalizeFailure(cleanupCtx, FinalizeFailureCommand{Claim: claim, DurableFence: core.DurableFence, DurableResult: core, Failure: failure})
	} else {
		_, err = a.options.Store.MergeAndFinalize(cleanupCtx, MergeAndFinalizeCommand{Request: MergeRequest{TenantKey: claim.TenantKey, RunKey: claim.RunKey, Strategy: claim.Merge, ExpectedBase: stored.Receipt.BaseRevision}, Claim: claim, DurableFence: core.DurableFence, DurableResult: core})
	}
	a.publish(cleanupCtx, claim.RunKey, err)
	if errors.Is(err, ErrMergeConflict) || errors.Is(err, ErrCanceled) {
		return nil
	}
	if err != nil {
		_ = a.options.Store.Release(cleanupCtx, claim)
	}
	return err
}

func (a *SessionAgent) Drain(ctx context.Context) (DrainResult, error) {
	if ctx == nil {
		return DrainResult{}, fmt.Errorf("%w: nil context", ErrInvalidCommand)
	}
	a.StopAdmission()
	for {
		result := a.drainSnapshot(ctx)
		if result.Remaining == 0 {
			return result, nil
		}
		didWork, err := a.RunNext(ctx)
		if ctx.Err() != nil {
			a.interruptActive()
			return a.drainSnapshot(context.Background()), ErrDrainDeadline
		}
		if err != nil && ctx.Err() == nil {
			return result, err
		}
		if !didWork {
			select {
			case <-ctx.Done():
				a.interruptActive()
				result = a.drainSnapshot(context.Background())
				return result, ErrDrainDeadline
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

func (a *SessionAgent) Shutdown(ctx context.Context) (DrainResult, error) {
	result, err := a.Drain(ctx)
	a.mu.Lock()
	if a.workerCancel != nil {
		a.workerCancel()
	}
	a.state = hostStopped
	a.mu.Unlock()
	if err == nil {
		done := make(chan struct{})
		go func() { a.workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			err = ErrDrainDeadline
		}
	}
	return result, err
}

func (a *SessionAgent) drainSnapshot(ctx context.Context) DrainResult {
	a.mu.Lock()
	keys := make([]RunKey, 0, len(a.runs))
	for key := range a.runs {
		keys = append(keys, key)
	}
	a.mu.Unlock()
	var result DrainResult
	for _, key := range keys {
		run, err := a.options.Store.Get(ctx, key)
		if err != nil {
			result.Remaining++
			continue
		}
		switch run.Receipt.ExecutionState {
		case ExecutionReleased:
			result.Completed++
		case ExecutionSuspended, ExecutionMergePending:
			result.Suspended++
		case ExecutionFailed:
			result.Failed++
		default:
			result.Remaining++
		}
	}
	return result
}

func (a *SessionAgent) interruptActive() {
	a.mu.Lock()
	keys := make([]RunKey, 0, len(a.active))
	for key, cancel := range a.active {
		cancel()
		keys = append(keys, key)
	}
	a.mu.Unlock()
	for _, key := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), a.options.CleanupTimeout)
		err := a.options.Attempts.Interrupt(ctx, key, CancelAttempt)
		cancel()
		if err != nil {
			continue
		}
	}
}

func (a *SessionAgent) publish(ctx context.Context, runKey RunKey, prior error) {
	if prior != nil || a.options.Events == nil {
		return
	}
	if err := a.options.Events.PublishAfterCommit(ctx, runKey); err != nil {
		return
	}
}

func validateRunRequest(ctx context.Context, request RunRequest) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !request.TenantKey.Valid() || !request.SessionKey.Valid() || request.RequestID == "" || request.AgentKey == "" || len(request.Messages) == 0 || request.Priority < -100 || request.Priority > 100 {
		return fmt.Errorf("%w: incomplete run request", ErrInvalidCommand)
	}
	if len(request.StepPolicy.Steps) > 0 || request.StepPolicy.Schema != "" {
		if err := request.StepPolicy.Validate(); err != nil {
			return err
		}
	}
	for _, message := range request.Messages {
		if err := agent.ValidateMessage(message); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidCommand, err)
		}
	}
	merge := normalizedMerge(request.Merge)
	if merge != MergeFastForward && merge != MergeNone {
		return fmt.Errorf("%w: invalid merge strategy", ErrInvalidCommand)
	}
	return nil
}

func normalizedMerge(strategy MergeStrategy) MergeStrategy {
	if strategy == "" {
		return MergeFastForward
	}
	return strategy
}

func stableRunKey(tenant agent.TenantKey, session SessionKey, requestID string) RunKey {
	digest := sha256.Sum256([]byte(string(tenant) + "\x00" + string(session) + "\x00" + requestID))
	return RunKey(fmt.Sprintf("run_%x", digest[:16]))
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func settled(receipt RunReceipt) bool {
	return receipt.ExecutionState == ExecutionReleased || receipt.ExecutionState == ExecutionSuspended || receipt.ExecutionState == ExecutionMergePending || receipt.ExecutionState == ExecutionFailed
}
