package agenttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/event"
)

// EventStoreFactory returns a fresh, empty event store driven by the supplied
// clock. The clock is a parameter because lease expiry and retry backoff are
// part of the contract, and wall-clock time cannot test them deterministically.
type EventStoreFactory func(t *testing.T, clock func() time.Time) event.Store

// TestEventStore runs the persisted event and outbox conformance suite.
//
// This is where at-least-once delivery is actually implemented, so its failure
// modes are the ones that are hardest to notice: a sequence allocated twice
// makes a replayed stream skip history, a lease that can be acknowledged by a
// worker that no longer holds it loses an event, and a retry schedule that is
// not honored turns a transient outage into a hot loop. The suite drives each of
// those rather than only the happy path.
func TestEventStore(t *testing.T, factory EventStoreFactory) {
	t.Helper()
	t.Run("appending allocates the sequence and is idempotent", func(t *testing.T) {
		testEventAppend(t, factory)
	})
	t.Run("an invalid envelope is refused before it is stored", func(t *testing.T) {
		testEventValidation(t, factory)
	})
	t.Run("a claim is exclusive until its lease expires", func(t *testing.T) {
		testEventClaim(t, factory)
	})
	t.Run("only the current lease may settle a delivery", func(t *testing.T) {
		testEventAckFencing(t, factory)
	})
	t.Run("a failed delivery is retried on schedule and can die", func(t *testing.T) {
		testEventNack(t, factory)
	})
	t.Run("replay never silently skips history", func(t *testing.T) {
		testEventReplay(t, factory)
	})
}

const (
	eventTenant = "tenant-1"
	eventStream = "session/session-1"
)

func eventEnvelope(eventID string, occurredAt time.Time) event.Envelope {
	return event.Envelope{
		TenantKey: eventTenant, EventID: eventID, StreamKey: eventStream,
		Type: "agent.run.completed", SchemaVersion: 1, Reliability: event.ReliabilityTerminal,
		AggregateType: "session", AggregateKey: "session-1", AggregateRevision: 1,
		CorrelationID: "run-1", CausationID: "run-1:started",
		OccurredAt: occurredAt, Payload: []byte(`{"outcome":"completed"}`),
	}
}

func appendEvent(t *testing.T, store event.Store, eventID string, occurredAt time.Time) event.Envelope {
	t.Helper()
	stored, err := store.Append(context.Background(), event.AppendCommand{Envelope: eventEnvelope(eventID, occurredAt)})
	if err != nil {
		t.Fatalf("Append(%q) error = %v", eventID, err)
	}
	return stored
}

func testEventAppend(t *testing.T, factory EventStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock.Now)
	ctx := context.Background()
	occurredAt := clock.now

	first := appendEvent(t, store, "event-1", occurredAt)
	if first.Sequence != 1 || first.PersistedAt.IsZero() {
		t.Fatalf("Append() = %+v; the store owns the sequence and persisted time", first)
	}
	second := appendEvent(t, store, "event-2", occurredAt)
	if second.Sequence != 2 {
		t.Fatalf("Append() sequence = %d, want 2", second.Sequence)
	}

	// A retried append is the same event. Allocating a second sequence for it
	// would make every later replay of the stream see a duplicate.
	replayed := appendEvent(t, store, "event-1", occurredAt)
	if replayed.Sequence != first.Sequence || !replayed.PersistedAt.Equal(first.PersistedAt) {
		t.Fatalf("replayed Append() = %+v, first = %+v", replayed, first)
	}
	conflicting := eventEnvelope("event-1", occurredAt)
	conflicting.Payload = []byte(`{"outcome":"failed"}`)
	if _, err := store.Append(ctx, event.AppendCommand{Envelope: conflicting}); !errors.Is(err, event.ErrIdempotencyConflict) {
		t.Fatalf("Append() with different content error = %v, want event.ErrIdempotencyConflict", err)
	}

	// Sequences are per stream, so one busy stream cannot advance another's.
	other := eventEnvelope("event-3", occurredAt)
	other.StreamKey = "session/session-2"
	stored, err := store.Append(ctx, event.AppendCommand{Envelope: other})
	if err != nil || stored.Sequence != 1 {
		t.Fatalf("Append() to a second stream = %+v, error %v", stored, err)
	}

	// A batch is atomic and ordered: it is how an aggregate publishes its events
	// inside its own transaction.
	batch, err := store.AppendBatch(ctx, event.AppendBatchCommand{Events: []event.AppendCommand{
		{Envelope: eventEnvelope("event-4", occurredAt)},
		{Envelope: eventEnvelope("event-5", occurredAt)},
	}})
	if err != nil || len(batch) != 2 || batch[0].Sequence != 3 || batch[1].Sequence != 4 {
		t.Fatalf("AppendBatch() = %+v, error %v", batch, err)
	}
	divergent := eventEnvelope("event-6", occurredAt)
	divergent.Payload = []byte(`{"outcome":"other"}`)
	if _, err := store.AppendBatch(ctx, event.AppendBatchCommand{Events: []event.AppendCommand{
		{Envelope: eventEnvelope("event-6", occurredAt)}, {Envelope: divergent},
	}}); !errors.Is(err, event.ErrIdempotencyConflict) {
		t.Fatalf("AppendBatch() with a divergent duplicate error = %v, want event.ErrIdempotencyConflict", err)
	}
	// The rejected batch must have stored nothing at all.
	result, err := store.Replay(ctx, event.ReplayQuery{Cursor: event.Cursor{TenantKey: eventTenant, StreamKey: eventStream}, Limit: 100})
	if err != nil || len(result.Events) != 4 {
		t.Fatalf("Replay() after a rejected batch = %d events, error %v", len(result.Events), err)
	}
}

