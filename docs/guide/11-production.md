# 11. 上生产

## 你现在遇到的问题

功能齐了，还差两件事：运行过程中发生的事要**可靠地**告诉下游（计费、审计、通知），以及进程重启不能把跑到一半的运行直接掐掉。

## 可靠的事件

第 5 章的 `Observation` 是可丢的进度信号，不能承载正确性。要"这条一定送达"，用 `event`：

```go
func RecordRunFinished(ctx context.Context, store event.Store, sessionKey string, result *agent.RunResult) (event.Envelope, error) {
	payload, err := json.Marshal(map[string]any{
		"outcome": result.Outcome, "stop_reason": result.StopReason,
		"steps": len(result.Steps), "tokens": result.Usage.TotalTokens,
	})
	if err != nil {
		return event.Envelope{}, err
	}
	return store.Append(ctx, event.AppendCommand{Envelope: event.Envelope{
		TenantKey: "local",
		// EventID 是幂等锚点：同一个 id 追加两次是同一条事件，重试才安全
		EventID:       sessionKey + ":run-finished",
		StreamKey:     sessionKey,
		Type:          "agent.run.finished",
		SchemaVersion: 1,
		Reliability:   event.ReliabilityTerminal, // 终态事实，消费者不能丢
		AggregateType: "session",
		AggregateKey:  sessionKey,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}})
}
```

关键不在这段代码，而在**它在哪个事务里**：生产环境中 `Append` 要和你的业务写入同一个事务提交。否则总有一个窗口——业务成功了但事件丢了，或者反过来。这就是 outbox 存在的理由。

重放是这份成本换来的东西：宕机过的消费者、或者以后新加的消费者，可以从同一批事件重建自己的视图，而不用求每个生产者重发。

```go
result, err := store.Replay(ctx, event.ReplayQuery{
	Cursor: event.Cursor{TenantKey: "local", StreamKey: sessionKey},
	Limit:  limit,
})
```

## 就绪与优雅关闭

```go
func NewLifecycle(db *sql.DB, runs app.RunController, checkpoints app.Checkpointer, events app.Flusher) (*app.App, error) {
	return app.New(
		app.Config{Budgets: app.Budgets{
			Startup:    30 * time.Second,
			Drain:      20 * time.Second,
			Cancel:     5 * time.Second,
			Checkpoint: 10 * time.Second,
			Wait:       20 * time.Second,
			Flush:      5 * time.Second,
			Close:      5 * time.Second,
		}},
		app.Dependencies{Runs: runs, Checkpoints: checkpoints, Events: events},
		storeComponent{db: db},
	)
}
```

预算不是装饰。关闭顺序是**停止准入 → 取消 → 落 checkpoint → 等待 → 冲刷 → 关闭**，每一步有自己的截止时间，所以一个慢的 flush 不会吃掉留给"给还在跑的运行落 checkpoint"的时间。先停准入是关键：正在关闭的进程会拒绝新运行，而不是接下来又立刻掐掉它。

就绪探针要真的探到底层：

```go
func (c storeComponent) Ready(ctx context.Context) app.ComponentHealth {
	if err := c.db.PingContext(ctx); err != nil {
		return app.ComponentHealth{Configured: true, Reason: "database unreachable: " + err.Error()}
	}
	return app.ComponentHealth{Configured: true, Ready: true, Generation: "schema-v1"}
}
```

配置了数据库不等于数据库是通的。一个"配置了就报 ready"的探针比没有探针更糟——它让坏掉的部署看起来是健康的。

`Dependencies` 里那三个是你的：什么算一次运行、怎么落 checkpoint、怎么冲刷待发事件。库定的是**顺序**，不是含义。

## 还需要你自己做的

- **凭据**：从哪读、怎么轮换
- **限流与配额**：谁能跑多少
- **多租户隔离**：`TenantKey` 只是标签，真正的隔离在你的存储和鉴权里
- **可观测性**：指标、追踪、日志脱敏

## 深入

- [event](../packages/event.md) —— 总线、outbox、inbox、投递语义、重放
- [app](../packages/app.md) —— 组件图、就绪、有界关闭
- 完整可运行代码：[`examples/guide/advanced/events.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/events.go) 与 [`lifecycle.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/lifecycle.go)
- [生产组合模式](../production.md)
