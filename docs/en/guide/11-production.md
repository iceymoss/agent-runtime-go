# 11. Going to production

## Where you are stuck

The features are done. Two things are missing: what happens during a run must reach downstream systems **reliably** (billing, audit, notifications), and restarting the process must not cut off runs that are halfway through.

## Events you can rely on

Chapter 5's `Observation` is a lossy progress signal and cannot carry correctness. When something must be delivered, use `event`:

```go
func RecordRunFinished(ctx context.Context, store event.Store, sessionKey string, result *agent.RunResult) (event.Envelope, error) {
	payload, err := json.Marshal(map[string]any{
		"outcome": result.Outcome, "stop_reason": result.StopReason,
		"steps": len(result.Steps), "tokens": result.Usage.TotalTokens,
	})
	if err != nil {
		return event.Envelope{}, err
	}
	return store.Append(ctx, event.AppendCommand{Envelope: event.Envelope{
		TenantKey: "local",
		// The event id is the idempotency anchor: appending the same id twice is
		// the same event, which is what makes a retry safe.
		EventID:       sessionKey + ":run-finished",
		StreamKey:     sessionKey,
		Type:          "agent.run.finished",
		SchemaVersion: 1,
		Reliability:   event.ReliabilityTerminal, // a terminal fact; consumers may not lose it
		AggregateType: "session",
		AggregateKey:  sessionKey,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}})
}
```

The important part is not this code but **which transaction it runs in**: in production the `Append` commits together with your business write. Otherwise there is always a window where the business succeeded and the event was lost, or the reverse. That is what an outbox is for.

Replay is what the cost buys: a consumer that was down, or one added later, rebuilds its view from the same events instead of asking every producer to resend.

```go
result, err := store.Replay(ctx, event.ReplayQuery{
	Cursor: event.Cursor{TenantKey: "local", StreamKey: sessionKey},
	Limit:  limit,
})
```

## Readiness and graceful shutdown

```go
func NewLifecycle(db *sql.DB, runs app.RunController, checkpoints app.Checkpointer, events app.Flusher) (*app.App, error) {
	return app.New(
		app.Config{Budgets: app.Budgets{
			Startup:    30 * time.Second,
			Drain:      20 * time.Second,
			Cancel:     5 * time.Second,
			Checkpoint: 10 * time.Second,
			Wait:       20 * time.Second,
			Flush:      5 * time.Second,
			Close:      5 * time.Second,
		}},
		app.Dependencies{Runs: runs, Checkpoints: checkpoints, Events: events},
		storeComponent{db: db},
	)
}
```

The budgets are the point. Shutdown runs **stop admission → cancel → checkpoint → wait → flush → close**, and each step has its own deadline, so a slow flush cannot eat the time reserved for checkpointing runs that are still in flight. Stopping admission first is what keeps a closing process from accepting a run it will kill a moment later.

A readiness probe must reach the thing it reports on:

```go
func (c storeComponent) Ready(ctx context.Context) app.ComponentHealth {
	if err := c.db.PingContext(ctx); err != nil {
		return app.ComponentHealth{Configured: true, Reason: "database unreachable: " + err.Error()}
	}
	return app.ComponentHealth{Configured: true, Ready: true, Generation: "schema-v1"}
}
```

A configured database is not a working one. A probe that reports ready because it was configured is worse than no probe: it makes a broken deployment look healthy.

The three `Dependencies` are yours: what a run is, how it is checkpointed, how pending events are flushed. The library owns the **ordering**, not the meaning.

## What is still yours

- **Credentials**: where they come from and how they rotate
- **Rate limits and quotas**: who may run how much
- **Multi-tenant isolation**: `TenantKey` is a label; real isolation is in your storage and authorization
- **Observability**: metrics, tracing, log redaction

## Going deeper

- [event](../packages/event.md) — bus, outbox, inbox, delivery semantics, replay
- [app](../packages/app.md) — component graph, readiness, bounded shutdown
- Complete runnable code: [`examples/guide/advanced/events.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/events.go) and [`lifecycle.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/lifecycle.go)
- [Production composition](../production.md)
