# providers/openaicompat

Official model adapter: any OpenAI-compatible `/chat/completions` API (OpenAI, DeepSeek, Qwen, Kimi, OpenRouter, vLLM, Ollama, and others) plugs in with no protocol code of your own.

```go
import "github.com/iceymoss/agent-runtime-go/providers/openaicompat"
```

## What it is

`openaicompat.Model` is a full implementation of `agent.Model`. You give it a base URL and API key; it handles:

- **SSE streaming:** streams from upstream by default, turning text deltas and tool-call fragments into canonical `agent.StreamChunk`s in real time;
- **Tool-call reassembly:** upstream may split one tool call's JSON args across many deltas; the adapter reassembles by index into a complete call before handing it to the runtime;
- **Usage normalization:** maps inconsistent token fields across vendors (including cached and reasoning tokens) into `agent.Usage` so component sums equal `TotalTokens`;
- **Error classification:** HTTP failures become `agent.ModelError` with retry semantics—401/403 → auth (not retryable), 429 → rate limit (retryable), 5xx → transport (retryable), other 4xx → rejected (not retryable).

```go
func New(baseURL, apiKey string, opts ...Option) *Model
```

`baseURL` is the API root (e.g. `https://api.openai.com/v1`); the adapter appends `/chat/completions`. An empty `apiKey` skips the `Authorization` header, which fits local Ollama, vLLM, and similar.

## Why you need it

Implementing `agent.Model` yourself means SSE parsing, fragment reassembly, terminal consistency (all stream deltas must match the final Response; the runtime checks), vendor usage dialects, and mapping HTTP errors into retry semantics the runtime understands. Every integrator rewrites and re-trips that code.

**When you do not need it:** if upstream is not OpenAI protocol (e.g. Anthropic Messages API), implement `agent.Model` yourself; see [agent](agent.md).

## How to use it

Minimal wiring (three lines):

```go
model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))
runner, err := agent.New(agent.Config{
	Key: "app.assistant", ModelName: "deepseek-chat", MaxSteps: 8,
}, model, registry)
```

`Config.ModelName` is sent as the request `model` field, so one adapter instance can serve multiple Agents with different model names.

Common endpoint patterns:

```go
// OpenAI
openaicompat.New("https://api.openai.com/v1", key)
// DeepSeek
openaicompat.New("https://api.deepseek.com/v1", key)
// Local Ollama (no auth)
openaicompat.New("http://127.0.0.1:11434/v1", "")
// OpenRouter (attribution headers)
openaicompat.New("https://openrouter.ai/api/v1", key,
	openaicompat.WithHeader("HTTP-Referer", "https://your-app.example"),
	openaicompat.WithHeader("X-Title", "Your App"))
```

### Options

| Option | Purpose |
|---|---|
| `WithName(name)` | Override the id returned by `Model.Name()` (default `openai-compatible`) |
| `WithHTTPClient(c)` | Custom timeout, proxy, transport retries (default client timeout 5 minutes) |
| `WithCapabilities(caps)` | Declare capabilities when they differ from defaults (default: tools and all tool-choice modes) |
| `WithHeader(k, v)` | Attach a custom header on every request |
| `WithoutStreaming()` | Use non-streaming requests and synthesize a chunk sequence from the full response; for unreliable SSE upstreams |
| `WithoutStreamUsage()` | Do not send `stream_options.include_usage`; for upstreams that reject that field (streaming-step usage will be zero) |

Runnable example (in-repo; any OpenAI-compatible endpoint):

```bash
OPENAI_API_KEY=sk-... go run ./examples/openai-compat
```

Sample output (varies by model):

```text
[tool call] get_time {"timezone":"Asia/Shanghai"}
The current date and time in Shanghai is Thu, 06 Aug 2026 12:45:10 CST.

[gpt-4o-mini] stop=complete steps=2 tokens=421
```

## FAQ

**Q: Upstream streaming often drops or is nonstandard—what then?**
Add `openaicompat.WithoutStreaming()`. The adapter sends non-streaming requests and synthesizes a chunk sequence locally; fully transparent to the runtime—at the cost of no per-token live output.

**Q: Errors saying the `stream_options` field is unknown?**
Some self-hosted services (older vLLM, etc.) do not support this OpenAI extension. Add `openaicompat.WithoutStreamUsage()` to drop it; streaming-step usage will be zero—pull billing stats from upstream logs if needed.

**Q: How do I know if an error is retryable?**
All upstream failures are `agent.ModelError`; use `errors.As`, then check `Retryable` and `Kind` (auth / rate_limit / transport / rejected / protocol). 429 and 5xx are marked retryable; auth errors and 4xx rejects are not.

**Q: What if the model does not support named tool choice?**
Declare real capabilities with `WithCapabilities`, e.g. `agent.Capabilities{Tools: true, ToolChoiceNone: true}`. The runtime checks required capabilities at assembly and rejects mismatched combos instead of silently downgrading at runtime.

**Q: Are images supported?**
Current wire projection covers text, tool calls, and tool results only. For multimodal, implement `agent.Model` yourself, or wait for a later release.

**Q: How to organize multiple providers in one process?**
One `openaicompat.New` instance per provider (different baseURL/key), distinguished with `WithName`. For name-based routing and directory management, use the [provider subpackage](provider.md).
