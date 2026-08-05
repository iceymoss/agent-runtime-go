package event

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

type streamIdentity struct {
	tenant agent.TenantKey
	stream string
}

type storedEvent struct {
	record OutboxRecord
	input  Envelope
}

// MemoryStore is a lock-linearized production reference implementation. It is
// intended for tests and single-process deployments, not durable persistence.
type MemoryStore struct {
	mu              sync.RWMutex
	now             func() time.Time
	maxPayloadBytes int
	events          map[string]*storedEvent
	sequences       map[streamIdentity]uint64
	streams         map[streamIdentity]map[uint64]string
	retentionFloor  map[streamIdentity]uint64
	nextLeaseToken  uint64
}

type MemoryStoreOption func(*MemoryStore)

func WithClock(clock func() time.Time) MemoryStoreOption {
	return func(store *MemoryStore) {
		if clock != nil {
			store.now = clock
		}
	}
}

func WithMaxPayloadBytes(limit int) MemoryStoreOption {
	return func(store *MemoryStore) { store.maxPayloadBytes = limit }
}

func NewMemoryStore(options ...MemoryStoreOption) *MemoryStore {
	store := &MemoryStore{
		now:            time.Now,
		events:         make(map[string]*storedEvent),
		sequences:      make(map[streamIdentity]uint64),
		streams:        make(map[streamIdentity]map[uint64]string),
		retentionFloor: make(map[streamIdentity]uint64),
	}
	for _, option := range options {
		if option != nil {
			option(store)
		}
	}
	return store
}

var _ Store = (*MemoryStore)(nil)

func (m *MemoryStore) Append(ctx context.Context, command AppendCommand) (Envelope, error) {
	events, err := m.AppendBatch(ctx, AppendBatchCommand{Events: []AppendCommand{command}})
	if err != nil {
		return Envelope{}, err
	}
	return events[0], nil
}

