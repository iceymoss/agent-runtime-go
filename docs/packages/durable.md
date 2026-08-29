# durable

`durable` 包提供持久化中立的 durable execution 契约：run 的权威状态、租约（lease）与栅栏（fence）、工具副作用台账（effect ledger）、用量台账（usage ledger）和显式驱动的 reconcile。它不启动任何 worker 或后台任务，所有操作都由调用方显式发起。

## 是什么

核心抽象是 `durable.Store` 接口：一个 run 的权威存储。run 状态是 `Snapshot`——完整的时间点值，包含身份（`Identity`）、状态机（`Status` × `Phase`）、单调递增的 `Revision` 和 `FenceToken`，以及根包的 `agent.Checkpoint`。所有边界都返回深拷贝，拿到的快照永远不会被后续写入改动。

写入权由租约保护：worker 通过 `Acquire` 拿到租约（fence 加一），之后每次写都携带 `Guard`（`RunKey` + `LeaseOwner` + `Revision` + `FenceToken` 四元组，从快照的 `Guard()` 方法获得）。存储在同一个原子操作里比较全部四项，任何一项过期即拒绝——这就是防止"僵尸 worker 复活后覆盖新 worker 数据"的机制。

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

围绕 Store 还有三个配套边界：`ExecutionLedger` 记录每次工具调用的副作用生命周期（`EffectPrepared -> EffectRunning -> EffectSucceeded/EffectFailed`，以及崩溃后的 `EffectUnknown`）；`UsageLedger` 记录不可变的用量事实（`UsageFact`）；`Reconciler`（`NewReconciler` 构造）批量扫描过期租约、吊销 fence 并把可恢复的 run 交给 `WorkSink`。`NewMemoryStore()` 返回同时实现 `Store`、`ExecutionLedger`、`UsageLedger` 的线程安全内存实现。

## 为什么需要它

没有这个包，你要自己解决崩溃恢复的全部难题：worker 半路挂掉后谁来接管、旧 worker 网络分区恢复后如何阻止它继续写（fence）、一个非幂等的工具调用（比如扣款）在崩溃时刻到底执行没执行（effect 台账把它标为 `EffectUnknown` 并拒绝自动重放）、以及重试之下用量如何不重复计费（usage 事实按 key 幂等）。这些语义每一条都容易写错，且错了很难在测试中暴露。

什么时候不需要它：如果你的 Agent 运行是短命的、可整体重跑、工具全部幂等（或没有副作用），根包的 `agent.CheckpointStore` 加简单重试就足够了，不必引入 lease/fence 模型。

## 怎么用

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

运行输出：

```text
created: true status: claimed
terminal: true revision: 4
```

关键行为：

- `Begin` 按 `RunKey` 幂等：同一 key 用完全相同的不可变输入重放返回 `created == false`；输入不同返回 `ErrRunConflict`。`Identity` 四个字段和两个 digest 都是必填。真实场景下 digest 用 `durable.DigestInput` / `durable.DigestConfig` 从不可变输入计算，不要手写。
- 每次成功写入（`Acquire`/`Renew`/`Save` 等）都会使 `Revision` 加一，所以下一次写必须用**上一次返回快照**的 `Guard()`，复用旧 guard 会得到 `ErrLeaseLost`。
- `Save` 校验 `Status` × `Phase` 的合法组合与相位边（例如不能从 `PhaseModelReady` 直接跳 `PhaseToolsReady`），且 `StatusCompleted` 要求当前相位已是 `PhaseFinalizing`。
- 进入非 `StatusRunning` 状态时租约字段被自动清空；终态（`StatusCompleted`/`StatusFailed`/`StatusAbandoned`）不可再变，任何写入返回 `ErrTerminal`。
- `MemoryStore` 是参考实现，进程重启即丢数据；生产环境需要基于数据库自己实现 `Store`/`ExecutionLedger`/`UsageLedger`，尤其要保证 `Save` 的四项比较和 `RevokeLease` 的"吊销 + 标记 unknown effect"在同一个事务里。

## 接到根包 `Agent.Run`

根包的 durable 执行入口是 `agent.CheckpointStore`。`durable.CheckpointAdapter` 把 `Store` + `ExecutionLedger` 投影成这个端口，调用方不必自己重写一遍相位状态机：

```go
adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
	Store:      store, // *MemoryStore 或你自己的数据库实现
	Ledger:     store,
	AttemptKey: "attempt-1", // 本次 attempt 的标识，写进 effect 台账
})
if err != nil {
	log.Fatal(err)
}

result, err := runner.Run(ctx, agent.RunRequest{
	Messages: []agent.Message{agent.NewUserMessage("修复失败的测试")},
	DurableRun: &agent.DurableRunConfig{
		Identity:        agent.RunIdentity{RunKey: "run-001", AgentKey: "coder", SessionID: "s-1", RequestID: "r-1"},
		CheckpointStore: adapter,
		LeaseOwner:      "worker-1",
		LeaseDuration:   time.Minute,
	},
})
```

