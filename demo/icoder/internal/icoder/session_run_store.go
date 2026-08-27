package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/session"
)

// SQLiteSessionRunStore is iCoder's persistent implementation of session.Store,
// the durable run queue behind session.SessionAgent: admission, claim leases,
// outcome reporting, cancellation, and the branch merge that publishes a run's
// result into the session.
//
// Every method is one immediate transaction. The reason is the claim: a lease is
// only worth having if reading it and acting on it cannot be interleaved with
// another worker doing the same, and a run executed twice is a turn committed
// twice. The legality rules themselves - what supersedes what, what is terminal,
// which queued run is next - come from the session package's exported state
// machine, so this adapter and the reference implementation cannot disagree
// about them.
type SQLiteSessionRunStore struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLiteSessionRunStore binds the run queue to an already-migrated database.
func NewSQLiteSessionRunStore(db *sql.DB) *SQLiteSessionRunStore {
	return &SQLiteSessionRunStore{db: db, now: func() time.Time { return time.Now().UTC() }}
}

var _ session.Store = (*SQLiteSessionRunStore)(nil)

// sessionRunMigrations creates the run queue tables. The session revision
// counter lives here rather than in the aggregate tables because this store owns
// the fast-forward that advances it, and a merge must decide and record the new
// revision in the same transaction that releases the run.
var sessionRunMigrations = []string{
	`CREATE TABLE IF NOT EXISTS session_runs (
		run_key TEXT PRIMARY KEY,
		tenant_key TEXT NOT NULL,
		session_key TEXT NOT NULL,
		request_id TEXT NOT NULL,
		admission_state TEXT NOT NULL,
		execution_state TEXT NOT NULL,
		cancel_mode TEXT NOT NULL DEFAULT '',
		claim_token INTEGER NOT NULL DEFAULT 0,
		lease_until INTEGER NOT NULL DEFAULT 0,
		priority INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		record BLOB NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS session_runs_request
		ON session_runs(tenant_key, session_key, request_id)`,
	`CREATE INDEX IF NOT EXISTS session_runs_queue
		ON session_runs(admission_state, execution_state, priority, created_at)`,
	`CREATE TABLE IF NOT EXISTS session_run_revisions (
		tenant_key TEXT NOT NULL,
		session_key TEXT NOT NULL,
		revision INTEGER NOT NULL,
		PRIMARY KEY (tenant_key, session_key)
	)`,
	`CREATE TABLE IF NOT EXISTS session_run_claims (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		next_token INTEGER NOT NULL
	)`,
}

// storedRunRecord is the whole run as it is persisted. Only the fields a query
// filters or orders on are lifted into columns; the rest travels as JSON so the
// contract's types stay the single definition of what a run is.
type storedRunRecord struct {
	Admission       session.Admission         `json:"admission"`
	Execution       session.ResolvedExecution `json:"execution"`
	Receipt         session.RunReceipt        `json:"receipt"`
	Claim           session.Claim             `json:"claim"`
	DurableFence    uint64                    `json:"durable_fence"`
	Result          *agent.RunResult          `json:"result,omitempty"`
	Failure         *storedRunFailure         `json:"failure,omitempty"`
	CancelMode      session.CancelMode        `json:"cancel_mode"`
	CancelReason    string                    `json:"cancel_reason"`
	BranchState     string                    `json:"branch_state"`
	SessionRevision uint64                    `json:"session_revision"`
}

// storedRunFailure is session.Failure without its Cause. A cause is a live Go
// error: it cannot round-trip through storage, and pretending otherwise would
// hand a resumed process a wrapped error it never actually saw.
type storedRunFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func storeFailure(failure *session.Failure) *storedRunFailure {
	if failure == nil {
		return nil
	}
	return &storedRunFailure{Code: failure.Code, Message: failure.Message, Retryable: failure.Retryable}
}

func (f *storedRunFailure) failure() *session.Failure {
	if f == nil {
		return nil
	}
	return &session.Failure{Code: f.Code, Message: f.Message, Retryable: f.Retryable}
}

