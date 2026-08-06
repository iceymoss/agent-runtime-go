# agent

The root `agent` package is the library core: it reliably runs the "model thinks → call tools → feed results back" loop. Everything else (model access, tool implementations, storage, permissions) is handed to your app through interfaces.

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

## What it is

The root package defines only two ports you must provide, and one executor you call:

```go
// Model port: any LLM adapted to this interface can be driven by the runtime
type Model interface {
	Name() string
	Capabilities() Capabilities
	Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}

// Tool port: business functions the model can call
type Tool interface {
	Definition() ToolDefinition
	ReplayPolicy() ReplayPolicy
	Execute(context.Context, ToolInvocation) (ToolResult, error)
}
```

The executor is `Agent`: assemble once with `agent.New(config, model, registry)`, then call `Run` concurrently and repeatedly. `Agent` itself does not keep session history, read environment variables, or connect to databases—those belong to your application.

Around these two ports, the root package includes everything the loop needs: JSON Schema validation, tool allowlists, max steps, stop conditions, context budget, loop detection, and feeding invalid args back for repair (tool repair).

## Why you need it

Without it, you handle these easy-to-get-wrong details yourself:

- Parse tool calls from the model, validate args, execute, splice results into history, request the model again—each step has edge cases (partial JSON, unregistered tool names, the model looping on the same tool);
- Streaming protocol consistency: concatenated text deltas must equal the final message; tool-call fragments must reassemble correctly;
- When to stop: normal completion, max steps, truncated output, over context budget—each needs different upper-layer handling.

**When you do not need other subpackages:** for "one request in, model answers (maybe after a few tool calls)," the root package is enough. Add session persistence, crash recovery, permission approval, and similar when those needs appear.

## How to use it

A complete minimal program: wire an OpenAI-compatible model, register one tool, run one turn.

```go
package main

import (
	"context"
	"fmt"
	"os"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
)

type WeatherInput struct {
	City string `json:"city" description:"城市名"`
}

func main() {
	// 1. Model: openaicompat is the official adapter; you can also implement Model yourself
	model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

	// 2. Tool: NewTool builds JSON Schema from the function; no hand-written schema
	weather := agent.MustNewTool("get_weather", "查询城市当前天气",
		func(ctx context.Context, in WeatherInput) (agent.ToolResult, error) {
			return agent.ToolResult{Content: `{"city":"` + in.City + `","temp":"26C"}`}, nil
		})
	registry := agent.NewRegistry()
	if err := registry.Register(weather); err != nil {
		panic(err)
	}

	// 3. Assemble and run
	runner, err := agent.New(agent.Config{
		Key:       "demo.weather",
		ModelName: "deepseek-chat",
		MaxSteps:  8,
	}, model, registry)
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("回答前先用工具确认事实。"),
			agent.NewUserMessage("北京现在多少度？"),
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
}
```

Sample output (varies by model):

```text
北京现在的气温是 26C。
```

Key points:

- `Config.AllowedTools` controls the allowlist: `nil` = all tools in the registry; `[]string{}` = explicitly disable tools; listed names = only those (missing names fail at `New`, not mid-run).
- `agent.New` validates the full config at assembly (non-nil model, valid schema, capabilities cover needs); config errors do not surface mid-request.
- `RunResult.Messages` holds only assistant/tool messages produced this turn; you persist the input user message under your own transaction rules.
- Judge results with three values together: `err`, `result.Outcome`, `result.StopReason`. `OutcomeCompleted` is normal completion; `OutcomeSuspended` means interrupted by steps/output length/context budget and the upper layer may resume; `OutcomeFailed` comes with a non-nil error.

### Two ways to define tools

Prefer `NewTool` / `MustNewTool` (Schema reflected from the struct):

```go
type Input struct {
	OrderID string `json:"order_id" description:"订单号"`
	Limit   int    `json:"limit,omitempty" description:"返回条数"`
}
// Non-pointer fields without omitempty enter required automatically;
// schema defaults to strict: undeclared properties are rejected and fed back for repair.
tool := agent.MustNewTool("find_order", "按订单号查询订单", handler)
```

When you need full Schema control (complex constraints, oneOf, etc.), implement the three `Tool` methods directly.

Tool errors come in two kinds with different semantics:

- `ToolResult{IsError: true}`: model-visible, correctable business errors (e.g. "order not found"); the loop continues;
- non-nil Go error from `Execute`: infrastructure failure; abort this run immediately.

### Observe run progress

`ObservationEmitter` exposes text deltas, tool start/end, and step completion for live UI:

```go
emitter := agent.NewObservationEmitter(64, func(o agent.Observation) {
	if o.Type == agent.ObservationTextDelta {
		fmt.Print(o.Text)
	}
})
defer emitter.Close()
result, err := runner.Run(ctx, agent.RunRequest{Messages: msgs, ObservationEmitter: emitter})
```

It is a bounded, non-blocking, droppable progress signal—authoritative results come from `RunResult` only. For reliable event delivery use the [event subpackage](event.md).

## FAQ

**Q: Is `MaxSteps` a retry count?**
No. It is the maximum number of model calls in one turn (each tool call is followed by another model call, counting as a step). Hitting the limit returns `OutcomeSuspended` + `StopReasonMaxSteps`. Note `MaxSteps` cannot be smaller than the loop-detection window (default 4).

**Q: If I register more tools on the Registry after assembling an Agent, does the Agent see them?**
No. `New` takes an immutable `ToolSet` snapshot; later Registry changes do not affect the assembled Agent. That is intentional: the in-run tool set must be stable and auditable.

**Q: What if the model keeps calling the same tool?**
Built-in loop detection: if the same (tool name + args) appears `LoopDetectThreshold` times within the last `LoopDetectWindow` steps, the run ends with `ErrLoopDetected`. Both are tunable on `Config`.

**Q: What if tool args from the model fail Schema validation?**
Failed args are fed back as an error result for the model to repair, up to `ToolRepairLimit` times (default 1, max 2). Still invalid after that, the run fails.

**Q: Is `RunResult.Text` the full reply?**
Only the last-step assistant text. Full process messages are in `RunResult.Messages`; per-step detail (tool calls, results, usage) is in `RunResult.Steps`.

**Q: Can I swap models or narrow tools dynamically each step?**
Use `RunRequest.StepPolicy`. It can pick a tool subset, tool choice, model name, and sampling params for the next step, but cannot rewrite history or replace the `Model` instance.

## Related docs

- Wire a real model: [providers/openaicompat](openaicompat.md)
- Stream protocol, stop semantics, recovery state machine: [Run-loop internals](../internals.md)
- Crash recovery and checkpoints: [durable](durable.md)
- Session persistence: [session](session.md), [message](message.md)
