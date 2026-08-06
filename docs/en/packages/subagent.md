# subagent

The `subagent` package orchestrates durable, independent child-agent runs: a parent spawns a child task and may suspend; a worker claims and executes the child; on completion a durable wake intent notifies the parent—the whole path is idempotent, budget-constrained, and crash-recoverable.

## What it is

The core is `Service` (from `New(Options)`) and three application ports: `Store` (durable transactions; built-in `MemoryStore` reference), `Runner` (actually runs a child agent), `ParentWaker` (wakes the parent):

```go
type Runner interface {
    Run(context.Context, RunRequest) (RunResult, error)
}

type ParentWaker interface {
    // Wake 必须按 WakeKey 幂等：已提交的唤醒意图可能被并发或重复投递。
    Wake(context.Context, WakeRequest) error
}

type Options struct {
    Store         Store
    Runner        Runner
    ParentWaker   ParentWaker
    WorkerID      string
    LeaseDuration time.Duration
    Clock         func() time.Time
}
```

`Service` public methods cover the full lifecycle: `Spawn` (idempotent create, returns `SpawnReceipt`), `RunNext` (claim and run at most one child; never starts a background goroutine), `Get` (query `Snapshot`), `Cancel` (propagate cancel down the subtree), `SettleUsage` (one-shot usage settlement), `Reconcile` (reclaim expired leases, redeliver undelivered wakes).

Child state machine: `queued → running → suspended/completed/failed/canceled`, where `ChildState.Terminal()` marks terminal states. Each spawn tree (`TreeKey`) has a budget ledger: `Limits` are caps, `Reservation` is the atomic reserve at Spawn, `Usage` is settled actual cost; the reserve-minus-actual difference is released back to the tree budget.

## Why you need it

"Parent spawns child" looks simple, but distributed settings must jointly solve: duplicate creates on retry (idempotency keys), concurrent spawns overselling budget (atomic reserve), tasks stuck after worker crash (lease expiry reclaim), "result committed but parent never notified" (commit facts first, then deliver wake—at-least-once), runaway recursive spawn (depth/fanout/cost caps), and partial failure when canceling a subtree. This package folds those invariants into the `Store` interface contract, with conformance tests (`conformance_test.go`) for custom Store implementations (e.g. Postgres).

When you do not need it: if children finish synchronously in-process, failures restart with the parent, and cross-process recovery is unnecessary—use direct calls or `errgroup` instead of durable orchestration.

## How to use it

```go
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

type echoRunner struct{}

func (echoRunner) Run(_ context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
	return subagent.RunResult{
		State:        subagent.ChildCompleted,
		ResultRef:    "result/echo",
		UsageFactKey: subagent.UsageFactKey("usage/" + string(request.Child.RunKey)),
		Usage:        subagent.Usage{CostMicros: 5},
	}, nil
}

type logWaker struct{}

func (logWaker) Wake(_ context.Context, request subagent.WakeRequest) error {
	fmt.Println("child finished:", request.Child.RunKey, request.State)
	return nil
}

func main() {
	ctx := context.Background()
	tenant := agent.TenantKey("tenant-a")

	service, err := subagent.New(subagent.Options{
		Store:         subagent.NewMemoryStore(),
		Runner:        echoRunner{},
		ParentWaker:   logWaker{},
		WorkerID:      "worker-1",
		LeaseDuration: time.Minute,
	})
	if err != nil {
		log.Fatal(err)
	}

	receipt, err := service.Spawn(ctx, subagent.SpawnRequest{
		RequestKey: "review-pr-42",
		Parent: subagent.ParentRef{
			TenantKey:  tenant,
			SessionKey: "session-1",
			RunKey:     "run-parent",
			TreeKey:    "tree-1",
		},
		AgentKey: "reviewer",
		Input:    []byte(`{"pr":42}`),
		Limits:   subagent.Limits{MaxDepth: 3, MaxFanout: 8, MaxCostMicros: 1000, MaxRuntime: time.Hour},
		Reserve:  subagent.Reservation{CostMicros: 100, Runtime: time.Minute},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("spawned:", receipt.Child.RunKey, receipt.State)

	if _, ok, err := service.RunNext(ctx, tenant); err != nil || !ok {
		log.Fatalf("run next: ok=%v err=%v", ok, err)
	}

	report, err := service.Reconcile(ctx, subagent.ReconcileRequest{TenantKey: tenant, Limit: 10})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wakes delivered: %d\n", report.WakesDelivered)
}
```

