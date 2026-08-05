# Event 与 Observations

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

`agent/event.Store` 提供 stable event ID、per-stream sequence、atomic batch、outbox lease、ack/nack 和 replay gap semantics。

权威事件应与拥有 aggregate 的 transaction 一起写入。iCoder 将 terminal event 与 session revision、messages 和 usage 放在同一个 SQLite transaction，读取时投影成 `event.Envelope`。

生产 outbox adapter 必须实现：

- identical EventID retry 返回原 envelope。
- conflicting EventID 返回 idempotency conflict。
- sequence 按 `(tenant, stream)` 单调分配。
- claim/ack/nack 比较 owner、token 和 fence。
- replay 不能静默跳过 retention gap。

需要后台投递时组合 `event.Dispatcher` 和应用实现的 `Publisher`。不要把 store 自己的 append 当作外部 publish。
