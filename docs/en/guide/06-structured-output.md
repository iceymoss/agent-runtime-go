# 6. Output your code can consume

## Where you are stuck

Every answer so far was written for a person to read. But many agents produce output for code — extracting invoice fields, classifying a ticket, deciding which queue to route to. One "Sure, here is the result:" in front of the JSON and your `json.Unmarshal` fails.

A prompt saying "output only JSON" does not hold. The provider has to enforce it.

## Constraining the model's output

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

`ResponseFormatJSON` only requires valid JSON and is supported more widely; `ResponseFormatJSONSchema` requires output conforming to the schema.

`RunRequest.ResponseFormat` overrides per call, so one agent can chat normally and constrain its output only where a caller needs machine-readable data.

If the model does not declare `Capabilities.StructuredOutput`, `agent.New` returns `ErrAgentConfigInvalid` — assembly-time failure, not a surprise halfway through.

**The runtime does not validate the output against the schema.** Enforcement is the provider's job, so an application that must be certain still unmarshals and checks.

## Reasoning models

DeepSeek-R1, Qwen3-thinking, and the o-series report their thinking separately from their answer. The runtime carries it as `PartReasoning`:

```go
result, _ := runner.Run(ctx, agent.RunRequest{Messages: messages})

answer := result.Text                       // the answer alone
thinking := result.Messages[0].Reasoning()  // the chain of thought
```

While streaming it arrives as `ObservationReasoningDelta`.

Two rules. `Message.Text()` excludes reasoning, so `RunResult.Text` is always the clean answer. And **adapters do not send reasoning back to the model** — the providers that emit it reject it as assistant input, and replaying a chain of thought as conversation changes the question being answered. History keeps it for display and audit.

A model that produces reasoning without declaring `Capabilities.Reasoning` is a protocol error: the application has to decide whether to render or redact it before the first token arrives.

## Going deeper

- [agent package reference](../packages/agent.md) — `ResponseFormat` and reasoning in full
