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

// SQLiteSessionService is iCoder's persistent implementation of session.Service,
// the aggregate that owns a conversation's revision, its usage totals, and the
// branches that may fast-forward into it.
//
// Its failure modes are quiet and expensive - a merge applied twice duplicates a
// turn, a usage fact counted twice double-bills, a finished session that still
// accepts work reports a total already sent somewhere as final - so every
// command runs inside one immediate transaction and every legality question is
// answered by the session package's exported state machine rather than by this
// adapter's own reading of the rules.
type SQLiteSessionService struct {
	db  *sql.DB
	now func() time.Time
}

// NewSQLiteSessionService binds the session aggregate to an already-migrated
// database.
func NewSQLiteSessionService(db *sql.DB) *SQLiteSessionService {
	return &SQLiteSessionService{db: db, now: func() time.Time { return time.Now().UTC() }}
}

var _ session.Service = (*SQLiteSessionService)(nil)

// sessionMigrations creates the aggregate tables.
//
// Revisions are kept as whole rows rather than as a delta log: GetRevision must
// answer "what did this session look like then" after arbitrary later mutations,
// and replaying deltas to answer it would make every read depend on the
// correctness of every past write.
var sessionMigrations = []string{
	`CREATE TABLE IF NOT EXISTS session_aggregates (
		tenant_key TEXT NOT NULL,
		session_key TEXT NOT NULL,
		status TEXT NOT NULL,
		revision INTEGER NOT NULL,
		create_command BLOB NOT NULL,
		snapshot BLOB NOT NULL,
		PRIMARY KEY (tenant_key, session_key)
	)`,
	`CREATE TABLE IF NOT EXISTS session_revisions (
		tenant_key TEXT NOT NULL,
		session_key TEXT NOT NULL,
		revision INTEGER NOT NULL,
		snapshot BLOB NOT NULL,
		PRIMARY KEY (tenant_key, session_key, revision)
	)`,
	`CREATE TABLE IF NOT EXISTS session_branches (
		tenant_key TEXT NOT NULL,
		branch_key TEXT NOT NULL,
		session_key TEXT NOT NULL,
		run_key TEXT NOT NULL,
		status TEXT NOT NULL,
		version INTEGER NOT NULL,
		branch BLOB NOT NULL,
		PRIMARY KEY (tenant_key, branch_key)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS session_branches_run
		ON session_branches(tenant_key, run_key)`,
	`CREATE TABLE IF NOT EXISTS session_usage_facts (
		tenant_key TEXT NOT NULL,
		usage_fact_key TEXT NOT NULL,
		session_key TEXT NOT NULL,
		prompt_tokens INTEGER NOT NULL,
		completion_tokens INTEGER NOT NULL,
		cost_micros INTEGER NOT NULL,
		PRIMARY KEY (tenant_key, usage_fact_key)
	)`,
}

// Create registers a session, or recognizes a replay of the same creation.
func (s *SQLiteSessionService) Create(ctx context.Context, command session.CreateCommand) (snapshot session.Snapshot, resultErr error) {
	if err := sessionScope(ctx, command.TenantKey, command.SessionKey); err != nil {
		return session.Snapshot{}, err
	}
	if command.UserKey == "" || command.AgentKey == "" || command.Identity == "" {
		return session.Snapshot{}, fmt.Errorf("%w: user, agent, and identity are required", session.ErrInvalidCommand)
	}
	if command.MetadataVersion == 0 {
		command.MetadataVersion = 1
	}
	if !session.ValidContextPivot(command.ContextPivot, 0) {
		return session.Snapshot{}, fmt.Errorf("%w: invalid initial context pivot", session.ErrSnapshotInvariant)
	}
	command.Metadata = append([]byte(nil), command.Metadata...)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)

	if stored, created, ok, err := s.loadSession(tx, command.TenantKey, command.SessionKey); err != nil {
		return session.Snapshot{}, err
	} else if ok {
		if session.SameCreate(command, created) {
			return stored, nil
		}
		return session.Snapshot{}, fmt.Errorf("%w: session key %q has different immutable input", session.ErrIdempotencyConflict, command.SessionKey)
	}

	now := s.now()
	snapshot = session.Snapshot{
		TenantKey: command.TenantKey, SessionKey: command.SessionKey, UserKey: command.UserKey,
		AgentKey: command.AgentKey, Identity: command.Identity, Status: session.StatusActive,
		Title: command.Title, ContextPivot: command.ContextPivot,
		RuntimeDefinitionKey: command.RuntimeDefinitionKey, MetadataVersion: command.MetadataVersion,
		Metadata: append([]byte(nil), command.Metadata...), CreatedAt: now, UpdatedAt: now,
	}
	createPayload, err := json.Marshal(command)
	if err != nil {
		return session.Snapshot{}, err
	}
	snapshotPayload, err := json.Marshal(snapshot)
	if err != nil {
		return session.Snapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_aggregates(tenant_key, session_key, status, revision, create_command, snapshot)
		VALUES(?, ?, ?, 0, ?, ?)`,
		string(command.TenantKey), string(command.SessionKey), string(session.StatusActive), createPayload, snapshotPayload); err != nil {
		return session.Snapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_revisions(tenant_key, session_key, revision, snapshot) VALUES(?, ?, 0, ?)`,
		string(command.TenantKey), string(command.SessionKey), snapshotPayload); err != nil {
		return session.Snapshot{}, err
	}
	return session.CloneSnapshot(snapshot), nil
}

