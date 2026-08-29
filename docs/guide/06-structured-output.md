# 6. 让输出能被代码消费

## 你现在遇到的问题

前面的答案都是给人读的。但很多 agent 的输出要交给代码——抽取发票字段、给工单分类、决定路由到哪个队列。这时候模型在 JSON 外面多说一句"好的，这是您要的结果："，你的 `json.Unmarshal` 就炸了。

只靠 prompt 说"只输出 JSON"扛不住。要让供应商去强制。

## 约束模型的输出

```go
runner, err := agent.New(agent.Config{
	Key: "extract", ModelName: "gpt-4o-mini", MaxSteps: 4,
	ResponseFormat: &agent.ResponseFormat{
		Kind:   agent.ResponseFormatJSONSchema,
		Name:   "invoice",
		Schema: json.RawMessage(`{"type":"object","properties":{"total":{"type":"number"}}}`),
		Strict: true,
	},
}, model, registry)
```

`ResponseFormatJSON` 只要求合法 JSON，兼容面更广；`ResponseFormatJSONSchema` 要求符合给定 schema。

`RunRequest.ResponseFormat` 可以按次覆盖——同一个 agent 平时正常对话，只在需要机器可读输出的那一次加约束。

模型没声明 `Capabilities.StructuredOutput` 时，`agent.New` 直接返回 `ErrAgentConfigInvalid`：装配期失败，不是跑到一半才发现。

**运行时不校验模型输出是否真的符合 schema**，强制是供应商的职责。真正要确定的应用仍然要自己 `Unmarshal` 并检查。

## 推理模型

DeepSeek-R1、Qwen3-thinking、o 系列会把思考过程和答案分开返回。运行时用 `PartReasoning` 承载思考过程：

```go
result, _ := runner.Run(ctx, agent.RunRequest{Messages: messages})

answer := result.Text                       // 只有答案
thinking := result.Messages[0].Reasoning()  // 思考过程
```

流式时通过 `ObservationReasoningDelta` 单独下发。

两条规则：`Message.Text()` 不包含 reasoning，所以 `RunResult.Text` 永远是干净的答案；**适配器不会把 reasoning 回传给模型**——产生它的供应商基本都会拒绝把自己的思考当 assistant 输入，而且把思维链当对话重放会改变模型在回答的问题。历史里保留它是为了展示和审计。

模型产出 reasoning 但没声明 `Capabilities.Reasoning` 会被判为协议错误：应用要在第一个 token 到达之前就知道该不该渲染或脱敏。

## 深入

- [agent 包参考](../packages/agent.md) —— `ResponseFormat` 与 reasoning 的完整语义
