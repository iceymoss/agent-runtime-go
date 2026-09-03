# 5. 把过程给用户看

## 你现在遇到的问题

`Run` 要等整轮跑完才返回。运维助手查日志再回答要十几秒，用户全程盯着一个不动的光标。

## 订阅进度

```go
func NewProgressEmitter(out io.Writer) *agent.ObservationEmitter {
	buffered := bufio.NewWriter(out)
	return agent.NewObservationEmitterWith(
		agent.ObservationOptions{QueueSize: 64, Lossless: true},
		func(observation agent.Observation) {
			switch observation.Type {
			case agent.ObservationTextDelta:
				fmt.Fprint(buffered, observation.Text)
			case agent.ObservationReasoningDelta:
				// 推理模型的思考是单独一路，UI 可以折叠或直接丢掉
			case agent.ObservationToolCall:
				fmt.Fprintf(buffered, "\n  [调用 %s %s]\n", observation.ToolCall.Name, observation.ToolCall.Input)
			case agent.ObservationStepFinished:
				buffered.Flush()
			}
		})
}
```

```go
emitter := NewProgressEmitter(os.Stdout)
result, err := runner.Run(ctx, agent.RunRequest{
	Messages:           transcript.Prompt(question),
	ObservationEmitter: emitter,
})
emitter.Close()
```

用户现在看到的是：

```text
  [调用 read_logs {"service":"checkout","level":"WARN"}]
checkout 的支付网关连续超时，连接池也被打满了。建议重启 checkout。
```

## 为什么必须写 Lossless

`agent.NewObservationEmitter`（不带 Options 的那个）是**有界、非阻塞、会丢的**：队列满了就丢弃并计数，绝不阻塞模型推进。这对进度遥测是对的取舍。

对"人正在读的文本"是错的。**任何比模型慢的消费者都会让增量被静默丢掉**——SSE、WebSocket、慢客户端全都算。读者看到一个被截断的回答，而 `RunResult.Text` 是完整的，你的日志里什么都看不出来。

`Lossless: true` 让入队等待有空位。代价是真实的：慢消费者会拖慢这次运行。所以上面那段用了 `bufio.Writer`——**把观察值交给缓冲，不要在回调里直接做网络写**。等待受运行的 context 约束，运行被取消时不会卡在停止读取的消费者上。

拿不准就看计数：

```go
if emitter.Dropped() > 0 {
	// 用户看到的不是完整答案
}
```

## 需要注意的

**`Observation` 是进度，不是权威。** 终态只看 `RunResult` 和 error。要"这条一定送达下游"，用第 11 章的 `event`。

**`Close()` 要调，而且要在读 `result` 之前。** 它停止接收并等待队列排空；不调的话最后几个增量可能还没被消费。

## 深入

- [agent 包参考](../packages/agent.md) —— 五种 Observation 的完整语义
- 完整可运行代码：[`examples/guide/streaming.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/streaming.go)
