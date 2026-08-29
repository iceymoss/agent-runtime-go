# 9. Surviving a crash

## Where you are stuck

Chapter 7 already used a durable run to make approval work — there it was just the means to "stop and wait". The real problem shows up at deploy time: the process restarts and a run that was halfway through is gone, while the tools it already called have already taken effect.

## The three parts of recovery

```go
config := &agent.DurableRunConfig{
	Identity: agent.RunIdentity{
		RunKey: runKey, AgentKey: "ops.assistant",
		SessionID: "local", RequestID: runKey,
	},
	CheckpointStore: adapter,
	LeaseOwner:      attemptKey,
	LeaseDuration:   5 * time.Minute,
	ToolResume:      resume,
}
```

**`RunKey`** is the run's identity. Resuming is running again with the same key; a different key is a different run. It carries a nonce so that retrying after a permanently failed run is a new run rather than an attempt to revive a terminal one.

**`CheckpointStore`** is where each step lands. `durable` provides an implementation and the bridge:

```go
adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
	Store: store, Ledger: store, AttemptKey: durable.AttemptKey(attemptKey),
})
```

One adapter is bound to one attempt (`AttemptKey` is part of the effect ledger's immutable identity), so build a fresh one per attempt rather than sharing it.

**`LeaseOwner` / `LeaseDuration`** is the lease. It keeps two workers from advancing one run at the same time — if the original worker turns out to be alive after all, the lease and fence stop one of them.

`durable.NewMemoryStore()` exercises the whole mechanism; swapping in a database changes only this.

## What already happened does not happen again

On resume, tool calls that already completed are deduped on `ToolExecutionKey` rather than re-run. That is what the `WithToolReplayPolicy` from chapter 2 is for:

```go
agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent) // reading logs: replay is harmless
agent.WithToolReplayPolicy(agent.ReplayPolicyNever)      // restarting: never replay
```

**The runtime does not promise exactly-once external side effects.** It promises a stable dedupe anchor; real idempotency needs the downstream system to cooperate — the restart API has to recognize a duplicate request too.

## Storing the conversation itself

`durable` stores how far a run got, not what was said. Chapter 4's `Transcript` stores the conversation. They are different things and easy to confuse.

Two heavier layers exist when you need them.

`message` owns **message aggregation**: appends, revision CAS, branch visibility. You need it when several writers might modify one conversation at once — a user editing history while the agent is still running, for example.

`session` owns the **session aggregate**: revision, accumulated usage, fast-forward branch merges. It also provides `SessionAgent`, a durable run queue with admission limits, claim leases, cancellation, and graceful draining, which suits background work.

All three ship in-memory references, so you can get everything working before choosing a database, then verify your adapter with the conformance suites ([chapter 12](./12-testing.md)).

## Worth knowing

**Decide which layer you need.** Just want the conversation to survive a restart? Chapter 4's own table is enough. Want a run that was halfway through to continue? That is `durable`. Want several workers competing for a queue of tasks? That is `session`.

**A tool that can suspend must either declare `OwnsToolExecutionLifecycle` or go through the `tool` subpackage's executor** (chapter 7). With real side effects this is not optional: the executor's effect ledger is what answers "did that restart go through before the crash".

## Going deeper

- [durable](../packages/durable.md) — checkpoints, leases, fences, effect ledger
- [message](../packages/message.md) · [session](../packages/session.md)
- Complete runnable code: [`examples/guide/durability.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/durability.go)
