package icoder

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
)

func TestSQLiteEventStoreIdempotencyAndBatchRollback(t *testing.T) {
	store, eventStore := openSQLiteEventStore(t)
	ctx := context.Background()

	first, err := eventStore.Append(ctx, sqliteTestAppend("shared", "tenant-a", "stream-a", "one"))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := eventStore.Append(ctx, sqliteTestAppend("shared", "tenant-a", "stream-a", "one"))
	if err != nil || repeated.Sequence != first.Sequence || !repeated.PersistedAt.Equal(first.PersistedAt) {
		t.Fatalf("idempotent Append() = %#v, %v; want original %#v", repeated, err, first)
	}
	conflict := sqliteTestAppend("shared", "tenant-b", "stream-b", "other")
	if _, err := eventStore.Append(ctx, conflict); !errors.Is(err, event.ErrIdempotencyConflict) {
		t.Fatalf("global EventID conflict error = %v", err)
	}

	batch := event.AppendBatchCommand{Events: []event.AppendCommand{
		sqliteTestAppend("batch-new", "tenant-a", "stream-a", "new"),
		sqliteTestAppend("shared", "tenant-a", "stream-a", "changed"),
	}}
	if _, err := eventStore.AppendBatch(ctx, batch); !errors.Is(err, event.ErrIdempotencyConflict) {
		t.Fatalf("AppendBatch() conflict error = %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM event_records WHERE event_id = 'batch-new'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back event count = %d, %v", count, err)
	}
}

