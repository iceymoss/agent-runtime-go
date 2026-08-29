---
name: new-tool
description: 新增一个 Agent Tool（NewTool 反射 schema 或实现 Tool 接口）。当用户说加工具、新增 tool、加个能力给模型调用时触发。
---

新增一个模型可调用的工具。先确认加在哪里：**库内示例**（`examples/`）、**demo 应用**（`demo/icoder/internal/icoder/tools.go`）还是调用方项目。库的根包**不注册任何隐式全局工具**，不要往根包塞业务工具。

## 步骤

### 1. 确认输入
- 工具名（registry 内唯一且长期稳定，改名 = 破坏性变更）
- 输入字段与类型、输出内容格式（通常是 JSON 字符串）
- 是否有副作用（写文件、执行命令、调外部 API）→ 决定 `ReplayPolicy`
- 是否需要外部审批或等待 → 决定是否返回 `ToolSuspension`
- 需要哪些权限边界（工作区路径、命令白名单、大小上限）

### 2. 首选 NewTool，由结构体生成 schema

```go
type SearchInput struct {
	Query string `json:"query" description:"Search keywords"`
	Limit int    `json:"limit,omitempty" description:"Max results"`
}

tool := agent.MustNewTool("search_docs", "Search project documentation.",
	func(ctx context.Context, input SearchInput) (agent.ToolResult, error) {
		if err := ctx.Err(); err != nil {
			return agent.ToolResult{}, err
		}
		hits, err := index.Search(input.Query, input.Limit)
		if err != nil {
			// 模型可以改正的失败：交回给模型，不要终止 run
			return agent.ToolResult{IsError: true, Content: "search failed: " + err.Error()}, nil
		}
		return agent.ToolResult{Content: encodeJSON(hits)}, nil
	},
	agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent),
)
```

- 非指针且无 `omitempty` 的字段自动进 `required`
- 默认 strict（拒绝未声明属性，校验失败会反馈给模型自行修复）；确有需要才 `WithoutStrictSchema()`
- 有副作用的工具用 `WithToolReplayPolicy` 声明重放语义，并用 `agent.ToolExecutionKeyFromContext(ctx)` 做幂等去重
- 只有需要完全掌控 schema 时才直接实现 `Tool` 接口（`Definition` / `ReplayPolicy` / `Execute`），参考 `examples/tool-agent/main.go`。schema 不得引用外部 `$ref`

### 3. 注册并放行

```go
registry := agent.NewRegistry()
if err := registry.Register(tool); err != nil { return nil, err }
```

`AllowedTools` 里加上工具名。注意 `nil` = 放开全部已注册工具，`[]string{}` = 显式禁用工具。

### 4. 写测试

```bash
go test ./... -run TestSearchDocsTool -count=1
```

用 `agenttest.TestTool` 覆盖合法输入、非法输入（schema 拒绝）、错误路径；有副作用的再覆盖重复执行的幂等性。

### 5. 验证

跑完整 CI 门槛（见 `/gotest`）。改到 `demo/icoder` 时记得单独 vet/test 该 module。

## 严格约束
- **权限必须在工具实现里做实**：路径限制（`filepath.Clean` + 校验落在 root 内、拒绝 symlink）、命令白名单、大小上限。prompt 和 skills 里的约束不算数，模型输出是不可信输入
- 可恢复失败返回 `ToolResult{IsError: true}`（模型可见、可改正）；不可恢复失败返回 Go error（终止本次 attempt 并保留 cause）。两者不要互换
- 返回给模型的内容不得包含 API key、完整 URL、内部路径或堆栈
- 需要等待外部事件时返回 `ToolSuspension`，不要在 `Execute` 里阻塞或自起轮询协程
- 尊重 `ctx.Done()`；资源 defer 关闭
