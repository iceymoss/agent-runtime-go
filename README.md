<p align="center">
  <img src="docs-site/public/logo.svg" width="112" height="112" alt="Agent Runtime for Go logo">
</p>

<h1 align="center">Agent Runtime for Go</h1>

English | [简体中文](README.zh-CN.md)

[![CI](https://github.com/iceymoss/agent-runtime-go/actions/workflows/ci.yml/badge.svg)](https://github.com/iceymoss/agent-runtime-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/iceymoss/agent-runtime-go.svg)](https://pkg.go.dev/github.com/iceymoss/agent-runtime-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/iceymoss/agent-runtime-go)](https://goreportcard.com/report/github.com/iceymoss/agent-runtime-go)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A composable, provider-agnostic agent runtime for Go.

The runtime executes the model/tool loop reliably; your application owns model adapters, prompts, tools, permissions, storage, and business APIs. Use just the root package for a single run, or add Session, Durable, MCP, Skills, and Sub-Agent capabilities as you need them.

> The project is currently at `v0.x` and the API may still change. Pin a version when depending on it.

## What it does

- Unified contracts for messages, images, models, streamed responses, and usage
- Multi-step model/tool loop with JSON Schema validation and tool allowlists
- Max steps, stop conditions, context budgets, and tool loop detection
- Lossy progress observations by default, with a lossless mode for text streamed to a reader
- Optional Session, Durable, Permission, Event, MCP, Skills, and Sub-Agent packages
- Immutable runtime definitions and reproducible artifact digests
- Testable provider/tool ports with no coupling to OpenAI, Anthropic, or any database

## Installation

```bash
go get github.com/iceymoss/agent-runtime-go
```

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

## Two-minute run

The repository ships a deterministic example that needs no API key:

```bash
go run ./examples/hello
```

Output:

```text
Hello from Agent Runtime for Go.
```

A minimal call takes three steps:

```go
runner, err := agent.New(agent.Config{
	Key:          "example.hello",
	ModelName:    "my-model",
	MaxSteps:     4,
	AllowedTools: []string{},
}, model, agent.NewRegistry())
if err != nil {
	return err
}

result, err := runner.Run(ctx, agent.RunRequest{Messages: []agent.Message{
	agent.NewSystemMessage("Answer clearly."),
	agent.NewUserMessage("Say hello."),
}})
if err != nil {
	return err
}
fmt.Println(result.Text)
```

Here `model` implements the root package's single model port:

```go
type Model interface {
	Name() string
	Capabilities() agent.Capabilities
	Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error)
}
```

A provider adapter projects `GenerateRequest` into the upstream protocol and converts upstream responses into canonical `StreamChunk` values.

The runtime requires the streamed chunks to add up to exactly the terminal response, so an adapter that already holds the whole answer — a non-streaming endpoint, a cached reply, a test fake — should return `agent.StreamResponse(response)` rather than assemble chunks by hand:

```go
func (m myModel) Stream(ctx context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	response, err := m.callUpstream(ctx, req)
	if err != nil {
		return nil, err
	}
	return agent.StreamResponse(response), nil
}
```

An adapter that genuinely streams emits its own deltas and finishes with one `ChunkFinish` whose response matches them. See [`examples/hello`](examples/hello/main.go) for a complete minimal implementation.

## Connecting a real model

Any OpenAI-compatible API (OpenAI, DeepSeek, Qwen, Kimi, vLLM, Ollama, ...) works out of the box with the official `providers/openaicompat` adapter — no need to implement `Model` yourself:

```go
import "github.com/iceymoss/agent-runtime-go/providers/openaicompat"

model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

runner, err := agent.New(agent.Config{
	Key:       "example.assistant",
	ModelName: "deepseek-chat",
	MaxSteps:  8,
}, model, agent.NewRegistry())
```

The adapter streams over SSE by default, assembles tool call fragments, normalizes usage (including cache tokens), and classifies failures as `agent.ModelError`. Options:

- `openaicompat.WithoutStreaming()`: use non-streaming requests for providers with unreliable SSE
- `openaicompat.WithCapabilities(...)`: declare capabilities that differ from the defaults
- `openaicompat.WithHeader(k, v)`: attach custom request headers (for example OpenRouter attribution)
- `openaicompat.WithHTTPClient(...)`: customize timeouts, proxies, and the transport

A runnable example that works with any OpenAI-compatible endpoint:

```bash
OPENAI_API_KEY=sk-... go run ./examples/openai-compat
```

Other protocols (such as the Anthropic Messages API) are integrated by implementing the `Model` interface.

## Model/tool loop

Run the complete example with tools:

```bash
go run ./examples/tool-agent
```

Execution flow:

```text
user message
  -> model requests get_weather
  -> runtime validates JSON Schema
  -> runtime executes get_weather
  -> tool result is appended to messages
  -> model returns the final answer
```

The easiest way to define a tool is the generic helper `agent.NewTool`, which generates the JSON Schema from a struct:

```go
type WeatherInput struct {
	City string `json:"city" description:"City name"`
	Days int    `json:"days,omitempty" description:"Forecast days"`
}

tool := agent.MustNewTool("get_weather", "Get the current weather for a city.",
	func(ctx context.Context, input WeatherInput) (agent.ToolResult, error) {
		return agent.ToolResult{Content: lookupWeather(input.City)}, nil
	})

registry := agent.NewRegistry()
_ = registry.Register(tool)
```

Non-pointer fields without `omitempty` become `required` automatically; the schema is strict by default (undeclared properties are rejected and fed back to the model for repair). When you need full control over the schema, implement the `Tool` interface directly:

```go
type Tool interface {
	Definition() agent.ToolDefinition
	ReplayPolicy() agent.ReplayPolicy
	Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error)
}
```

See [`examples/tool-agent`](examples/tool-agent/main.go) for the full code. The example uses a deterministic model so the two-step loop is easy to observe; swap in a real `agent.Model` implementation to go live.

## Wrapping your own agent

Do not let HTTP handlers, CLIs, or business services assemble prompts and tools everywhere. Provide a purpose-built facade instead:

```go
type SupportAgent struct {
	runner *agent.Agent
}

type SupportAgentConfig struct {
	Model agent.Model
	CRM   CRM
}

func NewSupportAgent(cfg SupportAgentConfig) (*SupportAgent, error) {
	registry := agent.NewRegistry()
	if err := registry.Register(newFindOrderTool(cfg.CRM)); err != nil {
		return nil, err
	}

	runner, err := agent.New(agent.Config{
		Key:          "support.assistant",
		ModelName:    cfg.Model.Name(),
		MaxSteps:     8,
		AllowedTools: []string{"find_order"},
	}, cfg.Model, registry)
	if err != nil {
		return nil, err
	}
	return &SupportAgent{runner: runner}, nil
}

func (a *SupportAgent) Reply(ctx context.Context, input string, history []agent.Message) (*agent.RunResult, error) {
	messages := make([]agent.Message, 0, len(history)+2)
	messages = append(messages, agent.NewSystemMessage(
		"You are a support assistant. Confirm facts with tools before answering.",
	))
	messages = append(messages, history...)
	messages = append(messages, agent.NewUserMessage(input))
	return a.runner.Run(ctx, agent.RunRequest{Messages: messages})
}
```

Recommended boundary:

```text
HTTP / CLI / Worker
        |
        v
Your Agent facade
  - Prompt and policy
  - Tool selection
  - Domain ports
  - History mapping
        |
        v
agent-runtime-go
  - Model/tool loop
  - Validation
  - Stop and recovery semantics
```

The application persists the current user message and `RunResult.Messages`. The root `Agent` holds no session state and can be shared concurrently across requests.

See the [Quickstart](docs/quickstart.md) for the full walkthrough, and [`demo/icoder`](demo/icoder/README.md) for a runnable code-agent reference application.

## Choosing subpackages

Import only what your current requirements need.

| Requirement | Use |
|---|---|
| One model/tool loop | root package `agent` |
| OpenAI-compatible model integration | `providers/openaicompat` |
| Retrying transient provider failures | `providers/retry` |
| Multi-model catalog and factory | `provider` |
| Prompt templates and versions | `prompt` |
| History normalization and token budgets | `context` |
| Message aggregation and revision CAS | `message` |
| Session, branch, claim/resume | `session` |
| Checkpoints, leases, fences, recovery | `durable` |
| allow/deny/ask and authorization | `permission` |
| Advanced tool lifecycle and effect ledger | `tool` |
| Reliable events, outbox, and replay | `event` |
| Untrusted instruction catalog | `skills` |
| MCP discovery and invocation | `mcp` |
| Immutable capability composition | `coordinator` |
| Independent child runs | `subagent` |
| Readiness and bounded shutdown | `app` |
| Adapter conformance tests | `agenttest`, test-only |

Each subpackage has its own guide covering what it is, why it exists, and how to use it; see the [documentation introduction](docs/README.md).

## How it works

```text
RunRequest.Messages
        |
        v
build GenerateRequest -> agent.Model.Stream
        |                       |
        |                  canonical chunks
        v                       v
validate response <- aggregate one model step
        |
        +-- final text -------> RunResult
        |
        +-- tool calls
                |
                v
        schema + allowlist validation
                |
                v
           Tool.Execute
                |
                v
        append tool results and repeat
```

The root package reads no environment variables, selects no credentials, connects to no database, and registers no implicit global tools. Those policies belong to the consuming application.

See [runtime internals](docs/internals.md) for detailed execution semantics.

## Examples

| Example | Demonstrates |
|---|---|
| [`examples/hello`](examples/hello/main.go) | Minimal model adapter and one run |
| [`examples/tool-agent`](examples/tool-agent/main.go) | Complete model/tool loop with a facade |
| [`examples/openai-compat`](examples/openai-compat/main.go) | Official adapter with a real model, `NewTool`, and streaming output |
| [`demo/icoder`](demo/icoder/README.md) | Provider, tools, permissions, sessions, SQLite, Skills, MCP, and Sub-Agent composed together |

## Documentation

Docs are bilingual. English: [docs/en](docs/en/README.md). Chinese (default site locale): [docs](docs/README.md).

- Getting started: [Quickstart](docs/en/quickstart.md) · [Core concepts](docs/en/concepts.md)
- Package guides (one document per package): see [`docs/en/packages/`](docs/en/packages/), starting with the [root package](docs/en/packages/agent.md) and [providers/openaicompat](docs/en/packages/openaicompat.md)
- Advanced: [Runtime internals](docs/en/internals.md) · [Production patterns](docs/en/production.md) · [iCoder tutorial](docs/en/icoder.md) · [Reference](docs/en/reference.md)

## Key semantics

- `AllowedTools == nil` uses every tool in the registry; an empty slice explicitly disables tools.
- `ToolResult{IsError: true}` is a model-visible, correctable tool error.
- A non-`nil` Go error terminates the current attempt and preserves the original cause.
- `RunResult.Messages` contains only the assistant/tool messages added during this run.
- Observations are bounded, non-blocking, and lossy; they are not an authoritative event log.
- Durable execution does not inherently guarantee exactly-once external side effects; downstream systems deduplicate with the stable `ExecutionKey`.
- Prompts, Skills, and model output are not permission boundaries; real permissions must be enforced in tools and application adapters.

## Verification

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

## Design boundary

The root package `agent` is the portable core and depends on no optional subpackage. Subpackages depend on the root as needed, and the application assembles concrete models, databases, permissions, and business policy in its composition root.

## License

[Apache License 2.0](LICENSE)