func TestSQLiteEventStoreBatchSequenceAndTransaction(t *testing.T) {
	store, eventStore := openSQLiteEventStore(t)
	ctx := context.Background()
	command := event.AppendBatchCommand{Events: []event.AppendCommand{
		sqliteTestAppend("a-1", "tenant-a", "a", "one"),
		sqliteTestAppend("b-1", "tenant-a", "b", "two"),
		sqliteTestAppend("a-2", "tenant-a", "a", "three"),
		sqliteTestAppend("a-2", "tenant-a", "a", "three"),
	}}
	appended, err := eventStore.AppendBatch(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint64{1, 1, 2, 2}
	for i := range appended {
		if appended[i].Sequence != want[i] {
			t.Fatalf("sequence[%d] = %d, want %d", i, appended[i].Sequence, want[i])
		}
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	inside, err := eventStore.AppendBatchTx(ctx, tx, event.AppendBatchCommand{Events: []event.AppendCommand{sqliteTestAppend("tx", "tenant-a", "a", "tx")}})
	if err != nil || inside[0].Sequence != 3 {
		t.Fatalf("AppendBatchTx() = %#v, %v", inside, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	after, err := eventStore.Append(ctx, sqliteTestAppend("after", "tenant-a", "a", "after"))
	if err != nil || after.Sequence != 3 {
		t.Fatalf("Append() after rollback = %#v, %v", after, err)
	}
}

func TestSQLiteEventStoreClaimReclaimAndStaleAck(t *testing.T) {
	_, eventStore := openSQLiteEventStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	eventStore.now = func() time.Time { return now }
	if _, err := eventStore.Append(ctx, sqliteTestAppend("claim", "tenant", "stream", "claim")); err != nil {
		t.Fatal(err)
	}
	first := claimSQLiteEvents(t, eventStore, "tenant", "owner-1", time.Minute)
	if first.Attempts != 1 || first.LeaseFence != 1 || first.LeaseToken == "" {
		t.Fatalf("first claim = %#v", first)
	}
	if claims, err := eventStore.Claim(ctx, event.ClaimCommand{TenantKey: "tenant", Owner: "owner-2", Limit: 1, LeaseDuration: time.Minute}); err != nil || len(claims) != 0 {
		t.Fatalf("claim during lease = %#v, %v", claims, err)
	}
	now = now.Add(time.Minute)
	second := claimSQLiteEvents(t, eventStore, "tenant", "owner-2", time.Minute)
	if second.Attempts != 2 || second.LeaseFence != 2 || second.LeaseToken == first.LeaseToken {
		t.Fatalf("reclaim = %#v; first = %#v", second, first)
	}
	if err := eventStore.Ack(ctx, sqliteAck(first)); !errors.Is(err, event.ErrLeaseLost) {
		t.Fatalf("stale Ack() error = %v", err)
	}
	if err := eventStore.Ack(ctx, sqliteAck(second)); err != nil {
		t.Fatalf("active Ack() error = %v", err)
	}
	if claims, err := eventStore.Claim(ctx, event.ClaimCommand{TenantKey: "tenant", Owner: "owner-3", Limit: 1, LeaseDuration: time.Minute}); err != nil || len(claims) != 0 {
		t.Fatalf("claim delivered event = %#v, %v", claims, err)
	}
}

func TestSQLiteEventStoreNackRetryAndDead(t *testing.T) {
	store, eventStore := openSQLiteEventStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	eventStore.now = func() time.Time { return now }
	if _, err := eventStore.Append(ctx, sqliteTestAppend("nack", "tenant", "stream", "nack")); err != nil {
		t.Fatal(err)
	}
	first := claimSQLiteEvents(t, eventStore, "tenant", "owner", time.Minute)
	if err := eventStore.Nack(ctx, event.NackCommand{AckCommand: sqliteAck(first), RetryAfter: time.Minute, MaxAttempts: 2, Reason: "retry"}); err != nil {
		t.Fatal(err)
	}
	if claims, err := eventStore.Claim(ctx, event.ClaimCommand{TenantKey: "tenant", Owner: "owner", Limit: 1, LeaseDuration: time.Minute}); err != nil || len(claims) != 0 {
		t.Fatalf("early retry claim = %#v, %v", claims, err)
	}
	now = now.Add(time.Minute)
	second := claimSQLiteEvents(t, eventStore, "tenant", "owner", time.Minute)
	if second.Attempts != 2 {
		t.Fatalf("retry attempts = %d", second.Attempts)
	}
	if err := eventStore.Nack(ctx, event.NackCommand{AckCommand: sqliteAck(second), MaxAttempts: 2, Reason: "permanent"}); err != nil {
		t.Fatal(err)
	}
	var state event.OutboxState
	var deadAt int64
	var reason string
	if err := store.db.QueryRow(`SELECT outbox_state, dead_at, last_error FROM event_records WHERE event_id = 'nack'`).Scan(&state, &deadAt, &reason); err != nil {
		t.Fatal(err)
	}
	if state != event.OutboxDead || deadAt != now.UnixNano() || reason != "permanent" {
		t.Fatalf("dead state = %q, %d, %q", state, deadAt, reason)
	}
}

func TestSQLiteEventStoreReplayCursorGapAndRetention(t *testing.T) {
	store, eventStore := openSQLiteEventStore(t)
	ctx := context.Background()
	for i, id := range []string{"one", "two", "three"} {
		if _, err := eventStore.Append(ctx, sqliteTestAppend(id, "tenant", "stream", id)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	result, err := eventStore.Replay(ctx, event.ReplayQuery{Cursor: event.Cursor{TenantKey: "tenant", StreamKey: "stream"}, Limit: 2})
	if err != nil || len(result.Events) != 2 || result.Next.Sequence != 2 || result.Next.EventID != "two" || result.Reconcile {
		t.Fatalf("Replay() = %#v, %v", result, err)
	}
	mismatch, err := eventStore.Replay(ctx, event.ReplayQuery{Cursor: event.Cursor{TenantKey: "tenant", StreamKey: "stream", Sequence: 2, EventID: "wrong"}})
	if err != nil || !mismatch.Reconcile || mismatch.Gap == nil || mismatch.Gap.Reason != event.GapSequenceInvariant || mismatch.Reset == nil || mismatch.Reset.Reason != event.ResetCursorUnknown {
		t.Fatalf("cursor mismatch = %#v, %v", mismatch, err)
	}
	if _, err := store.db.Exec(`DELETE FROM event_records WHERE event_id = 'two'`); err != nil {
		t.Fatal(err)
	}
	gap, err := eventStore.Replay(ctx, event.ReplayQuery{Cursor: event.Cursor{TenantKey: "tenant", StreamKey: "stream"}})
	if err != nil || !gap.Reconcile || len(gap.Events) != 1 || gap.Gap == nil || gap.Gap.ExpectedSequence != 2 || gap.Gap.ReceivedSequence != 4 || gap.Reset == nil || gap.Reset.Reason != event.ResetReconciliation {
		t.Fatalf("sequence gap = %#v, %v", gap, err)
	}
}

func TestSQLiteEventStoreRetentionFloor(t *testing.T) {
	_, eventStore := openSQLiteEventStore(t)
	ctx := context.Background()
	for _, id := range []string{"one", "two", "three"} {
		if _, err := eventStore.Append(ctx, sqliteTestAppend(id, "tenant", "stream", id)); err != nil {
			t.Fatal(err)
		}
	}
	if err := eventStore.SetRetentionFloor(ctx, "tenant", "stream", 3); err != nil {
		t.Fatal(err)
	}
	result, err := eventStore.Replay(ctx, event.ReplayQuery{Cursor: event.Cursor{TenantKey: "tenant", StreamKey: "stream", Sequence: 1, EventID: "one"}})
	if err != nil || !result.Reconcile || result.Gap == nil || result.Gap.Reason != event.GapRetentionExpired || result.Reset == nil || result.Reset.Reason != event.ResetCursorExpired || result.Reset.MinimumCursor.Sequence != 2 {
		t.Fatalf("retention replay = %#v, %v", result, err)
	}
}

func TestSQLiteEventStoreRetentionPreservesPendingOutbox(t *testing.T) {
	store, eventStore := openSQLiteEventStore(t)
	ctx := context.Background()
	if _, err := eventStore.Append(ctx, sqliteTestAppend("pending", "tenant", "stream", "pending")); err != nil {
		t.Fatal(err)
	}
	if err := eventStore.SetRetentionFloor(ctx, "tenant", "stream", 2); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM event_records WHERE event_id = 'pending'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pending event count = %d, %v", count, err)
	}
	claimed, err := eventStore.Claim(ctx, event.ClaimCommand{TenantKey: "tenant", Owner: "worker", Limit: 1, LeaseDuration: time.Minute})
	if err != nil || len(claimed) != 1 || claimed[0].Envelope.EventID != "pending" {
		t.Fatalf("Claim() = %#v, %v", claimed, err)
	}
}

func openSQLiteEventStore(t *testing.T) (*Store, *SQLiteEventStore) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store, NewSQLiteEventStore(store.db)
}

func sqliteTestAppend(id string, tenant agent.TenantKey, stream, value string) event.AppendCommand {
	return event.AppendCommand{Envelope: event.Envelope{
		TenantKey: tenant, EventID: id, StreamKey: stream, Type: "test.event", SchemaVersion: 1,
		Reliability: event.ReliabilityDomain, AggregateType: "test", AggregateKey: stream,
		OccurredAt: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC), Payload: []byte(`{"value":"` + value + `"}`),
	}}
}

func claimSQLiteEvents(t *testing.T, store *SQLiteEventStore, tenant agent.TenantKey, owner string, lease time.Duration) event.ClaimedEvent {
	t.Helper()
	claimed, err := store.Claim(context.Background(), event.ClaimCommand{TenantKey: tenant, Owner: owner, Limit: 1, LeaseDuration: lease})
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %#v, %v", claimed, err)
	}
	return claimed[0]
}

func sqliteAck(claimed event.ClaimedEvent) event.AckCommand {
	return event.AckCommand{TenantKey: claimed.Envelope.TenantKey, EventID: claimed.Envelope.EventID, LeaseOwner: claimed.LeaseOwner, LeaseToken: claimed.LeaseToken, LeaseFence: claimed.LeaseFence}
}
