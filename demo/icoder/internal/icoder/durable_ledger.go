package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
)

// This file implements the two ledgers that sit beside the durable run store:
// the effect ledger, which records what each tool call did, and the usage
// ledger, which records immutable billing facts. Both are keyed so that a retry
// of the same logical work is recognized rather than duplicated.

func scanEffect(row interface{ Scan(...any) error }) (durable.EffectRecord, error) {
	var record durable.EffectRecord
	var toolCall, result, failure []byte
	var preparedAt, startedAt, finishedAt int64
	var executionKey, runKey, attemptKey, status string
	var callID string
	if err := row.Scan(&executionKey, &record.Digest, &runKey, &attemptKey, &record.StepNumber, &record.Ordinal,
		&callID, &toolCall, &status, &record.FenceToken, &result, &failure, &preparedAt, &startedAt, &finishedAt); err != nil {
		return durable.EffectRecord{}, err
	}
	record.ExecutionKey = durable.ExecutionKey(executionKey)
	record.RunKey = durable.RunKey(runKey)
	record.AttemptKey = durable.AttemptKey(attemptKey)
	record.Status = durable.EffectStatus(status)
	record.PreparedAt, record.StartedAt, record.FinishedAt = fromNanos(preparedAt), fromNanos(startedAt), fromNanos(finishedAt)
	if err := json.Unmarshal(toolCall, &record.ToolCall); err != nil {
		return durable.EffectRecord{}, err
	}
	if len(result) > 0 {
		var value agent.ToolResult
		if err := json.Unmarshal(result, &value); err != nil {
			return durable.EffectRecord{}, err
		}
		record.Result = &value
	}
	if len(failure) > 0 {
		var value durable.EffectFailure
		if err := json.Unmarshal(failure, &value); err != nil {
			return durable.EffectRecord{}, err
		}
		record.Failure = &value
	}
	return record, nil
}

const effectColumns = `execution_key, digest, run_key, attempt_key, step_number, ordinal, call_id, tool_call, status, fence_token, result, failure, prepared_at, started_at, finished_at`

func selectEffect(ctx context.Context, tx *sql.Tx, key durable.ExecutionKey) (durable.EffectRecord, error) {
	record, err := scanEffect(tx.QueryRowContext(ctx, `SELECT `+effectColumns+` FROM durable_effects WHERE execution_key = ?`, string(key)))
	if errors.Is(err, sql.ErrNoRows) {
		return durable.EffectRecord{}, durableFailf(durable.ErrEffectNotFound, "load effect", "", string(key))
	}
	return record, err
}

// newEffectRecord reproduces the durable package's prepared-record identity. The
// execution key is byte-identical to agent.ToolExecution.IdempotencyKey, so the
// runtime and this ledger deduplicate on exactly the same anchor.
func newEffectRecord(snapshot durable.Snapshot, request durable.PrepareEffectRequest) (durable.EffectRecord, error) {
	inputDigest, err := durable.DigestToolInput(request.ToolCall.Input)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	identity := durable.Identity{
		RunKey: snapshot.Identity.RunKey, AgentKey: snapshot.Identity.AgentKey,
		SessionID: snapshot.Identity.SessionID, RequestID: snapshot.Identity.RequestID,
	}
	key := durable.ToolExecutionKey(identity, request.StepNumber, request.Ordinal, request.ToolCall)
	record := durable.EffectRecord{
		ExecutionKey: key, RunKey: durable.RunKey(snapshot.Identity.RunKey), AttemptKey: request.AttemptKey,
		StepNumber: request.StepNumber, Ordinal: request.Ordinal, ToolCall: request.ToolCall,
		Status: durable.EffectPrepared, PreparedAt: request.PreparedAt,
	}
	// The digest covers identity only, matching the durable package: attempt key
	// and prepared time are provenance, so a resumed attempt re-preparing the same
	// call recognizes it instead of reporting a conflict.
	record.Digest, err = durable.CanonicalDigest(struct {
		ExecutionKey durable.ExecutionKey `json:"execution_key"`
		RunKey       durable.RunKey       `json:"run_key"`
		StepNumber   int                  `json:"step_number"`
		Ordinal      int                  `json:"ordinal"`
		ToolCall     agent.ToolCall       `json:"tool_call"`
		InputDigest  string               `json:"input_digest"`
	}{key, record.RunKey, record.StepNumber, record.Ordinal, record.ToolCall, inputDigest})
	return record, err
}

