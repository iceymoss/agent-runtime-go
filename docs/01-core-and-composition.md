# Core 与 Composition

根包拥有 canonical values 和无状态 runtime：

```go
runner, err := agent.New(agent.Config{
    Key:          "my-agent",
    ModelName:    modelName,
    MaxSteps:     12,
    AllowedTools: []string{"read_file"},
}, model, registry)

result, err := runner.Run(ctx, agent.RunRequest{Messages: messages})
```

## Ownership

调用方负责：

- 实现 `agent.Model`。
- 实现和注册 `agent.Tool`。
- 生成 system/capability/history/user messages。
- 选择 Prompt、模型、工具和策略。
- 持久化 user message 与 `RunResult.Messages`。
- 将 SDK errors 映射到 CLI、HTTP 或领域错误。

runtime 负责：

- 每一步生成 `GenerateRequest`。
- 校验 model capabilities、stream protocol、usage 和 tool choice。
- 校验工具白名单与 JSON Schema。
- 顺序执行工具并回灌 tool result。
- 累计 steps、messages 和 usage。
- 最大步数、stop condition、context budget 和 loop detection。

## Composition Root

应用应该有一个显式 composition root，而不是让工具读取全局配置：

```text
config
  -> model adapter
  -> prompt / skills
  -> context planner
  -> workspace / permission / tools
  -> registry snapshot
  -> agent.Agent
  -> session facade / store
```

iCoder 的 composition root 是 `demo/icoder/internal/icoder/app.go`。它使用 plain Go constructor 注入依赖，没有 package-global registry。

## Result Contract

同时检查 error、`Outcome` 和 `StopReason`。`OutcomeSuspended` 可以与 `nil` error 同时出现，例如输出达到上限、最大步数或上下文预算耗尽。

`RunResult.Messages` 只包含本轮新增的 assistant/tool messages。调用方还要单独保存当前 user message。
