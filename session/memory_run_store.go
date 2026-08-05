package session

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type admissionIdentity struct {
	tenant  agent.TenantKey
	session SessionKey
	request string
}

type storedRun struct {
	admission       Admission
	execution       ResolvedExecution
	receipt         RunReceipt
	claim           Claim
	durableFence    uint64
	result          *agent.RunResult
	failure         *Failure
	cancelMode      CancelMode
	cancelReason    string
	branchState     string
	sessionRevision uint64
}

// MemoryRunStore is a lock-linearized, thread-safe persistent-store reference.
// Its lifetime, unlike its data, is process-local; production adapters implement
// the same transactions in a durable database.
type MemoryRunStore struct {
	mu        sync.Mutex
	runs      map[RunKey]*storedRun
	requests  map[admissionIdentity]RunKey
	revisions map[sessionIdentity]uint64
	nextClaim uint64
	changed   chan struct{}
}

var _ Store = (*MemoryRunStore)(nil)

func NewMemoryRunStore() *MemoryRunStore {
	return &MemoryRunStore{
		runs:      make(map[RunKey]*storedRun),
		requests:  make(map[admissionIdentity]RunKey),
		revisions: make(map[sessionIdentity]uint64),
		changed:   make(chan struct{}),
	}
}

func (s *MemoryRunStore) AdmitAndBegin(ctx context.Context, command AdmitAndBeginCommand) (RunReceipt, bool, error) {
	if err := contextError(ctx); err != nil {
		return RunReceipt{}, false, err
	}
	if err := validateLimits(command.Limits); err != nil {
		return RunReceipt{}, false, err
	}
	a := command.Admission
	if !a.TenantKey.Valid() || !a.SessionKey.Valid() || !a.RunKey.Valid() || !a.BranchKey.Valid() || a.RequestID == "" || a.AgentKey == "" || a.DefinitionDigest == "" || len(a.Messages) == 0 || a.Priority < -100 || a.Priority > 100 {
		return RunReceipt{}, false, fmt.Errorf("%w: incomplete admission", ErrInvalidCommand)
	}
	if a.Merge == "" {
		a.Merge = MergeFastForward
	}
	if a.Merge != MergeFastForward && a.Merge != MergeNone {
		return RunReceipt{}, false, fmt.Errorf("%w: unsupported merge strategy %q", ErrInvalidCommand, a.Merge)
	}
	execution, err := cloneExecution(command.Execution)
	if err != nil {
		return RunReceipt{}, false, err
	}
	a.Messages = cloneMessages(a.Messages)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return RunReceipt{}, false, err
	}
	id := admissionIdentity{tenant: a.TenantKey, session: a.SessionKey, request: a.RequestID}
	if key, ok := s.requests[id]; ok {
		existing := s.runs[key]
		if sameAdmission(existing.admission, a) {
			return existing.receipt, false, nil
		}
		return RunReceipt{}, false, ErrRunConflict
	}
	if existing, ok := s.runs[a.RunKey]; ok {
		if sameAdmission(existing.admission, a) {
			return existing.receipt, false, nil
		}
		return RunReceipt{}, false, ErrRunConflict
	}
	queuedTenant, queuedSession := s.queuedCountsLocked(a.TenantKey, a.SessionKey)
	if queuedTenant >= command.Limits.MaxQueuedPerTenant || queuedSession >= command.Limits.MaxQueuedPerSession {
		return RunReceipt{}, false, ErrQueueFull
	}
	current := s.revisions[sessionIdentity{tenant: a.TenantKey, session: a.SessionKey}]
	if a.BaseRevision != nil && *a.BaseRevision != current {
		return RunReceipt{}, false, fmt.Errorf("%w: base %d, current %d", ErrRevisionConflict, *a.BaseRevision, current)
	}
	base := current
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	receipt := RunReceipt{RunKey: a.RunKey, BranchKey: a.BranchKey, SessionKey: a.SessionKey, BaseRevision: base, AdmissionState: AdmissionQueued, ExecutionState: ExecutionQueued, DefinitionDigest: a.DefinitionDigest, ContextPlanKey: a.ContextPlanKey, ContextPlanDigest: a.ContextPlanDigest, StepPolicyDigest: a.StepPolicyDigest, CreatedAt: a.CreatedAt}
	s.runs[a.RunKey] = &storedRun{admission: a, execution: execution, receipt: receipt, branchState: string(BranchStatusOpen), sessionRevision: current}
	s.requests[id] = a.RunKey
	s.signalLocked()
	return receipt, true, nil
}

