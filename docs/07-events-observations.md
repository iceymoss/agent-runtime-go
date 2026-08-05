# Event 与 Observations

Observation 面向实时体验，Event 面向权威事实。两者可以描述相似动作，但可靠性语义完全不同。

## Observations

根 `ObservationEmitter` 用于 UI、终端增量和 metrics：

```go
emitter := agent.NewObservationEmitter(64, consume)
result, err := runner.Run(ctx, agent.RunRequest{
    Messages: messages,
    ObservationEmitter: emitter,
})
emitter.Close()
```

它有界、非阻塞、允许丢失，不包含权威 terminal success/error。

## Persisted Events

`event.Store` 提供 stable event ID、per-stream sequence、atomic batch、outbox lease、ack/nack 和 replay gap semantics。

权威事件应与拥有 aggregate 的 transaction 一起写入。iCoder 将 terminal event 与 session revision、messages 和 usage 放在同一个 SQLite transaction，读取时投影成 `event.Envelope`。

生产 outbox adapter 必须实现：

- identical EventID retry 返回原 envelope。
- conflicting EventID 返回 idempotency conflict。
- sequence 按 `(tenant, stream)` 单调分配。
- claim/ack/nack 比较 owner、token 和 fence。
- replay 不能静默跳过 retention gap。

需要后台投递时组合 `event.Dispatcher` 和应用实现的 `Publisher`。不要把 store 自己的 append 当作外部 publish。

## 如何选择

| 需求 | 使用 |
|---|---|
| UI 展示“正在调用工具” | Observation |
| Metrics 或可丢弃 trace | Observation |
| 计费、审计、业务状态变更 | Event |
| 跨服务投递与重试 | Event + outbox |
| 消费方幂等处理 | Event + inbox |

消费方必须以 `RunResult`、返回 error 和 persisted state 判断终态，不能根据最后一条 Observation 推断成功。
