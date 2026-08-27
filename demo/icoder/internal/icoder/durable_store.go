package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go/durable"
)

// SQLiteDurableStore is iCoder's persistent implementation of the durable run
// authority. It implements durable.Store, durable.ExecutionLedger,
// durable.UsageLedger, and the optional durable.EffectReplayer, which together
// let agent.Run execute durably through durable.CheckpointAdapter.
//
// Every contract method runs inside one immediate transaction, because the
// durable contract requires the four-way guard comparison (run key, lease
// owner, revision, fence token) and the write it authorizes to be atomic. The
// shared *sql.DB is opened with a single connection and _txlock=immediate, so
// transactions serialize rather than deadlock on upgrade.
type SQLiteDurableStore struct{ db *sql.DB }

// NewSQLiteDurableStore binds the durable ports to an already-migrated database.
func NewSQLiteDurableStore(db *sql.DB) *SQLiteDurableStore { return &SQLiteDurableStore{db: db} }

var (
	_ durable.Store           = (*SQLiteDurableStore)(nil)
	_ durable.ExecutionLedger = (*SQLiteDurableStore)(nil)
	_ durable.UsageLedger     = (*SQLiteDurableStore)(nil)
	_ durable.EffectReplayer  = (*SQLiteDurableStore)(nil)
)

// durableMigrations creates the durable tables. They are deliberately separate
// from the conversation tables: a run's recovery state has a different lifetime
// and a different owner than the session transcript it belongs to.
var durableMigrations = []string{
	`CREATE TABLE IF NOT EXISTS durable_runs (
		run_key TEXT PRIMARY KEY,
		agent_key TEXT NOT NULL,
		session_id TEXT NOT NULL,
		request_id TEXT NOT NULL,
		input_digest TEXT NOT NULL,
		config_digest TEXT NOT NULL,
		begin_digest TEXT NOT NULL,
		schema_version INTEGER NOT NULL,
		status TEXT NOT NULL,
		phase TEXT NOT NULL,
		revision INTEGER NOT NULL DEFAULT 0,
		fence_token INTEGER NOT NULL DEFAULT 0,
		lease_owner TEXT NOT NULL DEFAULT '',
		lease_until INTEGER NOT NULL DEFAULT 0,
		checkpoint BLOB NOT NULL,
		failure BLOB,
		updated_at INTEGER NOT NULL,
		UNIQUE (session_id, request_id)
	)`,
	`CREATE INDEX IF NOT EXISTS durable_runs_recovery
		ON durable_runs(status, lease_until, run_key)`,
	`CREATE TABLE IF NOT EXISTS durable_effects (
		execution_key TEXT PRIMARY KEY,
		digest TEXT NOT NULL,
		run_key TEXT NOT NULL,
		attempt_key TEXT NOT NULL,
		step_number INTEGER NOT NULL,
		ordinal INTEGER NOT NULL,
		call_id TEXT NOT NULL,
		tool_call BLOB NOT NULL,
		status TEXT NOT NULL,
		fence_token INTEGER NOT NULL DEFAULT 0,
		result BLOB,
		failure BLOB,
		prepared_at INTEGER NOT NULL,
		started_at INTEGER NOT NULL DEFAULT 0,
		finished_at INTEGER NOT NULL DEFAULT 0,
		UNIQUE (run_key, attempt_key, step_number, ordinal),
		UNIQUE (run_key, attempt_key, step_number, call_id)
	)`,
	`CREATE INDEX IF NOT EXISTS durable_effects_by_run ON durable_effects(run_key, step_number, ordinal)`,
	`CREATE TABLE IF NOT EXISTS durable_usage (
		tenant_key TEXT NOT NULL,
		usage_key TEXT NOT NULL,
		run_key TEXT NOT NULL,
		attempt_key TEXT NOT NULL,
		payload BLOB NOT NULL,
		PRIMARY KEY (tenant_key, usage_key)
	)`,
	`CREATE INDEX IF NOT EXISTS durable_usage_by_attempt ON durable_usage(tenant_key, attempt_key, usage_key)`,
}