func testEventValidation(t *testing.T, factory EventStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock.Now)
	ctx := context.Background()

	tests := []struct {
		name    string
		mutate  func(event.Envelope) event.Envelope
		wantErr error
	}{
		{
			name:    "a caller-assigned sequence",
			mutate:  func(e event.Envelope) event.Envelope { e.Sequence = 7; return e },
			wantErr: event.ErrInvalidEnvelope,
		},
		{
			name:    "a caller-assigned persisted time",
			mutate:  func(e event.Envelope) event.Envelope { e.PersistedAt = clock.now; return e },
			wantErr: event.ErrInvalidEnvelope,
		},
		{
			name:    "a missing stream",
			mutate:  func(e event.Envelope) event.Envelope { e.StreamKey = ""; return e },
			wantErr: event.ErrInvalidEnvelope,
		},
		{
			name:    "a missing schema version",
			mutate:  func(e event.Envelope) event.Envelope { e.SchemaVersion = 0; return e },
			wantErr: event.ErrInvalidEnvelope,
		},
		{
			name: "a non-UTC occurrence time",
			mutate: func(e event.Envelope) event.Envelope {
				e.OccurredAt = e.OccurredAt.In(time.FixedZone("x", 3600))
				return e
			},
			wantErr: event.ErrInvalidEnvelope,
		},
		{
			// Observations are lossy by design, so persisting one would imply a
			// guarantee the runtime never made.
			name:    "an observation reliability",
			mutate:  func(e event.Envelope) event.Envelope { e.Reliability = event.ReliabilityObservation; return e },
			wantErr: event.ErrInvalidEnvelope,
		},
		{
			name:    "a payload that is not JSON",
			mutate:  func(e event.Envelope) event.Envelope { e.Payload = []byte("not json"); return e },
			wantErr: event.ErrInvalidEnvelope,
		},
	}
	for index, test := range tests {
		t.Run(test.name+" is refused", func(t *testing.T) {
			envelope := test.mutate(eventEnvelope("event-invalid", clock.now))
			envelope.EventID = "event-invalid-" + string(rune('a'+index))
			if _, err := store.Append(ctx, event.AppendCommand{Envelope: envelope}); !errors.Is(err, test.wantErr) {
				t.Fatalf("Append() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func testEventClaim(t *testing.T, factory EventStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock.Now)
	ctx := context.Background()
	appendEvent(t, store, "event-1", clock.now)
	appendEvent(t, store, "event-2", clock.now)

	if _, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "", Limit: 1, LeaseDuration: time.Minute}); !errors.Is(err, event.ErrInvalidEnvelope) {
		t.Fatalf("Claim() without an owner error = %v, want event.ErrInvalidEnvelope", err)
	}
	claimed, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-1", Limit: 1, LeaseDuration: time.Minute})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim(limit 1) = %d, error %v", len(claimed), err)
	}
	receipt := claimed[0]
	if receipt.LeaseOwner != "worker-1" || receipt.LeaseToken == "" || receipt.LeaseFence == 0 || receipt.Attempts != 1 {
		t.Fatalf("Claim() receipt = %+v", receipt)
	}

	// A live lease is what keeps one event from being delivered twice at once.
	next, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-2", Limit: 10, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	for _, item := range next {
		if item.Envelope.EventID == receipt.Envelope.EventID {
			t.Fatal("Claim() handed out an event whose lease was still live")
		}
	}
	if other, err := store.Claim(ctx, event.ClaimCommand{TenantKey: "tenant-2", Owner: "worker-3", Limit: 10, LeaseDuration: time.Minute}); err != nil || len(other) != 0 {
		t.Fatalf("Claim() leaked across tenants: %d, error %v", len(other), err)
	}

	// A worker that died holds a lease nobody will release. Expiry is what makes
	// the event deliverable again, with a higher fence so the old worker cannot
	// still acknowledge it.
	clock.now = clock.now.Add(2 * time.Minute)
	reclaimed, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-3", Limit: 10, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatalf("Claim() after lease expiry error = %v", err)
	}
	var found bool
	for _, item := range reclaimed {
		if item.Envelope.EventID != receipt.Envelope.EventID {
			continue
		}
		found = true
		if item.LeaseFence <= receipt.LeaseFence {
			t.Fatalf("re-claim did not raise the fence: %d then %d", receipt.LeaseFence, item.LeaseFence)
		}
		if item.Attempts != 2 {
			t.Fatalf("re-claim attempts = %d, want 2", item.Attempts)
		}
	}
	if !found {
		t.Fatal("an expired lease was never offered to another worker")
	}
}

