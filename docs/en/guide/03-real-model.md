# 3. Connecting a real model

## Where you are stuck

The first two chapters used a model with a hard-coded answer. Now you want a real one — without being locked to a single vendor.

## Any OpenAI-compatible endpoint

OpenAI, DeepSeek, Qwen, Kimi, OpenRouter, vLLM, and Ollama all speak OpenAI's `/chat/completions`, so the official adapter works without implementing `Model` yourself:

```go
import "github.com/iceymoss/agent-runtime-go/providers/openaicompat"

model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

runner, err := agent.New(agent.Config{
	Key: "my.assistant", ModelName: "deepseek-chat", MaxSteps: 8,
}, model, registry)
```

Switching provider is two values: `baseURL` and `ModelName`. For a local Ollama, leave the key empty (`openaicompat.New("http://localhost:11434/v1", "")`) and the adapter omits the `Authorization` header.

The adapter handles SSE streaming, reassembles tool calls split across deltas, normalizes usage (including cache and reasoning tokens), parses `Retry-After`, and classifies HTTP failures into `agent.ModelError` with retry semantics.

Replace chapter 1's fake model with this line, add chapter 2's tool, and you have a working agent. The complete program is [`examples/openai-compat`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/openai-compat/main.go).

## Adding retries

Providers wobble. Wrap the model:

```go
import "github.com/iceymoss/agent-runtime-go/providers/retry"

model := retry.New(openaicompat.New(baseURL, apiKey), retry.Options{})
```

A zero `Options{}` means 3 attempts, 200ms base, 30s cap, with jitter. A provider's `Retry-After` wins over the backoff curve.

Writing this yourself is easy to get wrong: **wrapping only what `Stream()` returns retries almost nothing.** An SSE request usually succeeds — the HTTP response arrives, you get the channel — and then the connection drops partway through. That failure lives *inside* the stream, where such a wrapper never looks. This package retries both.

It deliberately does **not** retry once content has reached the caller: grafting a fresh stream onto a half-delivered one either fails the runtime's consistency check or, worse, passes it with the text duplicated. The error is forwarded and you decide whether to rerun the turn.

## More than one model

Applications often need two: a capable model to answer and a cheap one to summarize. The `provider` subpackage keeps that catalog and hands out models by role, with credentials passed in explicitly:

```go
import "github.com/iceymoss/agent-runtime-go/provider"
```

With a single model you do not need it — holding an `agent.Model` is enough.

## Versioned prompts

A system prompt hard-coded as a string literal means nobody can say which version production is running. The `prompt` subpackage gives templates a version you can record with each run:

```go
import "github.com/iceymoss/agent-runtime-go/prompt"
```

Also optional. `agent.NewSystemMessage("...")` from chapter 1 works; reach for this when you need to answer "which prompt produced that run last week".

## Worth knowing

**`ModelName` is the `model` field sent upstream**, so one adapter instance can serve several agents using different model names.

**Narrow the declared capabilities with `WithCapabilities` when your endpoint cannot honor them.** The adapter declares tools, all three tool-choice modes, structured output, and reasoning by default. When the declaration and reality disagree, the runtime refuses illegal requests at assembly — better than discovering it mid-conversation.

**Credentials are passed in by you.** The adapter reads no environment variables and no config files; the `os.Getenv` above is *your application* doing that.

## Going deeper

- [openaicompat](../packages/openaicompat.md) — every option: non-streaming fallback, custom headers, timeouts and proxies
- [retry](../packages/retry.md) — what is retried, what is not, and why
- [provider](../packages/provider.md) — multi-model catalog and factory
- [prompt](../packages/prompt.md) — templates and versions