// durableFailf builds an error that satisfies errors.Is for the durable
// sentinel. The durable package keeps its Error type's constructor unexported,
// so adapters outside the module wrap the sentinel instead.
func durableFailf(kind error, operation string, runKey durable.RunKey, detail string) error {
	if detail == "" {
		return fmt.Errorf("icoder durable: %s run=%q: %w", operation, runKey, kind)
	}
	return fmt.Errorf("icoder durable: %s run=%q: %s: %w", operation, runKey, detail, kind)
}

// runRow is the decoded durable_runs row plus the values every guarded write
// needs to compare before it may proceed.
type runRow struct {
	snapshot durable.Snapshot
	beginDig string
}

func (s *SQLiteDurableStore) begin(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

// finish commits on success and always releases the transaction. Rollback after
// a commit is a no-op that reports sql.ErrTxDone, which is not a failure.
func finishTx(tx *sql.Tx, err *error) {
	if *err != nil {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			*err = errors.Join(*err, rollbackErr)
		}
		return
	}
	if commitErr := tx.Commit(); commitErr != nil {
		*err = commitErr
	}
}

func nanos(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func fromNanos(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}

func selectRun(ctx context.Context, tx *sql.Tx, key durable.RunKey) (runRow, error) {
	var row runRow
	var checkpoint, failure []byte
	var leaseUntil int64
	err := tx.QueryRowContext(ctx, `SELECT agent_key, session_id, request_id, input_digest, config_digest, begin_digest, schema_version, status, phase, revision, fence_token, lease_owner, lease_until, checkpoint, failure FROM durable_runs WHERE run_key = ?`, string(key)).
		Scan(&row.snapshot.Identity.AgentKey, &row.snapshot.Identity.SessionID, &row.snapshot.Identity.RequestID,
			&row.snapshot.InputDigest, &row.snapshot.ConfigDigest, &row.beginDig, &row.snapshot.SchemaVersion,
			&row.snapshot.Status, &row.snapshot.Phase, &row.snapshot.Revision, &row.snapshot.FenceToken,
			&row.snapshot.LeaseOwner, &leaseUntil, &checkpoint, &failure)
	if errors.Is(err, sql.ErrNoRows) {
		return runRow{}, durableFailf(durable.ErrRunNotFound, "load", key, "")
	}
	if err != nil {
		return runRow{}, err
	}
	row.snapshot.Identity.RunKey = string(key)
	row.snapshot.LeaseUntil = fromNanos(leaseUntil)
	if err := json.Unmarshal(checkpoint, &row.snapshot.Checkpoint); err != nil {
		return runRow{}, fmt.Errorf("decode durable checkpoint: %w", err)
	}
	if len(failure) > 0 {
		var value durable.Failure
		if err := json.Unmarshal(failure, &value); err != nil {
			return runRow{}, fmt.Errorf("decode durable failure: %w", err)
		}
		row.snapshot.Failure = &value
	}
	return row, nil
}

// guardedRun enforces the durable write authority: the run must exist, must not
// be terminal, and must match the caller's owner, fence, and revision exactly.
func guardedRun(ctx context.Context, tx *sql.Tx, guard durable.Guard, operation string) (durable.Snapshot, error) {
	row, err := selectRun(ctx, tx, guard.RunKey)
	if err != nil {
		return durable.Snapshot{}, err
	}
	if isTerminalStatus(row.snapshot.Status) {
		return durable.Snapshot{}, durableFailf(durable.ErrTerminal, operation, guard.RunKey, "")
	}
	if row.snapshot.Status != durable.StatusRunning || row.snapshot.LeaseOwner != guard.LeaseOwner ||
		row.snapshot.FenceToken != guard.FenceToken || row.snapshot.Revision != guard.Revision {
		return durable.Snapshot{}, durableFailf(durable.ErrLeaseLost, operation, guard.RunKey, "stale owner, fence, or revision")
	}
	return row.snapshot, nil
}

func isTerminalStatus(status durable.Status) bool {
	return status == durable.StatusCompleted || status == durable.StatusFailed || status == durable.StatusAbandoned
}

// validStatusPhase and validPhaseEdge mirror the durable package's frozen v1
// state machine. They are duplicated here rather than exported from the library
// because they describe the contract every adapter must independently enforce.
func validStatusPhase(status durable.Status, phase durable.Phase) bool {
	switch status {
	case durable.StatusClaimed:
		return phase == durable.PhaseModelReady
	case durable.StatusRunning:
		return phase == durable.PhaseModelReady || phase == durable.PhaseModelInflight ||
			phase == durable.PhaseToolsReady || phase == durable.PhaseToolInflight || phase == durable.PhaseFinalizing
	case durable.StatusSuspended:
		return phase == durable.PhaseModelReady || phase == durable.PhaseToolsReady
	case durable.StatusCompleted, durable.StatusFailed, durable.StatusAbandoned:
		return phase == durable.PhaseTerminal
	default:
		return false
	}
}

func validPhaseEdge(from, to durable.Phase) bool {
	if to == durable.PhaseTerminal {
		return true
	}
	switch from {
	case durable.PhaseModelReady:
		return to == durable.PhaseModelReady || to == durable.PhaseModelInflight
	case durable.PhaseModelInflight:
		return to == durable.PhaseModelInflight || to == durable.PhaseModelReady || to == durable.PhaseToolsReady || to == durable.PhaseFinalizing
	case durable.PhaseToolsReady:
		return to == durable.PhaseToolsReady || to == durable.PhaseToolInflight || to == durable.PhaseModelReady || to == durable.PhaseFinalizing
	case durable.PhaseToolInflight:
		return to == durable.PhaseToolInflight || to == durable.PhaseToolsReady || to == durable.PhaseModelReady || to == durable.PhaseFinalizing
	case durable.PhaseFinalizing:
		return to == durable.PhaseFinalizing
	default:
		return false
	}
}

func safeSuspensionPhase(phase durable.Phase) durable.Phase {
	if phase == durable.PhaseToolsReady || phase == durable.PhaseToolInflight {
		return durable.PhaseToolsReady
	}
	return durable.PhaseModelReady
}

// Begin is create-if-absent by run key. A replay with byte-identical immutable
// input returns the stored snapshot; any difference is a conflict rather than a
// silent overwrite, because the digests pin what this run is allowed to be.
func (s *SQLiteDurableStore) Begin(ctx context.Context, request durable.BeginRequest) (snapshot durable.Snapshot, created bool, err error) {
	key := durable.RunKey(request.Identity.RunKey)
	if key == "" || request.Identity.AgentKey == "" || request.Identity.SessionID == "" || request.Identity.RequestID == "" || request.InputDigest == "" || request.ConfigDigest == "" {
		return durable.Snapshot{}, false, durableFailf(durable.ErrRunConflict, "begin", key, "missing immutable field")
	}
	beginDigest, err := durable.CanonicalDigest(request)
	if err != nil {
		return durable.Snapshot{}, false, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.Snapshot{}, false, err
	}
	defer finishTx(tx, &err)

	existing, loadErr := selectRun(ctx, tx, key)
	if loadErr == nil {
		if existing.beginDig != beginDigest {
			return durable.Snapshot{}, false, durableFailf(durable.ErrRunConflict, "begin", key, "immutable values differ")
		}
		return existing.snapshot, false, nil
	}
	if !errors.Is(loadErr, durable.ErrRunNotFound) {
		return durable.Snapshot{}, false, loadErr
	}
	var otherRun string
	pairErr := tx.QueryRowContext(ctx, `SELECT run_key FROM durable_runs WHERE session_id = ? AND request_id = ?`, request.Identity.SessionID, request.Identity.RequestID).Scan(&otherRun)
	if pairErr == nil && otherRun != string(key) {
		return durable.Snapshot{}, false, durableFailf(durable.ErrRunConflict, "begin", key, "session and request already identify another run")
	}
	if pairErr != nil && !errors.Is(pairErr, sql.ErrNoRows) {
		return durable.Snapshot{}, false, pairErr
	}
	snapshot = durable.Snapshot{
		SchemaVersion: durable.SnapshotSchemaVersion, Identity: request.Identity,
		InputDigest: request.InputDigest, ConfigDigest: request.ConfigDigest,
		Status: durable.StatusClaimed, Phase: durable.PhaseModelReady, Checkpoint: request.Checkpoint,
	}
	checkpoint, err := json.Marshal(snapshot.Checkpoint)
	if err != nil {
		return durable.Snapshot{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO durable_runs(run_key, agent_key, session_id, request_id, input_digest, config_digest, begin_digest, schema_version, status, phase, revision, fence_token, lease_owner, lease_until, checkpoint, failure, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, '', 0, ?, NULL, ?)`,
		string(key), request.Identity.AgentKey, request.Identity.SessionID, request.Identity.RequestID,
		request.InputDigest, request.ConfigDigest, beginDigest, durable.SnapshotSchemaVersion,
		string(durable.StatusClaimed), string(durable.PhaseModelReady), checkpoint, time.Now().UTC().UnixNano()); err != nil {
		return durable.Snapshot{}, false, err
	}
	return snapshot, true, nil
}

// Load is read-only and authorizes nothing on its own.
func (s *SQLiteDurableStore) Load(ctx context.Context, key durable.RunKey) (snapshot durable.Snapshot, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	row, err := selectRun(ctx, tx, key)
	if err != nil {
		return durable.Snapshot{}, err
	}
	return row.snapshot, nil
}

// Acquire grants or takes over a lease and raises the fence, which is what makes
// a stale worker's later write provably unauthorized.
func (s *SQLiteDurableStore) Acquire(ctx context.Context, request durable.AcquireRequest) (snapshot durable.Snapshot, err error) {
	if request.Owner == "" || request.Now.IsZero() || !request.LeaseUntil.After(request.Now) {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "acquire", request.RunKey, "invalid owner or lease interval")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	row, err := selectRun(ctx, tx, request.RunKey)
	if err != nil {
		return durable.Snapshot{}, err
	}
	current := row.snapshot
	if isTerminalStatus(current.Status) {
		return durable.Snapshot{}, durableFailf(durable.ErrTerminal, "acquire", request.RunKey, "")
	}
	if current.Status == durable.StatusRunning && current.LeaseUntil.After(request.Now) {
		return durable.Snapshot{}, durableFailf(durable.ErrLeaseHeld, "acquire", request.RunKey, "active lease")
	}
	if current.Status == durable.StatusSuspended && !request.AllowSuspended {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "acquire", request.RunKey, "suspended recovery not authorized")
	}
	if current.FenceToken == ^uint64(0) || current.Revision == ^uint64(0) {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "acquire", request.RunKey, "counter overflow")
	}
	current.Status, current.LeaseOwner, current.LeaseUntil = durable.StatusRunning, request.Owner, request.LeaseUntil
	current.FenceToken++
	current.Revision++
	if _, err = tx.ExecContext(ctx, `UPDATE durable_runs SET status = ?, lease_owner = ?, lease_until = ?, fence_token = ?, revision = ?, updated_at = ? WHERE run_key = ?`,
		string(current.Status), current.LeaseOwner, nanos(current.LeaseUntil), current.FenceToken, current.Revision, nanos(request.Now), string(request.RunKey)); err != nil {
		return durable.Snapshot{}, err
	}
	return current, nil
}

// Renew extends a live lease. An expired lease is never renewed, because another
// worker may already have taken it over.
func (s *SQLiteDurableStore) Renew(ctx context.Context, guard durable.Guard, now, leaseUntil time.Time) (snapshot durable.Snapshot, err error) {
	if now.IsZero() || !leaseUntil.After(now) {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "renew", guard.RunKey, "invalid lease interval")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	current, err := guardedRun(ctx, tx, guard, "renew")
	if err != nil {
		return durable.Snapshot{}, err
	}
	if !current.LeaseUntil.After(now) {
		return durable.Snapshot{}, durableFailf(durable.ErrLeaseLost, "renew", guard.RunKey, "lease expired")
	}
	current.LeaseUntil = leaseUntil
	current.Revision++
	if _, err = tx.ExecContext(ctx, `UPDATE durable_runs SET lease_until = ?, revision = ?, updated_at = ? WHERE run_key = ?`,
		nanos(leaseUntil), current.Revision, nanos(now), string(guard.RunKey)); err != nil {
		return durable.Snapshot{}, err
	}
	return current, nil
}

// Release suspends a run at a safe phase and drops the lease without changing
// the checkpoint. Callers that must persist a blocker use Save instead.
func (s *SQLiteDurableStore) Release(ctx context.Context, guard durable.Guard, phase durable.Phase) (snapshot durable.Snapshot, err error) {
	if phase != durable.PhaseModelReady && phase != durable.PhaseToolsReady {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "release", guard.RunKey, "suspension requires a safe v1 phase")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	current, err := guardedRun(ctx, tx, guard, "release")
	if err != nil {
		return durable.Snapshot{}, err
	}
	current.Status, current.Phase = durable.StatusSuspended, phase
	current.LeaseOwner, current.LeaseUntil = "", time.Time{}
	current.Revision++
	if _, err = tx.ExecContext(ctx, `UPDATE durable_runs SET status = ?, phase = ?, lease_owner = '', lease_until = 0, revision = ?, updated_at = ? WHERE run_key = ?`,
		string(current.Status), string(current.Phase), current.Revision, time.Now().UTC().UnixNano(), string(guard.RunKey)); err != nil {
		return durable.Snapshot{}, err
	}
	return current, nil
}

// Save is the single guarded mutation every run-lifecycle boundary goes through.
func (s *SQLiteDurableStore) Save(ctx context.Context, request durable.SaveRequest) (snapshot durable.Snapshot, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	snapshot, err = saveInTx(ctx, tx, request)
	return snapshot, err
}

// CompleteInTx finalizes a durable run inside a transaction the caller already
// owns, so the run's terminal state and the domain records it authorizes become
// visible together or not at all.
func (s *SQLiteDurableStore) CompleteInTx(ctx context.Context, tx *sql.Tx, guard durable.Guard, checkpoint durable.Checkpoint) (durable.Snapshot, error) {
	return saveInTx(ctx, tx, durable.SaveRequest{Guard: guard, Status: durable.StatusCompleted, Phase: durable.PhaseTerminal, Checkpoint: checkpoint})
}

func saveInTx(ctx context.Context, tx *sql.Tx, request durable.SaveRequest) (durable.Snapshot, error) {
	current, err := guardedRun(ctx, tx, request.Guard, "save")
	if err != nil {
		return durable.Snapshot{}, err
	}
	if current.Revision == ^uint64(0) {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "save", request.Guard.RunKey, "revision overflow")
	}
	if !validStatusPhase(request.Status, request.Phase) || request.Status == durable.StatusClaimed || !validPhaseEdge(current.Phase, request.Phase) {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "save", request.Guard.RunKey, "illegal status or phase edge")
	}
	if request.Status == durable.StatusCompleted && current.Phase != durable.PhaseFinalizing {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "save", request.Guard.RunKey, "completion requires finalizing")
	}
	if request.Failure != nil && (request.Status == durable.StatusRunning || request.Status == durable.StatusCompleted) {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "save", request.Guard.RunKey, "failure on non-failed state")
	}
	current.Status, current.Phase = request.Status, request.Phase
	current.Checkpoint, current.Failure = request.Checkpoint, request.Failure
	if request.Status != durable.StatusRunning {
		current.LeaseOwner, current.LeaseUntil = "", time.Time{}
	}
	current.Revision++
	checkpoint, err := json.Marshal(current.Checkpoint)
	if err != nil {
		return durable.Snapshot{}, err
	}
	var failure []byte
	if current.Failure != nil {
		if failure, err = json.Marshal(current.Failure); err != nil {
			return durable.Snapshot{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE durable_runs SET status = ?, phase = ?, revision = ?, lease_owner = ?, lease_until = ?, checkpoint = ?, failure = ?, updated_at = ? WHERE run_key = ?`,
		string(current.Status), string(current.Phase), current.Revision, current.LeaseOwner, nanos(current.LeaseUntil),
		checkpoint, failure, time.Now().UTC().UnixNano(), string(request.Guard.RunKey)); err != nil {
		return durable.Snapshot{}, err
	}
	return current, nil
}

// RevokeLease fences a worker that may still be alive and, in the same
// transaction, marks its running effects unknown. Doing both atomically is what
// lets recovery decide safely instead of guessing whether a tool already ran.
func (s *SQLiteDurableStore) RevokeLease(ctx context.Context, request durable.RevokeLeaseRequest) (snapshot durable.Snapshot, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	row, err := selectRun(ctx, tx, request.RunKey)
	if err != nil {
		return durable.Snapshot{}, err
	}
	current := row.snapshot
	if current.Revision != request.ExpectedRevision || current.FenceToken != request.ExpectedFence || current.Status != durable.StatusRunning {
		return durable.Snapshot{}, durableFailf(durable.ErrLeaseLost, "revoke", request.RunKey, "stale revision or fence")
	}
	if request.RequireExpired && current.LeaseUntil.After(request.Now) {
		return durable.Snapshot{}, durableFailf(durable.ErrLeaseHeld, "revoke", request.RunKey, "lease remains active")
	}
	if current.FenceToken == ^uint64(0) || current.Revision == ^uint64(0) {
		return durable.Snapshot{}, durableFailf(durable.ErrInvalidTransition, "revoke", request.RunKey, "counter overflow")
	}
	marked, err := tx.ExecContext(ctx, `UPDATE durable_effects SET status = ?, finished_at = ? WHERE run_key = ? AND status = ?`,
		string(durable.EffectUnknown), nanos(request.Now), string(request.RunKey), string(durable.EffectRunning))
	if err != nil {
		return durable.Snapshot{}, err
	}
	affected, err := marked.RowsAffected()
	if err != nil {
		return durable.Snapshot{}, err
	}
	phase := safeSuspensionPhase(current.Phase)
	if affected > 0 {
		phase = durable.PhaseToolsReady
	}
	if current.Phase == durable.PhaseFinalizing {
		// The finalizer-only phase is preserved while its stale worker is fenced.
		// The immediately expired recovery lease keeps the running snapshot
		// coherent and still lets a finalizer acquire a fresh fence.
		current.LeaseOwner, current.LeaseUntil = "recovery-revoked", request.Now
	} else {
		current.Status, current.Phase = durable.StatusSuspended, phase
		current.LeaseOwner, current.LeaseUntil = "", time.Time{}
	}
	current.FenceToken++
	current.Revision++
	if _, err = tx.ExecContext(ctx, `UPDATE durable_runs SET status = ?, phase = ?, lease_owner = ?, lease_until = ?, fence_token = ?, revision = ?, updated_at = ? WHERE run_key = ?`,
		string(current.Status), string(current.Phase), current.LeaseOwner, nanos(current.LeaseUntil),
		current.FenceToken, current.Revision, nanos(request.Now), string(request.RunKey)); err != nil {
		return durable.Snapshot{}, err
	}
	return current, nil
}

// Scan pages recoverable work in run-key order. ExpiredBefore narrows the page
// to leases that have lapsed, which is how the reconciler finds abandoned runs.
func (s *SQLiteDurableStore) Scan(ctx context.Context, request durable.ScanRequest) (page durable.ScanPage, err error) {
	if request.Limit <= 0 || request.Limit > 1000 {
		return durable.ScanPage{}, durableFailf(durable.ErrLimitExceeded, "scan", "", "invalid limit")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.ScanPage{}, err
	}
	defer finishTx(tx, &err)

	query := `SELECT run_key FROM durable_runs WHERE run_key > ?`
	args := []any{string(request.Cursor)}
	if len(request.Statuses) > 0 {
		query += ` AND status IN (`
		for i, status := range request.Statuses {
			if i > 0 {
				query += `, `
			}
			query += `?`
			args = append(args, string(status))
		}
		query += `)`
	}
	if !request.ExpiredBefore.IsZero() {
		query += ` AND status = ? AND lease_until <= ?`
		args = append(args, string(durable.StatusRunning), nanos(request.ExpiredBefore))
	}
	query += ` ORDER BY run_key LIMIT ?`
	args = append(args, request.Limit+1)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return durable.ScanPage{}, err
	}
	var keys []durable.RunKey
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			_ = rows.Close()
			return durable.ScanPage{}, err
		}
		keys = append(keys, durable.RunKey(key))
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return durable.ScanPage{}, err
	}
	if len(keys) > request.Limit {
		keys = keys[:request.Limit]
		page.Next = keys[len(keys)-1]
	}
	for _, key := range keys {
		row, loadErr := selectRun(ctx, tx, key)
		if loadErr != nil {
			return durable.ScanPage{}, loadErr
		}
		page.Snapshots = append(page.Snapshots, row.snapshot)
	}
	return page, nil
}

// Durable returns the durable run authority backed by this store's database, so
// callers compose recovery on the same connection and therefore the same
// transaction boundary as the conversation records.
func (s *Store) Durable() *SQLiteDurableStore { return NewSQLiteDurableStore(s.db) }