// PrepareEffect creates the effect row before the tool runs, so a crash always
// finds evidence that the call was about to happen.
func (s *SQLiteDurableStore) PrepareEffect(ctx context.Context, request durable.PrepareEffectRequest) (record durable.EffectRecord, created bool, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.EffectRecord{}, false, err
	}
	defer finishTx(tx, &err)
	snapshot, err := guardedRun(ctx, tx, request.Guard, "prepare effect")
	if err != nil {
		return durable.EffectRecord{}, false, err
	}
	record, err = newEffectRecord(snapshot, request)
	if err != nil {
		return durable.EffectRecord{}, false, err
	}
	existing, loadErr := selectEffect(ctx, tx, record.ExecutionKey)
	if loadErr == nil {
		if existing.Digest != record.Digest {
			return durable.EffectRecord{}, false, durableFailf(durable.ErrEffectConflict, "prepare effect", request.Guard.RunKey, "execution key immutable mismatch")
		}
		return existing, false, nil
	}
	if !errors.Is(loadErr, durable.ErrEffectNotFound) {
		return durable.EffectRecord{}, false, loadErr
	}
	toolCall, err := json.Marshal(record.ToolCall)
	if err != nil {
		return durable.EffectRecord{}, false, err
	}
	// The unique indexes on (run, attempt, step, ordinal) and (run, attempt,
	// step, call id) are what reject a duplicate slot rather than a duplicate key.
	if _, err = tx.ExecContext(ctx, `INSERT INTO durable_effects(`+effectColumns+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, NULL, ?, 0, 0)`,
		string(record.ExecutionKey), record.Digest, string(record.RunKey), string(record.AttemptKey),
		record.StepNumber, record.Ordinal, record.ToolCall.ID, toolCall, string(record.Status), nanos(record.PreparedAt)); err != nil {
		return durable.EffectRecord{}, false, durableFailf(durable.ErrEffectConflict, "prepare effect", request.Guard.RunKey, "duplicate step ordinal or call id")
	}
	return record, true, nil
}

// BeginEffect marks exactly one prepared effect running under the current fence.
// An unknown effect is never begun: it may already have changed the world.
func (s *SQLiteDurableStore) BeginEffect(ctx context.Context, guard durable.Guard, key durable.ExecutionKey, startedAt time.Time) (record durable.EffectRecord, err error) {
	return s.armEffect(ctx, guard, key, startedAt, false)
}

// ReplayEffect re-arms an effect that a revoked lease left unknown. It is the
// optional durable.EffectReplayer extension, and the adapter only calls it after
// the runtime has established that the tool's replay policy makes this safe.
func (s *SQLiteDurableStore) ReplayEffect(ctx context.Context, guard durable.Guard, key durable.ExecutionKey, startedAt time.Time) (record durable.EffectRecord, err error) {
	return s.armEffect(ctx, guard, key, startedAt, true)
}

func (s *SQLiteDurableStore) armEffect(ctx context.Context, guard durable.Guard, key durable.ExecutionKey, startedAt time.Time, replay bool) (record durable.EffectRecord, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	defer finishTx(tx, &err)
	operation := "begin effect"
	if replay {
		operation = "replay effect"
	}
	if _, err = guardedRun(ctx, tx, guard, operation); err != nil {
		return durable.EffectRecord{}, err
	}
	record, err = selectEffect(ctx, tx, key)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	if record.RunKey != guard.RunKey {
		return durable.EffectRecord{}, durableFailf(durable.ErrEffectNotFound, operation, guard.RunKey, "")
	}
	if record.Status == durable.EffectRunning && record.FenceToken == guard.FenceToken {
		return record, nil
	}
	if replay {
		if record.Status != durable.EffectUnknown {
			return durable.EffectRecord{}, durableFailf(durable.ErrInvalidTransition, operation, guard.RunKey, "effect is not unknown")
		}
	} else {
		if record.Status == durable.EffectUnknown {
			return durable.EffectRecord{}, durableFailf(durable.ErrToolEffectUnknown, operation, guard.RunKey, "automatic replay denied")
		}
		if record.Status != durable.EffectPrepared {
			return durable.EffectRecord{}, durableFailf(durable.ErrInvalidTransition, operation, guard.RunKey, "effect is not prepared")
		}
	}
	record.Status, record.FenceToken, record.StartedAt, record.FinishedAt = durable.EffectRunning, guard.FenceToken, startedAt, time.Time{}
	if _, err = tx.ExecContext(ctx, `UPDATE durable_effects SET status = ?, fence_token = ?, started_at = ?, finished_at = 0 WHERE execution_key = ?`,
		string(record.Status), record.FenceToken, nanos(startedAt), string(key)); err != nil {
		return durable.EffectRecord{}, err
	}
	return record, nil
}

// CompleteEffect stores the one durable outcome of a tool call. Repeating the
// identical completion is idempotent; a different one under a stale fence is not.
func (s *SQLiteDurableStore) CompleteEffect(ctx context.Context, request durable.CompleteEffectRequest) (record durable.EffectRecord, err error) {
	if (request.Result == nil) == (request.Failure == nil) {
		return durable.EffectRecord{}, durableFailf(durable.ErrInvalidTransition, "complete effect", request.Guard.RunKey, "exactly one result or failure is required")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	defer finishTx(tx, &err)
	if _, err = guardedRun(ctx, tx, request.Guard, "complete effect"); err != nil {
		return durable.EffectRecord{}, err
	}
	record, err = selectEffect(ctx, tx, request.ExecutionKey)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	if record.RunKey != request.Guard.RunKey {
		return durable.EffectRecord{}, durableFailf(durable.ErrEffectNotFound, "complete effect", request.Guard.RunKey, "")
	}
	settled := record.Status == durable.EffectSucceeded || record.Status == durable.EffectFailed
	if settled && record.FenceToken == request.Guard.FenceToken &&
		reflect.DeepEqual(record.Result, request.Result) && reflect.DeepEqual(record.Failure, request.Failure) &&
		record.FinishedAt.Equal(request.FinishedAt) {
		return record, nil
	}
	if record.Status != durable.EffectRunning || record.FenceToken != request.Guard.FenceToken {
		return durable.EffectRecord{}, durableFailf(durable.ErrLeaseLost, "complete effect", request.Guard.RunKey, "effect fence is stale")
	}
	record.Result, record.Failure, record.FinishedAt = request.Result, request.Failure, request.FinishedAt
	record.Status = durable.EffectFailed
	if request.Result != nil {
		record.Status = durable.EffectSucceeded
	}
	var result, failure []byte
	if record.Result != nil {
		if result, err = json.Marshal(record.Result); err != nil {
			return durable.EffectRecord{}, err
		}
	}
	if record.Failure != nil {
		if failure, err = json.Marshal(record.Failure); err != nil {
			return durable.EffectRecord{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE durable_effects SET status = ?, result = ?, failure = ?, finished_at = ? WHERE execution_key = ?`,
		string(record.Status), result, failure, nanos(record.FinishedAt), string(request.ExecutionKey)); err != nil {
		return durable.EffectRecord{}, err
	}
	return record, nil
}

