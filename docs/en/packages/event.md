# event

The `event` package defines portable Agent event contracts: reliable event appends, outbox delivery, consumer idempotency, and stream replay, plus an independent best-effort observation bus. Database transactions and transport adapters intentionally remain outside the package.

## What it is

The core data type is `Envelope`, a canonical immutable event representation carrying the tenant, event ID, stream key (`StreamKey`), a monotonically increasing storage-assigned `Sequence`, aggregate information, and a JSON `Payload`. Each event declares its reliability level through `Reliability`: `ReliabilitySessionSnapshot`, `ReliabilityTerminal`, and `ReliabilityDomain` are persisted, while `ReliabilityObservation` uses only the best-effort channel and is never persisted.

```go
type Store interface {
    Append(context.Context, AppendCommand) (Envelope, error)
    AppendBatch(context.Context, AppendBatchCommand) ([]Envelope, error)
    Claim(context.Context, ClaimCommand) ([]ClaimedEvent, error)
    Ack(context.Context, AckCommand) error
    Nack(context.Context, NackCommand) error
    Replay(context.Context, ReplayQuery) (ReplayResult, error)
}

type Publisher interface {
    Publish(context.Context, Envelope) error
}
```

`Store` serves as both the event log and outbox: `Append` adds an event and marks it pending delivery; `Claim` / `Ack` / `Nack` form a lease-based delivery protocol; and `Replay` reads a stream's history in cursor order without silently skipping missing ranges. If it detects expired retention or a sequence gap, it returns `Gap` / `Reset` with `Reconcile: true`, requiring the caller to fetch an authoritative snapshot before continuing.

Three supporting components surround `Store`: `Dispatcher` performs one bounded poll (starting no goroutines), passes claimed events to `Publisher`, and either Acks them or Nacks them with exponential backoff; `Inbox` provides in-process consumer idempotency by event ID; and `Bus` is a bounded observation queue completely separate from `Store`, nonblocking and dropping events when full.

The package provides `NewMemoryStore` as an in-memory `Store` reference implementation suitable for tests and single-process deployments. Production systems should implement `Store` with a database, usually joining `AppendBatch` to the owning aggregate's transaction in the classic outbox pattern.

## Why you need it

Without this package, you must implement atomicity between event appends and business writes, idempotent appends and conflict detection by event ID, lease exclusion among delivery processes (preventing two workers from delivering the same event and preventing an expired lease's Ack from overwriting a new lease), retry backoff and dead lettering, consumer deduplication, and gap detection during replay. These are the most tedious parts of outbox and event-sourcing infrastructure.

When you do not need it: in a single-process application where events are only used for logging or debugging and reliable delivery is unnecessary, `Bus` (or simply a logging library) is sufficient. You do not need `Store` and `Dispatcher`.

## How to use it

Append a domain event, then use `Dispatcher` for one delivery cycle:

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go/event"
)

type printPublisher struct{}

func (printPublisher) Publish(_ context.Context, envelope event.Envelope) error {
	fmt.Printf("deliver %s type=%s payload=%s\n", envelope.EventID, envelope.Type, envelope.Payload)
	return nil
}

