package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

// SQLiteSubagentStore is iCoder's persistent implementation of subagent.Store.
//
// It became necessary when delegation stopped completing inside one tool call.
// A parent now parks while its child runs, so the relationship, the tree budget
// it reserved, and the wake intent that will resume the parent all have to
// outlive the process - otherwise a restart during a delegation would leak the
// reservation and strand the parked parent forever.
//
// The adapter owns only what storage genuinely owns: finding the parent,
// counting siblings, and writing atomically. Depth, fan-out, cycle detection and
// budget arithmetic are called out to the exported state machine in the
// subagent package, because those are safety limits - an adapter that restated
// them slightly differently would not fail loudly, it would quietly authorize a
// deeper, wider, or more expensive tree than the caller allowed.
type SQLiteSubagentStore struct{ db *sql.DB }

// NewSQLiteSubagentStore binds the child-run authority to an already-migrated
// database.
func NewSQLiteSubagentStore(db *sql.DB) *SQLiteSubagentStore {
	return &SQLiteSubagentStore{db: db}
}

var _ subagent.Store = (*SQLiteSubagentStore)(nil)

// subagentMigrations creates the child-run tables.
//
// The snapshot is stored whole as JSON with the queryable parts lifted into
// columns. The state machine owns the shape of a snapshot; duplicating every
// field as a column would mean migrating this schema every time the library adds
// one, for no gain the indexes below do not already provide.
var subagentMigrations = []string{
	`CREATE TABLE IF NOT EXISTS subagent_relationships (
		tenant_key TEXT NOT NULL,
		relationship_key TEXT NOT NULL,
		request_key TEXT NOT NULL,
		spec_digest TEXT NOT NULL,
		parent_run_key TEXT NOT NULL,
		child_run_key TEXT NOT NULL,
		tree_key TEXT NOT NULL,
		wake_key TEXT NOT NULL,
		state TEXT NOT NULL,
		cancel_mode TEXT NOT NULL DEFAULT '',
		version INTEGER NOT NULL,
		claim_until INTEGER NOT NULL DEFAULT 0,
		wake_pending INTEGER NOT NULL DEFAULT 0,
		wake_delivered INTEGER NOT NULL DEFAULT 0,
		settled INTEGER NOT NULL DEFAULT 0,
		snapshot BLOB NOT NULL,
		PRIMARY KEY (tenant_key, relationship_key)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS subagent_relationships_request
		ON subagent_relationships(tenant_key, request_key)`,
	`CREATE INDEX IF NOT EXISTS subagent_relationships_parent
		ON subagent_relationships(tenant_key, parent_run_key, relationship_key)`,
	`CREATE INDEX IF NOT EXISTS subagent_relationships_child
		ON subagent_relationships(tenant_key, child_run_key)`,
	`CREATE INDEX IF NOT EXISTS subagent_relationships_queue
		ON subagent_relationships(tenant_key, state, relationship_key)`,
	`CREATE TABLE IF NOT EXISTS subagent_trees (
		tenant_key TEXT NOT NULL,
		tree_key TEXT NOT NULL,
		budget BLOB NOT NULL,
		PRIMARY KEY (tenant_key, tree_key)
	)`,
	`CREATE TABLE IF NOT EXISTS subagent_usage_facts (
		tenant_key TEXT NOT NULL,
		relationship_key TEXT NOT NULL,
		usage_fact_key TEXT NOT NULL,
		digest TEXT NOT NULL,
		PRIMARY KEY (tenant_key, relationship_key, usage_fact_key)
	)`,
	`CREATE TABLE IF NOT EXISTS subagent_cancel_requests (
		tenant_key TEXT NOT NULL,
		request_key TEXT NOT NULL,
		affected INTEGER NOT NULL,
		terminal INTEGER NOT NULL,
		pending INTEGER NOT NULL,
		PRIMARY KEY (tenant_key, request_key)
	)`,
	`CREATE TABLE IF NOT EXISTS subagent_facts (
		tenant_key TEXT NOT NULL,
		sequence INTEGER NOT NULL,
		fact BLOB NOT NULL,
		PRIMARY KEY (tenant_key, sequence)
	)`,
}

// subagentRecord is one stored relationship plus the two facts about it that are
// cheaper to keep beside the snapshot than to derive: the digest that decides
// whether a replayed spawn is the same spawn, and whether usage already settled.
type subagentRecord struct {
	snapshot   subagent.Snapshot
	specDigest string
	requestKey subagent.RequestKey
	settled    bool
}

// subagentWakeKey derives the wake identity for one relationship. It matches the
// library's reference implementation so a wake claimed by one adapter can be
// completed by another during a migration.
func subagentWakeKey(key subagent.RelationshipKey) subagent.WakeKey {
	return subagent.WakeKey("wake/" + string(key))
}

func subagentFactKey(key subagent.RelationshipKey, version uint64) subagent.FactKey {
	return subagent.FactKey(fmt.Sprintf("fact/%s/%d", key, version))
}

// utcTime normalizes a stored timestamp.
//
// Times are persisted in UTC so a snapshot read back is identical to the one
// written regardless of the writer's zone, which is what lets the contract's
// value comparisons (a replayed receipt, an unchanged tree limit) stay exact.
func utcTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC()
}