// AdmitAndBegin queues one run, or recognizes a replay of the same request.
func (s *SQLiteSessionRunStore) AdmitAndBegin(ctx context.Context, command session.AdmitAndBeginCommand) (receipt session.RunReceipt, created bool, resultErr error) {
	if err := runContextError(ctx); err != nil {
		return session.RunReceipt{}, false, err
	}
	if err := validateRunLimits(command.Limits); err != nil {
		return session.RunReceipt{}, false, err
	}
	admission := command.Admission
	if !admission.TenantKey.Valid() || !admission.SessionKey.Valid() || !admission.RunKey.Valid() || !admission.BranchKey.Valid() ||
		admission.RequestID == "" || admission.AgentKey == "" || admission.DefinitionDigest == "" ||
		len(admission.Messages) == 0 || admission.Priority < -100 || admission.Priority > 100 {
		return session.RunReceipt{}, false, fmt.Errorf("%w: incomplete admission", session.ErrInvalidCommand)
	}
	if admission.Merge == "" {
		admission.Merge = session.MergeFastForward
	}
	if admission.Merge != session.MergeFastForward && admission.Merge != session.MergeNone {
		return session.RunReceipt{}, false, fmt.Errorf("%w: unsupported merge strategy %q", session.ErrInvalidCommand, admission.Merge)
	}
	admission.Messages = session.CloneMessages(admission.Messages)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.RunReceipt{}, false, err
	}
	defer finishTx(tx, &resultErr)

	if existing, ok, err := s.runByRequest(tx, admission.TenantKey, admission.SessionKey, admission.RequestID); err != nil {
		return session.RunReceipt{}, false, err
	} else if ok {
		if session.SameAdmission(existing.Admission, admission) {
			return existing.Receipt, false, nil
		}
		return session.RunReceipt{}, false, session.ErrRunConflict
	}
	if existing, ok, err := s.run(tx, admission.RunKey); err != nil {
		return session.RunReceipt{}, false, err
	} else if ok {
		if session.SameAdmission(existing.Admission, admission) {
			return existing.Receipt, false, nil
		}
		return session.RunReceipt{}, false, session.ErrRunConflict
	}

	queuedTenant, queuedSession, err := s.queuedCounts(tx, admission.TenantKey, admission.SessionKey)
	if err != nil {
		return session.RunReceipt{}, false, err
	}
	if queuedTenant >= command.Limits.MaxQueuedPerTenant || queuedSession >= command.Limits.MaxQueuedPerSession {
		return session.RunReceipt{}, false, session.ErrQueueFull
	}
	current, err := s.sessionRevision(tx, admission.TenantKey, admission.SessionKey)
	if err != nil {
		return session.RunReceipt{}, false, err
	}
	if admission.BaseRevision != nil && *admission.BaseRevision != current {
		return session.RunReceipt{}, false, fmt.Errorf("%w: base %d, current %d", session.ErrRevisionConflict, *admission.BaseRevision, current)
	}
	if admission.CreatedAt.IsZero() {
		admission.CreatedAt = s.now()
	}
	admission.CreatedAt = admission.CreatedAt.UTC()
	record := storedRunRecord{
		Admission: admission,
		Execution: command.Execution,
		Receipt: session.RunReceipt{
			RunKey: admission.RunKey, BranchKey: admission.BranchKey, SessionKey: admission.SessionKey,
			BaseRevision: current, AdmissionState: session.AdmissionQueued, ExecutionState: session.ExecutionQueued,
			DefinitionDigest: admission.DefinitionDigest, ContextPlanKey: admission.ContextPlanKey,
			ContextPlanDigest: admission.ContextPlanDigest, StepPolicyDigest: admission.StepPolicyDigest,
			CreatedAt: admission.CreatedAt,
		},
		BranchState:     string(session.BranchStatusOpen),
		SessionRevision: current,
	}
	if err := s.insertRun(tx, record); err != nil {
		return session.RunReceipt{}, false, err
	}
	return record.Receipt, true, nil
}

