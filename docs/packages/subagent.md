# subagent

`subagent` 包编排持久化的、相互独立的子代理运行：父代理派生（Spawn）子任务后可以挂起，worker 认领并执行子任务，完成后通过持久化的唤醒意图（wake intent）通知父代理——整条链路幂等、带预算约束、崩溃可恢复。

## 是什么

核心是 `Service`（由 `New(Options)` 构造）和三个应用侧端口：`Store`（持久化事务，包内置 `MemoryStore` 参考实现）、`Runner`（真正执行一个子代理）、`ParentWaker`（唤醒父代理）：

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

`Service` 的公开方法构成完整生命周期：`Spawn`（幂等创建，返回 `SpawnReceipt`）、`RunNext`（认领并执行至多一个子任务，绝不起后台 goroutine）、`Get`（查询 `Snapshot`）、`Cancel`（沿子树传播取消）、`SettleUsage`（一次性结算用量）、`Reconcile`（回收过期租约、重投未送达的唤醒）。

子任务状态机为 `queued → running → suspended/completed/failed/canceled`，其中 `ChildState.Terminal()` 标识终态。每棵派生树（`TreeKey`）有一份预算账本：`Limits` 是上限，`Reservation` 是 Spawn 时的原子预留，`Usage` 是结算后的实际消耗，预留减实际的差额自动释放回树预算。

## 为什么需要它

"父代理派生子代理"看似简单，分布式环境里要同时解决：重试导致的重复创建（幂等键）、并发派生把预算超卖（原子预留）、worker 崩溃后任务卡死（租约过期回收）、"结果已提交但父代理没被通知"（先提交事实再投递唤醒，至少一次投递）、失控的递归派生（深度/扇出/成本上限）、取消一棵子树时的部分失败。这个包把这些不变量收敛进 `Store` 接口契约，并附带一套一致性测试（`conformance_test.go`）供自定义 Store 实现（如 Postgres）验证。

什么时候不需要它：子任务在同一进程内同步完成、失败直接随父任务重来、不需要跨进程恢复——那就直接函数调用或 `errgroup`，不必引入持久化编排。

## 怎么用

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

关键行为：

- `Spawn` 以 `(TenantKey, RequestKey)` 幂等：同键同内容返回同一张回执；同键不同内容返回 `ErrIdempotencyConflict`。`RequestKey`、`Parent` 三要素、`AgentKey`、非空 `Input` 都是必填。
- 根派生填 `TreeKey`、留空 `Parent.RelationshipKey`；嵌套派生反过来——填父关系键、留空 `TreeKey`（树由 Store 沿父链推导，两者同时填报 `ErrInvalidRequest`）。
- `RunNext` 是拉模式：认领一个到期任务、同步执行、按 `ExpectedVersion` 乐观提交（`CommitTerminal` 或 `CommitSuspended`），不做任何后台调度。轮询节奏、并发 worker 数由应用决定；多个 `Service`（不同 `WorkerID`）可共享同一 Store。
- `Runner.Run` 返回错误时不会丢任务：Service 自动转为 `ChildSuspended` 且 `Retryable: true`，等 `Reconcile` 回收后重试。
- 终态先落库、再投递唤醒：唤醒失败只影响 `ReconcileReport.WakeFailures`，事实不丢，下轮 `Reconcile` 重投。

## 常见问题

**Q: 重试 `Spawn` 报 `ErrIdempotencyConflict`，不是说幂等吗？**
A: 幂等要求同一个 `RequestKey` 携带完全相同的请求内容（Store 会对请求做摘要比对）。重试时改了 `Input`、`Limits` 或 `Reserve` 任何一个字节都算冲突。语义变了就换新的 `RequestKey`。

**Q: 并发 Spawn 一批子任务，部分报 `ErrCostBudgetExceeded`？**
A: 这是树预算的原子预留在起作用：所有兄弟任务的 `Reserve` 之和不能超过树的 `Limits`。一致性测试验证了 10 个并发预留 30 时预算 100 恰好放行 3 个。收到预算类错误（还有 `ErrTokenBudgetExceeded`、`ErrToolBudgetExceeded`、`ErrRuntimeBudgetExceeded`）时应等待已有子任务结算释放差额，或调小预留。

**Q: `SettleUsage` 第二次调用没报错但也没生效？**
A: 结算按 `UsageFactKey` 恰好一次：重复提交相同用量返回 `applied=false` 且不报错；相同键不同用量返回 `ErrUsageConflict`。实际用量超过预留会返回 `ErrUsageExceedsReserve`。

**Q: `ParentWaker.Wake` 为什么会收到重复的 `WakeKey`？**
A: 投递语义是至少一次：提交唤醒意图和确认送达是两步，中间崩溃就会重投。所以接口契约要求 `Wake` 按 `WakeKey` 幂等——用它去重即可，外部测试专门验证了"提交后唤醒失败，两次 Reconcile 后恰好送达一次"。

**Q: `Cancel` 报 `ErrTraversalLimit` 后子树处于什么状态？**
A: 完全没变。取消是全有或全无的：`MaxTraversal` 不够覆盖整棵子树时直接拒绝且不做部分变更，调大后重试。取消本身也按 `RequestKey` 幂等。

**Q: worker 崩溃后任务一直是 `running` 怎么办？**
A: 每次认领带 `LeaseDuration` 租约。定期（每租户）调用 `Reconcile`，过期租约会被回收重新入队，同时顺带重投未送达的唤醒。`ErrDepthExceeded` / `ErrFanoutExceeded` / `ErrCycle` 则是 Spawn 时的结构性保护，说明派生图超限或成环。
