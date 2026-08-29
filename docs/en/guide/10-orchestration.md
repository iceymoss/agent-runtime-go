# 10. Many agents, reproducibly

## Where you are stuck

Analyzing logs means reading a lot of lines and trying several angles, and all of that piles into the main conversation's context until it no longer fits. You want another agent to do it and hand back only the conclusion.

## Delegating to a child agent

A child is a full agent with its own context and its own — deliberately read-only — tool set. `subagent` owns its lifecycle:

```go
func NewDelegation(analyst *agent.Agent) (*subagent.Service, error) {
	return subagent.New(subagent.Options{
		Store:         subagent.NewMemoryStore(),
		Runner:        &childRunner{analyst: analyst},
		ParentWaker:   &parentWaker{woken: map[subagent.WakeKey]bool{}},
		WorkerID:      "worker-1",
		LeaseDuration: 5 * time.Minute,
	})
}
```

`Runner` is yours — it defines what "run a child" actually means:

```go
func (r *childRunner) Run(ctx context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
	result, err := r.analyst.Run(ctx, agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("You analyze logs. Report only what you see."),
			agent.NewUserMessage(string(request.Input)),
		},
	})
	if err != nil {
		// Retryable failures suspend rather than die, so the child can be
		// claimed again instead of being written off.
		return subagent.RunResult{
			State:   subagent.ChildSuspended,
			Failure: &subagent.Failure{Code: "attempt_failed", Message: err.Error(), Retryable: true},
		}, nil
	}
	return subagent.RunResult{
		State:        subagent.ChildCompleted,
		ResultRef:    subagent.ResultRef(result.Text),
		UsageFactKey: subagent.UsageFactKey(request.Child.RunKey),
		Usage: subagent.Usage{
			InputTokens:  int64(result.Usage.PromptTokens),
			OutputTokens: int64(result.Usage.CompletionTokens),
		},
	}, nil
}
```

Handing out work means handing out limits and a reservation:

```go
receipt, err := service.Spawn(ctx, subagent.SpawnRequest{
	RequestKey: subagent.RequestKey(parentRunKey + ":analyze"),
	Parent: subagent.ParentRef{
		TenantKey: "local", SessionKey: "local",
		RunKey: subagent.RunKey(parentRunKey), TreeKey: subagent.TreeKey(parentRunKey),
	},
	AgentKey: "ops.analyst",
	Input:    []byte(task),
	Limits: subagent.Limits{
		MaxDepth: 2, MaxFanout: 4,
		MaxInputTokens: 50_000, MaxOutputTokens: 10_000,
		MaxCostMicros: 1_000_000, MaxToolCalls: 20, MaxRuntime: 5 * time.Minute,
	},
	Reserve: subagent.Reservation{
		InputTokens: 20_000, OutputTokens: 4_000,
		CostMicros: 200_000, ToolCalls: 8, Runtime: time.Minute,
	},
})
```

The parent receives `snapshot.ResultRef` — **the conclusion, not the child's transcript.** That is the point of delegating: the context the child burned does not pollute the parent conversation.

## Limits are a safety line, not a report

`Reserve` is taken out of the tree budget **before** the child runs, which is what makes the limit binding. The tree above holds 50k input tokens and each child reserves 20k, so the third is refused:

```go
if err := spawn(2); !errors.Is(err, subagent.ErrTokenBudgetExceeded) {
	t.Fatalf("third child error = %v", err)
}
```

Without `MaxDepth`, an agent that can delegate can delegate a tree with no bottom; without the reservation, one child can spend the whole tree's allowance. The library exports those decisions as pure functions (`ValidateDepth`, `ValidateFanout`, `ValidateNoCycle`, `BudgetSnapshot.Reserve`), and storage adapters **should call them rather than restate them** — a restatement does not fail loudly, it quietly widens a limit nobody authorized.

## Asynchronous is the normal case

A child takes time, so the parent usually does not block on it: the delegate tool spawns and returns a `ToolSuspensionExternal` (chapter 7's mechanism), a worker advances the child, and the parent resumes with the handle once it finishes. A crash while the child works then leaves a resumable parent rather than a lost delegation and the tokens already spent on it.

`ParentWaker.Wake` **must be idempotent by WakeKey**: a committed wake intent can be delivered twice.

## Making a run reproducible

Three months later somebody asks which model, prompt, and tool set produced a particular run. `coordinator` composes those into an immutable `RuntimeDefinition` with a digest, and that digest is the run's identity — record it and you can resolve the same composition back later and verify the current binary still reproduces it.

## Going deeper

- [subagent](../packages/subagent.md) — lifecycle, tree budget, cancellation, wake queue
- [coordinator](../packages/coordinator.md) — composition, manifests, rebuilding by digest
- Complete runnable code: [`examples/guide/advanced/delegation.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/delegation.go)
- [iCoder tutorial](../icoder.md) — how a real application wires asynchronous delegation
