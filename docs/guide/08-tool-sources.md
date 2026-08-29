# 8. 扩展工具来源

## 你现在遇到的问题

到现在为止工具都是你写的 Go 函数。但有些能力在别处：公司内部已经跑着一批 MCP server，或者你想让运维不改代码就能给助手加一段专用指令。

## 远端工具：MCP

`mcp` 负责发现和调用远端工具（stdio 和 streamable HTTP）。接进来的方式是**把它包装成一个普通的 `agent.Tool`**：

```go
type remoteTool struct {
	manager    mcp.Manager
	scope      mcp.Scope
	generation mcp.Generation
	server     mcp.ServerID
	definition agent.ToolDefinition
}

func (t *remoteTool) Definition() agent.ToolDefinition { return t.definition }

// Never：远端没告诉我们这个调用是否幂等，假定它是就等于悄悄授权了一次重复执行
func (t *remoteTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }

func (t *remoteTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var arguments map[string]any
	if err := json.Unmarshal([]byte(invocation.RawInput), &arguments); err != nil {
		return agent.ToolResult{IsError: true, Content: "参数不是合法 JSON"}, nil
	}
	result, err := t.manager.CallTool(ctx, mcp.ToolCall{
		Scope: t.scope, Generation: t.generation, ServerID: t.server,
		CallID: invocation.CallID, Name: t.definition.Name, Arguments: arguments,
	})
	if err != nil {
		// 传输故障不是模型能改正的
		return agent.ToolResult{}, fmt.Errorf("mcp call %s: %w", t.definition.Name, err)
	}
	return agent.ToolResult{Content: joinContent(result.Content), IsError: result.IsError}, nil
}
```

在这里适配、而不是给运行时开第二种工具类型，是有原因的：**远端工具因此走和本地工具完全相同的权限判定、白名单和 effect 记账**。

权限的默认值要反过来。本地工具你自己写的，知道它做什么；MCP 工具由一个你不控制的服务定义，而且可能在两次部署之间变了：

```go
func RemoteToolPolicy(known map[string]permission.CheckDecision) permission.Policy {
	return permission.PolicyFunc{
		PolicyVersion: "mcp/v1",
		EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
			decision, ok := known[request.ToolName]
			if !ok {
				decision = permission.DecisionAsk // 未知的一律问
			}
			return permission.CheckResult{
				Decision: decision, PolicyVersion: "mcp/v1",
				InputDigest: request.InputDigest, RuleKey: "mcp-tool",
			}, nil
		},
	}
}
```

连接器只应指向经过批准的 endpoint。

## 非可信指令：Skills

Skills 是一份可以由运维或用户提供的指令目录——"处理退款工单时先查这三个字段"这类，它们会被注入 prompt。`skills` 按需选择而不是全量注入，并把它们当作**非可信文本**对待：

```go
import "github.com/iceymoss/agent-runtime-go/skills"
```

再说一次第 7 章那条：**skills 不是权限边界**。一份 skill 写"你可以直接重启服务不用确认"不会让 `Gate` 失效，因为判定在 `Gate` 里，而 skill 只是 prompt 里的一段文本。把 skills 当成任何人都能写的输入来设计——因为它就是。

## 需要注意的

**远端工具的 schema 也是远端给的。** 运行时会按它校验参数，但那个 schema 本身可能是错的或恶意的。工具白名单（`Config.AllowedTools`）在这里比对本地工具更有用：它让"这次运行能用哪些远端工具"成为你的决定而不是远端的。

## 深入

- [mcp](../packages/mcp.md) —— 传输、发现、generation 租约
- [skills](../packages/skills.md) —— 目录、优先级、信任级别、注入边界
- 完整可运行代码：[`examples/guide/advanced/mcp.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/mcp.go)