// MarkEffectUnknown records that a running effect's outcome can no longer be
// determined, which is the state reconciliation refuses to replay past.
func (s *SQLiteDurableStore) MarkEffectUnknown(ctx context.Context, guard durable.Guard, key durable.ExecutionKey, at time.Time) (record durable.EffectRecord, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	defer finishTx(tx, &err)
	if _, err = guardedRun(ctx, tx, guard, "mark effect unknown"); err != nil {
		return durable.EffectRecord{}, err
	}
	record, err = selectEffect(ctx, tx, key)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	if record.RunKey != guard.RunKey {
		return durable.EffectRecord{}, durableFailf(durable.ErrEffectNotFound, "mark effect unknown", guard.RunKey, "")
	}
	if record.Status == durable.EffectUnknown {
		return record, nil
	}
	if record.Status != durable.EffectRunning {
		return durable.EffectRecord{}, durableFailf(durable.ErrInvalidTransition, "mark effect unknown", guard.RunKey, "effect is not running")
	}
	record.Status, record.FinishedAt = durable.EffectUnknown, at
	if _, err = tx.ExecContext(ctx, `UPDATE durable_effects SET status = ?, finished_at = ? WHERE execution_key = ?`,
		string(record.Status), nanos(at), string(key)); err != nil {
		return durable.EffectRecord{}, err
	}
	return record, nil
}

// LoadEffect returns one effect record without authorizing any write.
func (s *SQLiteDurableStore) LoadEffect(ctx context.Context, key durable.ExecutionKey) (record durable.EffectRecord, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.EffectRecord{}, err
	}
	defer finishTx(tx, &err)
	return selectEffect(ctx, tx, key)
}

