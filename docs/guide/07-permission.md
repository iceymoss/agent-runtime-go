# 7. 危险操作要问人

## 你现在遇到的问题

到目前为止工具都是只读的。运维助手真正有用是在它能重启服务的时候——而那会中断正在处理的请求，不能由模型自己决定。

你需要某些工具在执行前停下来等人点头。

## 权限只能在工具里做实

先说最容易做错的：**prompt 不是权限边界**。system prompt 里写"重启前必须确认"拦不住任何东西，模型是概率的，而且用户输入可以覆盖它。

真正的检查要放在运行时和副作用之间，用一层包装：

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
		// 拒绝是模型能看见、能绕开的结果，不是崩溃
		return agent.ToolResult{IsError: true, StopTurn: true,
			Content: fmt.Sprintf("策略拒绝了 %s（规则 %s）", g.inner.Definition().Name, decision.RuleKey)}, nil

	case permission.DecisionAsk:
		// 带着 handle 回来，说明有人已经答复过了
		if invocation.Resume != nil && g.approved(g.inner.Definition().Name) {
			return g.inner.Execute(ctx, invocation)
		}
		if invocation.Resume != nil {
			return agent.ToolResult{IsError: true, StopTurn: true, Content: "审批被拒绝"}, nil
		}
		// 挂起整轮运行。运行时会落 checkpoint，所以进程可以死在这里，
		// 决定仍然可以事后作出。
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{
			Kind:        agent.ToolSuspensionApproval,
			RequestRef:  invocation.CallID,
			ResumeToken: digest,
		}}
	}
	return agent.ToolResult{}, fmt.Errorf("未知的权限判定 %q", decision.Decision)
}

// 会挂起的工具必须声明这个，否则恢复时会失败——理由见下面的「需要注意的」
func (g *Gate) OwnsToolExecutionLifecycle() bool { return true }
```

策略本身是一个纯判定，和工具分开，这样规则可审计、可版本化：

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
			// 默认拒绝很重要：以后新加的工具在有人决定之前不会被悄悄放行
			result.Decision, result.RuleKey = permission.DecisionDeny, "unknown-tool"
		}
		return result, nil
	},
}
```

## 挂起必然要求持久化

`ToolSuspensionError` 会让整轮运行以 `suspended` + `tool_suspended` 停下。但**恢复它需要 durable run**——`ToolResume` 这个字段在 `DurableRunConfig` 上，不在 `RunRequest` 上：

```go
result, err := runner.Run(ctx, agent.RunRequest{
	Messages:   transcript.Prompt(question),
	DurableRun: runs.Config(runKey, nil),      // 第一次
})
// ... 人做出决定 ...
result, err = runner.Run(ctx, agent.RunRequest{
	Messages:   transcript.Prompt(question),
	DurableRun: runs.Config(runKey, suspension), // 同一个 run key，带上 handle
})
```

也就是说**审批和持久化是同一个特性**：想要"停下来问人"，就必须让这次运行落 checkpoint，否则没有可以恢复的东西。最小的装配是：

```go
adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
	Store: store, Ledger: store, AttemptKey: durable.AttemptKey(attemptKey),
})
config := &agent.DurableRunConfig{
	Identity:        agent.RunIdentity{RunKey: runKey, AgentKey: "ops.assistant", SessionID: "local", RequestID: runKey},
	CheckpointStore: adapter,
	LeaseOwner:      attemptKey,
	LeaseDuration:   5 * time.Minute,
	ToolResume:      resume, // 第一次为 nil
}
```

`durable.NewMemoryStore()` 就能跑通全流程；换成数据库时只改这一处。第 9 章讲跨进程恢复。

## 需要注意的

**会挂起的工具必须声明 `OwnsToolExecutionLifecycle() bool { return true }`。** 否则运行时会在它外面开一个自己的 effect 边界，而挂起会让这个 effect 停在 `running` 状态、带着挂起那次 attempt 的 fence；恢复时是新 attempt、新 fence，于是失败：

```text
begin effect: effect is running, not prepared; a suspending tool must own
its execution lifecycle or use the tool subpackage's executor
```

声明它的含义是"这个工具的后端自己管 effect 记账"。上面的 Gate 可以这么声明，因为审批发生在任何东西被执行之前，它没有 effect 要记。**有真实副作用、且要跨崩溃保证只发生一次的工具**，应该走 `tool` 子包的 executor——它维护一份 effect 台账，并且能把执行本身挂起和重新武装。

**恢复用同一个 run key。** 换 key 就是另一次运行，之前那次会永远停在挂起状态。

**默认拒绝，不是默认放行。** 上面策略里的 `default` 分支是这一节的重点：以后有人加了新工具，它在被明确决定之前不会被执行。

## 深入

- [permission](../packages/permission.md) —— 策略、审批记录、跨进程回答、授权重校验
- [tool](../packages/tool.md) —— effect 台账、执行生命周期、`tool.NewAgentRegistry` 桥接
- 完整可运行代码：[`examples/guide/approval.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/approval.go) 与 [`durability.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/durability.go)
