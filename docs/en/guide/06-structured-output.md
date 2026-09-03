# 6. Output your code can consume

## Where you are stuck

The ops assistant also has to file a ticket, which means producing structured data rather than prose. One "Sure, here is the classification:" in front of the JSON and your `json.Unmarshal` fails.

A prompt saying "output only JSON" does not hold. The provider has to enforce it.

## Constraining the model's output

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
			agent.NewSystemMessage("Summarize these logs into one incident record."),
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
		return Incident{}, fmt.Errorf("the model did not return valid JSON: %w", err)
	}
	return incident, nil
}
```

`ResponseFormat` is set on `RunRequest` rather than `Config`, so the same assistant can hold an ordinary conversation and constrain its output only where a caller needs machine-readable data. On `Config` it would mean "this agent always emits JSON".

`ResponseFormatJSON` only requires valid JSON and is supported more widely; `ResponseFormatJSONSchema` requires output conforming to the schema.

If the model does not declare `Capabilities.StructuredOutput`, `agent.New` returns `ErrAgentConfigInvalid` — assembly-time failure, not a surprise halfway through.

**The runtime does not validate the output against the schema.** Enforcement is the provider's job, which is why the code above still unmarshals and checks the error. That is not redundant.

## Reasoning models

DeepSeek-R1, Qwen3-thinking, and the o-series report their thinking separately from their answer:

```go
answer := result.Text                       // the answer alone
thinking := result.Messages[0].Reasoning()  // the chain of thought
```

While streaming it arrives as `ObservationReasoningDelta` (chapter 5).

Two rules. `Message.Text()` excludes reasoning, so `RunResult.Text` is always the clean answer. And **adapters do not send reasoning back to the model** — the providers that emit it reject it as assistant input, and replaying a chain of thought as conversation changes the question being answered. History keeps it for display and audit.

A model that produces reasoning without declaring `Capabilities.Reasoning` is a protocol error: the application has to decide whether to render or redact it before the first token arrives.

## Going deeper

- [agent package reference](../packages/agent.md) — `ResponseFormat` and reasoning in full
- Complete runnable code: [`examples/guide/classify.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/classify.go)
