# durable

The `durable` package provides a persistence-neutral durable execution contract: authoritative run state, lease and fence, tool side-effect ledger (effect ledger), usage ledger, and explicitly driven reconcile. It starts no workers or background tasks; every operation is initiated by the caller.

## What it is

The core abstraction is the `durable.Store` interface: authoritative storage for a run. Run state is a `Snapshot`—a complete point-in-time value including identity (`Identity`), state machine (`Status` × `Phase`), monotonically increasing `Revision` and `FenceToken`, and root-package `agent.Checkpoint`. All boundaries return deep copies; a snapshot you hold is never mutated by later writes.

Write authority is protected by leases: a worker `Acquire`s a lease (fence increments), then every write carries a `Guard` (the `RunKey` + `LeaseOwner` + `Revision` + `FenceToken` quadruple from the snapshot's `Guard()` method). The store compares all four in one atomic operation and rejects if any is stale—this prevents a "zombie worker" from overwriting a new worker's data after network partition recovery.

```go
type Store interface {
    Begin(context.Context, BeginRequest) (Snapshot, bool, error)
    Load(context.Context, RunKey) (Snapshot, error)
    Acquire(context.Context, AcquireRequest) (Snapshot, error)
    Renew(context.Context, Guard, time.Time, time.Time) (Snapshot, error)
    Release(context.Context, Guard, Phase) (Snapshot, error)
    Save(context.Context, SaveRequest) (Snapshot, error)
    RevokeLease(context.Context, RevokeLeaseRequest) (Snapshot, error)
    Scan(context.Context, ScanRequest) (ScanPage, error)
}
```

Around Store there are three companion boundaries: `ExecutionLedger` records each tool call's side-effect lifecycle (`EffectPrepared -> EffectRunning -> EffectSucceeded/EffectFailed`, plus `EffectUnknown` after crash); `UsageLedger` records immutable usage facts (`UsageFact`); `Reconciler` (built with `NewReconciler`) batch-scans expired leases, revokes fences, and hands recoverable runs to `WorkSink`. `NewMemoryStore()` returns a thread-safe in-memory implementation of `Store`, `ExecutionLedger`, and `UsageLedger`.

## Why you need it

Without this package you have to solve the full crash-recovery problem set: who takes over after a worker dies mid-flight, how to stop an old worker from writing after partition recovery (fence), whether a non-idempotent tool call (for example a charge) actually ran at crash time (the effect ledger marks it `EffectUnknown` and refuses automatic replay), and how usage is not double-billed under retries (usage facts are idempotent by key). Each of these semantics is easy to get wrong and hard to expose in tests.

**When you do not need it**: if Agent runs are short-lived, fully re-runnable, and tools are all idempotent (or have no side effects), root-package `agent.CheckpointStore` plus simple retry is enough—no lease/fence model required.

## How to use it

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
)