映射规则：

- run 生命周期的每次变更（`ModelInflight` / `CommitModelResponse` / `PrepareTools` / `CommitTool` / `Suspend` / `Complete` / `Fail`）都落成**一次** `Store.Save`，相位由 checkpoint 推导：还有 `PendingToolCalls` 就是 `PhaseToolsReady`，已完成就是 `PhaseFinalizing`，否则回到 `PhaseModelReady`。
- 工具生命周期落到 effect 台账，key 用 `ToolExecutionKey`——它与根包 `ToolExecution.IdempotencyKey` 逐字节相同，所以两层用的是同一个去重锚点。
- `Suspend` 用 `Save` 而不是 `Release`：挂起的 run 必须连同 checkpoint 里的 blocker 一起持久化，后续 attempt 才能凭 `DurableRun.ToolResume` 精确恢复到那一个工具调用。
- `Acquire` 允许接管 `StatusSuspended` 的 run（`AllowSuspended: true`），因为审批挂起和子 Agent 挂起本来就要由后续 attempt 继续；仍在有效期内的租约依然会被拒绝。
- 错误同时满足两套哨兵：`ErrLeaseLost` 之类会被包成 `agent.ErrCheckpointConflict`，`ErrToolEffectUnknown` 会被包成 `agent.ErrToolExecutionUnknown`，两边都能用 `errors.Is` 判断。

### unknown effect 的重放

崩溃留下的 `EffectUnknown` 默认永远不会自动重放。如果某个工具的 `ReplayPolicy` 已经证明重放是安全的（`agent.ReplayPolicyIdempotent`），台账可以实现可选扩展接口：

```go
type EffectReplayer interface {
	ReplayEffect(context.Context, Guard, ExecutionKey, time.Time) (EffectRecord, error)
}
```

`MemoryStore` 已经实现了它。适配器只有在运行时明确告知"这个工具可安全重放"时才会调用；没有实现该接口的台账保持保守默认，`BeginTool` 直接返回 `agent.ErrToolExecutionUnknown`。

## 常见问题

**Q: `ErrLeaseHeld` 和 `ErrLeaseLost` 有什么区别？**
A: `ErrLeaseHeld` 出现在 `Acquire`：别的 worker 持有未过期的租约，你应该稍后重试或走 `Scan` 找其他工作。`ErrLeaseLost` 出现在持有 guard 的写入（`Save`/`Renew`/`Release` 等）：你的 owner、fence 或 revision 已过期，说明租约被吊销或被抢占，正确做法是放弃本地状态、重新 `Acquire` 拿新 fence。

**Q: 为什么 `Save` 到 `StatusCompleted` 被拒绝，报 "completion requires finalizing"？**
A: 完成必须两步走：先 `Save` 到 `Phase: PhaseFinalizing`（仍是 `StatusRunning`），再 `Save` 到 `StatusCompleted` + `PhaseTerminal`。这保证终态提交前有一个明确的收尾相位，崩溃恢复时能区分"结果已定但未落账"。

**Q: `EffectUnknown` 是什么？为什么 `BeginEffect` 对它报 `ErrToolEffectUnknown`？**
A: worker 崩溃或租约被吊销时，处于 `EffectRunning` 的副作用记录会被 `RevokeLease` 原子标记为 `EffectUnknown`——系统不知道这次工具调用（可能是扣款、发邮件）到底完成没有。台账拒绝对 unknown effect 自动重放，必须由人工或上层策略裁决后处理。`Reconciler` 会把这类 run 归入 `ClassificationOperatorRequired` 而不是重新入队。

**Q: `Release` 为什么只接受 `PhaseModelReady` 和 `PhaseToolsReady`？**
A: 主动挂起（suspend）只允许发生在安全相位——没有模型请求或工具调用正在飞行中。如果 run 正处于 inflight 相位，要么等它到达 ready 相位再 `Release`，要么走 `RevokeLease` 的恢复路径。

**Q: `RecordUsage` 重复上报会重复计费吗？**
A: 不会。`UsageFact` 按 (`TenantKey`, `UsageKey`) 幂等：完全相同的事实重放返回 `created == false`；同 key 但数值不同返回 `ErrUsageConflict`。用 `durable.UsageFactKey(tenant, attempt, execution, kind)` 派生确定性的 key，别自己拼随机字符串。

**Q: 快照能直接 JSON 序列化吗？**
A: 用包提供的 `MarshalSnapshot` / `UnmarshalSnapshot`：它们校验 `SchemaVersion`（当前为 `SnapshotSchemaVersion == 1`），解码是严格模式（未知字段报 `ErrSnapshotSchema`），v1 的字段顺序和 JSON tag 是冻结的。持久化实现应存这个编码而不是自己定义结构。