// ClaimNext leases the highest-priority eligible run to a worker.
//
// Admission limits are evaluated against what is currently claimed, so a run
// that would exceed a tenant's or a session's concurrency is skipped rather than
// queued behind a global stall.
func (s *SQLiteSessionRunStore) ClaimNext(ctx context.Context, workerID string, leaseUntil time.Time, limits session.Limits) (claim session.Claim, claimed bool, resultErr error) {
	if err := runContextError(ctx); err != nil {
		return session.Claim{}, false, err
	}
	if workerID == "" || leaseUntil.IsZero() {
		return session.Claim{}, false, fmt.Errorf("%w: worker and lease are required", session.ErrInvalidCommand)
	}
	if err := validateRunLimits(limits); err != nil {
		return session.Claim{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Claim{}, false, err
	}
	defer finishTx(tx, &resultErr)

	activeGlobal, activeTenant, activeSession, err := s.activeCounts(tx)
	if err != nil {
		return session.Claim{}, false, err
	}
	if activeGlobal >= limits.MaxActiveGlobal {
		return session.Claim{}, false, nil
	}
	candidates, err := s.eligibleRuns(tx)
	if err != nil {
		return session.Claim{}, false, err
	}
	var chosen *storedRunRecord
	for index := range candidates {
		candidate := &candidates[index]
		tenant := candidate.Admission.TenantKey
		scope := sessionScopeKey{tenant: tenant, session: candidate.Admission.SessionKey}
		if activeTenant[tenant] >= limits.MaxActivePerTenant || activeSession[scope] >= limits.MaxActivePerSession {
			continue
		}
		if chosen == nil || session.ClaimBefore(candidate.Admission, chosen.Admission) {
			chosen = candidate
		}
	}
	if chosen == nil {
		return session.Claim{}, false, nil
	}
	token, err := s.nextClaimToken(tx)
	if err != nil {
		return session.Claim{}, false, err
	}
	chosen.Claim = session.Claim{
		TenantKey: chosen.Admission.TenantKey, RunKey: chosen.Admission.RunKey, BranchKey: chosen.Admission.BranchKey,
		Merge: chosen.Admission.Merge, WorkerID: workerID, ClaimToken: token, LeaseUntil: leaseUntil.UTC(),
	}
	chosen.Receipt.AdmissionState = session.AdmissionClaimed
	if err := s.saveRun(tx, *chosen); err != nil {
		return session.Claim{}, false, err
	}
	return chosen.Claim, true, nil
}

// LoadBranchInput returns the immutable messages a run was admitted with, which
// is what makes a resumed attempt provably the same run.
func (s *SQLiteSessionRunStore) LoadBranchInput(ctx context.Context, runKey session.RunKey) (messages []agent.Message, resultErr error) {
	record, err := s.loadOne(ctx, runKey, &resultErr)
	if err != nil {
		return nil, err
	}
	return session.CloneMessages(record.Admission.Messages), nil
}

// LoadStepPolicy returns the step policy a run was admitted with, so a resumed
// attempt is bounded the same way the first one was.
func (s *SQLiteSessionRunStore) LoadStepPolicy(ctx context.Context, runKey session.RunKey) (policy session.StepPolicyArtifact, digest string, resultErr error) {
	record, err := s.loadOne(ctx, runKey, &resultErr)
	if err != nil {
		return session.StepPolicyArtifact{}, "", err
	}
	return record.Admission.StepPolicy, record.Admission.StepPolicyDigest, nil
}

// MarkRunning records that the claimed attempt has started under a durable fence.
func (s *SQLiteSessionRunStore) MarkRunning(ctx context.Context, claim session.Claim, durableFence uint64) error {
	return s.withClaim(ctx, claim, func(record *storedRunRecord) error {
		if err := s.applyPendingCancel(record); err != nil {
			return err
		}
		if record.Receipt.ExecutionState != session.ExecutionQueued && record.Receipt.ExecutionState != session.ExecutionSuspended {
			return session.ErrInvalidTransition
		}
		record.DurableFence = durableFence
		record.Receipt.ExecutionState = session.ExecutionRunning
		return nil
	})
}

// MarkSuspended parks a run and releases its claim, keeping the failure that
// says whether resuming it is allowed.
func (s *SQLiteSessionRunStore) MarkSuspended(ctx context.Context, claim session.Claim, failure session.Failure) error {
	return s.withClaim(ctx, claim, func(record *storedRunRecord) error {
		if record.CancelMode == session.CancelAbandon {
			s.abandonRecord(record)
			return session.ErrCanceled
		}
		if record.Receipt.ExecutionState != session.ExecutionRunning && record.Receipt.ExecutionState != session.ExecutionQueued {
			return session.ErrInvalidTransition
		}
		record.Receipt.ExecutionState = session.ExecutionSuspended
		record.Failure = storeFailure(&failure)
		releaseRecord(record)
		return nil
	})
}

// MarkMergePending records a finished attempt whose result is not yet published
// into the session. The claim is deliberately kept: the merge is a separate
// transaction, and the run must not be picked up by another worker in between.
func (s *SQLiteSessionRunStore) MarkMergePending(ctx context.Context, claim session.Claim, result agent.RunResult) error {
	return s.withClaim(ctx, claim, func(record *storedRunRecord) error {
		if record.CancelMode == session.CancelAbandon {
			s.abandonRecord(record)
			return session.ErrCanceled
		}
		if record.Receipt.ExecutionState != session.ExecutionRunning {
			return session.ErrInvalidTransition
		}
		record.Receipt.ExecutionState = session.ExecutionMergePending
		stored := result
		record.Result = &stored
		record.BranchState = string(session.BranchStatusReadyToMerge)
		return nil
	})
}

// MarkFailed ends a run without merging it.
func (s *SQLiteSessionRunStore) MarkFailed(ctx context.Context, claim session.Claim, failure session.Failure) error {
	return s.withClaim(ctx, claim, func(record *storedRunRecord) error {
		if record.CancelMode == session.CancelAbandon {
			s.abandonRecord(record)
			return session.ErrCanceled
		}
		if session.TerminalExecutionState(record.Receipt.ExecutionState) {
			return session.ErrInvalidTransition
		}
		record.Receipt.ExecutionState = session.ExecutionFailed
		record.Failure = storeFailure(&failure)
		releaseRecord(record)
		return nil
	})
}

// Resume returns a retryably suspended run to the queue.
//
// A suspension that is not retryable, or one that a cancellation fenced, stays
// where it is: resuming it would restart work someone already decided to stop.
func (s *SQLiteSessionRunStore) Resume(ctx context.Context, request session.ResumeRequest, limits session.Limits) (result session.ResumeResult, resultErr error) {
	if err := runContextError(ctx); err != nil {
		return session.ResumeResult{}, err
	}
	if !request.TenantKey.Valid() || !request.RunKey.Valid() || validateRunLimits(limits) != nil {
		return session.ResumeResult{}, session.ErrInvalidCommand
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.ResumeResult{}, err
	}
	defer finishTx(tx, &resultErr)
	record, ok, err := s.run(tx, request.RunKey)
	if err != nil {
		return session.ResumeResult{}, err
	}
	if !ok || record.Admission.TenantKey != request.TenantKey {
		return session.ResumeResult{}, session.ErrRunNotFound
	}
	state := record.Receipt.ExecutionState
	if state == session.ExecutionQueued || state == session.ExecutionRunning {
		return session.ResumeResult{RunKey: request.RunKey, AlreadyReady: true, State: state}, nil
	}
	if state != session.ExecutionSuspended || record.Receipt.AdmissionState != session.AdmissionReleased ||
		record.Failure == nil || !record.Failure.Retryable || record.CancelMode != "" {
		return session.ResumeResult{}, &session.TerminalStateError{
			RunKey: request.RunKey, State: state, Reason: "suspension is not retryable or is cancellation-fenced",
		}
	}
	queuedTenant, queuedSession, err := s.queuedCounts(tx, record.Admission.TenantKey, record.Admission.SessionKey)
	if err != nil {
		return session.ResumeResult{}, err
	}
	if queuedTenant >= limits.MaxQueuedPerTenant || queuedSession >= limits.MaxQueuedPerSession {
		return session.ResumeResult{}, session.ErrQueueFull
	}
	record.Receipt.AdmissionState = session.AdmissionQueued
	record.Receipt.ExecutionState = session.ExecutionQueued
	record.Failure = nil
	record.Result = nil
	if err := s.saveRun(tx, record); err != nil {
		return session.ResumeResult{}, err
	}
	return session.ResumeResult{RunKey: request.RunKey, Resumed: true, State: session.ExecutionQueued}, nil
}

// FinalizeFailure routes a failed attempt to suspension or to failure depending
// on whether the durable runtime left something resumable behind.
func (s *SQLiteSessionRunStore) FinalizeFailure(ctx context.Context, command session.FinalizeFailureCommand) error {
	if command.DurableResult.DurableSuspension != nil {
		return s.MarkSuspended(ctx, command.Claim, command.Failure)
	}
	return s.MarkFailed(ctx, command.Claim, command.Failure)
}

// MergeAndFinalize publishes a finished run into the session by fast-forwarding
// its revision, exactly once.
//
// A replay after the run was already released returns the recorded outcome
// rather than merging again, because the second merge would advance the session
// revision a second time for one turn.
func (s *SQLiteSessionRunStore) MergeAndFinalize(ctx context.Context, command session.MergeAndFinalizeCommand) (result session.MergeResult, resultErr error) {
	if err := runContextError(ctx); err != nil {
		return session.MergeResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.MergeResult{}, err
	}
	defer finishTx(tx, &resultErr)

	record, ok, err := s.run(tx, command.Request.RunKey)
	if err != nil {
		return session.MergeResult{}, err
	}
	if !ok || command.Request.TenantKey != record.Admission.TenantKey {
		return session.MergeResult{}, session.ErrRunNotFound
	}
	if record.Receipt.ExecutionState == session.ExecutionReleased {
		merged := mergeResultOf(record)
		if record.BranchState == string(session.BranchStatusConflicted) {
			current, err := s.sessionRevision(tx, record.Admission.TenantKey, record.Admission.SessionKey)
			if err != nil {
				return session.MergeResult{}, err
			}
			return merged, &session.MergeConflictError{
				RunKey: record.Admission.RunKey, SessionKey: record.Admission.SessionKey, BranchKey: record.Admission.BranchKey,
				BaseRevision: record.Receipt.BaseRevision, CurrentRevision: current, Reason: "fast-forward precondition failed",
			}
		}
		return merged, nil
	}
	if command.Claim.ClaimToken != 0 {
		if err := s.checkClaim(record, command.Claim); err != nil {
			return session.MergeResult{}, err
		}
	} else if record.Receipt.AdmissionState != session.AdmissionReleased || record.Receipt.ExecutionState != session.ExecutionMergePending {
		return session.MergeResult{}, session.ErrStaleClaim
	}
	if record.Receipt.ExecutionState != session.ExecutionMergePending ||
		(command.DurableFence != 0 && command.DurableFence != record.DurableFence) ||
		command.Request.RunKey != record.Admission.RunKey {
		return session.MergeResult{}, session.ErrStaleClaim
	}
	if command.Request.ExpectedBase != record.Receipt.BaseRevision {
		return session.MergeResult{}, session.ErrRunConflict
	}
	strategy := command.Request.Strategy
	if strategy == "" {
		strategy = record.Admission.Merge
	}
	if strategy == session.MergeNone {
		return mergeResultOf(record), nil
	}
	if strategy != session.MergeFastForward {
		return session.MergeResult{}, fmt.Errorf("%w: unsupported merge strategy", session.ErrInvalidCommand)
	}
	current, err := s.sessionRevision(tx, record.Admission.TenantKey, record.Admission.SessionKey)
	if err != nil {
		return session.MergeResult{}, err
	}
	if current != record.Receipt.BaseRevision {
		record.BranchState = string(session.BranchStatusConflicted)
		record.Receipt.ExecutionState = session.ExecutionReleased
		releaseRecord(&record)
		if err := s.saveRun(tx, record); err != nil {
			return session.MergeResult{}, err
		}
		return mergeResultOf(record), &session.MergeConflictError{
			RunKey: record.Admission.RunKey, SessionKey: record.Admission.SessionKey, BranchKey: record.Admission.BranchKey,
			BaseRevision: record.Receipt.BaseRevision, CurrentRevision: current, Reason: "fast-forward precondition failed",
		}
	}
	previous := current
	current++
	if err := s.setSessionRevision(tx, record.Admission.TenantKey, record.Admission.SessionKey, current); err != nil {
		return session.MergeResult{}, err
	}
	record.SessionRevision = current
	record.BranchState = string(session.BranchStatusMerged)
	stored := command.DurableResult
	record.Result = &stored
	record.Receipt.ExecutionState = session.ExecutionReleased
	releaseRecord(&record)
	if err := s.saveRun(tx, record); err != nil {
		return session.MergeResult{}, err
	}
	merged := mergeResultOf(record)
	merged.PreviousRevision = previous
	return merged, nil
}

// RequestCancel escalates a run's cancellation and applies whatever part of it
// can be applied without the worker's cooperation.
func (s *SQLiteSessionRunStore) RequestCancel(ctx context.Context, request session.CancelRequest) (result session.CancelResult, resultErr error) {
	if err := runContextError(ctx); err != nil {
		return session.CancelResult{}, err
	}
	if !request.TenantKey.Valid() || !request.RunKey.Valid() || !session.KnownCancelMode(request.Mode) {
		return session.CancelResult{}, fmt.Errorf("%w: invalid cancellation", session.ErrInvalidCommand)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.CancelResult{}, err
	}
	defer finishTx(tx, &resultErr)
	record, ok, err := s.run(tx, request.RunKey)
	if err != nil {
		return session.CancelResult{}, err
	}
	if !ok || record.Admission.TenantKey != request.TenantKey {
		return session.CancelResult{}, session.ErrRunNotFound
	}
	if session.TerminalExecutionState(record.Receipt.ExecutionState) {
		return session.CancelResult{RunKey: request.RunKey, AlreadyFinal: true, State: record.Receipt.ExecutionState}, nil
	}
	requested := session.CancelSupersedes(record.CancelMode, request.Mode)
	if requested {
		record.CancelMode, record.CancelReason = request.Mode, request.Reason
	}
	// A running worker has to notice the cancellation itself; anything not
	// running can be moved right now.
	if record.CancelMode == session.CancelSuspend && record.Receipt.ExecutionState == session.ExecutionQueued {
		record.Receipt.ExecutionState = session.ExecutionSuspended
		releaseRecord(&record)
	}
	if record.CancelMode == session.CancelAbandon && record.Receipt.ExecutionState != session.ExecutionRunning {
		s.abandonRecord(&record)
	}
	if err := s.saveRun(tx, record); err != nil {
		return session.CancelResult{}, err
	}
	return session.CancelResult{RunKey: request.RunKey, Requested: requested, State: record.Receipt.ExecutionState}, nil
}

// Abandon ends a run unconditionally, which is what a shutdown or an operator
// decision needs when nothing is going to finish it.
func (s *SQLiteSessionRunStore) Abandon(ctx context.Context, runKey session.RunKey, reason string) (resultErr error) {
	if err := runContextError(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finishTx(tx, &resultErr)
	record, ok, err := s.run(tx, runKey)
	if err != nil {
		return err
	}
	if !ok {
		return session.ErrRunNotFound
	}
	record.CancelMode, record.CancelReason = session.CancelAbandon, reason
	s.abandonRecord(&record)
	return s.saveRun(tx, record)
}

// Release hands a claim back. A merge-pending run returns to the queue instead
// of being released, because its result still has to be published.
func (s *SQLiteSessionRunStore) Release(ctx context.Context, claim session.Claim) error {
	return s.withClaim(ctx, claim, func(record *storedRunRecord) error {
		if record.Receipt.ExecutionState == session.ExecutionMergePending {
			record.Receipt.AdmissionState = session.AdmissionQueued
			record.Claim = session.Claim{}
			return nil
		}
		releaseRecord(record)
		return nil
	})
}

// Get returns one run's current receipt, result, and failure.
func (s *SQLiteSessionRunStore) Get(ctx context.Context, runKey session.RunKey) (result session.RunResult, resultErr error) {
	record, err := s.loadOne(ctx, runKey, &resultErr)
	if err != nil {
		return session.RunResult{}, err
	}
	return session.RunResult{
		Receipt: record.Receipt, SessionRevision: record.SessionRevision,
		CoreResult: record.Result, Failure: record.Failure.failure(),
	}, nil
}

// Reconcile recovers runs whose worker stopped without releasing its lease.
//
// An expired claim on a running attempt becomes suspended rather than queued: the
// attempt may have had effects, so it is resumed deliberately rather than
// silently restarted.
func (s *SQLiteSessionRunStore) Reconcile(ctx context.Context, now time.Time) (resultErr error) {
	if err := runContextError(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finishTx(tx, &resultErr)
	rows, err := tx.QueryContext(ctx, `SELECT record FROM session_runs WHERE admission_state = ? AND lease_until <= ? ORDER BY run_key`,
		string(session.AdmissionClaimed), now.UnixNano())
	if err != nil {
		return err
	}
	records, err := scanRunRecords(rows)
	if err != nil {
		return err
	}
	for _, record := range records {
		switch {
		case record.CancelMode == session.CancelAbandon:
			s.abandonRecord(&record)
		case record.CancelMode == session.CancelSuspend:
			record.Receipt.ExecutionState = session.ExecutionSuspended
			releaseRecord(&record)
		default:
			if record.Receipt.ExecutionState == session.ExecutionRunning {
				record.Receipt.ExecutionState = session.ExecutionSuspended
			}
			if record.Receipt.ExecutionState == session.ExecutionQueued {
				record.Receipt.AdmissionState = session.AdmissionQueued
				record.Claim = session.Claim{}
			} else {
				releaseRecord(&record)
			}
		}
		if err := s.saveRun(tx, record); err != nil {
			return err
		}
	}
	return nil
}

// sessionScopeKey counts concurrency per session within a tenant.
type sessionScopeKey struct {
	tenant  agent.TenantKey
	session session.SessionKey
}

func runContextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", session.ErrInvalidCommand)
	}
	return ctx.Err()
}

func validateRunLimits(limits session.Limits) error {
	if limits.MaxActiveGlobal <= 0 || limits.MaxActivePerTenant <= 0 || limits.MaxActivePerSession <= 0 ||
		limits.MaxQueuedPerTenant <= 0 || limits.MaxQueuedPerSession <= 0 {
		return fmt.Errorf("%w: all admission limits must be positive", session.ErrInvalidCommand)
	}
	return nil
}

// releaseRecord drops the claim and marks the run unowned. It is the one place
// admission state and the claim are cleared together, so a released run can
// never keep a lease that would block another worker.
func releaseRecord(record *storedRunRecord) {
	record.Receipt.AdmissionState = session.AdmissionReleased
	record.Claim = session.Claim{}
}

// abandonRecord ends a run because someone decided it must not continue.
func (s *SQLiteSessionRunStore) abandonRecord(record *storedRunRecord) {
	record.BranchState = string(session.BranchStatusAbandoned)
	record.Receipt.ExecutionState = session.ExecutionReleased
	releaseRecord(record)
}

// applyPendingCancel lets a cancellation that arrived while the run was queued
// take effect at the moment the worker tries to start it, which is the last
// point before the attempt could have any effect.
func (s *SQLiteSessionRunStore) applyPendingCancel(record *storedRunRecord) error {
	switch record.CancelMode {
	case session.CancelAbandon:
		s.abandonRecord(record)
		return session.ErrCanceled
	case session.CancelSuspend:
		record.Receipt.ExecutionState = session.ExecutionSuspended
		releaseRecord(record)
		return session.ErrCanceled
	}
	return nil
}

func mergeResultOf(record storedRunRecord) session.MergeResult {
	return session.MergeResult{
		RunKey: record.Admission.RunKey, BranchKey: record.Admission.BranchKey,
		SessionRevision: record.SessionRevision, BranchState: record.BranchState,
		ExecutionState: record.Receipt.ExecutionState,
	}
}

// withClaim performs one guarded mutation under a live claim.
//
// The claim is compared whole, not just by run key: a worker whose lease expired
// and was reissued to someone else must not be able to report an outcome for the
// attempt that replaced it.
func (s *SQLiteSessionRunStore) withClaim(ctx context.Context, claim session.Claim, mutate func(*storedRunRecord) error) (resultErr error) {
	if err := runContextError(ctx); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finishTx(tx, &resultErr)
	record, ok, err := s.run(tx, claim.RunKey)
	if err != nil {
		return err
	}
	if !ok {
		return session.ErrRunNotFound
	}
	if err := s.checkClaim(record, claim); err != nil {
		return err
	}
	mutateErr := mutate(&record)
	// A mutation that reports cancellation still has state worth keeping: the run
	// was moved to abandoned or suspended, and losing that would leave a claim
	// nobody holds.
	if mutateErr != nil && !errors.Is(mutateErr, session.ErrCanceled) {
		return mutateErr
	}
	if err := s.saveRun(tx, record); err != nil {
		return err
	}
	if mutateErr != nil {
		// Commit the recovered state first, then report why the caller's command
		// did not apply.
		if err := tx.Commit(); err != nil {
			return err
		}
		return mutateErr
	}
	return nil
}

func (s *SQLiteSessionRunStore) checkClaim(record storedRunRecord, claim session.Claim) error {
	if record.Receipt.AdmissionState != session.AdmissionClaimed || record.Claim != claim {
		return session.ErrStaleClaim
	}
	if !record.Claim.LeaseUntil.After(s.now()) {
		return session.ErrStaleClaim
	}
	return nil
}

func (s *SQLiteSessionRunStore) loadOne(ctx context.Context, runKey session.RunKey, resultErr *error) (storedRunRecord, error) {
	if err := runContextError(ctx); err != nil {
		return storedRunRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storedRunRecord{}, err
	}
	defer finishTx(tx, resultErr)
	record, ok, err := s.run(tx, runKey)
	if err != nil {
		return storedRunRecord{}, err
	}
	if !ok {
		return storedRunRecord{}, session.ErrRunNotFound
	}
	return record, nil
}

func (s *SQLiteSessionRunStore) run(tx *sql.Tx, runKey session.RunKey) (storedRunRecord, bool, error) {
	var payload []byte
	err := tx.QueryRow(`SELECT record FROM session_runs WHERE run_key = ?`, string(runKey)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRunRecord{}, false, nil
	}
	if err != nil {
		return storedRunRecord{}, false, err
	}
	var record storedRunRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return storedRunRecord{}, false, err
	}
	return record, true, nil
}

