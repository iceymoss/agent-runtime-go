# 7. Asking a human first

## Where you are stuck

Every tool so far has been read-only. The ops assistant becomes genuinely useful when it can restart a service — and that interrupts in-flight requests, so it is not the model's decision to make.

You need some tools to stop and wait for a person.

## Permission lives in the tool

The easiest thing to get wrong first: **a prompt is not a permission boundary.** "Always confirm before restarting" in a system prompt stops nothing — the model is probabilistic, and user input can override it.

The real check goes between the runtime and the side effect, as a wrapper:

```go
type Gate struct {
	inner    agent.Tool
	policy   permission.Policy
	approved func(tool string) bool
}

func (g *Gate) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	digest, err := agent.DigestToolInput(invocation.RawInput)
	if err != nil {
		return agent.ToolResult{}, err
	}
	decision, err := g.policy.Evaluate(ctx, permission.CheckRequest{
		RequestKey:    permission.RequestKey(invocation.CallID),
		ToolName:      g.inner.Definition().Name,
		ToolCallID:    invocation.CallID,
		InputDigest:   permission.InputDigest(digest),
		PolicyVersion: g.policy.Version(),
	}, nil)
	if err != nil {
		return agent.ToolResult{}, err
	}

	switch decision.Decision {
	case permission.DecisionAllow:
		return g.inner.Execute(ctx, invocation)

	case permission.DecisionDeny:
		// Denial is a result the model can see and work around, not a crash.
		return agent.ToolResult{IsError: true, StopTurn: true,
			Content: fmt.Sprintf("policy refused %s (rule %s)", g.inner.Definition().Name, decision.RuleKey)}, nil

	case permission.DecisionAsk:
		// A resumed invocation carries the handle this tool issued last time.
		// Its presence is what says a human has since answered.
		if invocation.Resume != nil && g.approved(g.inner.Definition().Name) {
			return g.inner.Execute(ctx, invocation)
		}
		if invocation.Resume != nil {
			return agent.ToolResult{IsError: true, StopTurn: true, Content: "approval was refused"}, nil
		}
		// Park the whole run. The runtime checkpoints it, so the process can die
		// here and the decision can still be answered afterwards.
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{
			Kind:        agent.ToolSuspensionApproval,
			RequestRef:  invocation.CallID,
			ResumeToken: digest,
		}}
	}
	return agent.ToolResult{}, fmt.Errorf("unknown permission decision %q", decision.Decision)
}

// Required for a tool that can suspend, or the resume fails — see "Worth knowing".
func (g *Gate) OwnsToolExecutionLifecycle() bool { return true }
```

The policy itself is a pure decision, separate from the tool, so the rule is auditable and versioned:

```go
permission.PolicyFunc{
	PolicyVersion: "ops/v1",
	EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
		result := permission.CheckResult{PolicyVersion: "ops/v1", InputDigest: request.InputDigest}
		switch request.ToolName {
		case "read_logs":
			result.Decision, result.RuleKey = permission.DecisionAllow, "read-only"
		case "restart_service":
			result.Decision, result.RuleKey = permission.DecisionAsk, "service-restart"
		default:
			// Refusing by default matters: a tool added later is denied until
			// someone decides about it, rather than silently permitted.
			result.Decision, result.RuleKey = permission.DecisionDeny, "unknown-tool"
		}
		return result, nil
	},
}
```

## Suspension requires durability

`ToolSuspensionError` stops the whole run with `suspended` + `tool_suspended`. But **resuming it needs a durable run** — `ToolResume` is a field on `DurableRunConfig`, not on `RunRequest`:

```go
result, err := runner.Run(ctx, agent.RunRequest{
	Messages:   transcript.Prompt(question),
	DurableRun: runs.Config(runKey, nil),        // first attempt
})
// ... a person decides ...
result, err = runner.Run(ctx, agent.RunRequest{
	Messages:   transcript.Prompt(question),
	DurableRun: runs.Config(runKey, suspension), // same run key, with the handle
})
```

So **approval and durability are one feature, not two**: to stop and ask, the run has to be checkpointed, or there is nothing to resume. The minimal wiring is:

```go
adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
	Store: store, Ledger: store, AttemptKey: durable.AttemptKey(attemptKey),
})
config := &agent.DurableRunConfig{
	Identity:        agent.RunIdentity{RunKey: runKey, AgentKey: "ops.assistant", SessionID: "local", RequestID: runKey},
	CheckpointStore: adapter,
	LeaseOwner:      attemptKey,
	LeaseDuration:   5 * time.Minute,
	ToolResume:      resume, // nil on the first attempt
}
```

`durable.NewMemoryStore()` runs the whole flow; swapping in a database changes only this. Chapter 9 covers recovery across processes.

## Worth knowing

**A tool that can suspend must declare `OwnsToolExecutionLifecycle() bool { return true }`.** Otherwise the runtime opens its own effect boundary around it, and suspending leaves that effect running under the fence of the attempt that parked it. The resumed attempt holds a newer fence, so it fails:

```text
begin effect: effect is running, not prepared; a suspending tool must own
its execution lifecycle or use the tool subpackage's executor
```

Declaring it says "this tool's backend accounts for its own effects". The Gate above can say that because approval happens before anything executes — it has no effects to account for. **A tool with real side effects that must survive a crash** should go through the `tool` subpackage's executor, which keeps an effect ledger and can park and re-arm the execution itself.

**Resume with the same run key.** A different key is a different run, and the original stays suspended forever.

**Deny by default, not allow.** The `default` branch in the policy above is the point of this section: a tool somebody adds later is not executed until it has been decided about.

## Going deeper

- [permission](../packages/permission.md) — policy, approval records, cross-process answers, revalidation
- [tool](../packages/tool.md) — effect ledger, execution lifecycle, the `tool.NewAgentRegistry` bridge
- Complete runnable code: [`examples/guide/approval.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/approval.go) and [`durability.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/durability.go)
