# 2. 让模型调用你的代码

## 你现在遇到的问题

上一章的 agent 只会说话。运维助手要有用，它得能真的去查日志——也就是调用你的 Go 函数。

## 定义一个工具

工具是一个函数加一个输入结构体，JSON Schema 由结构体反射生成：

```go
type ReadLogsInput struct {
	Service string `json:"service" description:"服务名，如 checkout"`
	Level   string `json:"level,omitempty" description:"只返回该级别及以上：INFO / WARN / ERROR"`
}

func ReadLogsTool(ops *Ops) agent.Tool {
	return agent.MustNewTool("read_logs", "读取一个服务最近的日志。",
		func(_ context.Context, in ReadLogsInput) (agent.ToolResult, error) {
			lines, ok := ops.logs[in.Service]
			if !ok {
				// 模型选了个不存在的服务。它换一个就可能对，
				// 所以这是一个结果，不是一次失败。
				return agent.ToolResult{
					IsError: true,
					Content: fmt.Sprintf("未知服务 %q，已知的有: %s", in.Service, strings.Join(ops.services(), ", ")),
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

非指针且没有 `omitempty` 的字段自动成为 `required`，所以 `Service` 必填、`Level` 可选。schema 默认 strict：模型传了未声明的字段会被拒绝并回灌给它改正。

**描述是写给模型看的**，它决定模型什么时候会调这个工具。这比写给人看的注释更重要。

注意 `ops` 是通过闭包传进去的。工具是你的应用和模型之间唯一的通道，所以"能做什么"的规则都住在这里，不在 prompt 里。

## 两种失败，语义完全不同

这是这一章最需要记住的。看第二个工具——它会改变世界：

```go
func RestartTool(ops *Ops) agent.Tool {
	return agent.MustNewTool("restart_service", "重启一个服务。这会中断正在处理的请求。",
		func(ctx context.Context, in RestartInput) (agent.ToolResult, error) {
			if _, ok := ops.logs[in.Service]; !ok {
				// 模型可以改正
				return agent.ToolResult{IsError: true, Content: "未知服务 " + in.Service}, nil
			}
			if err := ops.restart(ctx, in.Service); err != nil {
				// 编排器挂了，模型改不了。终止本次尝试并保留原始 cause。
				return agent.ToolResult{}, fmt.Errorf("restart %s: %w", in.Service, err)
			}
			return agent.ToolResult{Content: "已重启 " + in.Service + "（原因：" + in.Reason + "）"}, nil
		},
		agent.WithToolReplayPolicy(agent.ReplayPolicyNever))
}
```

`ToolResult{IsError: true}` 会作为工具结果回到模型手里，循环继续；返回 Go error 则中止这次运行，把原因原样交给你。

选错的后果是实打实的：把真实故障写成 `IsError` 会让模型徒劳重试，而且你的日志里看不到这次故障；把可修正的参数错误写成 Go error，会让本来能自己纠正的模型直接失败。

`WithToolReplayPolicy` 声明的是"崩溃恢复时这个调用能不能重放"。查日志是幂等的，重启不是——第 7、9 章会用到这个声明。

## 装配

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

跑起来就能看到循环真的循环了：

```text
> checkout 服务好像有问题，看一下？
  [调用 read_logs {"service":"checkout","level":"WARN"}]
checkout 的支付网关连续超时，连接池也被打满了。建议重启 checkout。
```

第 1 步模型请求 `read_logs`，运行时校验参数并执行，第 2 步模型拿着结果组织回答。

## 需要注意的

**装配之后再往 Registry 里注册工具，已装配的 Agent 看不到。** `agent.New` 拍了一个不可变的 `ToolSet` 快照——一次运行的工具集合必须稳定可审计。

**`AllowedTools` 的 `nil` 和 `[]string{}` 不是一回事。** `nil` 放开全部已注册工具，`[]string{}` 显式禁用所有工具。

**权限不在这一章。** 上面的 `restart_service` 现在谁都能调。第 7 章加上审批。

## 深入

- [agent 包参考](../packages/agent.md) —— `NewTool` 的全部选项、直接实现 `Tool` 接口的时机
- 完整可运行代码：[`examples/guide/tools.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/tools.go)
