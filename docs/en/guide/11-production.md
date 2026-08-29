# 11. Going to production

## Where you are stuck

The features are done, but two things are missing: what happens during a run has to reach downstream systems reliably (billing, audit, notifications), and restarting the process must not cut off runs that are halfway through.

## Events you can rely on

Chapter 5's `Observation` is a lossy progress signal and cannot carry correctness. When an event must reach a downstream system, use the `event` subpackage:

```go
import "github.com/iceymoss/agent-runtime-go/event"
```

It provides a transactional outbox, claim/lease/fence delivery semantics, and replay. The point is that the outbox write commits in the same transaction as your business write — otherwise there is always a window where the business succeeded and the event was lost, or the reverse.

## Readiness and graceful shutdown

The `app` subpackage turns a component dependency graph, readiness probes, and bounded shutdown into one thing:

```go
import "github.com/iceymoss/agent-runtime-go/app"
```

The shutdown order is deliberate: **stop admission → cancel → checkpoint → wait → flush → close**. Stopping admission first means a process that is shutting down refuses new runs instead of accepting one and killing it a moment later; checkpointing before waiting is what makes an interrupted run resumable rather than lost.

A readiness probe should actually reach the thing it reports on — a configured database is not a working one.

## What is still yours

The library does not do these; they belong to your composition root:

- **Credentials**: where they come from and how they rotate
- **Rate limits and quotas**: who may run how much
- **Multi-tenant isolation**: `TenantKey` is a label; the real isolation is in your storage and authorization
- **Observability**: metrics, tracing, log redaction

## Going deeper

- [event](../packages/event.md) — bus, outbox, inbox, replay
- [app](../packages/app.md) — component graph, readiness, bounded shutdown
- [Production composition](../production.md) — composing providers, permissions, state, and events into a service
