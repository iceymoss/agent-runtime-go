# MCP

`agent/mcp` 管理 tenant-scoped live MCP generations。应用提供：

- `ConfigSource`：返回 exact config generation。
- `Connector`：建立并初始化 approved transport。
- 可选 `SecretProvider`：解析 opaque secret refs。

```go
manager, err := mcp.NewManager(source, connector)
err = manager.StartScope(ctx, scope)
snapshot, ok := manager.Snapshot(scope)
```

每个 discovered `mcp.ToolDefinition` 可转换成 root definition：

```go
definition := discovered.AgentDefinition()
```

应用仍需实现 `agent.Tool.Execute`，用 exact scope、generation、server ID 和 upstream name 调用 `manager.CallTool`。iCoder 的 `mcpAgentTool` 就是这个 bridge。

## Security

- Stdio executable 必须来自应用 allowlist，不接受任意命令。
- HTTP endpoint 应使用 `HTTPPolicy` 限制 scheme、host、redirect、private network、headers 和 response size。
- 初始默认 `ReplayPolicyNever`，除非远端 effect 的幂等性得到独立证明。
- rich MCP content 转成 `agent.ToolResult` 时不能静默丢字段。
- generation refresh 后，正在使用的旧 generation 应通过 lease 固定。

iCoder 只支持显式 `--mcp-url` 的 Streamable HTTP server，并将 endpoint host 加入精确 allowlist；没有配置时不启动 MCP，也不注册 MCP tools。
