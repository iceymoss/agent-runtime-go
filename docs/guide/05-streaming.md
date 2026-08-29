# 5. 把过程给用户看

## 你现在遇到的问题

`Run` 要等整轮跑完才返回。用户盯着一个转圈的图标等十几秒，而模型其实早就开始吐字了。

## 订阅进度

`ObservationEmitter` 在运行过程中报告文本增量、工具调用、工具结果、步骤完成：

```go
emitter := agent.NewObservationEmitterWith(
	agent.ObservationOptions{QueueSize: 64, Lossless: true},
	func(o agent.Observation) {
		switch o.Type {
		case agent.ObservationTextDelta:
			fmt.Print(o.Text)
		case agent.ObservationToolCall:
			fmt.Printf("\n[调用 %s]\n", o.ToolCall.Name)
		}
	})

result, err := runner.Run(ctx, agent.RunRequest{
	Messages:           messages,
	ObservationEmitter: emitter,
})
emitter.Close()
```

## 为什么必须写 Lossless

`agent.NewObservationEmitter`（不带 Options 的那个）是**有界、非阻塞、会丢的**：队列满了就丢弃并计数，绝不阻塞模型推进。这对进度遥测是对的取舍。

对"人正在读的文本"是错的。任何比模型慢的消费者——SSE、WebSocket、慢客户端，全都算——都会让增量被静默丢掉，读者看到一个被截断的回答，而 `RunResult.Text` 是完整的，你在日志里什么都看不出来。

`Lossless: true` 让入队等待有空位。代价是真实的：慢消费者会拖慢这次运行，所以 `consume` 要保持廉价——把观察值交给带缓冲的 writer，不要在里面直接做网络写。等待受运行的 context 约束，运行被取消时不会卡在停止读取的消费者上。

拿不准就看计数：

```go
if emitter.Dropped() > 0 {
	// 观察值不完整
}
```

## 需要注意的

**`Observation` 是进度，不是权威。** 终态只看 `RunResult` 和 error。需要可靠的事件投递——比如要投给下游系统、要能重放——用第 11 章的 `event` 子包。

**推理模型的思维链是单独一路。** `ObservationReasoningDelta` 和 `ObservationTextDelta` 分开，UI 可以折叠或隐藏思考过程，不用去猜自己拿到的是哪半边。见[第 6 章](./06-structured-output.md)。

## 深入

- [agent 包参考](../packages/agent.md) —— 四种 Observation 的完整语义
