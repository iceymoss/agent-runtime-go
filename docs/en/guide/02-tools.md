# 2. Letting the model call your code

## Where you are stuck

The agent from chapter 1 can only talk. For an ops assistant to be useful it has to actually read the logs — that is, call your Go functions.

## Defining a tool

A tool is a function plus an input struct. The JSON Schema is generated from the struct by reflection:

```go
type ReadLogsInput struct {
	Service string `json:"service" description:"Service name, e.g. checkout"`
	Level   string `json:"level,omitempty" description:"Return this level and above: INFO / WARN / ERROR"`
}

func ReadLogsTool(ops *Ops) agent.Tool {
	return agent.MustNewTool("read_logs", "Read a service's recent logs.",
		func(_ context.Context, in ReadLogsInput) (agent.ToolResult, error) {
			lines, ok := ops.logs[in.Service]
			if !ok {
				// The model chose a service that does not exist. It can fix that
				// by choosing another, so this is a result, not a failure.
				return agent.ToolResult{
					IsError: true,
					Content: fmt.Sprintf("unknown service %q, known: %s", in.Service, strings.Join(ops.services(), ", ")),
				}, nil
			}
			if in.Level != "" {
				lines = filterByLevel(lines, in.Level)
			}
			return agent.ToolResult{Content: strings.Join(lines, "\n")}, nil
		},
		agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent))
}
```

Non-pointer fields without `omitempty` become `required`, so `Service` is mandatory and `Level` is optional. The schema is strict by default: undeclared properties are rejected and fed back to the model to correct.

**The description is written for the model.** It decides when the tool gets called, which makes it more important than a comment written for a human.

Note that `ops` arrives through a closure. Tools are the only channel between your application and the model, so the rules about what may happen live here, not in a prompt.

## Two failures, two meanings

This is the part of the chapter to remember. The second tool changes the world:

```go
func RestartTool(ops *Ops) agent.Tool {
	return agent.MustNewTool("restart_service", "Restart a service. This interrupts in-flight requests.",
		func(ctx context.Context, in RestartInput) (agent.ToolResult, error) {
			if _, ok := ops.logs[in.Service]; !ok {
				// The model can fix this
				return agent.ToolResult{IsError: true, Content: "unknown service " + in.Service}, nil
			}
			if err := ops.restart(ctx, in.Service); err != nil {
				// The orchestrator is down; the model cannot fix that. End the
				// attempt with the cause intact.
				return agent.ToolResult{}, fmt.Errorf("restart %s: %w", in.Service, err)
			}
			return agent.ToolResult{Content: "restarted " + in.Service + " (reason: " + in.Reason + ")"}, nil
		},
		agent.WithToolReplayPolicy(agent.ReplayPolicyNever))
}
```

`ToolResult{IsError: true}` goes back to the model as a tool result and the loop continues; a Go error ends the run and hands you the cause.

Choosing wrong costs something real. A genuine outage reported as `IsError` makes the model retry pointlessly and never appears in your logs; a correctable argument error reported as a Go error fails a run the model could have recovered from.

`WithToolReplayPolicy` declares whether the call may be replayed during crash recovery. Reading logs is idempotent; restarting is not — chapters 7 and 9 use that declaration.

## Assembly

```go
registry := agent.NewRegistry()
for _, tool := range []agent.Tool{ReadLogsTool(ops), RestartTool(ops)} {
	if err := registry.Register(tool); err != nil {
		return err
	}
}

runner, err := agent.New(agent.Config{
	Key: "ops.assistant", ModelName: modelName, MaxSteps: 8,
}, model, registry)
```

Run it and the loop is visible:

```text
> Something looks wrong with checkout, can you look?
  [calling read_logs {"service":"checkout","level":"WARN"}]
checkout's payment gateway keeps timing out and the connection pool is exhausted. I suggest restarting checkout.
```

Step one asked for `read_logs`, the runtime validated the arguments and executed it; step two wrote the answer from the result.

## Worth knowing

**Registering a tool after assembly has no effect on an existing Agent.** `agent.New` takes an immutable `ToolSet` snapshot — the tool set of a run must be stable and auditable.

**`AllowedTools` distinguishes `nil` from `[]string{}`.** `nil` means every registered tool; `[]string{}` means explicitly none.

**Permissions are not in this chapter.** Right now anything can call `restart_service`. Chapter 7 puts an approval in front of it.

## Going deeper

- [agent package reference](../packages/agent.md) — all `NewTool` options, and when to implement `Tool` directly
- Complete runnable code: [`examples/guide/tools.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/tools.go)
