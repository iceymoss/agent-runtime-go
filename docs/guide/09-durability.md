# 9. 崩溃了还能接着跑

## 你现在遇到的问题

第 7 章为了做审批已经用上了 durable run——那时它只是"能停下来等人"的手段。真正的问题在部署的时候暴露：进程被重启，一次跑到一半的运行就没了，而它已经调过的那几个有副作用的工具已经生效了。

## 恢复的三个部件

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

**`RunKey`** 是这次运行的身份。恢复就是用同一个 key 再跑一次；换 key 就是另一次运行。它带一个 nonce，这样"永久失败后重试"是一次新运行，而不是试图复活一个终态。

**`CheckpointStore`** 是每一步落到哪里。`durable` 给了实现和桥接：

```go
adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
	Store: store, Ledger: store, AttemptKey: durable.AttemptKey(attemptKey),
})
```

一个 adapter 绑定一次 attempt（`AttemptKey` 是 effect 台账不可变身份的一部分），所以每次尝试新建一个，别复用。

**`LeaseOwner` / `LeaseDuration`** 是租约。它保证同一次运行不会被两个 worker 同时推进——恢复时如果原来那个 worker 其实还活着，租约和 fence 会挡住其中一个。

`durable.NewMemoryStore()` 能跑通全部机制，换数据库时只改这一处。

## 已经做过的事不会再做一遍

恢复时已完成的工具调用凭 `ToolExecutionKey` 去重，不重跑。这就是第 2 章那个 `WithToolReplayPolicy` 的用处：

```go
agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent) // 查日志，重放无害
agent.WithToolReplayPolicy(agent.ReplayPolicyNever)      // 重启服务，绝不重放
```

**运行时不保证外部副作用 exactly-once。** 它保证的是给你一个稳定的去重锚点，真正的幂等要下游系统配合——重启接口自己也得能识别重复请求。

## 对话本身怎么存

`durable` 存的是"这次运行跑到哪了"，不是对话内容。第 4 章那个 `Transcript` 存的才是对话。两者是不同的东西，容易混。

对话有两个更重的层次，按需要选：

`message` 管**消息聚合**：追加、修订的 CAS、分支可见性。当多个写入者可能同时改一段对话时需要它（比如用户在编辑历史消息的同时 agent 还在跑）。

`session` 管**会话聚合**：会话的 revision、用量累计、分支的 fast-forward 合并。它还提供 `SessionAgent`——带准入配额、claim 租约、取消和优雅排空的持久化运行队列，适合后台任务。

三个包都给了内存参考实现，不接数据库也能先跑通。要接自己的存储时用 `agenttest` 的一致性套件验收（[第 12 章](./12-testing.md)）。

## 需要注意的

**先想清楚要的是哪一层。** 只想让对话在重启后还在？第 4 章存进你自己的表就够了。要"一次跑到一半的运行能接着跑"？那才需要 `durable`。要"多个 worker 抢同一批任务"？那是 `session` 的运行队列。

**能挂起的工具要么声明 `OwnsToolExecutionLifecycle`，要么走 `tool` 子包的 executor**（第 7 章）。这在有真实副作用时不是可选项：executor 维护的 effect 台账，才是"崩溃前那次重启到底执行了没有"的答案来源。

## 深入

- [durable](../packages/durable.md) —— checkpoint、租约、fence、effect 台账
- [message](../packages/message.md) · [session](../packages/session.md)
- 完整可运行代码：[`examples/guide/durability.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/durability.go)
