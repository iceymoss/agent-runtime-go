# 9. Surviving a crash

## Where you are stuck

A run can take minutes and call several tools with side effects along the way. The process gets restarted, OOM-killed, or deployed over — and the run is gone. The user sees a spinner and then nothing, while those side effects already happened.

## Making a run resumable

Add `DurableRun` to the request and every step is checkpointed:

```go
result, err := runner.Run(ctx, agent.RunRequest{
	Messages: messages,
	DurableRun: &agent.DurableRunConfig{
		Identity:        agent.RunIdentity{RunKey: runKey, AgentKey: "my.agent", SessionID: sessionID, RequestID: runKey},
		CheckpointStore: checkpoints,
		LeaseOwner:      workerID,
		LeaseDuration:   15 * time.Minute,
	},
})
```

You supply the `CheckpointStore`. The `durable` subpackage provides an implementation and the bridge to that port:

```go
import "github.com/iceymoss/agent-runtime-go/durable"

adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
	Store: myStore, Ledger: myLedger, AttemptKey: durable.AttemptKey(attemptKey),
})
```

Leases and fences keep two workers from advancing one run at the same time. On resume the run continues from its checkpoint, and tool calls that already completed are deduped on `ToolExecutionKey` rather than re-run.

**The runtime does not promise exactly-once external side effects.** It promises a stable dedupe anchor.

## Storing the conversation itself

`durable` stores how far a run got, not what was said. The conversation has two layers.

The `message` subpackage owns **message aggregation**: appends, revision CAS, branch visibility. You need it when several writers might modify one conversation concurrently.

The `session` subpackage owns the **session aggregate**: a session's revision, accumulated usage, and fast-forward merges of branches. It also provides `SessionAgent` — a durable run queue with admission limits, claim leases, cancellation, and graceful draining, which suits background work.

```go
import (
	"github.com/iceymoss/agent-runtime-go/message"
	"github.com/iceymoss/agent-runtime-go/session"
)
```

All three ship in-memory reference implementations, so you can get everything working before choosing a database. When you do connect your own storage, verify it with the conformance suites — see [chapter 12](./12-testing.md).

## Worth knowing

**Decide which layer you actually need.** Just want the conversation to survive a restart? Storing it in your own tables (chapter 4) is enough. Want a run that was halfway through to continue? That is what `durable` is for.

**Tools with side effects must declare a replay policy** (chapter 7), or the runtime cannot tell on resume which ones are safe to re-run.

## Going deeper

- [durable](../packages/durable.md) — checkpoints, leases, fences, effect ledger
- [message](../packages/message.md) — message aggregation and revision CAS
- [session](../packages/session.md) — session aggregate, branch merges, run queue
