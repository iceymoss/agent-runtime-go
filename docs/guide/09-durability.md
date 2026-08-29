# 9. 崩溃了还能接着跑

## 你现在遇到的问题

一次运行可能跑几分钟，中间调了几个有副作用的工具。进程被重启、被 OOM、被部署掉了——这次运行就没了，用户看到一个转圈然后什么都没有，而那几个副作用已经发生了。

## 让运行可恢复

给 `RunRequest` 加一个 `DurableRun`，运行的每一步都会落 checkpoint：

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

`CheckpointStore` 由你提供。`durable` 子包给了一个完整实现和到这个端口的桥接：

```go
import "github.com/iceymoss/agent-runtime-go/durable"

adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
	Store: myStore, Ledger: myLedger, AttemptKey: durable.AttemptKey(attemptKey),
})
```

租约（lease）和 fence 保证同一次运行不会被两个 worker 同时推进；恢复时从 checkpoint 接着跑，已完成的工具调用凭 `ToolExecutionKey` 去重而不是重跑。

**运行时不保证外部副作用 exactly-once**，它保证的是一个稳定的去重锚点。

## 对话本身怎么存

`durable` 存的是"这次运行跑到哪了"，不是对话内容。对话有两个层次：

`message` 子包管**消息聚合**：追加、修订的 CAS、分支可见性。当多个写入者可能同时改一段对话时需要它。

`session` 子包管**会话聚合**：会话的 revision、用量累计、分支的 fast-forward 合并。它还提供 `SessionAgent`——一个带准入配额、claim 租约、取消和优雅排空的持久化运行队列，适合后台任务。

```go
import (
	"github.com/iceymoss/agent-runtime-go/message"
	"github.com/iceymoss/agent-runtime-go/session"
)
```

三个包都给了内存参考实现，所以不接数据库也能先跑通。要接自己的存储时，用 `agenttest` 的一致性套件验收——见[第 12 章](./12-testing.md)。

## 需要注意的

**先想清楚你要的是哪一层。** 只是想让对话在重启后还在？第 4 章的"存进你自己的表"就够了。要的是"一次跑到一半的运行能接着跑"？那才需要 `durable`。

**有副作用的工具必须声明重放策略**（第 7 章），否则恢复时运行时无法判断哪些能安全重跑。

## 深入

- [durable](../packages/durable.md) —— checkpoint、租约、fence、effect ledger
- [message](../packages/message.md) —— 消息聚合与 revision CAS
- [session](../packages/session.md) —— 会话聚合、分支合并、运行队列