func (s *MemoryRunStore) ClaimNext(ctx context.Context, workerID string, leaseUntil time.Time, limits Limits) (Claim, bool, error) {
	if err := contextError(ctx); err != nil {
		return Claim{}, false, err
	}
	if workerID == "" || leaseUntil.IsZero() {
		return Claim{}, false, fmt.Errorf("%w: worker and lease are required", ErrInvalidCommand)
	}
	if err := validateLimits(limits); err != nil {
		return Claim{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	activeGlobal, activeTenant, activeSession := s.activeCountsLocked()
	if activeGlobal >= limits.MaxActiveGlobal {
		return Claim{}, false, nil
	}
	eligible := make([]*storedRun, 0)
	for _, run := range s.runs {
		if run.receipt.AdmissionState != AdmissionQueued || run.receipt.ExecutionState != ExecutionQueued && run.receipt.ExecutionState != ExecutionMergePending || run.cancelMode == CancelSuspend || run.cancelMode == CancelAbandon {
			continue
		}
		if activeTenant[run.admission.TenantKey] >= limits.MaxActivePerTenant || activeSession[sessionIdentity{tenant: run.admission.TenantKey, session: run.admission.SessionKey}] >= limits.MaxActivePerSession {
			continue
		}
		eligible = append(eligible, run)
	}
	if len(eligible) == 0 {
		return Claim{}, false, nil
	}
	sort.Slice(eligible, func(i, j int) bool {
		left, right := eligible[i].admission, eligible[j].admission
		if left.Priority != right.Priority {
			return left.Priority > right.Priority
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		if left.BranchKey != right.BranchKey {
			return left.BranchKey < right.BranchKey
		}
		return left.RunKey < right.RunKey
	})
	run := eligible[0]
	s.nextClaim++
	run.claim = Claim{TenantKey: run.admission.TenantKey, RunKey: run.admission.RunKey, BranchKey: run.admission.BranchKey, Merge: run.admission.Merge, WorkerID: workerID, ClaimToken: s.nextClaim, LeaseUntil: leaseUntil.UTC()}
	run.receipt.AdmissionState = AdmissionClaimed
	s.signalLocked()
	return run.claim, true, nil
}

func (s *MemoryRunStore) LoadBranchInput(ctx context.Context, runKey RunKey) ([]agent.Message, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runKey]
	if !ok {
		return nil, ErrRunNotFound
	}
	return cloneMessages(run.admission.Messages), nil
}

func (s *MemoryRunStore) LoadStepPolicy(ctx context.Context, runKey RunKey) (StepPolicyArtifact, string, error) {
	if err := contextError(ctx); err != nil {
		return StepPolicyArtifact{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runKey]
	if !ok {
		return StepPolicyArtifact{}, "", ErrRunNotFound
	}
	return cloneStepPolicy(run.admission.StepPolicy), run.admission.StepPolicyDigest, nil
}

func (s *MemoryRunStore) MarkRunning(ctx context.Context, claim Claim, durableFence uint64) error {
	return s.withClaim(ctx, claim, func(run *storedRun) error {
		if run.cancelMode == CancelAbandon {
			run.branchState = string(BranchStatusAbandoned)
			run.receipt.ExecutionState = ExecutionReleased
			s.releaseLocked(run)
			return ErrCanceled
		}
		if run.cancelMode == CancelSuspend {
			run.receipt.ExecutionState = ExecutionSuspended
			s.releaseLocked(run)
			return ErrCanceled
		}
		if run.receipt.ExecutionState != ExecutionQueued && run.receipt.ExecutionState != ExecutionSuspended {
			return ErrInvalidTransition
		}
		run.durableFence = durableFence
		run.receipt.ExecutionState = ExecutionRunning
		return nil
	})
}

func (s *MemoryRunStore) MarkSuspended(ctx context.Context, claim Claim, failure Failure) error {
	return s.withClaim(ctx, claim, func(run *storedRun) error {
		if run.cancelMode == CancelAbandon {
			run.branchState = string(BranchStatusAbandoned)
			run.receipt.ExecutionState = ExecutionReleased
			s.releaseLocked(run)
			return ErrCanceled
		}
		if run.receipt.ExecutionState != ExecutionRunning && run.receipt.ExecutionState != ExecutionQueued {
			return ErrInvalidTransition
		}
		run.receipt.ExecutionState = ExecutionSuspended
		run.failure = cloneFailure(&failure)
		s.releaseLocked(run)
		return nil
	})
}

func (s *MemoryRunStore) MarkMergePending(ctx context.Context, claim Claim, result agent.RunResult) error {
	return s.withClaim(ctx, claim, func(run *storedRun) error {
		if run.cancelMode == CancelAbandon {
			run.branchState = string(BranchStatusAbandoned)
			run.receipt.ExecutionState = ExecutionReleased
			s.releaseLocked(run)
			return ErrCanceled
		}
		if run.receipt.ExecutionState != ExecutionRunning {
			return ErrInvalidTransition
		}
		run.receipt.ExecutionState = ExecutionMergePending
		run.result = cloneCoreResult(&result)
		run.branchState = string(BranchStatusReadyToMerge)
		return nil
	})
}

func (s *MemoryRunStore) MarkFailed(ctx context.Context, claim Claim, failure Failure) error {
	return s.withClaim(ctx, claim, func(run *storedRun) error {
		if run.cancelMode == CancelAbandon {
			run.branchState = string(BranchStatusAbandoned)
			run.receipt.ExecutionState = ExecutionReleased
			s.releaseLocked(run)
			return ErrCanceled
		}
		if terminalExecution(run.receipt.ExecutionState) {
			return ErrInvalidTransition
		}
		run.receipt.ExecutionState = ExecutionFailed
		run.failure = cloneFailure(&failure)
		s.releaseLocked(run)
		return nil
	})
}

func (s *MemoryRunStore) Resume(ctx context.Context, request ResumeRequest, limits Limits) (ResumeResult, error) {
	if err := contextError(ctx); err != nil {
		return ResumeResult{}, err
	}
	if !request.TenantKey.Valid() || !request.RunKey.Valid() || validateLimits(limits) != nil {
		return ResumeResult{}, ErrInvalidCommand
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[request.RunKey]
	if !ok || run.admission.TenantKey != request.TenantKey {
		return ResumeResult{}, ErrRunNotFound
	}
	state := run.receipt.ExecutionState
	if state == ExecutionQueued || state == ExecutionRunning {
		return ResumeResult{RunKey: request.RunKey, AlreadyReady: true, State: state}, nil
	}
	if state != ExecutionSuspended || run.receipt.AdmissionState != AdmissionReleased || run.failure == nil || !run.failure.Retryable || run.cancelMode != "" {
		return ResumeResult{}, &TerminalStateError{RunKey: request.RunKey, State: state, Reason: "suspension is not retryable or is cancellation-fenced"}
	}
	queuedTenant, queuedSession := s.queuedCountsLocked(run.admission.TenantKey, run.admission.SessionKey)
	if queuedTenant >= limits.MaxQueuedPerTenant || queuedSession >= limits.MaxQueuedPerSession {
		return ResumeResult{}, ErrQueueFull
	}
	run.receipt.AdmissionState = AdmissionQueued
	run.receipt.ExecutionState = ExecutionQueued
	run.failure = nil
	run.result = nil
	s.signalLocked()
	return ResumeResult{RunKey: request.RunKey, Resumed: true, State: ExecutionQueued}, nil
}

func (s *MemoryRunStore) FinalizeFailure(ctx context.Context, command FinalizeFailureCommand) error {
	if command.DurableResult.DurableSuspension != nil {
		return s.MarkSuspended(ctx, command.Claim, command.Failure)
	}
	return s.MarkFailed(ctx, command.Claim, command.Failure)
}

func (s *MemoryRunStore) MergeAndFinalize(ctx context.Context, command MergeAndFinalizeCommand) (MergeResult, error) {
	if err := contextError(ctx); err != nil {
		return MergeResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[command.Request.RunKey]
	if !ok {
		return MergeResult{}, ErrRunNotFound
	}
	if command.Request.TenantKey != run.admission.TenantKey {
		return MergeResult{}, ErrRunNotFound
	}
	if run.receipt.ExecutionState == ExecutionReleased {
		result := s.mergeResultLocked(run)
		if run.branchState == string(BranchStatusConflicted) {
			current := s.revisions[sessionIdentity{tenant: run.admission.TenantKey, session: run.admission.SessionKey}]
			return result, &MergeConflictError{RunKey: run.admission.RunKey, SessionKey: run.admission.SessionKey, BranchKey: run.admission.BranchKey, BaseRevision: run.receipt.BaseRevision, CurrentRevision: current, Reason: "fast-forward precondition failed"}
		}
		return result, nil
	}
	if command.Claim.ClaimToken != 0 {
		var err error
		run, err = s.claimedLocked(command.Claim)
		if err != nil {
			return MergeResult{}, err
		}
	} else if run.receipt.AdmissionState != AdmissionReleased || run.receipt.ExecutionState != ExecutionMergePending {
		return MergeResult{}, ErrStaleClaim
	}
	if run.receipt.ExecutionState != ExecutionMergePending || (command.DurableFence != 0 && command.DurableFence != run.durableFence) || command.Request.TenantKey != run.admission.TenantKey || command.Request.RunKey != run.admission.RunKey {
		return MergeResult{}, ErrStaleClaim
	}
	if command.Request.ExpectedBase != run.receipt.BaseRevision {
		return MergeResult{}, ErrRunConflict
	}
	strategy := command.Request.Strategy
	if strategy == "" {
		strategy = run.admission.Merge
	}
	if strategy == MergeNone {
		return s.mergeResultLocked(run), nil
	}
	if strategy != MergeFastForward {
		return MergeResult{}, fmt.Errorf("%w: unsupported merge strategy", ErrInvalidCommand)
	}
	id := sessionIdentity{tenant: run.admission.TenantKey, session: run.admission.SessionKey}
	current := s.revisions[id]
	if current != run.receipt.BaseRevision {
		run.branchState = string(BranchStatusConflicted)
		run.receipt.ExecutionState = ExecutionReleased
		s.releaseLocked(run)
		return s.mergeResultLocked(run), &MergeConflictError{RunKey: run.admission.RunKey, SessionKey: run.admission.SessionKey, BranchKey: run.admission.BranchKey, BaseRevision: run.receipt.BaseRevision, CurrentRevision: current, Reason: "fast-forward precondition failed"}
	}
	previous := current
	current++
	s.revisions[id] = current
	run.sessionRevision = current
	run.branchState = string(BranchStatusMerged)
	run.result = cloneCoreResult(&command.DurableResult)
	run.receipt.ExecutionState = ExecutionReleased
	s.releaseLocked(run)
	result := s.mergeResultLocked(run)
	result.PreviousRevision = previous
	return result, nil
}

func (s *MemoryRunStore) RequestCancel(ctx context.Context, request CancelRequest) (CancelResult, error) {
	if err := contextError(ctx); err != nil {
		return CancelResult{}, err
	}
	if !request.TenantKey.Valid() || !request.RunKey.Valid() || cancelRank(request.Mode) == 0 {
		return CancelResult{}, fmt.Errorf("%w: invalid cancellation", ErrInvalidCommand)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[request.RunKey]
	if !ok || run.admission.TenantKey != request.TenantKey {
		return CancelResult{}, ErrRunNotFound
	}
	if terminalExecution(run.receipt.ExecutionState) {
		return CancelResult{RunKey: request.RunKey, AlreadyFinal: true, State: run.receipt.ExecutionState}, nil
	}
	requested := cancelRank(request.Mode) > cancelRank(run.cancelMode)
	if requested {
		run.cancelMode, run.cancelReason = request.Mode, request.Reason
	}
	if run.cancelMode == CancelSuspend && run.receipt.ExecutionState == ExecutionQueued {
		run.receipt.ExecutionState = ExecutionSuspended
		s.releaseLocked(run)
	}
	if run.cancelMode == CancelAbandon && run.receipt.ExecutionState != ExecutionRunning {
		run.branchState = string(BranchStatusAbandoned)
		run.receipt.ExecutionState = ExecutionReleased
		s.releaseLocked(run)
	}
	s.signalLocked()
	return CancelResult{RunKey: request.RunKey, Requested: requested, State: run.receipt.ExecutionState}, nil
}

func (s *MemoryRunStore) Abandon(ctx context.Context, runKey RunKey, reason string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runKey]
	if !ok {
		return ErrRunNotFound
	}
	run.cancelMode, run.cancelReason = CancelAbandon, reason
	run.branchState = string(BranchStatusAbandoned)
	run.receipt.ExecutionState = ExecutionReleased
	s.releaseLocked(run)
	return nil
}

func (s *MemoryRunStore) Release(ctx context.Context, claim Claim) error {
	return s.withClaim(ctx, claim, func(run *storedRun) error {
		if run.receipt.ExecutionState == ExecutionMergePending {
			run.receipt.AdmissionState = AdmissionQueued
			run.claim = Claim{}
			return nil
		}
		s.releaseLocked(run)
		return nil
	})
}

func (s *MemoryRunStore) Get(ctx context.Context, runKey RunKey) (RunResult, error) {
	if err := contextError(ctx); err != nil {
		return RunResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runKey]
	if !ok {
		return RunResult{}, ErrRunNotFound
	}
	return cloneRunResult(run), nil
}

func (s *MemoryRunStore) Reconcile(ctx context.Context, now time.Time) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, run := range s.runs {
		if run.receipt.AdmissionState != AdmissionClaimed || run.claim.LeaseUntil.After(now) {
			continue
		}
		if run.cancelMode == CancelAbandon {
			run.branchState = string(BranchStatusAbandoned)
			run.receipt.ExecutionState = ExecutionReleased
			s.releaseLocked(run)
			continue
		}
		if run.cancelMode == CancelSuspend {
			run.receipt.ExecutionState = ExecutionSuspended
			s.releaseLocked(run)
			continue
		}
		if run.receipt.ExecutionState == ExecutionRunning {
			run.receipt.ExecutionState = ExecutionSuspended
		}
		if run.receipt.ExecutionState == ExecutionQueued {
			run.receipt.AdmissionState = AdmissionQueued
			run.claim = Claim{}
		} else {
			s.releaseLocked(run)
		}
	}
	s.signalLocked()
	return nil
}

func (s *MemoryRunStore) withClaim(ctx context.Context, claim Claim, mutate func(*storedRun) error) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, err := s.claimedLocked(claim)
	if err != nil {
		return err
	}
	if err := mutate(run); err != nil {
		return err
	}
	s.signalLocked()
	return nil
}

func (s *MemoryRunStore) claimedLocked(claim Claim) (*storedRun, error) {
	run, ok := s.runs[claim.RunKey]
	if !ok {
		return nil, ErrRunNotFound
	}
	if run.receipt.AdmissionState != AdmissionClaimed || run.claim != claim {
		return nil, ErrStaleClaim
	}
	if !run.claim.LeaseUntil.After(time.Now().UTC()) {
		return nil, ErrStaleClaim
	}
	return run, nil
}

func (s *MemoryRunStore) releaseLocked(run *storedRun) {
	run.receipt.AdmissionState = AdmissionReleased
	run.claim = Claim{}
	s.signalLocked()
}

func (s *MemoryRunStore) queuedCountsLocked(tenant agent.TenantKey, session SessionKey) (int, int) {
	var tenantCount, sessionCount int
	for _, run := range s.runs {
		if run.receipt.AdmissionState != AdmissionQueued || run.admission.TenantKey != tenant {
			continue
		}
		tenantCount++
		if run.admission.SessionKey == session {
			sessionCount++
		}
	}
	return tenantCount, sessionCount
}

func (s *MemoryRunStore) activeCountsLocked() (int, map[agent.TenantKey]int, map[sessionIdentity]int) {
	tenant := make(map[agent.TenantKey]int)
	session := make(map[sessionIdentity]int)
	global := 0
	for _, run := range s.runs {
		if run.receipt.AdmissionState != AdmissionClaimed {
			continue
		}
		global++
		tenant[run.admission.TenantKey]++
		session[sessionIdentity{tenant: run.admission.TenantKey, session: run.admission.SessionKey}]++
	}
	return global, tenant, session
}

func (s *MemoryRunStore) mergeResultLocked(run *storedRun) MergeResult {
	return MergeResult{RunKey: run.admission.RunKey, BranchKey: run.admission.BranchKey, SessionRevision: run.sessionRevision, BranchState: run.branchState, ExecutionState: run.receipt.ExecutionState}
}

func (s *MemoryRunStore) signalLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func sameAdmission(left, right Admission) bool {
	left.CreatedAt, right.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func cancelRank(mode CancelMode) int {
	switch mode {
	case CancelAttempt:
		return 1
	case CancelSuspend:
		return 2
	case CancelAbandon:
		return 3
	default:
		return 0
	}
}

func terminalExecution(state ExecutionState) bool {
	return state == ExecutionReleased || state == ExecutionFailed
}

func cloneRunResult(run *storedRun) RunResult {
	receipt := run.receipt
	return RunResult{Receipt: receipt, SessionRevision: run.sessionRevision, CoreResult: cloneCoreResult(run.result), Failure: cloneFailure(run.failure)}
}