func (s *SQLiteSessionRunStore) runByRequest(tx *sql.Tx, tenant agent.TenantKey, key session.SessionKey, requestID string) (storedRunRecord, bool, error) {
	var payload []byte
	err := tx.QueryRow(`SELECT record FROM session_runs WHERE tenant_key = ? AND session_key = ? AND request_id = ?`,
		string(tenant), string(key), requestID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return storedRunRecord{}, false, nil
	}
	if err != nil {
		return storedRunRecord{}, false, err
	}
	var record storedRunRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return storedRunRecord{}, false, err
	}
	return record, true, nil
}

// eligibleRuns returns everything a worker could legally claim right now,
// leaving the concurrency limits and the ordering to the caller.
func (s *SQLiteSessionRunStore) eligibleRuns(tx *sql.Tx) ([]storedRunRecord, error) {
	rows, err := tx.Query(`SELECT record FROM session_runs
		WHERE admission_state = ? AND execution_state IN (?, ?) AND cancel_mode NOT IN (?, ?)
		ORDER BY priority DESC, created_at, run_key`,
		string(session.AdmissionQueued), string(session.ExecutionQueued), string(session.ExecutionMergePending),
		string(session.CancelSuspend), string(session.CancelAbandon))
	if err != nil {
		return nil, err
	}
	return scanRunRecords(rows)
}

