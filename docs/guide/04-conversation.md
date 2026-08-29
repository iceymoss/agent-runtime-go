# 4. 多轮对话与上下文

## 你现在遇到的问题

前三章每次 `Run` 都是独立的一轮，模型不记得上一句。要做多轮对话，得自己把历史接起来——运行时不存任何东西。

## 历史怎么接

`RunResult.Messages` 里**只有这一轮新产生的** assistant 和 tool 消息。user message 由你保存。所以一轮完整的循环是：

```go
// 输入 = system + 已存历史 + 这轮的问题
messages := append([]agent.Message{systemMessage}, stored...)
messages = append(messages, agent.NewUserMessage(input))

result, err := runner.Run(ctx, agent.RunRequest{Messages: messages})
if err != nil {
	return err
}

// 只提交真正完成的轮次
if result.Outcome == agent.OutcomeCompleted {
	stored = append(stored, agent.NewUserMessage(input))
	stored = append(stored, result.Messages...)
}
```

user message 不在 `RunResult.Messages` 里是刻意的：只有你的应用知道一轮半途失败的对话该不该留下。

没完成的轮次不能提交——它的 tool call 还没有对应的结果，写进历史会让下一轮的输入不合法。

存到哪里随你：内存切片、你自己的表、或者第 9 章的 `message` 子包。运行时只接收 `[]agent.Message`。完整例子见 [`examples/chat`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/chat/main.go)。

## 上下文装不下了怎么办

对话越长，历史越长，迟早超出模型的上下文窗口。运行时能**发现**这件事——在 `Config.ContextWindow` 设了窗口大小之后，超预算会以 `OutcomeSuspended` + `StopReasonContextBudget` 停下——但它不会替你压缩历史。压缩是策略，策略属于你。

`context` 子包提供这套策略：token 预算计算、历史归一化、压缩计划、把旧对话换成一份摘要（pivot）。

```go
import agentcontext "github.com/iceymoss/agent-runtime-go/context"
```

它做三件事：`NormalizeHistory` 修复不合法的历史（比如缺了结果的 tool call）；`Planner` 算出这轮该带哪些消息、会占多少 token；`Compactor` 在装不下时把旧消息压成摘要产物，并记录一个可验证的 pivot，这样下次运行能证明自己是从哪一点接上的。

对话不长的应用可以完全不用它——先设好 `ContextWindow`，等真的撞上再引入。

## 需要注意的

**token 计数默认是估算的。** 库不绑定任何 tokenizer，`context` 让你传入自己的计数器。用字节数粗算能跑，但接近窗口上限时会不准，生产环境应该接真实的 tokenizer。

**压缩会丢信息，所以它是可验证的。** 压缩产物带 digest 和覆盖范围，恢复时会校验；如果你自己实现摘要，别绕过这个校验，否则恢复出来的历史无法证明对应原始对话。

## 深入

- [context](../packages/context.md) —— 预算、归一化、压缩计划、pivot 的完整语义
- [agent 的「持久化一段对话」](../packages/agent.md)
