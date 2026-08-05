package event_test

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
)

func TestMemoryStoreConcurrentSequenceAndTenantIsolation(t *testing.T) {
	store := event.NewMemoryStore()
	const count = 64
	sequences := make(chan uint64, count)
	errorsSeen := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			envelope, err := store.Append(context.Background(), event.AppendCommand{Envelope: testEnvelope(agent.TenantKey("tenant-a"), "stream", eventID(index))})
			if err != nil {
				errorsSeen <- err
				return
			}
			sequences <- envelope.Sequence
		}(index)
	}
	wait.Wait()
	close(errorsSeen)
	close(sequences)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	got := make([]int, 0, count)
	for sequence := range sequences {
		got = append(got, int(sequence))
	}
	sort.Ints(got)
	for index, sequence := range got {
		if sequence != index+1 {
			t.Fatalf("sequence[%d] = %d, want %d", index, sequence, index+1)
		}
	}

	other, err := store.Append(context.Background(), event.AppendCommand{Envelope: testEnvelope(agent.TenantKey("tenant-b"), "stream", "other")})
	if err != nil {
		t.Fatal(err)
	}
	if other.Sequence != 1 {
		t.Fatalf("other tenant sequence = %d, want 1", other.Sequence)
	}
	if _, err := store.Get(context.Background(), agent.TenantKey("tenant-b"), eventID(0)); !errors.Is(err, event.ErrNotFound) {
		t.Fatalf("cross-tenant get error = %v, want ErrNotFound", err)
	}
}

func TestMemoryStoreIdempotencyBatchRollbackAndClone(t *testing.T) {
	store := event.NewMemoryStore()
	firstInput := testEnvelope(agent.TenantKey("tenant"), "stream", "event-1")
	firstInput.Payload = []byte(`"one"`)
	first, err := store.Append(context.Background(), event.AppendCommand{Envelope: firstInput})
	if err != nil {
		t.Fatal(err)
	}
	first.Payload[0] = 'x'
	firstInput.Payload[0] = 'y'

	retryInput := testEnvelope(agent.TenantKey("tenant"), "stream", "event-1")
	retryInput.Payload = []byte(`"one"`)
	retry, err := store.Append(context.Background(), event.AppendCommand{Envelope: retryInput})
	if err != nil {
		t.Fatal(err)
	}
	if string(retry.Payload) != `"one"` || retry.Sequence != 1 {
		t.Fatalf("idempotent retry = %#v", retry)
	}

	conflict := retryInput
	conflict.Payload = []byte(`"different"`)
	_, err = store.AppendBatch(context.Background(), event.AppendBatchCommand{Events: []event.AppendCommand{
		{Envelope: testEnvelope(agent.TenantKey("tenant"), "stream", "event-2")},
		{Envelope: conflict},
	}})
	if !errors.Is(err, event.ErrIdempotencyConflict) {
		t.Fatalf("batch error = %v, want ErrIdempotencyConflict", err)
	}
	second, err := store.Append(context.Background(), event.AppendCommand{Envelope: testEnvelope(agent.TenantKey("tenant"), "stream", "event-2")})
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 {
		t.Fatalf("sequence after rollback = %d, want 2", second.Sequence)
	}
}

func TestMemoryStoreLeaseExpiryAndStaleAck(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	store := event.NewMemoryStore(event.WithClock(func() time.Time { return now }))
	if _, err := store.Append(context.Background(), event.AppendCommand{Envelope: testEnvelope(agent.TenantKey("tenant"), "stream", "event")}); err != nil {
		t.Fatal(err)
	}
	first := claimOne(t, store, "owner-a", time.Minute)
	now = now.Add(2 * time.Minute)
	second := claimOne(t, store, "owner-b", time.Minute)
	if second.LeaseFence <= first.LeaseFence || second.LeaseToken == first.LeaseToken {
		t.Fatalf("reclaim did not advance lease: first=%#v second=%#v", first, second)
	}
	if err := store.Ack(context.Background(), receipt(first)); !errors.Is(err, event.ErrLeaseLost) {
		t.Fatalf("stale ack error = %v, want ErrLeaseLost", err)
	}
	if err := store.Ack(context.Background(), receipt(second)); err != nil {
		t.Fatal(err)
	}
	record, err := store.Get(context.Background(), agent.TenantKey("tenant"), "event")
	if err != nil {
		t.Fatal(err)
	}
	if record.State != event.OutboxDelivered || record.Attempts != 2 {
		t.Fatalf("record = %#v", record)
	}
}