// Get returns the session's current snapshot.
func (s *SQLiteSessionService) Get(ctx context.Context, query session.GetQuery) (snapshot session.Snapshot, resultErr error) {
	if err := sessionScope(ctx, query.TenantKey, query.SessionKey); err != nil {
		return session.Snapshot{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	stored, _, ok, err := s.loadSession(tx, query.TenantKey, query.SessionKey)
	if err != nil {
		return session.Snapshot{}, err
	}
	if !ok {
		return session.Snapshot{}, session.ErrSessionNotFound
	}
	return stored, nil
}

// GetRevision returns what the session looked like at one past revision, which
// is what lets a branch prove it is still based on the revision it forked from.
func (s *SQLiteSessionService) GetRevision(ctx context.Context, query session.RevisionQuery) (snapshot session.Snapshot, resultErr error) {
	if err := sessionScope(ctx, query.TenantKey, query.SessionKey); err != nil {
		return session.Snapshot{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	if _, _, ok, err := s.loadSession(tx, query.TenantKey, query.SessionKey); err != nil {
		return session.Snapshot{}, err
	} else if !ok {
		return session.Snapshot{}, session.ErrSessionNotFound
	}
	var payload []byte
	err = tx.QueryRowContext(ctx, `SELECT snapshot FROM session_revisions WHERE tenant_key = ? AND session_key = ? AND revision = ?`,
		string(query.TenantKey), string(query.SessionKey), int64(query.Revision)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Snapshot{}, session.ErrRevisionNotFound
	}
	if err != nil {
		return session.Snapshot{}, err
	}
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return session.Snapshot{}, err
	}
	return snapshot, nil
}

// CreateBranch forks a run off the session's current revision, idempotently by
// run key so a retried run does not get a second branch to merge from.
func (s *SQLiteSessionService) CreateBranch(ctx context.Context, command session.CreateBranchCommand) (branch session.Branch, resultErr error) {
	if err := sessionScope(ctx, command.TenantKey, command.SessionKey); err != nil || !command.RunKey.Valid() {
		return session.Branch{}, fmt.Errorf("%w: tenant, session, and run are required", session.ErrInvalidCommand)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Branch{}, err
	}
	defer finishTx(tx, &resultErr)

	if existing, ok, err := s.branchByRun(tx, command.TenantKey, command.RunKey); err != nil {
		return session.Branch{}, err
	} else if ok {
		if existing.SessionKey == command.SessionKey && existing.BaseRevision == command.BaseRevision {
			return existing, nil
		}
		return session.Branch{}, fmt.Errorf("%w: run key %q has different immutable input", session.ErrIdempotencyConflict, command.RunKey)
	}
	stored, _, ok, err := s.loadSession(tx, command.TenantKey, command.SessionKey)
	if err != nil {
		return session.Branch{}, err
	}
	if !ok {
		return session.Branch{}, session.ErrSessionNotFound
	}
	if stored.Revision != command.BaseRevision {
		return session.Branch{}, fmt.Errorf("%w: base %d, current %d", session.ErrRevisionConflict, command.BaseRevision, stored.Revision)
	}
	if stored.Status == session.StatusCompleted || stored.Status == session.StatusAbandoned {
		return session.Branch{}, fmt.Errorf("%w: terminal session", session.ErrInvalidSessionTransition)
	}
	now := s.now()
	branch = session.Branch{
		TenantKey: command.TenantKey, BranchKey: session.DeriveBranchKey(command.TenantKey, command.RunKey),
		SessionKey: command.SessionKey, RunKey: command.RunKey,
		BaseRevision: command.BaseRevision, HeadRevision: command.BaseRevision,
		Status: session.BranchStatusOpen, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.insertBranch(tx, branch); err != nil {
		return session.Branch{}, err
	}
	return branch, nil
}

// GetBranch returns one branch, scoped to the session that owns it.
func (s *SQLiteSessionService) GetBranch(ctx context.Context, query session.GetBranchQuery) (branch session.Branch, resultErr error) {
	if err := sessionScope(ctx, query.TenantKey, query.SessionKey); err != nil || !query.BranchKey.Valid() {
		return session.Branch{}, fmt.Errorf("%w: tenant, session, and branch are required", session.ErrInvalidCommand)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Branch{}, err
	}
	defer finishTx(tx, &resultErr)
	stored, ok, err := s.branch(tx, query.TenantKey, query.BranchKey)
	if err != nil {
		return session.Branch{}, err
	}
	if !ok || stored.SessionKey != query.SessionKey {
		return session.Branch{}, session.ErrBranchNotFound
	}
	return stored, nil
}

// MarkBranchReady declares a branch mergeable at a head revision.
func (s *SQLiteSessionService) MarkBranchReady(ctx context.Context, command session.BranchCommand) (session.Branch, error) {
	return s.transitionBranch(ctx, command.TenantKey, command.SessionKey, command.BranchKey, command.ExpectedVersion,
		session.BranchStatusReadyToMerge, command.HeadRevision, session.MergeKindNone)
}

// MarkBranchConflict records that a branch can no longer fast-forward.
func (s *SQLiteSessionService) MarkBranchConflict(ctx context.Context, command session.ConflictCommand) (session.Branch, error) {
	if command.MergeKind == session.MergeKindNone {
		command.MergeKind = session.MergeKindFastForward
	}
	if command.MergeKind != session.MergeKindFastForward {
		return session.Branch{}, fmt.Errorf("%w: unsupported merge kind %q", session.ErrInvalidCommand, command.MergeKind)
	}
	return s.transitionBranch(ctx, command.TenantKey, command.SessionKey, command.BranchKey, command.ExpectedVersion,
		session.BranchStatusConflicted, 0, command.MergeKind)
}

// AbandonBranch closes a branch without merging it.
func (s *SQLiteSessionService) AbandonBranch(ctx context.Context, command session.BranchCommand) (session.Branch, error) {
	return s.transitionBranch(ctx, command.TenantKey, command.SessionKey, command.BranchKey, command.ExpectedVersion,
		session.BranchStatusAbandoned, 0, session.MergeKindNone)
}

func (s *SQLiteSessionService) transitionBranch(ctx context.Context, tenant agent.TenantKey, sessionKey session.SessionKey,
	key session.BranchKey, expected uint64, target session.BranchStatus, head uint64, kind session.MergeKind) (branch session.Branch, resultErr error) {
	if err := sessionScope(ctx, tenant, sessionKey); err != nil || !key.Valid() {
		return session.Branch{}, fmt.Errorf("%w: incomplete branch command", session.ErrInvalidCommand)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Branch{}, err
	}
	defer finishTx(tx, &resultErr)
	stored, ok, err := s.branch(tx, tenant, key)
	if err != nil {
		return session.Branch{}, err
	}
	if !ok || stored.SessionKey != sessionKey {
		return session.Branch{}, session.ErrBranchNotFound
	}
	if stored.Status == session.BranchStatusMerged || stored.Status == session.BranchStatusAbandoned {
		return session.Branch{}, session.ErrBranchClosed
	}
	if stored.Version != expected {
		return session.Branch{}, fmt.Errorf("%w: expected %d, current %d", session.ErrBranchVersionConflict, expected, stored.Version)
	}
	if !session.ValidBranchTransition(stored.Status, target) {
		return session.Branch{}, fmt.Errorf("%w: %s to %s", session.ErrInvalidBranchTransition, stored.Status, target)
	}
	switch target {
	case session.BranchStatusReadyToMerge:
		if head == 0 {
			head = stored.HeadRevision
		}
		if head < stored.BaseRevision {
			return session.Branch{}, fmt.Errorf("%w: head revision precedes base", session.ErrInvalidCommand)
		}
		stored.HeadRevision = head
		stored.MergeKind = session.MergeKindNone
	case session.BranchStatusConflicted:
		stored.MergeKind = kind
	}
	stored.Status = target
	stored.Version++
	stored.UpdatedAt = s.now()
	if err := s.saveBranch(tx, stored); err != nil {
		return session.Branch{}, err
	}
	return stored, nil
}

// CommitMerge fast-forwards a ready branch into the session exactly once.
//
// The preconditions are checked together with the write: the branch must still
// be ready at the version the caller saw, and the session must still sit on the
// revision the branch forked from. Anything else is reported as a merge conflict
// rather than resolved here, because resolving it would mean inventing a history
// neither side agreed to.
func (s *SQLiteSessionService) CommitMerge(ctx context.Context, command session.MergeCommit) (result session.MergeResult, resultErr error) {
	if err := sessionScope(ctx, command.TenantKey, command.SessionKey); err != nil || !command.BranchKey.Valid() {
		return session.MergeResult{}, fmt.Errorf("%w: incomplete merge command", session.ErrInvalidCommand)
	}
	if command.MergeKind == session.MergeKindNone {
		command.MergeKind = session.MergeKindFastForward
	}
	if command.MergeKind != session.MergeKindFastForward {
		return session.MergeResult{}, fmt.Errorf("%w: unsupported merge kind %q", session.ErrInvalidCommand, command.MergeKind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.MergeResult{}, err
	}
	defer finishTx(tx, &resultErr)

	branch, ok, err := s.branch(tx, command.TenantKey, command.BranchKey)
	if err != nil {
		return session.MergeResult{}, err
	}
	if !ok || branch.SessionKey != command.SessionKey {
		return session.MergeResult{}, session.ErrBranchNotFound
	}
	stored, _, ok, err := s.loadSession(tx, command.TenantKey, command.SessionKey)
	if err != nil {
		return session.MergeResult{}, err
	}
	if !ok {
		return session.MergeResult{}, session.ErrSessionNotFound
	}
	conflict := func(reason string) *session.MergeConflictError {
		return &session.MergeConflictError{
			SessionKey: command.SessionKey, BranchKey: command.BranchKey, BaseRevision: branch.BaseRevision,
			CurrentRevision: stored.Revision, BranchVersion: branch.Version, Reason: reason,
		}
	}
	if branch.Status != session.BranchStatusReadyToMerge || branch.Version != command.ExpectedVersion || stored.Revision != branch.BaseRevision {
		return session.MergeResult{}, conflict("fast-forward precondition failed")
	}
	if stored.Status == session.StatusCompleted || stored.Status == session.StatusAbandoned {
		return session.MergeResult{}, conflict("session is terminal")
	}
	previous := stored.Revision
	next := session.CloneSnapshot(stored)
	next.Revision++
	next.UpdatedAt = s.now()
	branch.Status = session.BranchStatusMerged
	branch.MergeKind = command.MergeKind
	branch.MergedRevision = next.Revision
	branch.Version++
	branch.UpdatedAt = next.UpdatedAt
	if err := s.commitSnapshot(tx, next); err != nil {
		return session.MergeResult{}, err
	}
	if err := s.saveBranch(tx, branch); err != nil {
		return session.MergeResult{}, err
	}
	return session.MergeResult{PreviousRevision: previous, SessionRevision: next.Revision, Session: next, Branch: branch}, nil
}

// AddUsage folds one immutable usage fact into the session totals.
//
// The fact key is the dedupe anchor: a retried report is recognized and ignored,
// because usage that accumulates twice is spend that was billed twice.
func (s *SQLiteSessionService) AddUsage(ctx context.Context, command session.UsageCommand) (snapshot session.Snapshot, resultErr error) {
	if err := sessionScope(ctx, command.TenantKey, command.SessionKey); err != nil || !command.UsageFactKey.Valid() {
		return session.Snapshot{}, fmt.Errorf("%w: tenant, session, and usage fact are required", session.ErrInvalidCommand)
	}
	if command.PromptTokens < 0 || command.CompletionTokens < 0 || command.CostMicros < 0 {
		return session.Snapshot{}, fmt.Errorf("%w: usage deltas must be nonnegative fixed-point integers", session.ErrInvalidCommand)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	stored, _, ok, err := s.loadSession(tx, command.TenantKey, command.SessionKey)
	if err != nil {
		return session.Snapshot{}, err
	}
	if !ok {
		return session.Snapshot{}, session.ErrSessionNotFound
	}
	if existing, ok, err := s.usageFact(tx, command.TenantKey, command.UsageFactKey); err != nil {
		return session.Snapshot{}, err
	} else if ok {
		if existing == (sessionUsageDelta{
			session: command.SessionKey, promptTokens: command.PromptTokens,
			completionTokens: command.CompletionTokens, costMicros: command.CostMicros,
		}) {
			return stored, nil
		}
		return session.Snapshot{}, fmt.Errorf("%w: usage fact %q has different immutable input", session.ErrIdempotencyConflict, command.UsageFactKey)
	}
	if stored.Revision != command.ExpectedRevision {
		return session.Snapshot{}, fmt.Errorf("%w: expected %d, current %d", session.ErrRevisionConflict, command.ExpectedRevision, stored.Revision)
	}
	if stored.Status == session.StatusCompleted || stored.Status == session.StatusAbandoned {
		return session.Snapshot{}, fmt.Errorf("%w: terminal session", session.ErrInvalidSessionTransition)
	}
	next := session.CloneSnapshot(stored)
	var valid bool
	if next.PromptTokens, valid = session.AddNonNegative(next.PromptTokens, command.PromptTokens); !valid {
		return session.Snapshot{}, fmt.Errorf("%w: prompt token overflow", session.ErrInvalidCommand)
	}
	if next.CompletionTokens, valid = session.AddNonNegative(next.CompletionTokens, command.CompletionTokens); !valid {
		return session.Snapshot{}, fmt.Errorf("%w: completion token overflow", session.ErrInvalidCommand)
	}
	if next.CostMicros, valid = session.AddNonNegative(next.CostMicros, command.CostMicros); !valid {
		return session.Snapshot{}, fmt.Errorf("%w: cost overflow", session.ErrInvalidCommand)
	}
	next.Revision = stored.Revision + 1
	next.UpdatedAt = s.now()
	if err := s.commitSnapshot(tx, next); err != nil {
		return session.Snapshot{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_usage_facts(tenant_key, usage_fact_key, session_key, prompt_tokens, completion_tokens, cost_micros)
		VALUES(?, ?, ?, ?, ?, ?)`,
		string(command.TenantKey), string(command.UsageFactKey), string(command.SessionKey),
		command.PromptTokens, command.CompletionTokens, command.CostMicros); err != nil {
		return session.Snapshot{}, err
	}
	return next, nil
}

// SetSummary points the session at the compacted summary message that stands in
// for its older turns.
func (s *SQLiteSessionService) SetSummary(ctx context.Context, command session.SummaryCommand) (snapshot session.Snapshot, resultErr error) {
	if err := sessionScope(ctx, command.TenantKey, command.SessionKey); err != nil {
		return session.Snapshot{}, err
	}
	hasKey := command.SummaryMessageKey.Valid()
	hasRevision := command.SummaryAtRevision != 0
	if hasKey != hasRevision {
		return session.Snapshot{}, fmt.Errorf("%w: summary key and revision must be paired", session.ErrSnapshotInvariant)
	}
	if hasKey && (command.SummaryVisibleAtRevision == 0 || command.SummaryVisibleAtRevision > command.SummaryAtRevision || command.SummaryAtRevision > command.ExpectedRevision) {
		return session.Snapshot{}, fmt.Errorf("%w: summary is not visible at the expected revision", session.ErrSnapshotInvariant)
	}
	if !hasKey && command.SummaryVisibleAtRevision != 0 {
		return session.Snapshot{}, fmt.Errorf("%w: empty summary has visibility", session.ErrSnapshotInvariant)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	stored, _, ok, err := s.loadSession(tx, command.TenantKey, command.SessionKey)
	if err != nil {
		return session.Snapshot{}, err
	}
	if !ok {
		return session.Snapshot{}, session.ErrSessionNotFound
	}
	if stored.Revision != command.ExpectedRevision {
		return session.Snapshot{}, fmt.Errorf("%w: expected %d, current %d", session.ErrRevisionConflict, command.ExpectedRevision, stored.Revision)
	}
	if stored.Status == session.StatusCompleted || stored.Status == session.StatusAbandoned {
		return session.Snapshot{}, fmt.Errorf("%w: terminal session", session.ErrInvalidSessionTransition)
	}
	next := session.CloneSnapshot(stored)
	next.SummaryMessageKey = command.SummaryMessageKey
	next.SummaryAtRevision = command.SummaryAtRevision
	next.Revision = stored.Revision + 1
	next.UpdatedAt = s.now()
	if err := s.commitSnapshot(tx, next); err != nil {
		return session.Snapshot{}, err
	}
	return next, nil
}

// Transition moves the session's status, optionally recording a new context
// pivot at the revision the transition creates.
func (s *SQLiteSessionService) Transition(ctx context.Context, command session.TransitionCommand) (snapshot session.Snapshot, resultErr error) {
	if err := sessionScope(ctx, command.TenantKey, command.SessionKey); err != nil || !session.KnownStatus(command.Status) {
		return session.Snapshot{}, fmt.Errorf("%w: incomplete transition command", session.ErrInvalidCommand)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return session.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	stored, _, ok, err := s.loadSession(tx, command.TenantKey, command.SessionKey)
	if err != nil {
		return session.Snapshot{}, err
	}
	if !ok {
		return session.Snapshot{}, session.ErrSessionNotFound
	}
	if stored.Revision != command.ExpectedRevision {
		return session.Snapshot{}, fmt.Errorf("%w: expected %d, current %d", session.ErrRevisionConflict, command.ExpectedRevision, stored.Revision)
	}
	if !session.ValidStatusTransition(stored.Status, command.Status) {
		return session.Snapshot{}, fmt.Errorf("%w: %s to %s", session.ErrInvalidSessionTransition, stored.Status, command.Status)
	}
	next := session.CloneSnapshot(stored)
	next.Status = command.Status
	if command.ContextPivot != nil {
		if !session.ValidContextPivot(*command.ContextPivot, next.Revision+1) {
			return session.Snapshot{}, fmt.Errorf("%w: invalid context pivot", session.ErrSnapshotInvariant)
		}
		next.ContextPivot = *command.ContextPivot
	}
	next.Revision = stored.Revision + 1
	next.UpdatedAt = s.now()
	if err := s.commitSnapshot(tx, next); err != nil {
		return session.Snapshot{}, err
	}
	return next, nil
}

// sessionUsageDelta is the immutable part of one usage report, kept comparable
// so a replayed fact can be recognized as the same fact.
type sessionUsageDelta struct {
	session          session.SessionKey
	promptTokens     int64
	completionTokens int64
	costMicros       int64
}

// sessionScope rejects a command that names no tenant or session before any
// storage is touched, and honours a cancelled context the same way the
// reference implementation does.
func sessionScope(ctx context.Context, tenant agent.TenantKey, key session.SessionKey) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", session.ErrInvalidCommand)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !tenant.Valid() || !key.Valid() {
		return fmt.Errorf("%w: tenant and session key are required", session.ErrInvalidCommand)
	}
	return nil
}

// loadSession returns the current snapshot and the creation command it was
// registered with, which is what a replayed Create is compared against.
func (s *SQLiteSessionService) loadSession(tx *sql.Tx, tenant agent.TenantKey, key session.SessionKey) (session.Snapshot, session.CreateCommand, bool, error) {
	var snapshotPayload, createPayload []byte
	err := tx.QueryRow(`SELECT snapshot, create_command FROM session_aggregates WHERE tenant_key = ? AND session_key = ?`,
		string(tenant), string(key)).Scan(&snapshotPayload, &createPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Snapshot{}, session.CreateCommand{}, false, nil
	}
	if err != nil {
		return session.Snapshot{}, session.CreateCommand{}, false, err
	}
	var snapshot session.Snapshot
	if err := json.Unmarshal(snapshotPayload, &snapshot); err != nil {
		return session.Snapshot{}, session.CreateCommand{}, false, err
	}
	var created session.CreateCommand
	if err := json.Unmarshal(createPayload, &created); err != nil {
		return session.Snapshot{}, session.CreateCommand{}, false, err
	}
	return snapshot, created, true, nil
}

// commitSnapshot writes the new current snapshot and pins it as an addressable
// revision in the same statement pair, so a revision can never be current
// without also being readable by GetRevision.
func (s *SQLiteSessionService) commitSnapshot(tx *sql.Tx, snapshot session.Snapshot) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE session_aggregates SET status = ?, revision = ?, snapshot = ? WHERE tenant_key = ? AND session_key = ?`,
		string(snapshot.Status), int64(snapshot.Revision), payload, string(snapshot.TenantKey), string(snapshot.SessionKey)); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO session_revisions(tenant_key, session_key, revision, snapshot) VALUES(?, ?, ?, ?)
		ON CONFLICT(tenant_key, session_key, revision) DO UPDATE SET snapshot = excluded.snapshot`,
		string(snapshot.TenantKey), string(snapshot.SessionKey), int64(snapshot.Revision), payload)
	return err
}

func (s *SQLiteSessionService) branch(tx *sql.Tx, tenant agent.TenantKey, key session.BranchKey) (session.Branch, bool, error) {
	var payload []byte
	err := tx.QueryRow(`SELECT branch FROM session_branches WHERE tenant_key = ? AND branch_key = ?`,
		string(tenant), string(key)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Branch{}, false, nil
	}
	if err != nil {
		return session.Branch{}, false, err
	}
	var branch session.Branch
	if err := json.Unmarshal(payload, &branch); err != nil {
		return session.Branch{}, false, err
	}
	return branch, true, nil
}

func (s *SQLiteSessionService) branchByRun(tx *sql.Tx, tenant agent.TenantKey, run session.RunKey) (session.Branch, bool, error) {
	var payload []byte
	err := tx.QueryRow(`SELECT branch FROM session_branches WHERE tenant_key = ? AND run_key = ?`,
		string(tenant), string(run)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Branch{}, false, nil
	}
	if err != nil {
		return session.Branch{}, false, err
	}
	var branch session.Branch
	if err := json.Unmarshal(payload, &branch); err != nil {
		return session.Branch{}, false, err
	}
	return branch, true, nil
}

func (s *SQLiteSessionService) insertBranch(tx *sql.Tx, branch session.Branch) error {
	payload, err := json.Marshal(branch)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO session_branches(tenant_key, branch_key, session_key, run_key, status, version, branch)
		VALUES(?, ?, ?, ?, ?, ?, ?)`,
		string(branch.TenantKey), string(branch.BranchKey), string(branch.SessionKey), string(branch.RunKey),
		string(branch.Status), int64(branch.Version), payload)
	return err
}

func (s *SQLiteSessionService) saveBranch(tx *sql.Tx, branch session.Branch) error {
	payload, err := json.Marshal(branch)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE session_branches SET status = ?, version = ?, branch = ? WHERE tenant_key = ? AND branch_key = ?`,
		string(branch.Status), int64(branch.Version), payload, string(branch.TenantKey), string(branch.BranchKey))
	return err
}

func (s *SQLiteSessionService) usageFact(tx *sql.Tx, tenant agent.TenantKey, key session.UsageFactKey) (sessionUsageDelta, bool, error) {
	var (
		delta      sessionUsageDelta
		sessionRaw string
	)
	err := tx.QueryRow(`SELECT session_key, prompt_tokens, completion_tokens, cost_micros FROM session_usage_facts
		WHERE tenant_key = ? AND usage_fact_key = ?`, string(tenant), string(key)).
		Scan(&sessionRaw, &delta.promptTokens, &delta.completionTokens, &delta.costMicros)
	if errors.Is(err, sql.ErrNoRows) {
		return sessionUsageDelta{}, false, nil
	}
	if err != nil {
		return sessionUsageDelta{}, false, err
	}
	delta.session = session.SessionKey(sessionRaw)
	return delta, true, nil
}