func (m *MemoryStore) AppendBatch(ctx context.Context, command AppendBatchCommand) ([]Envelope, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if len(command.Events) == 0 {
		return []Envelope{}, nil
	}
	inputs := make([]Envelope, len(command.Events))
	for index, item := range command.Events {
		inputs[index] = item.Envelope.Clone()
		if err := m.validateAppend(inputs[index]); err != nil {
			return nil, fmt.Errorf("append event %d: %w", index, err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	// Validate every idempotency decision against both committed and earlier
	// batch input before allocating any sequence.
	seen := make(map[string]Envelope, len(inputs))
	for _, input := range inputs {
		if previous, ok := seen[input.EventID]; ok && !sameImmutableInput(previous, input) {
			return nil, fmt.Errorf("%w: event id %q differs within batch", ErrIdempotencyConflict, input.EventID)
		}
		seen[input.EventID] = input
		if existing, ok := m.events[input.EventID]; ok && !sameImmutableInput(existing.input, input) {
			return nil, fmt.Errorf("%w: event id %q has different immutable input", ErrIdempotencyConflict, input.EventID)
		}
	}

	next := make(map[streamIdentity]uint64)
	created := make(map[string]Envelope)
	results := make([]Envelope, len(inputs))
	now := m.now().UTC()
	for index, input := range inputs {
		if existing, ok := m.events[input.EventID]; ok {
			results[index] = existing.record.Envelope.Clone()
			continue
		}
		if existing, ok := created[input.EventID]; ok {
			results[index] = existing.Clone()
			continue
		}
		identity := streamIdentity{tenant: input.TenantKey, stream: input.StreamKey}
		sequence, ok := next[identity]
		if !ok {
			sequence = m.sequences[identity]
		}
		sequence++
		next[identity] = sequence
		input.Sequence = sequence
		input.PersistedAt = now
		created[input.EventID] = input.Clone()
		results[index] = input.Clone()
	}

	for identity, sequence := range next {
		m.sequences[identity] = sequence
	}
	for eventID, envelope := range created {
		identity := streamIdentity{tenant: envelope.TenantKey, stream: envelope.StreamKey}
		if m.streams[identity] == nil {
			m.streams[identity] = make(map[uint64]string)
		}
		m.streams[identity][envelope.Sequence] = eventID
		m.events[eventID] = &storedEvent{
			input:  seen[eventID].Clone(),
			record: OutboxRecord{Envelope: envelope.Clone(), State: OutboxPending, NextAttempt: now},
		}
	}
	return cloneEnvelopes(results), nil
}

func (m *MemoryStore) Claim(ctx context.Context, command ClaimCommand) ([]ClaimedEvent, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if !command.TenantKey.Valid() || command.Owner == "" || command.Limit <= 0 || command.LeaseDuration <= 0 {
		return nil, fmt.Errorf("%w: tenant, owner, positive limit, and lease duration are required", ErrInvalidEnvelope)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	candidates := make([]*storedEvent, 0)
	for _, event := range m.events {
		record := &event.record
		if record.Envelope.TenantKey != command.TenantKey || record.State == OutboxDelivered || record.State == OutboxDead {
			continue
		}
		if record.State == OutboxLeased && record.LeaseExpires.After(now) {
			continue
		}
		if record.NextAttempt.After(now) {
			continue
		}
		candidates = append(candidates, event)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i].record, candidates[j].record
		if !left.NextAttempt.Equal(right.NextAttempt) {
			return left.NextAttempt.Before(right.NextAttempt)
		}
		if !left.Envelope.PersistedAt.Equal(right.Envelope.PersistedAt) {
			return left.Envelope.PersistedAt.Before(right.Envelope.PersistedAt)
		}
		return left.Envelope.EventID < right.Envelope.EventID
	})
	if len(candidates) > command.Limit {
		candidates = candidates[:command.Limit]
	}
	claimed := make([]ClaimedEvent, 0, len(candidates))
	for _, event := range candidates {
		record := &event.record
		m.nextLeaseToken++
		record.State = OutboxLeased
		record.Attempts++
		record.LeaseOwner = command.Owner
		record.LeaseFence++
		record.LeaseToken = fmt.Sprintf("lease-%d", m.nextLeaseToken)
		record.LeaseExpires = now.Add(command.LeaseDuration)
		claimed = append(claimed, ClaimedEvent{
			Envelope: record.Envelope.Clone(), Attempts: record.Attempts,
			LeaseOwner: record.LeaseOwner, LeaseToken: record.LeaseToken,
			LeaseFence: record.LeaseFence, ExpiresAt: record.LeaseExpires,
		})
	}
	return claimed, nil
}

func (m *MemoryStore) Ack(ctx context.Context, command AckCommand) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.activeLease(command, m.now().UTC())
	if err != nil {
		return err
	}
	record.State = OutboxDelivered
	record.DeliveredAt = m.now().UTC()
	clearLease(record)
	return nil
}

func (m *MemoryStore) Nack(ctx context.Context, command NackCommand) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if command.RetryAfter < 0 {
		return fmt.Errorf("%w: retry delay must not be negative", ErrInvalidEnvelope)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now().UTC()
	record, err := m.activeLease(command.AckCommand, now)
	if err != nil {
		return err
	}
	record.LastError = truncate(command.Reason, 1024)
	if command.Dead || command.MaxAttempts > 0 && record.Attempts >= command.MaxAttempts {
		record.State = OutboxDead
		record.DeadAt = now
	} else {
		record.State = OutboxPending
		record.NextAttempt = now.Add(command.RetryAfter)
	}
	clearLease(record)
	return nil
}

func (m *MemoryStore) Replay(ctx context.Context, query ReplayQuery) (ReplayResult, error) {
	if err := contextError(ctx); err != nil {
		return ReplayResult{}, err
	}
	cursor := query.Cursor
	if !cursor.TenantKey.Valid() || cursor.StreamKey == "" || query.Limit < 0 || cursor.Sequence == 0 && cursor.EventID != "" || cursor.Sequence > 0 && cursor.EventID == "" {
		return ReplayResult{}, fmt.Errorf("%w: invalid replay cursor", ErrInvalidStream)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	identity := streamIdentity{tenant: cursor.TenantKey, stream: cursor.StreamKey}
	stream := m.streams[identity]
	floor := m.retentionFloor[identity]
	if floor == 0 {
		floor = 1
	}
	if cursor.Sequence > 0 {
		if cursor.Sequence < floor {
			return reconciliation(cursor, floor, GapRetentionExpired, ResetCursorExpired), nil
		}
		if stream[cursor.Sequence] != cursor.EventID {
			return reconciliation(cursor, floor, GapSequenceInvariant, ResetCursorUnknown), nil
		}
	}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	result := ReplayResult{Next: cursor}
	high := m.sequences[identity]
	expected := cursor.Sequence + 1
	if expected < floor {
		expected = floor
	}
	for sequence := expected; sequence <= high && len(result.Events) < limit; sequence++ {
		eventID, ok := stream[sequence]
		if !ok {
			return reconciliationAt(result, cursor, floor, sequence, high), nil
		}
		envelope := m.events[eventID].record.Envelope.Clone()
		result.Events = append(result.Events, envelope)
		result.Next = Cursor{TenantKey: cursor.TenantKey, StreamKey: cursor.StreamKey, Sequence: sequence, EventID: eventID}
	}
	return result, nil
}

// SetRetentionFloor advances a stream's retained lower bound and removes older
// envelopes. It exists to exercise production retention/reconciliation logic.
func (m *MemoryStore) SetRetentionFloor(tenant agent.TenantKey, stream string, floor uint64) error {
	if !tenant.Valid() || stream == "" || floor == 0 {
		return fmt.Errorf("%w: invalid retention floor", ErrInvalidStream)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	identity := streamIdentity{tenant: tenant, stream: stream}
	if floor < m.retentionFloor[identity] {
		return fmt.Errorf("%w: retention floor cannot move backwards", ErrSequenceConflict)
	}
	for sequence, eventID := range m.streams[identity] {
		if sequence < floor {
			delete(m.streams[identity], sequence)
			delete(m.events, eventID)
		}
	}
	m.retentionFloor[identity] = floor
	return nil
}

func (m *MemoryStore) Get(ctx context.Context, tenant agent.TenantKey, eventID string) (OutboxRecord, error) {
	if err := contextError(ctx); err != nil {
		return OutboxRecord{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	event, ok := m.events[eventID]
	if !ok || event.record.Envelope.TenantKey != tenant {
		return OutboxRecord{}, ErrNotFound
	}
	return cloneRecord(event.record), nil
}

func (m *MemoryStore) activeLease(command AckCommand, now time.Time) (*OutboxRecord, error) {
	if !command.TenantKey.Valid() || command.EventID == "" || command.LeaseOwner == "" || command.LeaseToken == "" || command.LeaseFence == 0 {
		return nil, fmt.Errorf("%w: incomplete lease receipt", ErrLeaseLost)
	}
	event, ok := m.events[command.EventID]
	if !ok || event.record.Envelope.TenantKey != command.TenantKey {
		return nil, ErrLeaseLost
	}
	record := &event.record
	if record.State != OutboxLeased || record.LeaseOwner != command.LeaseOwner || record.LeaseToken != command.LeaseToken || record.LeaseFence != command.LeaseFence || !record.LeaseExpires.After(now) {
		return nil, ErrLeaseLost
	}
	return record, nil
}

func (m *MemoryStore) validateAppend(envelope Envelope) error {
	if !envelope.TenantKey.Valid() || envelope.EventID == "" || envelope.StreamKey == "" || envelope.Type == "" || envelope.SchemaVersion == 0 || envelope.AggregateType == "" || envelope.AggregateKey == "" || envelope.OccurredAt.IsZero() {
		return ErrInvalidEnvelope
	}
	if envelope.Sequence != 0 || !envelope.PersistedAt.IsZero() {
		return fmt.Errorf("%w: sequence and persisted time are store-owned", ErrInvalidEnvelope)
	}
	if envelope.OccurredAt.Location() != time.UTC {
		return fmt.Errorf("%w: occurred time must be UTC", ErrInvalidEnvelope)
	}
	if envelope.Reliability != ReliabilitySessionSnapshot && envelope.Reliability != ReliabilityTerminal && envelope.Reliability != ReliabilityDomain {
		return fmt.Errorf("%w: observation is not persisted", ErrInvalidEnvelope)
	}
	if m.maxPayloadBytes > 0 && len(envelope.Payload) > m.maxPayloadBytes {
		return ErrPayloadTooLarge
	}
	if !json.Valid(envelope.Payload) {
		return fmt.Errorf("%w: payload must be valid JSON", ErrInvalidEnvelope)
	}
	return nil
}

func sameImmutableInput(left, right Envelope) bool {
	return reflect.DeepEqual(left, right)
}

func clearLease(record *OutboxRecord) {
	record.LeaseOwner = ""
	record.LeaseToken = ""
	record.LeaseExpires = time.Time{}
}

func cloneRecord(record OutboxRecord) OutboxRecord {
	record.Envelope = record.Envelope.Clone()
	return record
}

func cloneEnvelopes(events []Envelope) []Envelope {
	cloned := make([]Envelope, len(events))
	for index, event := range events {
		cloned[index] = event.Clone()
	}
	return cloned
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func reconciliation(cursor Cursor, floor uint64, gap GapReason, reset ResetReason) ReplayResult {
	return ReplayResult{
		Next:      cursor,
		Gap:       &Gap{ExpectedSequence: cursor.Sequence + 1, ReceivedSequence: floor, Reason: gap},
		Reset:     &Reset{Reason: reset, SnapshotRequired: true, MinimumCursor: Cursor{TenantKey: cursor.TenantKey, StreamKey: cursor.StreamKey, Sequence: floor - 1}},
		Reconcile: true,
	}
}

func reconciliationAt(partial ReplayResult, cursor Cursor, floor, missing, high uint64) ReplayResult {
	partial.Gap = &Gap{ExpectedSequence: missing, ReceivedSequence: high + 1, Reason: GapSequenceInvariant}
	partial.Reset = &Reset{Reason: ResetReconciliation, SnapshotRequired: true, MinimumCursor: Cursor{TenantKey: cursor.TenantKey, StreamKey: cursor.StreamKey, Sequence: floor - 1}}
	partial.Reconcile = true
	return partial
}
