package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	toollifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

// SQLiteToolLedger is iCoder's persistent tool.ExecutionLedger. The advanced
// tool executor uses it to record what each tool invocation was authorized to
// do and what it actually did, so an approval that was granted minutes ago can
// be revalidated against the exact prepared execution rather than a re-derived
// guess.
//
// It is a lifecycle consumer, not a second source of truth: the durable run
// authority still owns recovery. This ledger exists because the executor needs
// per-invocation records that survive a suspended run, which is exactly what an
// approval that pauses a run requires.
type SQLiteToolLedger struct{ db *sql.DB }

// NewSQLiteToolLedger binds the executor ledger to an already-migrated database.
func NewSQLiteToolLedger(db *sql.DB) *SQLiteToolLedger { return &SQLiteToolLedger{db: db} }

var _ toollifecycle.ExecutionLedger = (*SQLiteToolLedger)(nil)

// ToolLedger returns the executor ledger backed by this store's database.
func (s *Store) ToolLedger() *SQLiteToolLedger { return NewSQLiteToolLedger(s.db) }

var toolLedgerMigrations = []string{
	`CREATE TABLE IF NOT EXISTS tool_executions (
		execution_key TEXT PRIMARY KEY,
		prepared_digest TEXT NOT NULL,
		run_key TEXT NOT NULL,
		attempt_key TEXT NOT NULL,
		session_ref TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		call_id TEXT NOT NULL,
		action TEXT NOT NULL,
		effect_class TEXT NOT NULL,
		status TEXT NOT NULL,
		fence_token INTEGER NOT NULL DEFAULT 0,
		revision INTEGER NOT NULL DEFAULT 0,
		prepared BLOB NOT NULL,
		result BLOB,
		failure BLOB,
		suspension BLOB,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS tool_executions_by_run ON tool_executions(run_key, created_at)`,
}

const toolExecutionColumns = `execution_key, prepared_digest, status, fence_token, revision, prepared, result, failure, suspension`

func scanToolExecution(row interface{ Scan(...any) error }) (toollifecycle.ExecutionRecord, string, error) {
	var record toollifecycle.ExecutionRecord
	var executionKey, preparedDigest, status string
	var prepared, result, failure, suspension []byte
	if err := row.Scan(&executionKey, &preparedDigest, &status, &record.FenceToken, &record.Revision, &prepared, &result, &failure, &suspension); err != nil {
		return toollifecycle.ExecutionRecord{}, "", err
	}
	record.Status = toollifecycle.ExecutionStatus(status)
	if err := json.Unmarshal(prepared, &record.Prepared); err != nil {
		return toollifecycle.ExecutionRecord{}, "", err
	}
	if len(result) > 0 {
		var value agent.ToolResult
		if err := json.Unmarshal(result, &value); err != nil {
			return toollifecycle.ExecutionRecord{}, "", err
		}
		record.Result = &value
	}
	if len(failure) > 0 {
		var value toollifecycle.Failure
		if err := json.Unmarshal(failure, &value); err != nil {
			return toollifecycle.ExecutionRecord{}, "", err
		}
		record.Failure = &value
	}
	if len(suspension) > 0 {
		var value toollifecycle.Suspension
		if err := json.Unmarshal(suspension, &value); err != nil {
			return toollifecycle.ExecutionRecord{}, "", err
		}
		record.Suspension = &value
	}
	return record, preparedDigest, nil
}

