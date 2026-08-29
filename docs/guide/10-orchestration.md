# 10. 多 Agent 与可复现

## 你现在遇到的问题

分析日志要读很多行、试很多次，这些过程全部堆进主对话的上下文，几轮之后就装不下了。你想让另一个 agent 去做这件事，只把结论拿回来。

## 委派给子 Agent

child 是一个完整的 agent，有自己的上下文和自己的（只读）工具集。`subagent` 管它的生命周期：

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

`Runner` 是你实现的——它决定"跑一个 child"具体是什么：

```go
func (r *childRunner) Run(ctx context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
	result, err := r.analyst.Run(ctx, agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("你是日志分析助手，只回答看到了什么。"),
			agent.NewUserMessage(string(request.Input)),
		},
	})
	if err != nil {
		// 可重试的失败挂起而不是判死，这样 child 还能被再次认领
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

派活的时候要给限额和预留：

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

父运行拿到的是 `snapshot.ResultRef`——**结论，不是 child 的完整历史**。这正是委派的价值：child 烧掉的上下文不会污染父对话。

## 限额是安全线，不是报表

`Reserve` 在 child **跑之前**就从树预算里扣掉，这才让限额有约束力。上面那棵树是 50k input，每个 child 预留 20k，所以第三个直接被拒：

```go
if err := spawn(2); !errors.Is(err, subagent.ErrTokenBudgetExceeded) {
	t.Fatalf("third child error = %v", err)
}
```

没有 `MaxDepth`，一个会委派的 agent 可以委派出没有底的一棵树；没有预留，一个 child 能把整棵树的额度花光。库把这些判定导出成纯函数（`ValidateDepth`、`ValidateFanout`、`ValidateNoCycle`、`BudgetSnapshot.Reserve`），存储适配器**应该调用它们而不是自己重写**——重写一份不会大声报错，只会悄悄放宽一个你从未授权的限额。

## 异步才是常态

child 要跑一会儿，所以父运行通常不阻塞等它：delegate 工具 spawn 之后返回 `ToolSuspensionExternal`（第 7 章那套挂起机制），worker 推进 child，完成后凭 handle 恢复父运行。这样进程在 child 执行期间崩溃，父运行仍然可以从 checkpoint 恢复，而不是丢掉这次委派和已经花掉的 token。

`ParentWaker.Wake` **必须按 WakeKey 幂等**：已提交的唤醒意图可能被投递两次。

## 让一次运行可复现

三个月后有人问"上周那次运行用的什么模型、什么 prompt、什么工具集"。`coordinator` 把这些组合成一个不可变的 `RuntimeDefinition` 并算出 digest，那个 digest 就是这次运行的身份——记下它，以后可以把同一个组合重新解析出来，验证当前二进制是否还能复现它。

## 深入

- [subagent](../packages/subagent.md) —— 生命周期、树预算、取消、唤醒队列
- [coordinator](../packages/coordinator.md) —— 组合、manifest、按 digest 重建
- 完整可运行代码：[`examples/guide/advanced/delegation.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/delegation.go)
- [iCoder 教程](../icoder.md) —— 一个真实应用怎么把异步委派接起来
