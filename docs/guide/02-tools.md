# 2. 让模型调用你的代码

## 你现在遇到的问题

上一章的 agent 只会说话。要让它做事——查数据库、调内部 API、读文件——就得把你的 Go 函数暴露给模型。

## 定义一个工具

工具就是一个函数加一个输入结构体。JSON Schema 由结构体反射生成，你不用手写：

```go
type WeatherInput struct {
	City string `json:"city" description:"城市名"`
	Days int    `json:"days,omitempty" description:"预报天数"`
}

weather := agent.MustNewTool("get_weather", "查询一个城市的天气。",
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

非指针且没有 `omitempty` 的字段自动成为 `required`，所以 `City` 必填、`Days` 可选。schema 默认是 strict 的：模型传了未声明的字段会被拒绝并回灌给它改正。

工具的**描述是给模型看的**，它决定模型什么时候会调用这个工具。写清楚用途和边界，比写给人看的注释更重要。

## 两种失败，语义完全不同

这是这一章最需要记住的事：

```go
// 模型能看见、能自己改正 —— 循环继续
return agent.ToolResult{IsError: true, Content: "未知城市: " + in.City}, nil

// 模型改不了 —— 终止本次运行，保留原始 cause
return agent.ToolResult{}, fmt.Errorf("weather API unreachable: %w", err)
```

参数不对、资源不存在、用户没权限——这些模型换个参数就可能成功，用 `IsError: true`，它会作为工具结果回到模型手里。

数据库连不上、凭据失效、磁盘满了——模型再试一百次也没用，返回 Go error，运行时会中止这次运行并把原因原样交给你。

选错的后果是实打实的：把真实故障写成 `IsError` 会让模型徒劳地重试，而且你的日志里看不到这次故障；把可修正的参数错误写成 Go error，会让本来能自己纠正的模型直接失败。

## 看循环真的循环起来

把工具注册进去之后，运行时会自动完成"模型请求 → 校验 → 执行 → 回灌 → 再问模型"：

```go
runner, err := agent.New(agent.Config{
	Key: "my.weather", ModelName: "fake-v1", MaxSteps: 8,
}, model, registry)
if err != nil {
	panic(err)
}

result, err := runner.Run(context.Background(), agent.RunRequest{
	Messages: []agent.Message{agent.NewUserMessage("杭州天气怎么样？")},
})
if err != nil {
	panic(err)
}
fmt.Println(result.Text)
fmt.Println("steps:", len(result.Steps))
```

```text
杭州现在晴，28 度。
steps: 2
```

`steps: 2` 就是证据：第 1 步模型请求 `get_weather`，第 2 步它拿到结果后组织回答。完整可运行的版本见 [`examples/tool-agent`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/tool-agent/main.go)。

## 需要注意的

**装配之后再往 Registry 里注册工具，已装配的 Agent 看不到。** `agent.New` 会拍一个不可变的 `ToolSet` 快照。这是刻意的：一次运行的工具集合必须稳定可审计。

**`AllowedTools` 的 `nil` 和 `[]string{}` 不是一回事。** `nil` 表示放开全部已注册工具，`[]string{}` 表示显式禁用所有工具。别把两者归一化处理。

**权限要在工具实现里做实。** prompt 里写"不要删除文件"不是安全边界——模型输出、prompt、skills 都不是。真正的检查必须在 `Execute` 里，见[第 7 章](./07-permission.md)。

## 深入

- [agent 包参考](../packages/agent.md) —— `NewTool` 的全部选项、直接实现 `Tool` 接口的时机
- [tool 子包](../packages/tool.md) —— 需要副作用台账、审批挂起、重放语义时