Output:

```text
spawned: run/c7e3d980eae0577952b4f4e6f85eb8f5 queued
child finished: run/c7e3d980eae0577952b4f4e6f85eb8f5 completed
wakes delivered: 1
```

Key behavior:

- `Spawn` is idempotent on `(TenantKey, RequestKey)`: same key and content returns the same receipt; same key with different content returns `ErrIdempotencyConflict`. `RequestKey`, parent triad, `AgentKey`, and non-empty `Input` are required.
- Root spawn fills `TreeKey` and leaves `Parent.RelationshipKey` empty; nested spawn does the reverse—fill parent relationship key, leave `TreeKey` empty (tree is derived along the parent chain by Store; filling both returns `ErrInvalidRequest`).
- `RunNext` is pull-based: claim one due task, run synchronously, optimistically commit with `ExpectedVersion` (`CommitTerminal` or `CommitSuspended`)—no background scheduling. Poll cadence and worker concurrency are application concerns; multiple `Service` instances (different `WorkerID`) may share one Store.
- If `Runner.Run` returns an error, the task is not dropped: Service converts to `ChildSuspended` with `Retryable: true`, then retries after `Reconcile` reclaim.
- Terminal state is persisted first, then wake is delivered: wake failure only affects `ReconcileReport.WakeFailures`; facts are kept and the next `Reconcile` redelivers.

## FAQ

**Q: Retrying `Spawn` returns `ErrIdempotencyConflict`—isn't it idempotent?**
A: Idempotency requires the same `RequestKey` with identical request content (Store digests and compares). Changing any byte of `Input`, `Limits`, or `Reserve` on retry is a conflict. If semantics changed, use a new `RequestKey`.

**Q: Concurrent Spawns of siblings—some get `ErrCostBudgetExceeded`?**
A: Tree-budget atomic reserve at work: sibling `Reserve` sums cannot exceed tree `Limits`. Conformance tests verify that 10 concurrent reserves of 30 against budget 100 admit exactly 3. On budget errors (also `ErrTokenBudgetExceeded`, `ErrToolBudgetExceeded`, `ErrRuntimeBudgetExceeded`), wait for existing children to settle and release the difference, or reduce reserves.

**Q: Second `SettleUsage` call neither errors nor applies?**
A: Settlement is exactly-once per `UsageFactKey`: resubmitting the same usage returns `applied=false` without error; same key with different usage returns `ErrUsageConflict`. Actual usage over reserve returns `ErrUsageExceedsReserve`.

**Q: Why can `ParentWaker.Wake` see duplicate `WakeKey`s?**
A: Delivery is at-least-once: committing the wake intent and confirming delivery are two steps; a crash in between causes redelivery. The contract requires `Wake` to be idempotent on `WakeKey`—dedupe on it. External tests specifically verify "wake fails after commit; after two Reconciles, delivery happens exactly once."

**Q: After `Cancel` returns `ErrTraversalLimit`, what state is the subtree in?**
A: Unchanged. Cancel is all-or-nothing: if `MaxTraversal` cannot cover the whole subtree, the call is rejected with no partial mutation—raise the limit and retry. Cancel itself is also idempotent on `RequestKey`.

**Q: Worker crashed and the task stays `running`?**
A: Each claim carries a `LeaseDuration` lease. Call `Reconcile` periodically (per tenant); expired leases are reclaimed and re-queued, and undelivered wakes are redelivered. `ErrDepthExceeded` / `ErrFanoutExceeded` / `ErrCycle` are structural Spawn protections—the spawn graph exceeded limits or formed a cycle.
