# event

`event` 包定义 Agent 的可移植事件合约：可靠事件的追加、outbox 投递、消费幂等与流回放，外加一条独立的 best-effort 观测总线。数据库事务和传输适配器有意留在包外。

## 是什么

核心数据类型是 `Envelope`——规范化的不可变事件表示，携带租户、事件 ID、流键（`StreamKey`）、由存储分配的单调 `Sequence`、聚合信息和 JSON `Payload`。每个事件通过 `Reliability` 声明自己的可靠性等级：`ReliabilitySessionSnapshot`、`ReliabilityTerminal`、`ReliabilityDomain` 三种会被持久化，`ReliabilityObservation` 只走 best-effort 通道、绝不落盘。

```go
type Store interface {
    Append(context.Context, AppendCommand) (Envelope, error)
    AppendBatch(context.Context, AppendBatchCommand) ([]Envelope, error)
    Claim(context.Context, ClaimCommand) ([]ClaimedEvent, error)
    Ack(context.Context, AckCommand) error
    Nack(context.Context, NackCommand) error
    Replay(context.Context, ReplayQuery) (ReplayResult, error)
}

type Publisher interface {
    Publish(context.Context, Envelope) error
}
```

`Store` 同时承担事件日志和 outbox 两种角色：`Append` 追加事件并置为待投递；`Claim` / `Ack` / `Nack` 是带租约（lease）的投递协议；`Replay` 按游标顺序读取一条流的历史，且从不静默跳过缺失的区间——检测到留存过期或序列断裂时返回 `Gap` / `Reset` 并置 `Reconcile: true`，要求调用方先取权威快照再继续。

围绕 `Store` 有三个配套组件：`Dispatcher` 做单次有界轮询（不启动任何 goroutine），把认领到的事件交给 `Publisher` 并根据结果 Ack 或按指数退避 Nack；`Inbox` 为消费端提供按事件 ID 的进程内幂等；`Bus` 是与 `Store` 完全分离的有界观测队列，非阻塞、满了就丢。

包内提供 `NewMemoryStore` 作为 `Store` 的内存参考实现，适合测试与单进程部署；生产环境需要基于数据库实现 `Store`，通常让 `AppendBatch` 加入拥有方聚合的同一个事务（经典 outbox 模式）。

## 为什么需要它

没有这个包，你要自己实现：事件追加与业务写入的原子性、按事件 ID 的幂等追加与冲突检测、多投递进程之间的租约互斥（防止同一事件被两个 worker 同时投递、防止过期租约的 Ack 覆盖新租约）、重试退避与死信、消费端去重，以及回放时的断档检测。这些正是 outbox / event-sourcing 基础设施里最繁琐的部分。

什么时候不需要它：单进程应用里如果事件只是用来打日志或调试观察，不需要可靠投递，直接用 `Bus`（或干脆用日志库）就够了，不必引入 `Store` 和 `Dispatcher`。

## 怎么用

追加一个域事件，然后用 `Dispatcher` 完成一轮投递：

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go/event"
)

type printPublisher struct{}

func (printPublisher) Publish(_ context.Context, envelope event.Envelope) error {
	fmt.Printf("deliver %s type=%s payload=%s\n", envelope.EventID, envelope.Type, envelope.Payload)
	return nil
}