func selectToolExecution(ctx context.Context, tx *sql.Tx, key string) (toollifecycle.ExecutionRecord, string, error) {
	record, digest, err := scanToolExecution(tx.QueryRowContext(ctx, `SELECT `+toolExecutionColumns+` FROM tool_executions WHERE execution_key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return toollifecycle.ExecutionRecord{}, "", toollifecycle.ErrToolNotFound
	}
	return record, digest, err
}

// Prepare records the immutable identity of one authorized invocation. Replaying
// the identical prepared execution is not an error; a different one under the
// same key is, because the key is supposed to pin exactly this call.
func (l *SQLiteToolLedger) Prepare(ctx context.Context, prepared toollifecycle.PreparedExecution) (record toollifecycle.ExecutionRecord, created bool, err error) {
	digest, err := agent.CanonicalDigest(prepared)
	if err != nil {
		return toollifecycle.ExecutionRecord{}, false, err
	}
	payload, err := json.Marshal(prepared)
	if err != nil {
		return toollifecycle.ExecutionRecord{}, false, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return toollifecycle.ExecutionRecord{}, false, err
	}
	defer finishTx(tx, &err)
	existing, existingDigest, loadErr := selectToolExecution(ctx, tx, prepared.ExecutionKey)
	if loadErr == nil {
		if existingDigest != digest {
			return toollifecycle.ExecutionRecord{}, false, toollifecycle.ErrExecutionConflict
		}
		return existing, false, nil
	}
	if !errors.Is(loadErr, toollifecycle.ErrToolNotFound) {
		return toollifecycle.ExecutionRecord{}, false, loadErr
	}
	now := time.Now().UTC().UnixNano()
	if _, err = tx.ExecContext(ctx, `INSERT INTO tool_executions(execution_key, prepared_digest, run_key, attempt_key, session_ref, tool_name, call_id, action, effect_class, status, fence_token, revision, prepared, result, failure, suspension, created_at, updated_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 1, ?, NULL, NULL, NULL, ?, ?)`,
		prepared.ExecutionKey, digest, prepared.RunKey, prepared.AttemptKey, prepared.SessionRef,
		prepared.ToolName, prepared.CallID, prepared.Action, string(prepared.EffectClass),
		string(toollifecycle.StatusPrepared), payload, now, now); err != nil {
		return toollifecycle.ExecutionRecord{}, false, err
	}
	return toollifecycle.ExecutionRecord{Prepared: prepared, Status: toollifecycle.StatusPrepared, Revision: 1}, true, nil
}

// Begin moves a prepared execution to running under the caller's fence. It is
// the last durable write before the tool may change anything outside the process.
func (l *SQLiteToolLedger) Begin(ctx context.Context, key string, fence uint64) (record toollifecycle.ExecutionRecord, err error) {
	return l.transition(ctx, key, func(current toollifecycle.ExecutionRecord) (toollifecycle.ExecutionRecord, error) {
		if current.Status != toollifecycle.StatusPrepared {
			return toollifecycle.ExecutionRecord{}, toollifecycle.ErrExecutionConflict
		}
		current.Status, current.FenceToken, current.Revision = toollifecycle.StatusRunning, fence, current.Revision+1
		return current, nil
	})
}

// Reject records that authorization refused an execution that never ran, which
// is why it requires the prepared fence rather than a running one.
func (l *SQLiteToolLedger) Reject(ctx context.Context, key string, fence uint64, failure toollifecycle.Failure) (record toollifecycle.ExecutionRecord, err error) {
	return l.transition(ctx, key, func(current toollifecycle.ExecutionRecord) (toollifecycle.ExecutionRecord, error) {
		if current.Status != toollifecycle.StatusPrepared || current.Prepared.FenceToken != fence {
			return toollifecycle.ExecutionRecord{}, toollifecycle.ErrStaleFence
		}
		stored := failure
		current.Status, current.Failure, current.FenceToken = toollifecycle.StatusFailed, &stored, fence
		current.Revision++
		return current, nil
	})
}

// Suspend parks a running execution and stores the handle a later attempt
// resumes from. A parked execution has neither failed nor become ambiguous, so
// it gets a state of its own rather than being folded into either.
func (l *SQLiteToolLedger) Suspend(ctx context.Context, command toollifecycle.SuspendExecution) (record toollifecycle.ExecutionRecord, err error) {
	if command.Suspension.Kind == "" {
		return toollifecycle.ExecutionRecord{}, fmt.Errorf("icoder tool ledger: a suspension kind is required: %w", toollifecycle.ErrInvalidConfiguration)
	}
	return l.transition(ctx, command.ExecutionKey, func(current toollifecycle.ExecutionRecord) (toollifecycle.ExecutionRecord, error) {
		if current.Status != toollifecycle.StatusRunning || current.FenceToken != command.FenceToken {
			return toollifecycle.ExecutionRecord{}, toollifecycle.ErrStaleFence
		}
		stored := command.Suspension
		current.Status, current.Suspension = toollifecycle.StatusSuspended, &stored
		current.Revision++
		return current, nil
	})
}

// Resume re-arms a parked execution so the tool can be invoked again with its
// own handle.
func (l *SQLiteToolLedger) Resume(ctx context.Context, key string, fence uint64) (record toollifecycle.ExecutionRecord, err error) {
	return l.transition(ctx, key, func(current toollifecycle.ExecutionRecord) (toollifecycle.ExecutionRecord, error) {
		if current.Status != toollifecycle.StatusSuspended || current.FenceToken != fence {
			return toollifecycle.ExecutionRecord{}, toollifecycle.ErrStaleFence
		}
		current.Status = toollifecycle.StatusRunning
		current.Revision++
		return current, nil
	})
}

// Complete stores the one outcome of a running execution.
func (l *SQLiteToolLedger) Complete(ctx context.Context, command toollifecycle.CompleteExecution) (record toollifecycle.ExecutionRecord, err error) {
	return l.transition(ctx, command.ExecutionKey, func(current toollifecycle.ExecutionRecord) (toollifecycle.ExecutionRecord, error) {
		if current.Status != toollifecycle.StatusRunning || current.FenceToken != command.FenceToken {
			return toollifecycle.ExecutionRecord{}, toollifecycle.ErrStaleFence
		}
		current.Result, current.Failure = command.Result, command.Failure
		current.Status, current.Suspension = toollifecycle.StatusFailed, nil
		if command.Result != nil {
			current.Status = toollifecycle.StatusSucceeded
		}
		current.Revision++
		return current, nil
	})
}

// MarkUnknown records that an effect may have happened but its result could not
// be stored. Such an execution is never replayed automatically.
func (l *SQLiteToolLedger) MarkUnknown(ctx context.Context, key string, fence uint64, failure toollifecycle.Failure) (record toollifecycle.ExecutionRecord, err error) {
	return l.transition(ctx, key, func(current toollifecycle.ExecutionRecord) (toollifecycle.ExecutionRecord, error) {
		if current.Status != toollifecycle.StatusRunning || current.FenceToken != fence {
			return toollifecycle.ExecutionRecord{}, toollifecycle.ErrStaleFence
		}
		stored := failure
		current.Status, current.Failure = toollifecycle.StatusUnknown, &stored
		current.Revision++
		return current, nil
	})
}

// Load returns one execution record, which is how a resumed approval finds the
// exact prepared execution it must be revalidated against.
func (l *SQLiteToolLedger) Load(ctx context.Context, key string) (record toollifecycle.ExecutionRecord, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return toollifecycle.ExecutionRecord{}, err
	}
	defer finishTx(tx, &err)
	record, _, err = selectToolExecution(ctx, tx, key)
	return record, err
}

// transition applies one guarded state change inside a single transaction. The
// apply function owns the legality rules; this helper owns atomicity.
func (l *SQLiteToolLedger) transition(ctx context.Context, key string, apply func(toollifecycle.ExecutionRecord) (toollifecycle.ExecutionRecord, error)) (record toollifecycle.ExecutionRecord, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return toollifecycle.ExecutionRecord{}, err
	}
	defer finishTx(tx, &err)
	current, _, err := selectToolExecution(ctx, tx, key)
	if err != nil {
		return toollifecycle.ExecutionRecord{}, err
	}
	updated, err := apply(current)
	if err != nil {
		return toollifecycle.ExecutionRecord{}, err
	}
	var result, failure, suspension []byte
	if updated.Result != nil {
		if result, err = json.Marshal(updated.Result); err != nil {
			return toollifecycle.ExecutionRecord{}, err
		}
	}
	if updated.Failure != nil {
		if failure, err = json.Marshal(updated.Failure); err != nil {
			return toollifecycle.ExecutionRecord{}, err
		}
	}
	if updated.Suspension != nil {
		if suspension, err = json.Marshal(updated.Suspension); err != nil {
			return toollifecycle.ExecutionRecord{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tool_executions SET status = ?, fence_token = ?, revision = ?, result = ?, failure = ?, suspension = ?, updated_at = ? WHERE execution_key = ?`,
		string(updated.Status), updated.FenceToken, updated.Revision, result, failure, suspension, time.Now().UTC().UnixNano(), key); err != nil {
		return toollifecycle.ExecutionRecord{}, err
	}
	return updated, nil
}