func normalizeSubagentLimits(limits subagent.Limits) subagent.Limits {
	limits.Deadline = utcTime(limits.Deadline)
	return limits
}

func normalizeSubagentSnapshot(snapshot subagent.Snapshot) subagent.Snapshot {
	snapshot.Receipt.CreatedAt = utcTime(snapshot.Receipt.CreatedAt)
	snapshot.Receipt.EffectiveLimit = normalizeSubagentLimits(snapshot.Receipt.EffectiveLimit)
	snapshot.ClaimUntil = utcTime(snapshot.ClaimUntil)
	snapshot.TerminalAt = utcTime(snapshot.TerminalAt)
	return snapshot
}

// Spawn admits one child under the tree's limits, or reports why it may not run.
//
// Everything it decides - identity, cycle, depth, fan-out, reservation - happens
// inside one transaction, because a reservation that is granted but not written
// would let two concurrent siblings both believe they fit in the same budget.
func (s *SQLiteSubagentStore) Spawn(ctx context.Context, request subagent.SpawnRequest, now time.Time) (receipt subagent.SpawnReceipt, created bool, resultErr error) {
	if err := subagent.ValidateSpawn(request, now); err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	specDigest, err := subagent.SpecDigest(request)
	if err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	defer finishTx(tx, &resultErr)

	tenant := request.Parent.TenantKey
	if existing, ok, err := s.recordByRequest(tx, tenant, request.RequestKey); err != nil {
		return subagent.SpawnReceipt{}, false, err
	} else if ok {
		if existing.specDigest != specDigest {
			return subagent.SpawnReceipt{}, false, subagent.ErrIdempotencyConflict
		}
		return existing.snapshot.Receipt, false, nil
	}

	depth, treeKey, budget, err := s.resolveParent(tx, request)
	if err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	child := subagent.NewChildRef(tenant, request.RequestKey, depth)
	if err := subagent.ValidateNoCycle(request.Parent.RunKey, child.RunKey, s.parentLookup(tx, tenant, &resultErr)); err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	if err := subagent.ValidateDepth(depth, budget.Limits); err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	fanout, err := s.directFanout(tx, tenant, request.Parent.RunKey)
	if err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	if err := subagent.ValidateFanout(fanout, budget.Limits); err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	reserved, err := budget.Reserve(request.Reserve)
	if err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	if err := s.saveTree(tx, tenant, treeKey, reserved); err != nil {
		return subagent.SpawnReceipt{}, false, err
	}

	parent := request.Parent
	parent.TreeKey = treeKey
	record := &subagentRecord{
		snapshot: normalizeSubagentSnapshot(subagent.Snapshot{
			Receipt: subagent.SpawnReceipt{
				RequestKey: request.RequestKey, Child: child, State: subagent.ChildQueued,
				SpecDigest: specDigest, ParentBlocked: true, Reservation: request.Reserve,
				EffectiveLimit: subagent.EffectiveLimits(reserved.Limits, request.Reserve, request.Deadline, now),
				CreatedAt:      now,
			},
			Parent: parent, AgentKey: request.AgentKey,
			Input: append([]byte(nil), request.Input...), ContextRefs: append([]subagent.ContextRef(nil), request.ContextRefs...),
			State: subagent.ChildQueued, Version: 1,
		}),
		specDigest: specDigest,
		requestKey: request.RequestKey,
	}
	if err := s.insertRecord(tx, record); err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	if err := s.appendFact(tx, record, subagent.FactChildAccepted, now); err != nil {
		return subagent.SpawnReceipt{}, false, err
	}
	return record.snapshot.Receipt, true, nil
}

// Get returns one relationship snapshot, scoped to its tenant.
func (s *SQLiteSubagentStore) Get(ctx context.Context, tenantKey agent.TenantKey, relationshipKey subagent.RelationshipKey) (snapshot subagent.Snapshot, resultErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	record, err := s.record(tx, tenantKey, relationshipKey)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	return record.snapshot, nil
}

// ClaimNext leases one queued child to a worker.
//
// A child whose deadline already passed is abandoned rather than handed out: it
// could not finish inside its budget, and starting it would spend the tree's
// reservation on work that is guaranteed to be discarded.
func (s *SQLiteSubagentStore) ClaimNext(ctx context.Context, request subagent.ClaimRequest) (snapshot subagent.Snapshot, claimed bool, resultErr error) {
	if request.TenantKey == "" || request.Owner == "" || request.Now.IsZero() || request.Lease <= 0 {
		return subagent.Snapshot{}, false, subagent.ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.Snapshot{}, false, err
	}
	defer finishTx(tx, &resultErr)

	records, err := s.recordsByState(tx, request.TenantKey, subagent.ChildQueued)
	if err != nil {
		return subagent.Snapshot{}, false, err
	}
	for _, record := range records {
		deadline := record.snapshot.Receipt.EffectiveLimit.Deadline
		if !deadline.IsZero() && !request.Now.Before(deadline) {
			if err := s.cancelRecord(tx, record, subagent.CancelAbandon, request.Now, "deadline"); err != nil {
				return subagent.Snapshot{}, false, err
			}
			continue
		}
		record.snapshot.State = subagent.ChildRunning
		record.snapshot.Receipt.State = subagent.ChildRunning
		record.snapshot.Version++
		record.snapshot.ClaimOwner = request.Owner
		record.snapshot.ClaimUntil = utcTime(request.Now.Add(request.Lease))
		if err := s.saveRecord(tx, record); err != nil {
			return subagent.Snapshot{}, false, err
		}
		if err := s.appendFact(tx, record, subagent.FactChildRunning, request.Now); err != nil {
			return subagent.Snapshot{}, false, err
		}
		return record.snapshot, true, nil
	}
	return subagent.Snapshot{}, false, nil
}