// ListEffects returns a run's effects in execution order, which is the order an
// operator needs when deciding what a partially executed run actually did.
func (s *SQLiteDurableStore) ListEffects(ctx context.Context, runKey durable.RunKey) (records []durable.EffectRecord, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finishTx(tx, &err)
	rows, err := tx.QueryContext(ctx, `SELECT `+effectColumns+` FROM durable_effects WHERE run_key = ? ORDER BY step_number, ordinal`, string(runKey))
	if err != nil {
		return nil, err
	}
	records = make([]durable.EffectRecord, 0)
	for rows.Next() {
		record, scanErr := scanEffect(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		records = append(records, record)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return records, nil
}

// RecordUsage is idempotent on (tenant, usage key). Replaying the identical fact
// reports created == false; the same key with different numbers is a conflict,
// because usage facts are immutable evidence, not a mutable counter.
func (s *SQLiteDurableStore) RecordUsage(ctx context.Context, fact durable.UsageFact) (stored durable.UsageFact, created bool, err error) {
	if fact.TenantKey == "" || fact.UsageKey == "" || fact.RunKey == "" || fact.AttemptKey == "" || fact.Kind == "" ||
		fact.Provider == "" || fact.Model == "" || fact.PricingVersion == "" || fact.Currency == "" || fact.OccurredAt.IsZero() {
		return durable.UsageFact{}, false, durableFailf(durable.ErrUsageConflict, "record usage", fact.RunKey, "missing immutable usage field")
	}
	if fact.InputTokens < 0 || fact.OutputTokens < 0 || fact.CacheReadTokens < 0 || fact.CacheWriteTokens < 0 || fact.CostMicros < 0 {
		return durable.UsageFact{}, false, durableFailf(durable.ErrUsageConflict, "record usage", fact.RunKey, "negative usage delta")
	}
	payload, err := json.Marshal(fact)
	if err != nil {
		return durable.UsageFact{}, false, err
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.UsageFact{}, false, err
	}
	defer finishTx(tx, &err)
	var existing []byte
	loadErr := tx.QueryRowContext(ctx, `SELECT payload FROM durable_usage WHERE tenant_key = ? AND usage_key = ?`, string(fact.TenantKey), string(fact.UsageKey)).Scan(&existing)
	if loadErr == nil {
		var current durable.UsageFact
		if err = json.Unmarshal(existing, &current); err != nil {
			return durable.UsageFact{}, false, err
		}
		if !current.OccurredAt.Equal(fact.OccurredAt) || !sameUsageValues(current, fact) {
			return durable.UsageFact{}, false, durableFailf(durable.ErrUsageConflict, "record usage", fact.RunKey, "immutable delta differs")
		}
		return current, false, nil
	}
	if !errors.Is(loadErr, sql.ErrNoRows) {
		return durable.UsageFact{}, false, loadErr
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO durable_usage(tenant_key, usage_key, run_key, attempt_key, payload) VALUES(?, ?, ?, ?, ?)`,
		string(fact.TenantKey), string(fact.UsageKey), string(fact.RunKey), string(fact.AttemptKey), payload); err != nil {
		return durable.UsageFact{}, false, err
	}
	return fact, true, nil
}

// sameUsageValues compares everything except the timestamp, which is compared
// separately because time.Time equality must use Equal rather than ==.
func sameUsageValues(left, right durable.UsageFact) bool {
	left.OccurredAt, right.OccurredAt = time.Time{}, time.Time{}
	return left == right
}

// LoadUsage returns one recorded fact.
func (s *SQLiteDurableStore) LoadUsage(ctx context.Context, tenant durable.TenantKey, key durable.UsageKey) (fact durable.UsageFact, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return durable.UsageFact{}, err
	}
	defer finishTx(tx, &err)
	var payload []byte
	if err = tx.QueryRowContext(ctx, `SELECT payload FROM durable_usage WHERE tenant_key = ? AND usage_key = ?`, string(tenant), string(key)).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return durable.UsageFact{}, durable.ErrUsageNotFound
		}
		return durable.UsageFact{}, err
	}
	err = json.Unmarshal(payload, &fact)
	return fact, err
}

// ListAttemptUsage returns every fact one attempt produced, in key order.
func (s *SQLiteDurableStore) ListAttemptUsage(ctx context.Context, tenant durable.TenantKey, attempt durable.AttemptKey) (facts []durable.UsageFact, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finishTx(tx, &err)
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM durable_usage WHERE tenant_key = ? AND attempt_key = ? ORDER BY usage_key`, string(tenant), string(attempt))
	if err != nil {
		return nil, err
	}
	facts = make([]durable.UsageFact, 0)
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var fact durable.UsageFact
		if err = json.Unmarshal(payload, &fact); err != nil {
			_ = rows.Close()
			return nil, err
		}
		facts = append(facts, fact)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return facts, nil
}
