# Tool 与 Permission

根 Tool 解决“模型可以调用什么”，Permission 解决“当前主体是否允许执行这次具体 effect”。这两个问题不能只靠 Prompt 合并。

## Root Tool

普通工具实现：

```go
type Tool interface {
    Definition() ToolDefinition
    ReplayPolicy() ReplayPolicy
    Execute(context.Context, ToolInvocation) (ToolResult, error)
}
```

可修正错误使用 `ToolResult{IsError: true}`。非 `nil` Go error 表示 attempt 失败。`ReplayPolicyIdempotent` 只是一项声明，真正去重仍由工具或下游使用 `ExecutionKey` 完成。

## Workspace Tool

Code Agent 应让所有文件和命令工具依赖应用自有 `Workspace` port。安全策略必须在实现中执行：

- canonical root containment。
- 拒绝绝对路径和 `..` escape。
- 处理 symlink 与文件大小。
- 写入使用 expected digest/CAS。
- 命令使用结构化 program/args/working directory/timeout。

Prompt 不是安全边界。

工作目录同样是应用状态。iCoder 的 `/cd` 由 REPL host 调用 `Workspace.ChangeDirectory`，所有本地工具再从该目录解析相对路径；自然语言中的 `cd` 不会修改工具状态。`get_working_directory` 允许模型查询当前真实 cwd。

iCoder 的 `run_command` 只允许 `go test/vet/build/fmt` 和 `git status/diff/log/show`，并限制 workspace、timeout 与输出大小。生产环境仍应把命令放进 OS sandbox，而不是依赖 allowlist 作为完整隔离。

网络工具同样应通过应用 port 隔离。iCoder 定义 `WeatherProvider`，再用 `weatherTool` 转成 `agent.Tool`；默认实现调用 Open-Meteo，权限动作是 `network.read`。生产实现应集中控制 endpoint allowlist、timeout、response limit、proxy、credential 和 egress audit。

## Permission Bridge

`permission` 的核心是 `Policy`、`Store` 和 `Service`。调用方把 canonical tool input 映射成准确的 `Subject`、`Resource` 和 `Action`：

```go
decision, err := service.Check(ctx, permission.CheckRequest{
    RequestKey: stableKey,
    Subject: subject,
    Resource: resource,
    ToolName: invocation.Name,
    Action: "workspace.write",
    InputDigest: inputDigest,
    PolicyVersion: policy.Version(),
})
```

iCoder 的 `authorizedTool` 展示根 `agent.Tool` 到 `permission.Service` 的 bridge：read/search 自动 allow，write 默认 ask，`--allow-writes` 才 allow。

## Advanced Tool Lifecycle

需要 interceptor、effect ledger、fence 和 unknown-effect classification 时使用 `tool.Executor`。根 runtime 仍调用 `agent.Tool`，应用需要一个 adapter 将 invocation 映射到 `tool.ExecuteRequest`。

当前不要假定 `ask -> approve -> 重调 Execute` 会自动恢复同一 prepared execution。生产集成必须显式设计 approval revalidation 和 ledger transition。

## 错误选择

| 情况 | 返回 |
|---|---|
| 参数语义可修正 | `ToolResult{IsError: true}` |
| 资源不存在，模型可换方案 | `ToolResult{IsError: true}` |
| Context cancelled | `ctx.Err()` |
| 数据库或网络基础设施失败 | 非 `nil` Go error |
| effect 已发生但结果未知 | 进入 unknown/resolve 流程，不盲目重试 |

## 测试

Tool 测试至少覆盖正常输入、schema 拒绝、可修正错误、context cancellation 和 replay/idempotency。敏感工具还需证明 permission subject/resource/action 映射准确。
