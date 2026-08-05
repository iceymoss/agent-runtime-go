# MCP 与 Sub-Agent

MCP 扩展可调用工具来源，Sub-Agent 扩展独立执行单元。两者都不是根 model/tool loop 的必选依赖。

## MCP

`mcp` 管理 tenant-scoped live MCP generations。应用提供：

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

## Sub-Agent

`subagent` 编排独立 child run，而不是在 parent goroutine 中递归共享可变状态。

应用提供 child `Runner` 与幂等 `ParentWaker`：

```go
service, err := subagent.New(subagent.Options{
    Store: subagent.NewMemoryStore(),
    Runner: childRunner,
    ParentWaker: parentWaker,
    WorkerID: "worker-1",
    LeaseDuration: time.Minute,
})
```

典型流程：

```text
Spawn -> reserve depth/fanout/budget
RunNext -> claim and execute one child
Commit -> terminal or suspended child state
Reconcile -> recover lease and deliver parent wake
```

生产实现应将 child 映射为独立 durable run。Parent wake 必须按稳定 `WakeKey` 幂等，child 不能直接修改 parent session 的可变内存。

## 选择边界

- 远程能力只是一个工具：MCP。
- 任务需要独立状态、预算、取消和恢复：Sub-Agent。
- 一个本地函数即可完成：普通 Tool，不要创建 child run。