func testEventAckFencing(t *testing.T, factory EventStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock.Now)
	ctx := context.Background()
	appendEvent(t, store, "event-1", clock.now)
	claimed, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-1", Limit: 1, LeaseDuration: time.Minute})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %d, error %v", len(claimed), err)
	}
	receipt := claimed[0]
	ack := event.AckCommand{
		TenantKey: eventTenant, EventID: receipt.Envelope.EventID,
		LeaseOwner: receipt.LeaseOwner, LeaseToken: receipt.LeaseToken, LeaseFence: receipt.LeaseFence,
	}

	// Every part of the receipt is load-bearing: a worker that no longer holds
	// the lease must not be able to mark the event delivered.
	tests := []struct {
		name   string
		mutate func(event.AckCommand) event.AckCommand
	}{
		{name: "a foreign owner", mutate: func(c event.AckCommand) event.AckCommand { c.LeaseOwner = "worker-2"; return c }},
		{name: "a stale token", mutate: func(c event.AckCommand) event.AckCommand { c.LeaseToken = "lease-stale"; return c }},
		{name: "a stale fence", mutate: func(c event.AckCommand) event.AckCommand { c.LeaseFence++; return c }},
		{name: "an unknown event", mutate: func(c event.AckCommand) event.AckCommand { c.EventID = "absent"; return c }},
		{name: "an incomplete receipt", mutate: func(c event.AckCommand) event.AckCommand { c.LeaseToken = ""; return c }},
	}
	for _, test := range tests {
		t.Run(test.name+" cannot acknowledge", func(t *testing.T) {
			if err := store.Ack(ctx, test.mutate(ack)); !errors.Is(err, event.ErrLeaseLost) {
				t.Fatalf("Ack() error = %v, want event.ErrLeaseLost", err)
			}
		})
	}

	if err := store.Ack(ctx, ack); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	// A delivered event is finished: acknowledging again is a stale worker, and
	// it must never be offered for delivery a second time.
	if err := store.Ack(ctx, ack); !errors.Is(err, event.ErrLeaseLost) {
		t.Fatalf("replayed Ack() error = %v, want event.ErrLeaseLost", err)
	}
	clock.now = clock.now.Add(2 * time.Minute)
	if again, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-2", Limit: 10, LeaseDuration: time.Minute}); err != nil || len(again) != 0 {
		t.Fatalf("Claim() re-offered a delivered event: %d, error %v", len(again), err)
	}
}

