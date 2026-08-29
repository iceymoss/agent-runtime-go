# 6. 让输出能被代码消费

## 你现在遇到的问题

运维助手还要把事件写进工单系统，那就得产出结构化数据而不是一段话。模型在 JSON 外面多说一句"好的，这是分类结果："，你的 `json.Unmarshal` 就炸了。

只靠 prompt 说"只输出 JSON"扛不住。要让供应商去强制。

## 约束模型的输出

```go
type Incident struct {
	Service  string `json:"service"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

var incidentSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "service":  {"type": "string"},
    "severity": {"type": "string", "enum": ["low", "high"]},
    "summary":  {"type": "string"}
  },
  "required": ["service", "severity", "summary"],
  "additionalProperties": false
}`)

func Classify(ctx context.Context, runner *agent.Agent, logs string) (Incident, error) {
	result, err := runner.Run(ctx, agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("把日志归纳成一条事件记录。"),
			agent.NewUserMessage(logs),
		},
		ResponseFormat: &agent.ResponseFormat{
			Kind: agent.ResponseFormatJSONSchema, Name: "incident",
			Schema: incidentSchema, Strict: true,
		},
	})
	if err != nil {
		return Incident{}, err
	}
	var incident Incident
	if err := json.Unmarshal([]byte(result.Text), &incident); err != nil {
		return Incident{}, fmt.Errorf("模型返回的不是合法 JSON: %w", err)
	}
	return incident, nil
}
```

`ResponseFormat` 放在 `RunRequest` 上而不是 `Config` 上，所以同一个助手平时正常对话，只在需要机器可读输出的那一次加约束。放在 `Config` 上就是"这个 agent 永远输出 JSON"。

`ResponseFormatJSON` 只要求合法 JSON，兼容面更广；`ResponseFormatJSONSchema` 要求符合 schema。

模型没声明 `Capabilities.StructuredOutput` 时 `agent.New` 直接返回 `ErrAgentConfigInvalid`——装配期失败，不是跑到一半才发现。

**运行时不校验模型输出是否真的符合 schema**，强制是供应商的职责。所以上面仍然 `Unmarshal` 并检查错误——这不是多余的。

## 推理模型

DeepSeek-R1、Qwen3-thinking、o 系列会把思考过程和答案分开返回：

```go
answer := result.Text                       // 只有答案
thinking := result.Messages[0].Reasoning()  // 思考过程
```

流式时通过 `ObservationReasoningDelta` 单独下发（第 5 章）。

两条规则：`Message.Text()` 不包含 reasoning，所以 `RunResult.Text` 永远是干净的答案；**适配器不会把 reasoning 回传给模型**——产生它的供应商基本都会拒绝把自己的思考当 assistant 输入，而且把思维链当对话重放会改变模型在回答的问题。历史里保留它是为了展示和审计。

模型产出 reasoning 但没声明 `Capabilities.Reasoning` 会被判为协议错误：应用要在第一个 token 到达之前就知道该不该渲染或脱敏。

## 深入

- [agent 包参考](../packages/agent.md) —— `ResponseFormat` 与 reasoning 的完整语义
- 完整可运行代码：[`examples/guide/classify.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/classify.go)
