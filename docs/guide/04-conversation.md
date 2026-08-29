# 4. 多轮对话与上下文

## 你现在遇到的问题

前三章每次 `Run` 都是独立的一轮。用户说"那就重启它"的时候，模型不知道"它"是什么——上一轮的内容根本没传过去。

## 历史怎么接

运行时不存任何东西，`RunResult.Messages` 里**只有这一轮新产生的** assistant 和 tool 消息。把它们接起来是你的事：

```go
type Transcript struct {
	System   agent.Message
	messages []agent.Message
}

// 一轮的输入 = system + 已存历史 + 这轮的问题
func (t *Transcript) Prompt(input string) []agent.Message {
	messages := make([]agent.Message, 0, len(t.messages)+2)
	messages = append(messages, t.System)
	messages = append(messages, t.messages...)
	return append(messages, agent.NewUserMessage(input))
}

func (t *Transcript) Commit(input string, result *agent.RunResult) bool {
	if result == nil || result.Outcome != agent.OutcomeCompleted {
		return false
	}
	t.messages = append(t.messages, agent.NewUserMessage(input))
	t.messages = append(t.messages, result.Messages...)
	return true
}
```

用起来：

```go
result, err := runner.Run(ctx, agent.RunRequest{Messages: transcript.Prompt(question)})
if err != nil {
	return err
}
if !transcript.Commit(question, result) {
	fmt.Printf("[这一轮未完成: %s/%s，不写入历史]\n", result.Outcome, result.StopReason)
}
```

**user message 由你保存，不在 `RunResult.Messages` 里。** 这是刻意的：只有你的应用知道一轮半途失败的对话该不该留下。

**没完成的轮次不能提交。** 它的 tool call 还没有对应结果，写进历史会让下一轮的输入不合法——模型会看到一个"调用了工具但没有结果"的消息序列。`Commit` 返回 false 就是在挡这件事。

存到哪里随你：内存切片、你自己的表、或者第 9 章的 `message` 子包。运行时只接收 `[]agent.Message`，换存储只改这个结构体的内部。

## 上下文装不下了怎么办

对话越长历史越长，迟早超出模型的上下文窗口。运行时能**发现**这件事——`Config.ContextWindow` 设了窗口大小之后，超预算会以 `OutcomeSuspended` + `StopReasonContextBudget` 停下：

```go
runner, err := agent.New(agent.Config{
	Key: "ops.assistant", ModelName: modelName, MaxSteps: 8,
	ContextWindow: 128_000,
}, model, registry)
```

但它不会替你压缩历史。压缩是策略，策略属于你。`context` 子包提供这套策略：

```go
import agentcontext "github.com/iceymoss/agent-runtime-go/context"
```

它做三件事：`NormalizeHistory` 修复不合法的历史（比如缺了结果的 tool call）；`Planner` 算出这轮该带哪些消息、会占多少 token；`Compactor` 在装不下时把旧消息压成摘要产物，并记录一个可验证的 pivot，让下次运行能证明自己是从哪一点接上的。

对话不长的应用先设好 `ContextWindow`，等真撞上再引入。

## 需要注意的

**token 计数默认是估算的。** 库不绑定任何 tokenizer，`context` 让你传自己的计数器。按字节粗算能跑，但接近窗口上限时会不准。

**压缩会丢信息，所以它是可验证的。** 压缩产物带 digest 和覆盖范围，恢复时会校验。自己实现摘要时别绕过这个校验，否则恢复出来的历史无法证明对应原始对话。

## 深入

- [context](../packages/context.md) —— 预算、归一化、压缩计划、pivot
- 完整可运行代码：[`examples/guide/conversation.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/conversation.go)