func main() {
	ctx := context.Background()
	store := event.NewMemoryStore()

	stored, err := store.Append(ctx, event.AppendCommand{Envelope: event.Envelope{
		TenantKey:         "tenant-1",
		EventID:           "event-1",
		StreamKey:         "session-1",
		Type:              "session.updated",
		SchemaVersion:     1,
		Reliability:       event.ReliabilityDomain,
		AggregateType:     "session",
		AggregateKey:      "session-1",
		AggregateRevision: 1,
		OccurredAt:        time.Now().UTC(),
		Payload:           []byte(`{"status":"ok"}`),
	}})
	if err != nil {
		panic(err)
	}
	fmt.Println("sequence:", stored.Sequence) // 1，由存储分配

	dispatcher, err := event.NewDispatcher(store, printPublisher{}, event.DispatcherConfig{
		TenantKey:     "tenant-1",
		Owner:         "worker-1",
		BatchSize:     16,
		LeaseDuration: time.Minute,
		BaseBackoff:   time.Second,
		MaxBackoff:    time.Minute,
		MaxAttempts:   5,
	})
	if err != nil {
		panic(err)
	}
	stats, err := dispatcher.RunOnce(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("claimed=%d delivered=%d retried=%d dead=%d\n",
		stats.Claimed, stats.Delivered, stats.Retried, stats.Dead)
}
```

Output:

```text
sequence: 1
deliver event-1 type=session.updated payload={"status":"ok"}
claimed=1 delivered=1 retried=0 dead=0
```

Key behavior:

- Storage assigns `Envelope.Sequence` and `PersistedAt`; they must be zero values on input or `Append` returns `ErrInvalidEnvelope`.
- `OccurredAt` must be UTC, `Payload` must be valid JSON, and `TenantKey`, `EventID`, `StreamKey`, `Type`, `SchemaVersion`, `AggregateType`, and `AggregateKey` are all required.
- `RunOnce` claims at most `BatchSize` due events and delivers them one by one. A successful `Publish` is Acked; a failure is Nacked for backoff and retry. It moves to dead letter (`OutboxDead`) if the error matches `ErrPublishPermanent` or attempts reach `MaxAttempts`. Only storage or context errors are returned from `RunOnce`.
- `Dispatcher` does not loop or start goroutines. The caller controls polling cadence with timers, signals, or another mechanism. `DispatchBatch` is an alias for `RunOnce`.
- `AppendBatch` is atomic: if any event in the batch fails validation or has an idempotency conflict, nothing is written and no stream sequence numbers are consumed.

## FAQ

**Q: What happens if I append the same `EventID` twice?**
A: If the immutable inputs are exactly identical, the second append idempotently returns the first persisted result with the same sequence number. This makes retries safe when the database write succeeded but the response was lost. Different inputs return `ErrIdempotencyConflict`.

**Q: Why does `Append` reject `ReliabilityObservation` events?**
A: Observation events are defined as best-effort and never persisted; they use `Bus.TryPublish`. Conversely, `Bus` accepts only `ReliabilityObservation`. This is an intentional hard boundary: observation data cannot drive authoritative state, and reliable events cannot be silently dropped.

**Q: When does `Ack` return `ErrLeaseLost`?**
A: The lease is no longer valid, most commonly because delivery exceeded `LeaseDuration` and another owner reclaimed the event. `Ack` / `Nack` must carry the complete credentials returned by `Claim` (`LeaseOwner`, `LeaseToken`, `LeaseFence`). A mismatch or expired lease is rejected, preventing an old worker from overwriting a new worker's state. Simply abandon the operation; the event will be delivered again.

**Q: How should I handle `Replay` returning `Reconcile: true`?**
A: History cannot be read continuously from the cursor: it is before the retention floor, the cursor's `EventID` does not match, or the stream has a sequence gap. Do not treat `Events` as complete history. Follow `Reset.MinimumCursor`, first fetch the authoritative accumulated snapshot (`SnapshotRequired: true`), then continue subscribing from the new cursor.

**Q: How does a consumer prevent processing the same event twice?**
A: Delivery is at-least-once. Wrap handling with `Inbox.Consume(ctx, consumer, envelope, handler)`: a repeated `EventID` for the same consumer returns `Duplicate: true` without invoking the handler again. If the handler returns an error, no receipt is retained and a retry executes it again. `Inbox` is in-process; cross-process consumers must persist deduplication receipts in their own storage.

**Q: Can `MemoryStore` be used directly in production?**
A: It is a locked, linearizable reference implementation suitable for tests and single-process deployments, and all data is lost when the process restarts. Production systems should implement and persist `Store`, especially preserving `AppendBatch` atomicity and conditional-update semantics for lease fields.
