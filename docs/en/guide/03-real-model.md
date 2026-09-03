# 3. Connecting a real model

## Where you are stuck

The first two chapters used a model with a hard-coded answer. Now you want a real one — without being locked to a vendor, and without one provider hiccup failing the whole turn.

## Any OpenAI-compatible endpoint

OpenAI, DeepSeek, Qwen, Kimi, OpenRouter, vLLM, and Ollama all speak OpenAI's `/chat/completions`, so the official adapter works directly:

```go
func NewModel() (model agent.Model, name string) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return &scriptedModel{}, "scripted-v1"
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = "gpt-4o-mini"
	}
	// retry wraps the adapter rather than the other way round: it needs the
	// adapter's error classification to decide what is worth repeating.
	return retry.New(openaicompat.New(baseURL, key), retry.Options{}), modelName
}
```

Switching provider is two values: `baseURL` and `ModelName`. For a local Ollama, leave the key empty (`openaicompat.New("http://localhost:11434/v1", "")`) and the adapter omits the `Authorization` header.

**Credentials are read by the application, not the library.** The `os.Getenv` above is your code — the runtime reads no environment variables and no config files, so this function is the only place that knows where the key lives.

Keeping a fake model for the no-key case pays off: examples, tests, and CI all run without real credentials.

## Adding retries

```go
model := retry.New(openaicompat.New(baseURL, key), retry.Options{})
```

A zero `Options{}` means 3 attempts, 200ms base, 30s cap, with jitter. A provider's `Retry-After` wins over the backoff curve.

Writing this yourself is easy to get wrong: **wrapping only what `Stream()` returns retries almost nothing.** An SSE request usually succeeds — the HTTP response arrives, you get the channel — and then the connection drops partway through. That failure lives *inside* the stream, where such a wrapper never looks. This package retries both.

It deliberately does **not** retry once content has reached the caller: grafting a fresh stream onto a half-delivered one either fails the runtime's consistency check or, worse, passes it with the text duplicated. The error is forwarded and you decide whether to rerun the turn.

## Writing your own adapter

A protocol that is not OpenAI-compatible (Anthropic Messages, Bedrock) means implementing `Model`'s three methods yourself. One rule is easy to trip over: **the streamed chunks must add up to exactly the terminal response.**

An adapter that already holds the whole answer hands it to `agent.StreamResponse`, which produces a compliant chunk sequence:

```go
func say(text string) <-chan agent.StreamChunk {
	message := agent.NewAssistantMessage(text)
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{
		Message: message, FinishReason: agent.FinishStop, ModelName: "scripted-v1",
		Usage: agent.Usage{PromptTokens: 20, CompletionTokens: 12, TotalTokens: 32},
	})
}
```

An adapter that genuinely streams token by token emits its own deltas. Verify it with the conformance suite from [chapter 12](./12-testing.md) — wiring it into the library's own `openaicompat` found three real defects.

## Several models, versioned prompts

Applications often need two models: a capable one to answer and a cheap one to summarize. `provider` keeps that catalog and hands out models by role.

A system prompt hard-coded as a string literal means nobody can say which version production is running; `prompt` gives templates a version you can record with each run.

Both are optional — one model plus `agent.NewSystemMessage("...")` works. Reach for them when you need to answer "which prompt produced that run last week".

## Worth knowing

**`ModelName` is the `model` field sent upstream**, so one adapter instance can serve several agents using different model names.

**Narrow the declared capabilities with `WithCapabilities` when your endpoint cannot honor them.** The adapter declares tools, all three tool-choice modes, structured output, and reasoning by default. When declaration and reality disagree, the runtime refuses illegal requests at assembly — better than discovering it mid-conversation.

## Going deeper

- [openaicompat](../packages/openaicompat.md) — non-streaming fallback, custom headers, timeouts and proxies
- [retry](../packages/retry.md) — what is retried, what is not, and why
- [provider](../packages/provider.md) · [prompt](../packages/prompt.md)
- Complete runnable code: [`examples/guide/model.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/model.go)