func testEventNack(t *testing.T, factory EventStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock.Now)
	ctx := context.Background()
	appendEvent(t, store, "event-1", clock.now)

	claimed, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-1", Limit: 1, LeaseDuration: time.Minute})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %d, error %v", len(claimed), err)
	}
	receipt := claimed[0]
	nack := event.NackCommand{
		AckCommand: event.AckCommand{
			TenantKey: eventTenant, EventID: receipt.Envelope.EventID,
			LeaseOwner: receipt.LeaseOwner, LeaseToken: receipt.LeaseToken, LeaseFence: receipt.LeaseFence,
		},
		RetryAfter: 5 * time.Minute, Reason: "upstream unavailable",
	}
	if err := store.Nack(ctx, event.NackCommand{AckCommand: nack.AckCommand, RetryAfter: -time.Second}); !errors.Is(err, event.ErrInvalidEnvelope) {
		t.Fatalf("Nack() with a negative delay error = %v, want event.ErrInvalidEnvelope", err)
	}
	if err := store.Nack(ctx, nack); err != nil {
		t.Fatalf("Nack() error = %v", err)
	}

	// Honoring the retry delay is what turns a transient outage into backoff
	// rather than a hot loop.
	clock.now = clock.now.Add(time.Minute)
	if early, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-2", Limit: 10, LeaseDuration: time.Minute}); err != nil || len(early) != 0 {
		t.Fatalf("Claim() ignored the retry delay: %d, error %v", len(early), err)
	}
	clock.now = clock.now.Add(5 * time.Minute)
	retried, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-2", Limit: 10, LeaseDuration: time.Minute})
	if err != nil || len(retried) != 1 || retried[0].Attempts != 2 {
		t.Fatalf("Claim() after the delay = %+v, error %v", retried, err)
	}

	// An event that has exhausted its attempts stops being retried instead of
	// blocking the outbox forever.
	dead := event.NackCommand{
		AckCommand: event.AckCommand{
			TenantKey: eventTenant, EventID: retried[0].Envelope.EventID,
			LeaseOwner: retried[0].LeaseOwner, LeaseToken: retried[0].LeaseToken, LeaseFence: retried[0].LeaseFence,
		},
		MaxAttempts: 2, Reason: "permanent",
	}
	if err := store.Nack(ctx, dead); err != nil {
		t.Fatalf("Nack(max attempts) error = %v", err)
	}
	clock.now = clock.now.Add(time.Hour)
	if again, err := store.Claim(ctx, event.ClaimCommand{TenantKey: eventTenant, Owner: "worker-3", Limit: 10, LeaseDuration: time.Minute}); err != nil || len(again) != 0 {
		t.Fatalf("Claim() re-offered a dead event: %d, error %v", len(again), err)
	}
}

func testEventReplay(t *testing.T, factory EventStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock.Now)
	ctx := context.Background()
	first := appendEvent(t, store, "event-1", clock.now)
	second := appendEvent(t, store, "event-2", clock.now)
	appendEvent(t, store, "event-3", clock.now)

	page, err := store.Replay(ctx, event.ReplayQuery{Cursor: event.Cursor{TenantKey: eventTenant, StreamKey: eventStream}, Limit: 2})
	if err != nil || len(page.Events) != 2 || page.Reconcile {
		t.Fatalf("Replay() = %+v, error %v", page, err)
	}
	if page.Events[0].Sequence != 1 || page.Events[1].Sequence != 2 {
		t.Fatalf("Replay() is not in sequence order: %+v", page.Events)
	}
	if page.Next.Sequence != second.Sequence || page.Next.EventID != second.EventID {
		t.Fatalf("Replay() next cursor = %+v", page.Next)
	}
	tail, err := store.Replay(ctx, event.ReplayQuery{Cursor: page.Next, Limit: 100})
	if err != nil || len(tail.Events) != 1 || tail.Events[0].Sequence != 3 {
		t.Fatalf("Replay(next) = %+v, error %v", tail, err)
	}

	// A cursor whose event ID does not match its sequence describes a stream this
	// store never produced. Silently returning the "closest" history is how a
	// consumer ends up with a gap it cannot see, so the store demands
	// reconciliation instead.
	forged := event.Cursor{TenantKey: eventTenant, StreamKey: eventStream, Sequence: first.Sequence, EventID: "event-forged"}
	reconcile, err := store.Replay(ctx, event.ReplayQuery{Cursor: forged, Limit: 100})
	if err != nil {
		t.Fatalf("Replay(forged cursor) error = %v", err)
	}
	if !reconcile.Reconcile || reconcile.Reset == nil || reconcile.Reset.Reason != event.ResetCursorUnknown {
		t.Fatalf("Replay(forged cursor) = %+v; an unverifiable cursor must demand reconciliation", reconcile)
	}
	if len(reconcile.Events) != 0 {
		t.Fatalf("Replay(forged cursor) returned %d events alongside a reset", len(reconcile.Events))
	}

	invalid := []event.Cursor{
		{TenantKey: eventTenant},
		{TenantKey: eventTenant, StreamKey: eventStream, EventID: "event-1"},
		{TenantKey: eventTenant, StreamKey: eventStream, Sequence: 1},
	}
	for index, cursor := range invalid {
		t.Run("an incoherent cursor is refused", func(t *testing.T) {
			if _, err := store.Replay(ctx, event.ReplayQuery{Cursor: cursor, Limit: 10}); !errors.Is(err, event.ErrInvalidStream) {
				t.Fatalf("Replay(cursor %d) error = %v, want event.ErrInvalidStream", index, err)
			}
		})
	}
	if empty, err := store.Replay(ctx, event.ReplayQuery{Cursor: event.Cursor{TenantKey: eventTenant, StreamKey: "session/absent"}, Limit: 10}); err != nil || len(empty.Events) != 0 {
		t.Fatalf("Replay(unknown stream) = %+v, error %v", empty, err)
	}
}