func scanRunRecords(rows *sql.Rows) (records []storedRunRecord, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var record storedRunRecord
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *SQLiteSessionRunStore) queuedCounts(tx *sql.Tx, tenant agent.TenantKey, key session.SessionKey) (int, int, error) {
	var tenantCount, sessionCount int
	err := tx.QueryRow(`SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN session_key = ? THEN 1 ELSE 0 END), 0)
		FROM session_runs WHERE admission_state = ? AND tenant_key = ?`,
		string(key), string(session.AdmissionQueued), string(tenant)).Scan(&tenantCount, &sessionCount)
	return tenantCount, sessionCount, err
}

func (s *SQLiteSessionRunStore) activeCounts(tx *sql.Tx) (int, map[agent.TenantKey]int, map[sessionScopeKey]int, error) {
	rows, err := tx.Query(`SELECT tenant_key, session_key, COUNT(*) FROM session_runs
		WHERE admission_state = ? GROUP BY tenant_key, session_key`, string(session.AdmissionClaimed))
	if err != nil {
		return 0, nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	var (
		global  int
		tenants = make(map[agent.TenantKey]int)
		scopes  = make(map[sessionScopeKey]int)
	)
	for rows.Next() {
		var (
			tenant, key string
			count       int
		)
		if err := rows.Scan(&tenant, &key, &count); err != nil {
			return 0, nil, nil, err
		}
		global += count
		tenants[agent.TenantKey(tenant)] += count
		scopes[sessionScopeKey{tenant: agent.TenantKey(tenant), session: session.SessionKey(key)}] += count
	}
	if err := rows.Err(); err != nil {
		return 0, nil, nil, err
	}
	return global, tenants, scopes, nil
}

func (s *SQLiteSessionRunStore) sessionRevision(tx *sql.Tx, tenant agent.TenantKey, key session.SessionKey) (uint64, error) {
	var revision int64
	err := tx.QueryRow(`SELECT revision FROM session_run_revisions WHERE tenant_key = ? AND session_key = ?`,
		string(tenant), string(key)).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return uint64(revision), nil
}

func (s *SQLiteSessionRunStore) setSessionRevision(tx *sql.Tx, tenant agent.TenantKey, key session.SessionKey, revision uint64) error {
	_, err := tx.Exec(`INSERT INTO session_run_revisions(tenant_key, session_key, revision) VALUES(?, ?, ?)
		ON CONFLICT(tenant_key, session_key) DO UPDATE SET revision = excluded.revision`,
		string(tenant), string(key), int64(revision))
	return err
}

// nextClaimToken issues a monotonic fence. It is a stored counter rather than a
// timestamp because two claims issued in the same instant must still be
// distinguishable, and a stale worker's token must never be reachable again.
func (s *SQLiteSessionRunStore) nextClaimToken(tx *sql.Tx) (uint64, error) {
	if _, err := tx.Exec(`INSERT INTO session_run_claims(id, next_token) VALUES(1, 1)
		ON CONFLICT(id) DO UPDATE SET next_token = next_token + 1`); err != nil {
		return 0, err
	}
	var token int64
	if err := tx.QueryRow(`SELECT next_token FROM session_run_claims WHERE id = 1`).Scan(&token); err != nil {
		return 0, err
	}
	return uint64(token), nil
}

func (s *SQLiteSessionRunStore) insertRun(tx *sql.Tx, record storedRunRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO session_runs(run_key, tenant_key, session_key, request_id, admission_state,
		execution_state, cancel_mode, claim_token, lease_until, priority, created_at, record)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(record.Admission.RunKey), string(record.Admission.TenantKey), string(record.Admission.SessionKey),
		record.Admission.RequestID, string(record.Receipt.AdmissionState), string(record.Receipt.ExecutionState),
		string(record.CancelMode), int64(record.Claim.ClaimToken), nanos(record.Claim.LeaseUntil),
		record.Admission.Priority, record.Admission.CreatedAt.UnixNano(), payload)
	return err
}

func (s *SQLiteSessionRunStore) saveRun(tx *sql.Tx, record storedRunRecord) error {
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE session_runs SET admission_state = ?, execution_state = ?, cancel_mode = ?,
		claim_token = ?, lease_until = ?, record = ? WHERE run_key = ?`,
		string(record.Receipt.AdmissionState), string(record.Receipt.ExecutionState), string(record.CancelMode),
		int64(record.Claim.ClaimToken), nanos(record.Claim.LeaseUntil), payload, string(record.Admission.RunKey))
	return err
}