func TestInboxDuplicateDeliveryAndRollback(t *testing.T) {
	inbox := event.NewInbox()
	envelope := testEnvelope(agent.TenantKey("tenant"), "stream", "event")
	var calls atomic.Int32
	handler := func(context.Context, event.Envelope) error {
		calls.Add(1)
		return nil
	}
	first, err := inbox.Consume(context.Background(), "consumer", envelope, handler)
	if err != nil || !first.Applied {
		t.Fatalf("first consume = %#v, %v", first, err)
	}
	second, err := inbox.Consume(context.Background(), "consumer", envelope, handler)
	if err != nil || !second.Duplicate || calls.Load() != 1 {
		t.Fatalf("duplicate consume = %#v, %v, calls %d", second, err, calls.Load())
	}

	failure := errors.New("failure")
	if _, err := inbox.Consume(context.Background(), "other", envelope, func(context.Context, event.Envelope) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("handler error = %v", err)
	}
	result, err := inbox.Consume(context.Background(), "other", envelope, handler)
	if err != nil || !result.Applied {
		t.Fatalf("retry after rollback = %#v, %v", result, err)
	}
}

func TestReplayGapResetAndDeepClone(t *testing.T) {
	store := event.NewMemoryStore()
	for index := 1; index <= 3; index++ {
		if _, err := store.Append(context.Background(), event.AppendCommand{Envelope: testEnvelope(agent.TenantKey("tenant"), "stream", eventID(index))}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.Replay(context.Background(), event.ReplayQuery{Cursor: event.Cursor{TenantKey: agent.TenantKey("tenant"), StreamKey: "stream"}})
	if err != nil || len(first.Events) != 3 {
		t.Fatalf("initial replay events = %d, err = %v", len(first.Events), err)
	}
	first.Events[0].Payload[0] = 'x'
	again, err := store.Replay(context.Background(), event.ReplayQuery{Cursor: event.Cursor{TenantKey: agent.TenantKey("tenant"), StreamKey: "stream"}})
	if err != nil || string(again.Events[0].Payload) != "{}" {
		t.Fatalf("replay clone payload = %q, err = %v", again.Events[0].Payload, err)
	}
	if err := store.SetRetentionFloor(agent.TenantKey("tenant"), "stream", 3); err != nil {
		t.Fatal(err)
	}
	expired, err := store.Replay(context.Background(), event.ReplayQuery{Cursor: event.Cursor{TenantKey: agent.TenantKey("tenant"), StreamKey: "stream", Sequence: 1, EventID: eventID(1)}})
	if err != nil || !expired.Reconcile || expired.Gap == nil || expired.Reset == nil || expired.Reset.Reason != event.ResetCursorExpired {
		t.Fatalf("expired replay = %#v, %v", expired, err)
	}
	unknown, err := store.Replay(context.Background(), event.ReplayQuery{Cursor: event.Cursor{TenantKey: agent.TenantKey("tenant"), StreamKey: "stream", Sequence: 3, EventID: "wrong"}})
	if err != nil || !unknown.Reconcile || unknown.Reset == nil || unknown.Reset.Reason != event.ResetCursorUnknown {
		t.Fatalf("unknown replay = %#v, %v", unknown, err)
	}
}

func TestBusNonblockingAndClone(t *testing.T) {
	bus := event.NewBus(1)
	envelope := testEnvelope(agent.TenantKey("tenant"), "stream", "observation")
	envelope.Reliability = event.ReliabilityObservation
	if !bus.TryPublish(envelope) {
		t.Fatal("first publish rejected")
	}
	if bus.TryPublish(envelope) {
		t.Fatal("full bus accepted publish")
	}
	envelope.Payload[0] = 'x'
	if got := <-bus.Events(); string(got.Payload) != "{}" {
		t.Fatalf("bus retained caller payload: %q", got.Payload)
	}
	bus.Close()
	if bus.TryPublish(envelope) {
		t.Fatal("closed bus accepted publish")
	}
}

func testEnvelope(tenant agent.TenantKey, stream, id string) event.Envelope {
	return event.Envelope{
		TenantKey: tenant, EventID: id, StreamKey: stream, Type: "session.updated",
		SchemaVersion: 1, Reliability: event.ReliabilitySessionSnapshot,
		AggregateType: "session", AggregateKey: "session-1", AggregateRevision: 1,
		OccurredAt: time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC), Payload: []byte("{}"),
	}
}

func eventID(index int) string {
	return "event-" + strconv.Itoa(index)
}

func claimOne(t *testing.T, store *event.MemoryStore, owner string, duration time.Duration) event.ClaimedEvent {
	t.Helper()
	claimed, err := store.Claim(context.Background(), event.ClaimCommand{TenantKey: agent.TenantKey("tenant"), Owner: owner, Limit: 1, LeaseDuration: duration})
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d events, want 1", len(claimed))
	}
	return claimed[0]
}

func receipt(claim event.ClaimedEvent) event.AckCommand {
	return event.AckCommand{
		TenantKey: claim.Envelope.TenantKey, EventID: claim.Envelope.EventID,
		LeaseOwner: claim.LeaseOwner, LeaseToken: claim.LeaseToken, LeaseFence: claim.LeaseFence,
	}
}
