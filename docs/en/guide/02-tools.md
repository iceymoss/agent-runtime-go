# 2. Letting the model call your code

## Where you are stuck

The agent from chapter 1 can only talk. To make it do anything — query a database, call an internal API, read a file — you have to expose your Go functions to the model.

## Defining a tool

A tool is a function plus an input struct. The JSON Schema is generated from the struct by reflection; you do not write it:

```go
type WeatherInput struct {
	City string `json:"city" description:"City name"`
	Days int    `json:"days,omitempty" description:"Forecast days"`
}

weather := agent.MustNewTool("get_weather", "Get the current weather for a city.",
	func(ctx context.Context, in WeatherInput) (agent.ToolResult, error) {
		report, err := weatherAPI.Query(ctx, in.City)
		if err != nil {
			return agent.ToolResult{}, err
		}
		return agent.ToolResult{Content: report}, nil
	})

registry := agent.NewRegistry()
if err := registry.Register(weather); err != nil {
	panic(err)
}
```

Non-pointer fields without `omitempty` become `required`, so `City` is mandatory and `Days` is optional. The schema is strict by default: undeclared properties are rejected and fed back to the model to correct.

**The description is written for the model.** It decides when the tool gets called, which makes it more important than a comment written for a human.

## Two failures, two meanings

This is the part of the chapter to remember:

```go
// The model can see this and fix it — the loop continues
return agent.ToolResult{IsError: true, Content: "unknown city: " + in.City}, nil

// The model cannot fix this — end the attempt, keep the cause
return agent.ToolResult{}, fmt.Errorf("weather API unreachable: %w", err)
```

Bad arguments, a missing resource, a user without permission — the model might succeed with different arguments, so use `IsError: true` and the message goes back to it as a tool result.

A database that is down, expired credentials, a full disk — a hundred more attempts will not help, so return a Go error and the runtime ends the run with the cause intact.

Choosing wrong costs you something real. A genuine outage reported as `IsError` makes the model retry pointlessly and never appears in your logs; a correctable argument error reported as a Go error fails a run the model could have recovered from.

## Watching the loop actually loop

Once the tool is registered, the runtime does the whole "model asks → validate → execute → feed back → ask again" cycle:

```go
runner, err := agent.New(agent.Config{
	Key: "my.weather", ModelName: "fake-v1", MaxSteps: 8,
}, model, registry)
if err != nil {
	panic(err)
}

result, err := runner.Run(context.Background(), agent.RunRequest{
	Messages: []agent.Message{agent.NewUserMessage("What is the weather in Hangzhou?")},
})
if err != nil {
	panic(err)
}
fmt.Println(result.Text)
fmt.Println("steps:", len(result.Steps))
```

```text
Hangzhou is sunny and 28 C.
steps: 2
```

`steps: 2` is the evidence: the first step asked for `get_weather`, the second wrote the answer from its result. The complete runnable version is [`examples/tool-agent`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/tool-agent/main.go).

## Worth knowing

**Registering a tool after assembly has no effect on an existing Agent.** `agent.New` takes an immutable `ToolSet` snapshot, on purpose: the tool set of a run must be stable and auditable.

**`AllowedTools` distinguishes `nil` from `[]string{}`.** `nil` means every registered tool; `[]string{}` means explicitly no tools. Do not normalize them into one thing.

**Enforce permissions inside the tool.** "Do not delete files" in a prompt is not a security boundary — neither prompts, nor skills, nor model output are. The real check belongs in `Execute`; see [chapter 7](./07-permission.md).

## Going deeper

- [agent package reference](../packages/agent.md) — all `NewTool` options, and when to implement `Tool` directly
- [tool subpackage](../packages/tool.md) — when you need an effect ledger, approval suspension, or replay semantics
