# 封装自己的 Agent

一个好用的业务 Agent 通常不是 `agent.Agent` 的 type alias，而是一个拥有明确用途的 facade。Facade 决定策略，runtime 执行机制。

## 第一步：定义用途

先用一句话描述 Agent，例如：

> 根据订单事实回答售后问题，但不能自行退款。

这句话会决定 Prompt、工具集合、权限和返回值。不要先注册所有工具，再让 Prompt 决定安全边界。

## 第二步：定义应用 Port

工具依赖应用拥有的最窄接口：

```go
type OrderReader interface {
	FindOrder(ctx context.Context, orderID string) (Order, error)
}
```

SDK 不需要知道数据来自 MySQL、HTTP 还是测试内存实现。

## 第三步：把 Port 包装成 Tool

```go
type findOrderTool struct {
	orders OrderReader
}

func (t findOrderTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "find_order",
		Description: "Find an order by its public order ID.",
		Strict:      true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"order_id": map[string]any{"type": "string"},
			},
			"required": []any{"order_id"},
		},
	}
}

func (t findOrderTool) ReplayPolicy() agent.ReplayPolicy {
	return agent.ReplayPolicyIdempotent
}
```

`Execute` 应解析已通过 schema 的 JSON，调用 port，并把结果编码为模型可理解的短文本或 JSON。不存在的订单等可修正情况返回 `IsError: true`；数据库断连等 attempt 失败返回 Go error。

## 第四步：构造 Facade

```go
type SupportAgent struct {
	runner *agent.Agent
}

func NewSupportAgent(model agent.Model, orders OrderReader) (*SupportAgent, error) {
	registry := agent.NewRegistry()
	if err := registry.Register(findOrderTool{orders: orders}); err != nil {
		return nil, err
	}
	runner, err := agent.New(agent.Config{
		Key:          "support.assistant",
		ModelName:    model.Name(),
		MaxSteps:     8,
		AllowedTools: []string{"find_order"},
	}, model, registry)
	if err != nil {
		return nil, err
	}
	return &SupportAgent{runner: runner}, nil
}
```

构造函数是 composition root。模型、数据库 adapter 和权限服务都通过参数注入，不使用 package-global registry。

## 第五步：提供业务方法

```go
func (a *SupportAgent) Reply(ctx context.Context, input string, history []agent.Message) (*agent.RunResult, error) {
	messages := make([]agent.Message, 0, len(history)+2)
	messages = append(messages, agent.NewSystemMessage(
		"Answer support questions. Verify order facts with tools. Never claim a refund was issued.",
	))
	messages = append(messages, history...)
	messages = append(messages, agent.NewUserMessage(input))
	return a.runner.Run(ctx, agent.RunRequest{Messages: messages})
}
```

HTTP controller 只负责绑定参数、加载历史、调用 `Reply`、保存 turn 和返回响应。它不应直接组装工具。

## 第六步：持久化 Turn

一次成功 turn 按顺序保存：

```text
current user message
RunResult.Messages...
usage and terminal status
```

这些内容应在应用拥有的事务里保存。不要在数据库事务内执行模型调用或外部副作用。

## 测试层次

1. Tool 单元测试：schema、输入解析、业务 port 错误映射。
2. Model adapter conformance：终态、usage、错误和 cancellation。
3. Facade 测试：使用确定性 fake model 验证 Prompt、工具白名单和结果。
4. 集成测试：真实 adapter 的 wire mapping，避免依赖不稳定模型输出断言文案。

## 何时增加子包

- 历史过长：增加 `context`。
- 多模型和动态配置：增加 `provider` 与 `coordinator`。
- 多轮会话和并发 revision：增加 `message`、`session`。
- 跨进程恢复：增加 `durable`、`event`。
- 工具审批：增加 `permission`、`tool`。

从根包开始，出现真实需求后再增加机制。