func main() {
	ctx := context.Background()
	store := event.NewMemoryStore()

	stored, err := store.Append(ctx, event.AppendCommand{Envelope: event.Envelope{
		TenantKey:         "tenant-1",
		EventID:           "event-1",
		StreamKey:         "session-1",
		Type:              "session.updated",
		SchemaVersion:     1,
		Reliability:       event.ReliabilityDomain,
		AggregateType:     "session",
		AggregateKey:      "session-1",
		AggregateRevision: 1,
		OccurredAt:        time.Now().UTC(),
		Payload:           []byte(`{"status":"ok"}`),
	}})
	if err != nil {
		panic(err)
	}
	fmt.Println("sequence:", stored.Sequence) // 1，由存储分配

	dispatcher, err := event.NewDispatcher(store, printPublisher{}, event.DispatcherConfig{
		TenantKey:     "tenant-1",
		Owner:         "worker-1",
		BatchSize:     16,
		LeaseDuration: time.Minute,
		BaseBackoff:   time.Second,
		MaxBackoff:    time.Minute,
		MaxAttempts:   5,
	})
	if err != nil {
		panic(err)
	}
	stats, err := dispatcher.RunOnce(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("claimed=%d delivered=%d retried=%d dead=%d\n",
		stats.Claimed, stats.Delivered, stats.Retried, stats.Dead)
}
```

运行输出：

```text
sequence: 1
deliver event-1 type=session.updated payload={"status":"ok"}
claimed=1 delivered=1 retried=0 dead=0
```

关键行为：

- `Envelope.Sequence` 和 `PersistedAt` 由存储分配，输入时必须为零值，否则 `Append` 返回 `ErrInvalidEnvelope`。
- `OccurredAt` 必须是 UTC 时间，`Payload` 必须是合法 JSON，`TenantKey`、`EventID`、`StreamKey`、`Type`、`SchemaVersion`、`AggregateType`、`AggregateKey` 均为必填。
- `RunOnce` 一次认领至多 `BatchSize` 条到期事件并逐条投递：`Publish` 成功则 Ack；失败则 Nack 进入退避重试，返回的错误匹配 `ErrPublishPermanent` 或尝试次数达到 `MaxAttempts` 时转入死信（`OutboxDead`）。存储或 context 错误才会从 `RunOnce` 返回。
- `Dispatcher` 自身不循环、不起 goroutine，调用方自行决定轮询节奏（定时器、信号触发等）。`DispatchBatch` 是 `RunOnce` 的别名。
- `AppendBatch` 原子生效：批内任何一条校验或幂等冲突失败，整批都不写入，流序列号也不消耗。

## 常见问题

**Q: 用相同的 `EventID` 追加两次会怎样？**
A: 如果两次的不可变输入完全相同，第二次幂等返回第一次持久化的结果（序列号不变），这让"写库成功但响应丢失"的重试是安全的。如果输入不同，返回 `ErrIdempotencyConflict`。

**Q: 为什么 `Append` 拒绝 `ReliabilityObservation` 事件？**
A: 观测事件被定义为 best-effort，绝不持久化，走 `Bus.TryPublish` 通道；反过来 `Bus` 也只接受 `ReliabilityObservation`。这是刻意的硬边界：观测数据不允许驱动权威状态，可靠事件也不允许被静默丢弃。

**Q: `Ack` 返回 `ErrLeaseLost` 是什么情况？**
A: 租约已经失效——最常见的是投递耗时超过 `LeaseDuration`，事件被另一个 owner 重新认领。`Ack` / `Nack` 必须携带 `Claim` 返回的完整凭据（`LeaseOwner`、`LeaseToken`、`LeaseFence`），任何一项对不上或租约已过期都会被拒绝，从而防止过期 worker 覆盖新 worker 的状态。收到该错误直接放弃即可，事件会被再次投递。

**Q: `Replay` 返回 `Reconcile: true` 该怎么处理？**
A: 说明从游标位置无法连续读取历史：游标落在留存下界（retention floor）之前、游标处的 `EventID` 对不上，或流中出现序列缺口。此时不要把 `Events` 当完整历史用，应按 `Reset.MinimumCursor` 的指引先获取权威累积快照（`SnapshotRequired: true`），再从新游标继续订阅。

**Q: 消费端如何防止同一事件被处理两次？**
A: 投递语义是 at-least-once，用 `Inbox.Consume(ctx, consumer, envelope, handler)` 包住处理逻辑：同一 consumer 名下重复的 `EventID` 返回 `Duplicate: true` 且不再调用 handler；handler 返回错误时不留回执，重试仍会执行。注意 `Inbox` 是进程内实现，跨进程消费需要把去重回执持久化到消费者自己的存储。

**Q: `MemoryStore` 能直接用于生产吗？**
A: 它是加锁线性化的参考实现，适合测试和单进程部署，进程重启后数据即丢失。生产环境应实现 `Store` 接口并持久化，尤其要保证 `AppendBatch` 的原子性和租约字段的条件更新语义。
