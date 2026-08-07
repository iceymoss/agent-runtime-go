package icoder

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
)

// SQLiteEventStore persists canonical events and their outbox delivery state.
type SQLiteEventStore struct {
	db  *sql.DB
	now func() time.Time
}

func NewSQLiteEventStore(db *sql.DB) *SQLiteEventStore {
	return &SQLiteEventStore{db: db, now: time.Now}
}

var _ event.Store = (*SQLiteEventStore)(nil)

func (s *SQLiteEventStore) Append(ctx context.Context, command event.AppendCommand) (event.Envelope, error) {
	events, err := s.AppendBatch(ctx, event.AppendBatchCommand{Events: []event.AppendCommand{command}})
	if err != nil {
		return event.Envelope{}, err
	}
	return events[0], nil
}

func (s *SQLiteEventStore) AppendBatch(ctx context.Context, command event.AppendBatchCommand) ([]event.Envelope, error) {
	if err := contextErrorSQLite(ctx); err != nil {
		return nil, err
	}
	if len(command.Events) == 0 {
		return []event.Envelope{}, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	events, err := s.AppendBatchTx(ctx, tx, command)
	if err != nil {
		return nil, errors.Join(err, tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

// AppendBatchTx appends events inside a caller-owned transaction. The caller
// remains responsible for committing or rolling back the transaction.
func (s *SQLiteEventStore) AppendBatchTx(ctx context.Context, tx *sql.Tx, command event.AppendBatchCommand) ([]event.Envelope, error) {
	if err := contextErrorSQLite(ctx); err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, errors.New("event store: nil transaction")
	}
	if len(command.Events) == 0 {
		return []event.Envelope{}, nil
	}

	inputs := make([]event.Envelope, len(command.Events))
	seen := make(map[string]event.Envelope, len(command.Events))
	existing := make(map[string]event.Envelope, len(command.Events))
	for i, item := range command.Events {
		inputs[i] = item.Envelope.Clone()
		if err := validateSQLiteEnvelope(inputs[i]); err != nil {
			return nil, fmt.Errorf("append event %d: %w", i, err)
		}
		if previous, ok := seen[inputs[i].EventID]; ok && !sameSQLiteInput(previous, inputs[i]) {
			return nil, fmt.Errorf("%w: event id %q differs within batch", event.ErrIdempotencyConflict, inputs[i].EventID)
		}
		seen[inputs[i].EventID] = inputs[i]
	}
	for eventID, input := range seen {
		stored, found, err := selectSQLiteEnvelope(ctx, tx, eventID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		immutable := stored.Clone()
		immutable.Sequence = 0
		immutable.PersistedAt = time.Time{}
		if !sameSQLiteInput(immutable, input) {
			return nil, fmt.Errorf("%w: event id %q has different immutable input", event.ErrIdempotencyConflict, eventID)
		}
		existing[eventID] = stored
	}

	now := s.now().UTC()
	created := make(map[string]event.Envelope, len(seen))
	results := make([]event.Envelope, len(inputs))
	for i, input := range inputs {
		if stored, ok := existing[input.EventID]; ok {
			results[i] = stored.Clone()
			continue
		}
		if stored, ok := created[input.EventID]; ok {
			results[i] = stored.Clone()
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_streams(tenant_key, stream_key) VALUES(?, ?) ON CONFLICT DO NOTHING`, input.TenantKey, input.StreamKey); err != nil {
			return nil, err
		}
		var sequence uint64
		if err := tx.QueryRowContext(ctx, `UPDATE event_streams SET last_sequence = last_sequence + 1 WHERE tenant_key = ? AND stream_key = ? RETURNING last_sequence`, input.TenantKey, input.StreamKey).Scan(&sequence); err != nil {
			return nil, err
		}
		input.Sequence = sequence
		input.PersistedAt = now
		if err := insertSQLiteRecord(ctx, tx, input); err != nil {
			return nil, err
		}
		created[input.EventID] = input.Clone()
		results[i] = input.Clone()
	}
	return results, nil
}

func (s *SQLiteEventStore) Claim(ctx context.Context, command event.ClaimCommand) ([]event.ClaimedEvent, error) {
	if err := contextErrorSQLite(ctx); err != nil {
		return nil, err
	}
	if !command.TenantKey.Valid() || command.Owner == "" || command.Limit <= 0 || command.LeaseDuration <= 0 {
		return nil, fmt.Errorf("%w: tenant, owner, positive limit, and lease duration are required", event.ErrInvalidEnvelope)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	rollback := func(err error) ([]event.ClaimedEvent, error) { return nil, errors.Join(err, tx.Rollback()) }
	now := s.now().UTC()
	rows, err := tx.QueryContext(ctx, `SELECT event_id FROM event_records
		WHERE tenant_key = ? AND outbox_state NOT IN (?, ?)
		AND (outbox_state != ? OR lease_expires <= ?) AND next_attempt <= ?
		ORDER BY next_attempt, persisted_at, event_id LIMIT ?`,
		command.TenantKey, event.OutboxDelivered, event.OutboxDead, event.OutboxLeased, now.UnixNano(), now.UnixNano(), command.Limit)
	if err != nil {
		return rollback(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return rollback(err)
	}

	claimed := make([]event.ClaimedEvent, 0, len(ids))
	for _, id := range ids {
		token, err := randomLeaseToken()
		if err != nil {
			return rollback(err)
		}
		expires := now.Add(command.LeaseDuration)
		row := tx.QueryRowContext(ctx, `UPDATE event_records SET outbox_state = ?, attempts = attempts + 1,
			lease_owner = ?, lease_token = ?, lease_fence = lease_fence + 1, lease_expires = ?
			WHERE event_id = ? RETURNING attempts, lease_fence`, event.OutboxLeased, command.Owner, token, expires.UnixNano(), id)
		var attempts uint32
		var fence uint64
		if err := row.Scan(&attempts, &fence); err != nil {
			return rollback(err)
		}
		envelope, found, err := selectSQLiteEnvelope(ctx, tx, id)
		if err != nil || !found {
			if err == nil {
				err = event.ErrNotFound
			}
			return rollback(err)
		}
		claimed = append(claimed, event.ClaimedEvent{Envelope: envelope, Attempts: attempts, LeaseOwner: command.Owner, LeaseToken: token, LeaseFence: fence, ExpiresAt: expires})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (s *SQLiteEventStore) Ack(ctx context.Context, command event.AckCommand) error {
	if err := contextErrorSQLite(ctx); err != nil {
		return err
	}
	if err := validateLeaseReceipt(command); err != nil {
		return err
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE event_records SET outbox_state = ?, delivered_at = ?,
		lease_owner = '', lease_token = '', lease_expires = 0
		WHERE event_id = ? AND tenant_key = ? AND outbox_state = ? AND lease_owner = ?
		AND lease_token = ? AND lease_fence = ? AND lease_expires > ?`, event.OutboxDelivered, now.UnixNano(), command.EventID,
		command.TenantKey, event.OutboxLeased, command.LeaseOwner, command.LeaseToken, command.LeaseFence, now.UnixNano())
	if err != nil {
		return err
	}
	return requireLeaseUpdate(result)
}

func (s *SQLiteEventStore) Nack(ctx context.Context, command event.NackCommand) error {
	if err := contextErrorSQLite(ctx); err != nil {
		return err
	}
	if command.RetryAfter < 0 {
		return fmt.Errorf("%w: retry delay must not be negative", event.ErrInvalidEnvelope)
	}
	if err := validateLeaseReceipt(command.AckCommand); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	var attempts uint32
	err = tx.QueryRowContext(ctx, `SELECT attempts FROM event_records WHERE event_id = ? AND tenant_key = ?
		AND outbox_state = ? AND lease_owner = ? AND lease_token = ? AND lease_fence = ? AND lease_expires > ?`,
		command.EventID, command.TenantKey, event.OutboxLeased, command.LeaseOwner, command.LeaseToken, command.LeaseFence, now.UnixNano()).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.Join(event.ErrLeaseLost, tx.Rollback())
	}
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}
	state := event.OutboxPending
	deadAt := int64(0)
	if command.Dead || command.MaxAttempts > 0 && attempts >= command.MaxAttempts {
		state, deadAt = event.OutboxDead, now.UnixNano()
	}
	_, err = tx.ExecContext(ctx, `UPDATE event_records SET outbox_state = ?,
		next_attempt = CASE WHEN ? = ? THEN ? ELSE next_attempt END, dead_at = ?, last_error = ?,
		lease_owner = '', lease_token = '', lease_expires = 0 WHERE event_id = ?`,
		state, state, event.OutboxPending, now.Add(command.RetryAfter).UnixNano(), deadAt, truncateSQLite(command.Reason, 1024), command.EventID)
	if err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func (s *SQLiteEventStore) Replay(ctx context.Context, query event.ReplayQuery) (event.ReplayResult, error) {
	if err := contextErrorSQLite(ctx); err != nil {
		return event.ReplayResult{}, err
	}
	cursor := query.Cursor
	if !cursor.TenantKey.Valid() || cursor.StreamKey == "" || query.Limit < 0 || cursor.Sequence == 0 && cursor.EventID != "" || cursor.Sequence > 0 && cursor.EventID == "" {
		return event.ReplayResult{}, fmt.Errorf("%w: invalid replay cursor", event.ErrInvalidStream)
	}
	var high, floor uint64
	err := s.db.QueryRowContext(ctx, `SELECT last_sequence, retention_floor FROM event_streams WHERE tenant_key = ? AND stream_key = ?`, cursor.TenantKey, cursor.StreamKey).Scan(&high, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		high, floor = 0, 1
	} else if err != nil {
		return event.ReplayResult{}, err
	}
	if cursor.Sequence > 0 {
		if cursor.Sequence < floor {
			return sqliteReconciliation(cursor, floor, event.GapRetentionExpired, event.ResetCursorExpired), nil
		}
		var eventID string
		err := s.db.QueryRowContext(ctx, `SELECT event_id FROM event_records WHERE tenant_key = ? AND stream_key = ? AND sequence = ?`, cursor.TenantKey, cursor.StreamKey, cursor.Sequence).Scan(&eventID)
		if errors.Is(err, sql.ErrNoRows) || err == nil && eventID != cursor.EventID {
			return sqliteReconciliation(cursor, floor, event.GapSequenceInvariant, event.ResetCursorUnknown), nil
		}
		if err != nil {
			return event.ReplayResult{}, err
		}
	}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	result := event.ReplayResult{Next: cursor}
	expected := cursor.Sequence + 1
	if expected < floor {
		expected = floor
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, tenant_key, stream_key, sequence, event_type, schema_version,
		reliability, aggregate_type, aggregate_key, aggregate_revision, correlation_id, causation_id, occurred_at, persisted_at, payload
		FROM event_records WHERE tenant_key = ? AND stream_key = ? AND sequence >= ? ORDER BY sequence LIMIT ?`, cursor.TenantKey, cursor.StreamKey, expected, limit)
	if err != nil {
		return event.ReplayResult{}, err
	}
	defer rows.Close()
	for rows.Next() {
		envelope, err := scanSQLiteEnvelope(rows)
		if err != nil {
			return event.ReplayResult{}, err
		}
		if envelope.Sequence != expected {
			return sqliteReconciliationAt(result, cursor, floor, expected, high), nil
		}
		result.Events = append(result.Events, envelope)
		result.Next = event.Cursor{TenantKey: cursor.TenantKey, StreamKey: cursor.StreamKey, Sequence: expected, EventID: envelope.EventID}
		expected++
	}
	if err := rows.Err(); err != nil {
		return event.ReplayResult{}, err
	}
	if expected <= high && len(result.Events) < limit {
		return sqliteReconciliationAt(result, cursor, floor, expected, high), nil
	}
	return result, nil
}

// SetRetentionFloor advances retention for tests and maintenance operations.
func (s *SQLiteEventStore) SetRetentionFloor(ctx context.Context, tenant agent.TenantKey, stream string, floor uint64) error {
	if !tenant.Valid() || stream == "" || floor == 0 {
		return fmt.Errorf("%w: invalid retention floor", event.ErrInvalidStream)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_streams(tenant_key, stream_key) VALUES(?, ?) ON CONFLICT DO NOTHING`, tenant, stream); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	var current uint64
	if err := tx.QueryRowContext(ctx, `SELECT retention_floor FROM event_streams WHERE tenant_key = ? AND stream_key = ?`, tenant, stream).Scan(&current); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if floor < current {
		return errors.Join(fmt.Errorf("%w: retention floor cannot move backwards", event.ErrSequenceConflict), tx.Rollback())
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_records WHERE tenant_key = ? AND stream_key = ? AND sequence < ? AND outbox_state IN (?, ?)`, tenant, stream, floor, event.OutboxDelivered, event.OutboxDead); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	if _, err := tx.ExecContext(ctx, `UPDATE event_streams SET retention_floor = ? WHERE tenant_key = ? AND stream_key = ?`, floor, tenant, stream); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func insertSQLiteRecord(ctx context.Context, tx *sql.Tx, envelope event.Envelope) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO event_records(event_id, tenant_key, stream_key, sequence, event_type,
		schema_version, reliability, aggregate_type, aggregate_key, aggregate_revision, correlation_id, causation_id,
		occurred_at, persisted_at, payload, outbox_state, next_attempt)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, envelope.EventID, envelope.TenantKey, envelope.StreamKey,
		envelope.Sequence, envelope.Type, envelope.SchemaVersion, envelope.Reliability, envelope.AggregateType, envelope.AggregateKey,
		envelope.AggregateRevision, envelope.CorrelationID, envelope.CausationID, envelope.OccurredAt.UnixNano(), envelope.PersistedAt.UnixNano(),
		envelope.Payload, event.OutboxPending, envelope.PersistedAt.UnixNano())
	return err
}

type sqliteScanner interface {
	Scan(...any) error
}

func scanSQLiteEnvelope(scanner sqliteScanner) (event.Envelope, error) {
	var envelope event.Envelope
	var tenant string
	var occurredAt, persistedAt int64
	err := scanner.Scan(&envelope.EventID, &tenant, &envelope.StreamKey, &envelope.Sequence, &envelope.Type, &envelope.SchemaVersion,
		&envelope.Reliability, &envelope.AggregateType, &envelope.AggregateKey, &envelope.AggregateRevision, &envelope.CorrelationID,
		&envelope.CausationID, &occurredAt, &persistedAt, &envelope.Payload)
	envelope.TenantKey = agent.TenantKey(tenant)
	envelope.OccurredAt = time.Unix(0, occurredAt).UTC()
	envelope.PersistedAt = time.Unix(0, persistedAt).UTC()
	return envelope, err
}

func selectSQLiteEnvelope(ctx context.Context, tx *sql.Tx, eventID string) (event.Envelope, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT event_id, tenant_key, stream_key, sequence, event_type, schema_version,
		reliability, aggregate_type, aggregate_key, aggregate_revision, correlation_id, causation_id, occurred_at, persisted_at, payload
		FROM event_records WHERE event_id = ?`, eventID)
	envelope, err := scanSQLiteEnvelope(row)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Envelope{}, false, nil
	}
	return envelope, err == nil, err
}

func selectStoredEvent(ctx context.Context, db *sql.DB, eventID string) (event.Envelope, bool, error) {
	row := db.QueryRowContext(ctx, `SELECT event_id, tenant_key, stream_key, sequence, event_type, schema_version,
		reliability, aggregate_type, aggregate_key, aggregate_revision, correlation_id, causation_id, occurred_at, persisted_at, payload
		FROM event_records WHERE event_id = ?`, eventID)
	envelope, err := scanSQLiteEnvelope(row)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Envelope{}, false, nil
	}
	return envelope, err == nil, err
}

func validateSQLiteEnvelope(envelope event.Envelope) error {
	if !envelope.TenantKey.Valid() || envelope.EventID == "" || envelope.StreamKey == "" || envelope.Type == "" || envelope.SchemaVersion == 0 || envelope.AggregateType == "" || envelope.AggregateKey == "" || envelope.OccurredAt.IsZero() {
		return event.ErrInvalidEnvelope
	}
	if envelope.Sequence != 0 || !envelope.PersistedAt.IsZero() {
		return fmt.Errorf("%w: sequence and persisted time are store-owned", event.ErrInvalidEnvelope)
	}
	if envelope.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("%w: occurred time must be UTC", event.ErrInvalidEnvelope)
	}
	if envelope.Reliability != event.ReliabilitySessionSnapshot && envelope.Reliability != event.ReliabilityTerminal && envelope.Reliability != event.ReliabilityDomain {
		return fmt.Errorf("%w: observation is not persisted", event.ErrInvalidEnvelope)
	}
	if !json.Valid(envelope.Payload) {
		return fmt.Errorf("%w: payload must be valid JSON", event.ErrInvalidEnvelope)
	}
	return nil
}

func sameSQLiteInput(left, right event.Envelope) bool {
	return left.TenantKey == right.TenantKey && left.EventID == right.EventID && left.StreamKey == right.StreamKey &&
		left.Sequence == right.Sequence && left.Type == right.Type && left.SchemaVersion == right.SchemaVersion &&
		left.Reliability == right.Reliability && left.AggregateType == right.AggregateType && left.AggregateKey == right.AggregateKey &&
		left.AggregateRevision == right.AggregateRevision && left.CorrelationID == right.CorrelationID && left.CausationID == right.CausationID &&
		left.OccurredAt.Equal(right.OccurredAt) && left.PersistedAt.Equal(right.PersistedAt) && bytes.Equal(left.Payload, right.Payload)
}

func randomLeaseToken() (string, error) {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func validateLeaseReceipt(command event.AckCommand) error {
	if !command.TenantKey.Valid() || command.EventID == "" || command.LeaseOwner == "" || command.LeaseToken == "" || command.LeaseFence == 0 {
		return fmt.Errorf("%w: incomplete lease receipt", event.ErrLeaseLost)
	}
	return nil
}

func requireLeaseUpdate(result sql.Result) error {
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return event.ErrLeaseLost
	}
	return nil
}

func sqliteReconciliation(cursor event.Cursor, floor uint64, gap event.GapReason, reset event.ResetReason) event.ReplayResult {
	return event.ReplayResult{
		Next: cursor, Gap: &event.Gap{ExpectedSequence: cursor.Sequence + 1, ReceivedSequence: floor, Reason: gap},
		Reset: &event.Reset{Reason: reset, SnapshotRequired: true, MinimumCursor: event.Cursor{TenantKey: cursor.TenantKey, StreamKey: cursor.StreamKey, Sequence: floor - 1}}, Reconcile: true,
	}
}

func sqliteReconciliationAt(partial event.ReplayResult, cursor event.Cursor, floor, missing, high uint64) event.ReplayResult {
	partial.Gap = &event.Gap{ExpectedSequence: missing, ReceivedSequence: high + 1, Reason: event.GapSequenceInvariant}
	partial.Reset = &event.Reset{Reason: event.ResetReconciliation, SnapshotRequired: true, MinimumCursor: event.Cursor{TenantKey: cursor.TenantKey, StreamKey: cursor.StreamKey, Sequence: floor - 1}}
	partial.Reconcile = true
	return partial
}

func contextErrorSQLite(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", event.ErrInvalidEnvelope)
	}
	return ctx.Err()
}

func truncateSQLite(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