// CommitSuspended parks a running child without ending it, which is how a worker
// hands back work it cannot finish right now.
func (s *SQLiteSubagentStore) CommitSuspended(ctx context.Context, command subagent.SuspendCommand) (snapshot subagent.Snapshot, resultErr error) {
	if command.TenantKey == "" || command.RelationshipKey == "" || command.ExpectedVersion == 0 || command.Owner == "" || command.SuspendedAt.IsZero() {
		return subagent.Snapshot{}, subagent.ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	record, err := s.record(tx, command.TenantKey, command.RelationshipKey)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	if record.snapshot.State != subagent.ChildRunning || record.snapshot.Version != command.ExpectedVersion || record.snapshot.ClaimOwner != command.Owner {
		return subagent.Snapshot{}, subagent.ErrStaleVersion
	}
	record.snapshot.State = subagent.ChildSuspended
	record.snapshot.Receipt.State = subagent.ChildSuspended
	record.snapshot.Failure = cloneSubagentFailure(command.Failure)
	record.snapshot.ClaimOwner = ""
	record.snapshot.ClaimUntil = time.Time{}
	record.snapshot.Version++
	if err := s.saveRecord(tx, record); err != nil {
		return subagent.Snapshot{}, err
	}
	return record.snapshot, nil
}

// CommitTerminal records the one durable outcome of a child and arms the wake
// that tells its parent.
//
// The terminal state and the wake intent commit together, before anything tries
// to deliver the wake. A crash in between therefore leaves a child that is
// finished and still owed a wake, which is recoverable; the reverse ordering
// would lose the result.
func (s *SQLiteSubagentStore) CommitTerminal(ctx context.Context, command subagent.TerminalCommand) (snapshot subagent.Snapshot, resultErr error) {
	if command.TenantKey == "" || command.RelationshipKey == "" || command.ExpectedVersion == 0 || command.Owner == "" || command.CompletedAt.IsZero() {
		return subagent.Snapshot{}, subagent.ErrInvalidRequest
	}
	if !command.Result.State.Terminal() || command.Result.UsageFactKey == "" {
		return subagent.Snapshot{}, fmt.Errorf("%w: terminal state and usage fact key required", subagent.ErrInvalidRequest)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	defer finishTx(tx, &resultErr)
	record, err := s.record(tx, command.TenantKey, command.RelationshipKey)
	if err != nil {
		return subagent.Snapshot{}, err
	}
	if record.snapshot.State.Terminal() {
		// A replayed commit is the same commit; a different one would rewrite an
		// outcome the parent may already have consumed.
		if sameSubagentTerminal(record.snapshot, command.Result) {
			return record.snapshot, nil
		}
		return subagent.Snapshot{}, subagent.ErrInvalidTransition
	}
	if record.snapshot.State != subagent.ChildRunning || record.snapshot.Version != command.ExpectedVersion || record.snapshot.ClaimOwner != command.Owner {
		return subagent.Snapshot{}, subagent.ErrStaleVersion
	}
	if err := s.settle(tx, record, command.Result.UsageFactKey, command.Result.Usage); err != nil {
		return subagent.Snapshot{}, err
	}
	record.snapshot.State = command.Result.State
	record.snapshot.Receipt.State = command.Result.State
	record.snapshot.ResultRef = command.Result.ResultRef
	record.snapshot.FailureRef = command.Result.FailureRef
	record.snapshot.Failure = cloneSubagentFailure(command.Result.Failure)
	record.snapshot.TerminalAt = utcTime(command.CompletedAt)
	record.snapshot.ClaimOwner = ""
	record.snapshot.ClaimUntil = time.Time{}
	record.snapshot.WakePending = true
	record.snapshot.Version++
	if err := s.saveRecord(tx, record); err != nil {
		return subagent.Snapshot{}, err
	}
	if err := s.appendFact(tx, record, subagent.FactChildTerminal, command.CompletedAt); err != nil {
		return subagent.Snapshot{}, err
	}
	return record.snapshot, nil
}

// SettleUsage converts a child's reservation into recorded spend, exactly once
// per usage fact key.
func (s *SQLiteSubagentStore) SettleUsage(ctx context.Context, command subagent.SettleCommand) (budget subagent.BudgetSnapshot, applied bool, resultErr error) {
	if command.TenantKey == "" || command.RelationshipKey == "" || command.UsageFactKey == "" {
		return subagent.BudgetSnapshot{}, false, subagent.ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	defer finishTx(tx, &resultErr)
	record, err := s.record(tx, command.TenantKey, command.RelationshipKey)
	if err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	usageDigest, err := durable.CanonicalDigest(command.Usage)
	if err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	if stored, ok, err := s.usageFactDigest(tx, command.TenantKey, command.RelationshipKey, command.UsageFactKey); err != nil {
		return subagent.BudgetSnapshot{}, false, err
	} else if ok {
		if stored != usageDigest {
			return subagent.BudgetSnapshot{}, false, subagent.ErrUsageConflict
		}
		current, err := s.treeBudget(tx, command.TenantKey, record.snapshot.Parent.TreeKey)
		if err != nil {
			return subagent.BudgetSnapshot{}, false, err
		}
		return current, false, nil
	}
	if err := s.settle(tx, record, command.UsageFactKey, command.Usage); err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	if err := s.saveRecord(tx, record); err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	current, err := s.treeBudget(tx, command.TenantKey, record.snapshot.Parent.TreeKey)
	if err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	return current, true, nil
}

// RequestCancel cancels a run and everything it delegated, transitively.
//
// The walk is bounded by MaxTraversal because a delegation graph is caller data:
// an unbounded traversal is a denial-of-service surface, not a completeness
// guarantee. The whole result is recorded under the request key so a retried
// cancel is the same cancel rather than a second one.
func (s *SQLiteSubagentStore) RequestCancel(ctx context.Context, request subagent.CancelRequest, now time.Time) (result subagent.CancelResult, resultErr error) {
	if request.TenantKey == "" || request.RequestKey == "" || request.RunKey == "" ||
		(request.Mode != subagent.CancelSuspend && request.Mode != subagent.CancelAbandon) || request.MaxTraversal <= 0 {
		return subagent.CancelResult{}, subagent.ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.CancelResult{}, err
	}
	defer finishTx(tx, &resultErr)

	if existing, ok, err := s.cancelResult(tx, request.TenantKey, request.RequestKey); err != nil {
		return subagent.CancelResult{}, err
	} else if ok {
		return existing, nil
	}

	queue := []subagent.RunKey{request.RunKey}
	seen := make(map[subagent.RunKey]struct{})
	records := make([]*subagentRecord, 0)
	for len(queue) > 0 {
		if len(seen) >= request.MaxTraversal {
			return subagent.CancelResult{}, subagent.ErrTraversalLimit
		}
		parentRun := queue[0]
		queue = queue[1:]
		if _, ok := seen[parentRun]; ok {
			continue
		}
		seen[parentRun] = struct{}{}
		children, err := s.recordsByParent(tx, request.TenantKey, parentRun)
		if err != nil {
			return subagent.CancelResult{}, err
		}
		for _, child := range children {
			records = append(records, child)
			queue = append(queue, child.snapshot.Receipt.Child.RunKey)
		}
	}

	result = subagent.CancelResult{Affected: len(records)}
	for _, record := range records {
		if record.snapshot.State.Terminal() {
			result.Terminal++
			continue
		}
		if err := s.cancelRecord(tx, record, request.Mode, now, request.Reason); err != nil {
			return subagent.CancelResult{}, err
		}
		result.Pending++
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO subagent_cancel_requests(tenant_key, request_key, affected, terminal, pending) VALUES(?, ?, ?, ?, ?)`,
		string(request.TenantKey), request.RequestKey, result.Affected, result.Terminal, result.Pending); err != nil {
		return subagent.CancelResult{}, err
	}
	return result, nil
}

// ClaimWake returns one undelivered wake intent without consuming it, so a
// delivery that crashes leaves the intent for the next attempt.
func (s *SQLiteSubagentStore) ClaimWake(ctx context.Context, tenantKey agent.TenantKey) (claim subagent.WakeClaim, ok bool, resultErr error) {
	if tenantKey == "" {
		return subagent.WakeClaim{}, false, subagent.ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return subagent.WakeClaim{}, false, err
	}
	defer finishTx(tx, &resultErr)
	rows, err := tx.QueryContext(ctx, `SELECT snapshot, spec_digest, request_key, settled FROM subagent_relationships
		WHERE tenant_key = ? AND wake_pending = 1 AND wake_delivered = 0 ORDER BY relationship_key LIMIT 1`, string(tenantKey))
	if err != nil {
		return subagent.WakeClaim{}, false, err
	}
	records, err := scanSubagentRecords(rows)
	if err != nil {
		return subagent.WakeClaim{}, false, err
	}
	if len(records) == 0 {
		return subagent.WakeClaim{}, false, nil
	}
	record := records[0]
	return subagent.WakeClaim{Request: subagentWakeRequest(record.snapshot), Version: record.snapshot.Version}, true, nil
}

// CompleteWake marks one wake intent delivered. It is idempotent because a
// delivery that succeeded but failed to record is indistinguishable from a
// delivery that has not happened, and re-delivering is the safe half.
func (s *SQLiteSubagentStore) CompleteWake(ctx context.Context, tenantKey agent.TenantKey, wakeKey subagent.WakeKey, version uint64, now time.Time) (resultErr error) {
	if tenantKey == "" || wakeKey == "" || version == 0 || now.IsZero() {
		return subagent.ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer finishTx(tx, &resultErr)
	rows, err := tx.QueryContext(ctx, `SELECT snapshot, spec_digest, request_key, settled FROM subagent_relationships
		WHERE tenant_key = ? AND wake_key = ?`, string(tenantKey), string(wakeKey))
	if err != nil {
		return err
	}
	records, err := scanSubagentRecords(rows)
	if err != nil {
		return err
	}
	if len(records) == 0 {
		return subagent.ErrNotFound
	}
	record := records[0]
	if record.snapshot.WakeDelivered {
		return nil
	}
	if record.snapshot.Version != version || !record.snapshot.WakePending {
		return subagent.ErrWakeConflict
	}
	record.snapshot.WakeDelivered = true
	record.snapshot.WakePending = false
	record.snapshot.Version++
	if err := s.saveRecord(tx, record); err != nil {
		return err
	}
	if err := s.appendFact(tx, record, subagent.FactChildConsumed, now); err != nil {
		return err
	}
	return nil
}

// Recover returns children stranded by a dead worker to the queue.
//
// An expired claim and an operational suspend are both "nobody is working on
// this any more". A suspend that carries a cancel mode is a decision, not a
// failure, so it is left alone.
func (s *SQLiteSubagentStore) Recover(ctx context.Context, request subagent.ReconcileRequest) (recovered int, resultErr error) {
	if request.TenantKey == "" || request.Now.IsZero() || request.Limit < 0 {
		return 0, subagent.ErrInvalidRequest
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer finishTx(tx, &resultErr)
	rows, err := tx.QueryContext(ctx, `SELECT snapshot, spec_digest, request_key, settled FROM subagent_relationships
		WHERE tenant_key = ? AND ((state = ? AND claim_until <= ?) OR (state = ? AND cancel_mode = ''))
		ORDER BY relationship_key`,
		string(request.TenantKey), string(subagent.ChildRunning), request.Now.UnixNano(), string(subagent.ChildSuspended))
	if err != nil {
		return 0, err
	}
	records, err := scanSubagentRecords(rows)
	if err != nil {
		return 0, err
	}
	for _, record := range records {
		if request.Limit > 0 && recovered >= request.Limit {
			break
		}
		record.snapshot.State = subagent.ChildQueued
		record.snapshot.Receipt.State = subagent.ChildQueued
		record.snapshot.ClaimOwner = ""
		record.snapshot.ClaimUntil = time.Time{}
		record.snapshot.Version++
		if err := s.saveRecord(tx, record); err != nil {
			return 0, err
		}
		recovered++
	}
	return recovered, nil
}

// Facts returns the committed lifecycle log, which is what an audit reads.
func (s *SQLiteSubagentStore) Facts(ctx context.Context, tenantKey agent.TenantKey, after uint64, limit int) (facts []subagent.CommittedFact, resultErr error) {
	if tenantKey == "" || limit < 0 {
		return nil, subagent.ErrInvalidRequest
	}
	query := `SELECT fact FROM subagent_facts WHERE tenant_key = ? AND sequence > ? ORDER BY sequence`
	arguments := []any{string(tenantKey), int64(after)}
	if limit > 0 {
		query += ` LIMIT ?`
		arguments = append(arguments, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	facts = make([]subagent.CommittedFact, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var fact subagent.CommittedFact
		if err := json.Unmarshal(payload, &fact); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

// record loads one relationship or reports that this tenant has no such child.
func (s *SQLiteSubagentStore) record(tx *sql.Tx, tenantKey agent.TenantKey, relationshipKey subagent.RelationshipKey) (*subagentRecord, error) {
	rows, err := tx.Query(`SELECT snapshot, spec_digest, request_key, settled FROM subagent_relationships
		WHERE tenant_key = ? AND relationship_key = ?`, string(tenantKey), string(relationshipKey))
	if err != nil {
		return nil, err
	}
	records, err := scanSubagentRecords(rows)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, subagent.ErrNotFound
	}
	return records[0], nil
}

func (s *SQLiteSubagentStore) recordByRequest(tx *sql.Tx, tenantKey agent.TenantKey, requestKey subagent.RequestKey) (*subagentRecord, bool, error) {
	rows, err := tx.Query(`SELECT snapshot, spec_digest, request_key, settled FROM subagent_relationships
		WHERE tenant_key = ? AND request_key = ?`, string(tenantKey), string(requestKey))
	if err != nil {
		return nil, false, err
	}
	records, err := scanSubagentRecords(rows)
	if err != nil || len(records) == 0 {
		return nil, false, err
	}
	return records[0], true, nil
}

func (s *SQLiteSubagentStore) recordsByState(tx *sql.Tx, tenantKey agent.TenantKey, state subagent.ChildState) ([]*subagentRecord, error) {
	rows, err := tx.Query(`SELECT snapshot, spec_digest, request_key, settled FROM subagent_relationships
		WHERE tenant_key = ? AND state = ? ORDER BY relationship_key`, string(tenantKey), string(state))
	if err != nil {
		return nil, err
	}
	return scanSubagentRecords(rows)
}

func (s *SQLiteSubagentStore) recordsByParent(tx *sql.Tx, tenantKey agent.TenantKey, parentRun subagent.RunKey) ([]*subagentRecord, error) {
	rows, err := tx.Query(`SELECT snapshot, spec_digest, request_key, settled FROM subagent_relationships
		WHERE tenant_key = ? AND parent_run_key = ? ORDER BY relationship_key`, string(tenantKey), string(parentRun))
	if err != nil {
		return nil, err
	}
	return scanSubagentRecords(rows)
}

func scanSubagentRecords(rows *sql.Rows) (records []*subagentRecord, resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	for rows.Next() {
		var (
			payload    []byte
			specDigest string
			requestKey string
			settled    bool
		)
		if err := rows.Scan(&payload, &specDigest, &requestKey, &settled); err != nil {
			return nil, err
		}
		var snapshot subagent.Snapshot
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			return nil, err
		}
		records = append(records, &subagentRecord{
			snapshot: snapshot, specDigest: specDigest,
			requestKey: subagent.RequestKey(requestKey), settled: settled,
		})
	}
	return records, rows.Err()
}

func (s *SQLiteSubagentStore) insertRecord(tx *sql.Tx, record *subagentRecord) error {
	payload, err := json.Marshal(record.snapshot)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO subagent_relationships(
		tenant_key, relationship_key, request_key, spec_digest, parent_run_key, child_run_key, tree_key,
		wake_key, state, cancel_mode, version, claim_until, wake_pending, wake_delivered, settled, snapshot)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(record.snapshot.Receipt.Child.TenantKey), string(record.snapshot.Receipt.Child.RelationshipKey),
		string(record.requestKey), record.specDigest, string(record.snapshot.Parent.RunKey),
		string(record.snapshot.Receipt.Child.RunKey), string(record.snapshot.Parent.TreeKey),
		string(subagentWakeKey(record.snapshot.Receipt.Child.RelationshipKey)),
		string(record.snapshot.State), string(record.snapshot.CancelMode), int64(record.snapshot.Version),
		nanos(record.snapshot.ClaimUntil), record.snapshot.WakePending, record.snapshot.WakeDelivered,
		record.settled, payload)
	return err
}

// saveRecord writes back the whole snapshot together with the columns queries
// filter on, so an index can never disagree with the state machine it indexes.
func (s *SQLiteSubagentStore) saveRecord(tx *sql.Tx, record *subagentRecord) error {
	record.snapshot = normalizeSubagentSnapshot(record.snapshot)
	payload, err := json.Marshal(record.snapshot)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE subagent_relationships SET state = ?, cancel_mode = ?, version = ?, claim_until = ?,
		wake_pending = ?, wake_delivered = ?, settled = ?, snapshot = ?
		WHERE tenant_key = ? AND relationship_key = ?`,
		string(record.snapshot.State), string(record.snapshot.CancelMode), int64(record.snapshot.Version),
		nanos(record.snapshot.ClaimUntil), record.snapshot.WakePending, record.snapshot.WakeDelivered,
		record.settled, payload,
		string(record.snapshot.Receipt.Child.TenantKey), string(record.snapshot.Receipt.Child.RelationshipKey))
	return err
}

// resolveParent finds the tree this spawn joins and how deep the child sits.
//
// A root spawn creates the tree with the limits it was given; a nested spawn
// inherits the tree its parent already belongs to. Re-declaring different limits
// for an existing tree is a conflict rather than an update, because the siblings
// already admitted were measured against the old ones.
func (s *SQLiteSubagentStore) resolveParent(tx *sql.Tx, request subagent.SpawnRequest) (uint16, subagent.TreeKey, subagent.BudgetSnapshot, error) {
	tenant := request.Parent.TenantKey
	if request.Parent.RelationshipKey == "" {
		treeKey := request.Parent.TreeKey
		if treeKey == "" {
			treeKey = subagent.TreeKey(request.Parent.RunKey)
		}
		budget, ok, err := s.loadTree(tx, tenant, treeKey)
		if err != nil {
			return 0, "", subagent.BudgetSnapshot{}, err
		}
		if !ok {
			return 1, treeKey, subagent.BudgetSnapshot{Limits: normalizeSubagentLimits(request.Limits)}, nil
		}
		if budget.Limits != normalizeSubagentLimits(request.Limits) {
			return 0, "", subagent.BudgetSnapshot{}, subagent.ErrIdempotencyConflict
		}
		return 1, treeKey, budget, nil
	}
	parent, err := s.record(tx, tenant, request.Parent.RelationshipKey)
	if errors.Is(err, subagent.ErrNotFound) {
		return 0, "", subagent.BudgetSnapshot{}, subagent.ErrNotFound
	}
	if err != nil {
		return 0, "", subagent.BudgetSnapshot{}, err
	}
	if parent.snapshot.Receipt.Child.RunKey != request.Parent.RunKey {
		return 0, "", subagent.BudgetSnapshot{}, subagent.ErrNotFound
	}
	if parent.snapshot.State.Terminal() {
		return 0, "", subagent.BudgetSnapshot{}, subagent.ErrInvalidTransition
	}
	treeKey := parent.snapshot.Parent.TreeKey
	budget, ok, err := s.loadTree(tx, tenant, treeKey)
	if err != nil {
		return 0, "", subagent.BudgetSnapshot{}, err
	}
	if !ok {
		return 0, "", subagent.BudgetSnapshot{}, subagent.ErrStoreInvariant
	}
	return parent.snapshot.Receipt.Child.Depth + 1, treeKey, budget, nil
}

func (s *SQLiteSubagentStore) loadTree(tx *sql.Tx, tenantKey agent.TenantKey, treeKey subagent.TreeKey) (budget subagent.BudgetSnapshot, ok bool, resultErr error) {
	rows, err := tx.Query(`SELECT budget FROM subagent_trees WHERE tenant_key = ? AND tree_key = ?`, string(tenantKey), string(treeKey))
	if err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	defer func() { resultErr = errors.Join(resultErr, rows.Close()) }()
	if !rows.Next() {
		return subagent.BudgetSnapshot{}, false, rows.Err()
	}
	var payload []byte
	if err := rows.Scan(&payload); err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	if err := json.Unmarshal(payload, &budget); err != nil {
		return subagent.BudgetSnapshot{}, false, err
	}
	return budget, true, rows.Err()
}

// treeBudget reads a tree's budget, treating a missing tree as a broken
// invariant rather than an empty budget: a relationship always has a tree.
func (s *SQLiteSubagentStore) treeBudget(tx *sql.Tx, tenantKey agent.TenantKey, treeKey subagent.TreeKey) (subagent.BudgetSnapshot, error) {
	budget, ok, err := s.loadTree(tx, tenantKey, treeKey)
	if err != nil {
		return subagent.BudgetSnapshot{}, err
	}
	if !ok {
		return subagent.BudgetSnapshot{}, subagent.ErrStoreInvariant
	}
	return budget, nil
}

func (s *SQLiteSubagentStore) saveTree(tx *sql.Tx, tenantKey agent.TenantKey, treeKey subagent.TreeKey, budget subagent.BudgetSnapshot) error {
	budget.Limits = normalizeSubagentLimits(budget.Limits)
	payload, err := json.Marshal(budget)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO subagent_trees(tenant_key, tree_key, budget) VALUES(?, ?, ?)
		ON CONFLICT(tenant_key, tree_key) DO UPDATE SET budget = excluded.budget`,
		string(tenantKey), string(treeKey), payload)
	return err
}

func (s *SQLiteSubagentStore) directFanout(tx *sql.Tx, tenantKey agent.TenantKey, parentRun subagent.RunKey) (int, error) {
	var count int
	err := tx.QueryRow(`SELECT COUNT(*) FROM subagent_relationships
		WHERE tenant_key = ? AND parent_run_key = ? AND state <> ?`,
		string(tenantKey), string(parentRun), string(subagent.ChildCanceled)).Scan(&count)
	return count, err
}

// parentLookup gives the library's cycle check a way to walk the delegation
// graph. Only storage can walk it; only the library decides what a cycle is.
func (s *SQLiteSubagentStore) parentLookup(tx *sql.Tx, tenantKey agent.TenantKey, resultErr *error) subagent.ParentLookup {
	return func(run subagent.RunKey) (subagent.RunKey, bool) {
		var parent string
		err := tx.QueryRow(`SELECT parent_run_key FROM subagent_relationships WHERE tenant_key = ? AND child_run_key = ?`,
			string(tenantKey), string(run)).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return "", false
		}
		if err != nil {
			*resultErr = errors.Join(*resultErr, err)
			return "", false
		}
		return subagent.RunKey(parent), true
	}
}

func (s *SQLiteSubagentStore) usageFactDigest(tx *sql.Tx, tenantKey agent.TenantKey, relationshipKey subagent.RelationshipKey, factKey subagent.UsageFactKey) (string, bool, error) {
	var stored string
	err := tx.QueryRow(`SELECT digest FROM subagent_usage_facts WHERE tenant_key = ? AND relationship_key = ? AND usage_fact_key = ?`,
		string(tenantKey), string(relationshipKey), string(factKey)).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return stored, err == nil, err
}

// settle applies one usage fact to the tree budget exactly once.
//
// The arithmetic belongs to BudgetSnapshot.Settle so that the released remainder
// really does return to the tree; this function only decides whether the fact is
// new, and records it if so.
func (s *SQLiteSubagentStore) settle(tx *sql.Tx, record *subagentRecord, factKey subagent.UsageFactKey, usage subagent.Usage) error {
	if err := subagent.ValidateUsage(usage); err != nil {
		return err
	}
	usageDigest, err := durable.CanonicalDigest(usage)
	if err != nil {
		return err
	}
	tenant := record.snapshot.Receipt.Child.TenantKey
	relationship := record.snapshot.Receipt.Child.RelationshipKey
	if stored, ok, err := s.usageFactDigest(tx, tenant, relationship, factKey); err != nil {
		return err
	} else if ok {
		if stored != usageDigest {
			return subagent.ErrUsageConflict
		}
		return nil
	}
	if record.settled {
		return subagent.ErrUsageConflict
	}
	budget, err := s.treeBudget(tx, tenant, record.snapshot.Parent.TreeKey)
	if err != nil {
		return err
	}
	settled, err := budget.Settle(record.snapshot.Receipt.Reservation, usage)
	if err != nil {
		return err
	}
	if err := s.saveTree(tx, tenant, record.snapshot.Parent.TreeKey, settled); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO subagent_usage_facts(tenant_key, relationship_key, usage_fact_key, digest) VALUES(?, ?, ?, ?)`,
		string(tenant), string(relationship), string(factKey), usageDigest); err != nil {
		return err
	}
	record.settled = true
	record.snapshot.UsageFactKey = factKey
	record.snapshot.Usage = usage
	return nil
}

// cancelRecord applies one cancellation to one child.
//
// Escalation is one-way: abandon overrides a suspend, and a repeated suspend is
// a no-op. Abandoning also settles a zero usage fact, so the reservation the
// child will now never spend goes back to its siblings instead of being held
// until the tree is gone.
func (s *SQLiteSubagentStore) cancelRecord(tx *sql.Tx, record *subagentRecord, mode subagent.CancelMode, now time.Time, reason string) error {
	if record.snapshot.State.Terminal() {
		return nil
	}
	if record.snapshot.CancelMode == subagent.CancelAbandon ||
		(record.snapshot.CancelMode == subagent.CancelSuspend && mode == subagent.CancelSuspend) {
		return nil
	}
	record.snapshot.CancelMode = mode
	record.snapshot.Version++
	if err := s.appendFact(tx, record, subagent.FactChildCancelRequested, now); err != nil {
		return err
	}
	if mode == subagent.CancelSuspend {
		record.snapshot.State = subagent.ChildSuspended
		record.snapshot.Receipt.State = subagent.ChildSuspended
		record.snapshot.ClaimOwner = ""
		record.snapshot.ClaimUntil = time.Time{}
		return s.saveRecord(tx, record)
	}
	usageKey := subagent.UsageFactKey("cancel/" + string(record.snapshot.Receipt.Child.RunKey))
	if err := s.settle(tx, record, usageKey, subagent.Usage{}); err != nil && !errors.Is(err, subagent.ErrUsageConflict) {
		return err
	}
	record.snapshot.State = subagent.ChildCanceled
	record.snapshot.Receipt.State = subagent.ChildCanceled
	record.snapshot.Failure = &subagent.Failure{Code: "canceled", Message: reason}
	record.snapshot.TerminalAt = utcTime(now)
	record.snapshot.ClaimOwner = ""
	record.snapshot.ClaimUntil = time.Time{}
	record.snapshot.WakePending = true
	record.snapshot.Version++
	if err := s.saveRecord(tx, record); err != nil {
		return err
	}
	return s.appendFact(tx, record, subagent.FactChildTerminal, now)
}

// appendFact writes one lifecycle fact at the record's current revision, which
// is what makes the log readable in commit order rather than wall-clock order.
func (s *SQLiteSubagentStore) appendFact(tx *sql.Tx, record *subagentRecord, kind subagent.FactKind, now time.Time) error {
	tenant := record.snapshot.Receipt.Child.TenantKey
	fact := subagent.CommittedFact{
		FactKey:         subagentFactKey(record.snapshot.Receipt.Child.RelationshipKey, record.snapshot.Version),
		TenantKey:       tenant,
		Kind:            kind,
		RelationshipKey: record.snapshot.Receipt.Child.RelationshipKey,
		ParentRunKey:    record.snapshot.Parent.RunKey,
		ChildRunKey:     record.snapshot.Receipt.Child.RunKey,
		Revision:        record.snapshot.Version,
		OccurredAt:      utcTime(now),
	}
	payload, err := json.Marshal(fact)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO subagent_facts(tenant_key, sequence, fact)
		VALUES(?, (SELECT COALESCE(MAX(sequence), 0) + 1 FROM subagent_facts WHERE tenant_key = ?), ?)`,
		string(tenant), string(tenant), payload)
	return err
}

func (s *SQLiteSubagentStore) cancelResult(tx *sql.Tx, tenantKey agent.TenantKey, requestKey string) (subagent.CancelResult, bool, error) {
	var result subagent.CancelResult
	err := tx.QueryRow(`SELECT affected, terminal, pending FROM subagent_cancel_requests WHERE tenant_key = ? AND request_key = ?`,
		string(tenantKey), requestKey).Scan(&result.Affected, &result.Terminal, &result.Pending)
	if errors.Is(err, sql.ErrNoRows) {
		return subagent.CancelResult{}, false, nil
	}
	if err != nil {
		return subagent.CancelResult{}, false, err
	}
	return result, true, nil
}

func cloneSubagentFailure(value *subagent.Failure) *subagent.Failure {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func sameSubagentTerminal(snapshot subagent.Snapshot, result subagent.RunResult) bool {
	if snapshot.State != result.State || snapshot.ResultRef != result.ResultRef ||
		snapshot.FailureRef != result.FailureRef || snapshot.UsageFactKey != result.UsageFactKey ||
		snapshot.Usage != result.Usage {
		return false
	}
	if snapshot.Failure == nil || result.Failure == nil {
		return snapshot.Failure == result.Failure
	}
	return *snapshot.Failure == *result.Failure
}

func subagentWakeRequest(snapshot subagent.Snapshot) subagent.WakeRequest {
	return subagent.WakeRequest{
		WakeKey: subagentWakeKey(snapshot.Receipt.Child.RelationshipKey),
		Parent:  snapshot.Parent,
		Child:   snapshot.Receipt.Child,
		State:   snapshot.State,
	}
}