func main() {
	ctx := context.Background()
	store := durable.NewMemoryStore()

	snap, created, err := store.Begin(ctx, durable.BeginRequest{
		Identity: durable.Identity{
			RunKey: "run-001", AgentKey: "assistant",
			SessionID: "chat-001", RequestID: "req-001",
		},
		InputDigest:  "sha256:input",
		ConfigDigest: "sha256:config",
		Checkpoint:   agent.Checkpoint{History: []agent.Message{agent.NewUserMessage("你好")}},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("created:", created, "status:", snap.Status)

	now := time.Now().UTC()
	leased, err := store.Acquire(ctx, durable.AcquireRequest{
		RunKey: "run-001", Owner: "worker-1",
		Now: now, LeaseUntil: now.Add(30 * time.Second),
	})
	if err != nil {
		log.Fatal(err)
	}

	running, err := store.Save(ctx, durable.SaveRequest{
		Guard: leased.Guard(), Status: durable.StatusRunning,
		Phase: durable.PhaseModelInflight, Checkpoint: leased.Checkpoint,
	})
	if err != nil {
		log.Fatal(err)
	}

	finalizing, err := store.Save(ctx, durable.SaveRequest{
		Guard: running.Guard(), Status: durable.StatusRunning,
		Phase: durable.PhaseFinalizing, Checkpoint: running.Checkpoint,
	})
	if err != nil {
		log.Fatal(err)
	}

	done, err := store.Save(ctx, durable.SaveRequest{
		Guard: finalizing.Guard(), Status: durable.StatusCompleted,
		Phase: durable.PhaseTerminal, Checkpoint: finalizing.Checkpoint,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("terminal:", done.Terminal(), "revision:", done.Revision)
}
```

Output:

```text
created: true status: claimed
terminal: true revision: 4
```

Key behavior:

- `Begin` is idempotent by `RunKey`: replaying the same key with identical immutable inputs returns `created == false`; different inputs return `ErrRunConflict`. All four `Identity` fields and both digests are required. In real use, compute digests with `durable.DigestInput` / `durable.DigestConfig` from immutable inputs—do not hand-write them.
- Every successful write (`Acquire`/`Renew`/`Save`, etc.) increments `Revision`, so the next write must use `Guard()` from **the snapshot returned by the previous write**; reusing an old guard yields `ErrLeaseLost`.
- `Save` validates legal `Status` × `Phase` combinations and phase edges (for example you cannot jump from `PhaseModelReady` straight to `PhaseToolsReady`), and `StatusCompleted` requires the current phase to already be `PhaseFinalizing`.
- Entering a non-`StatusRunning` status clears lease fields automatically; terminal states (`StatusCompleted`/`StatusFailed`/`StatusAbandoned`) cannot change further—any write returns `ErrTerminal`.
- `MemoryStore` is a reference implementation; data is lost on restart. Production needs database-backed `Store`/`ExecutionLedger`/`UsageLedger`, especially ensuring `Save`'s four-way compare and `RevokeLease`'s "revoke + mark unknown effect" run in the same transaction.

## FAQ

**Q: What is the difference between `ErrLeaseHeld` and `ErrLeaseLost`?**
A: `ErrLeaseHeld` appears on `Acquire`: another worker holds an unexpired lease—retry later or `Scan` for other work. `ErrLeaseLost` appears on guarded writes (`Save`/`Renew`/`Release`, etc.): your owner, fence, or revision is stale, meaning the lease was revoked or stolen. Discard local state and `Acquire` again for a new fence.

**Q: Why is `Save` to `StatusCompleted` rejected with "completion requires finalizing"?**
A: Completion is two steps: first `Save` to `Phase: PhaseFinalizing` (still `StatusRunning`), then `Save` to `StatusCompleted` + `PhaseTerminal`. That guarantees an explicit wrap-up phase before terminal commit so crash recovery can tell "result decided but not yet booked".

**Q: What is `EffectUnknown`, and why does `BeginEffect` return `ErrToolEffectUnknown` for it?**
A: When a worker crashes or a lease is revoked, side-effect records in `EffectRunning` are atomically marked `EffectUnknown` by `RevokeLease`—the system does not know whether that tool call (charge, email, etc.) completed. The ledger refuses automatic replay of unknown effects; human or upper-layer policy must decide. `Reconciler` classifies such runs as `ClassificationOperatorRequired` rather than re-queuing them.

**Q: Why does `Release` only accept `PhaseModelReady` and `PhaseToolsReady`?**
A: Voluntary suspend is only allowed in safe phases—no model request or tool call in flight. If the run is in an inflight phase, wait until it reaches a ready phase then `Release`, or take the `RevokeLease` recovery path.

**Q: Does repeating `RecordUsage` double-bill?**
A: No. `UsageFact` is idempotent on (`TenantKey`, `UsageKey`): identical facts replay with `created == false`; same key with different values returns `ErrUsageConflict`. Derive deterministic keys with `durable.UsageFactKey(tenant, attempt, execution, kind)`; do not invent random strings.

**Q: Can snapshots be JSON-serialized directly?**
A: Use package helpers `MarshalSnapshot` / `UnmarshalSnapshot`: they validate `SchemaVersion` (currently `SnapshotSchemaVersion == 1`), decode in strict mode (unknown fields yield `ErrSnapshotSchema`), and v1 field order and JSON tags are frozen. Persistence implementations should store this encoding rather than defining their own structure.
