# mcp

`mcp` 包是一个租户隔离的 MCP（Model Context Protocol）能力运行时：按租户维护与外部 MCP server 的连接（stdio 或 streamable HTTP），把上游工具规范化成带命名空间的工具目录，并在严格校验之后代理工具调用。

## 是什么

核心是 `Manager` 接口，由 `NewManager(source ConfigSource, connector Connector)` 构造。`ConfigSource` 提供每个租户的服务器配置（应用自己实现，通常读数据库或配置中心），`Connector` 负责真正建立连接——包内置 `NewStdioConnector`（子进程）和 `NewStreamableHTTPConnector`（HTTP）两种：

```go
type ConfigSource interface {
    Snapshot(context.Context, Scope) (ConfigSnapshot, error)
}

type Connector interface {
    Connect(context.Context, ConnectRequest) (Client, error)
}

type Manager interface {
    StartScope(context.Context, Scope) error
    Snapshot(Scope) (Snapshot, bool)
    CallTool(context.Context, ToolCall) (ToolResult, error)
    AcquireGeneration(context.Context, Scope, Generation) (GenerationLease, error)
    Reload(context.Context, Scope) (Snapshot, error)
    // 以及 CloseScope / WaitReady / Refresh / Status / Close
}
```

每次成功加载配置并连接后，Manager 生成一个不可变的连接代（`Snapshot.Generation`，内容摘要 + 连接序号）。快照中的每个工具是一条 `ToolDefinition`，其 `Canonical` 字段是 `mcp__<serverID>__<toolName>` 形式的全局唯一名，可通过 `AgentDefinition()` 直接转换为根包的 `agent.ToolDefinition` 注册给模型。

Manager 只持有活跃的传输连接，不做任何持久化；配置的真相源始终在应用侧的 `ConfigSource`。

## 为什么需要它

MCP server 配置往往来自租户输入（命令参数、HTTP 端点、header），直接透传就是命令注入和 SSRF 的入口。没有这个包，你需要自己实现：配置进程前的白名单校验、secret 与配置分离、连接生命周期与代际切换（换配置时旧连接何时安全关闭）、工具调用的边界检查。这个包的安全边界（外部安全测试锁定的行为）：

- **校验先于连接**：非法配置在 `normalizeConfigSnapshot` 阶段就被拒绝，绝不会到达 `Connector`——stdio 参数含换行、`ServerKey` 不合法、HTTP header 命中禁用名单，都直接返回错误。
- **stdio 是白名单制**：`StdioRegistry` 中的可执行文件必须是绝对路径且真实可执行；租户配置只能通过 `ServerKey` 引用注册项，额外参数默认全部拒绝（除非注册项提供 `ValidateArgs`），环境变量只能设置 `AllowedEnv` 中列出的名字，值一律经 `SecretProvider` 按租户解析，不接受明文。
- **HTTP 默认最小暴露**：端点必须是绝对 URL 且不含 userinfo、query、fragment；默认要求 HTTPS，且解析出的 IP 不得是回环/私网地址（`HTTPPolicy.AllowInsecureLoopback` / `AllowPrivateNetworks` 可显式放开）；`Host`、`Content-Length`、`proxy-*`、`sec-*` 等 header 被永久禁止。
- **调用边界**：`CallTool` 校验 generation、租户、server、工具名都存在于该代快照中，超时用 `CallTimeout`（默认 30s）截断，结果超过 `MaxResultBytes`（默认 4MB）返回 `ErrResultTooLarge`。

什么时候不需要它：如果你只是在单进程里调用自己写的 Go 工具函数（用根包的 `agent.Registry` 即可），或者只连一个自己完全掌控的 MCP server 且无多租户诉求，直接使用一个 MCP 客户端库就够了。

## 怎么用

```go
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/mcp"
)

type staticSource struct{}

func (staticSource) Snapshot(_ context.Context, _ mcp.Scope) (mcp.ConfigSnapshot, error) {
	return mcp.ConfigSnapshot{
		Generation: "v1",
		Servers: []mcp.ServerConfig{{
			ID:       "search",
			Enabled:  true,
			Required: true,
			Transport: mcp.TransportConfig{
				Kind: mcp.TransportStreamableHTTP,
				HTTP: &mcp.HTTPConfig{Endpoint: "https://mcp.example.com/v1"},
			},
		}},
	}, nil
}

func main() {
	ctx := context.Background()

	connector, err := mcp.NewStreamableHTTPConnector(http.DefaultClient, mcp.HTTPPolicy{}, nil)
	if err != nil {
		log.Fatal(err)
	}
	manager, err := mcp.NewManager(staticSource{}, connector)
	if err != nil {
		log.Fatal(err)
	}
	defer manager.Close(ctx)

	scope := mcp.Scope{TenantKey: agent.TenantKey("tenant-a")}
	if err := manager.StartScope(ctx, scope); err != nil {
		log.Fatal(err)
	}

	snapshot, ok := manager.Snapshot(scope)
	if !ok {
		log.Fatal("scope not ready")
	}
	for _, server := range snapshot.Servers {
		for _, tool := range server.Tools {
			fmt.Println(tool.Canonical) // 例如 mcp__search__lookup
		}
	}

	result, err := manager.CallTool(ctx, mcp.ToolCall{
		Scope:      scope,
		Generation: snapshot.Generation,
		ServerID:   "search",
		Name:       "lookup",
		Arguments:  map[string]any{"query": "golang"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.Content[0].Text)
}
```

关键行为：

- `NewManager` 不连接任何东西；`StartScope` 才对该租户拉配置并建连，租户之间完全独立（外部测试验证构造函数不枚举租户）。
- `ToolCall.Generation` 必填且必须是当前持有的代；配置变更用 `Reload` 产生新代，旧代在所有租约释放后才关闭连接，避免在途调用被掐断。需要跨多次调用固定一代时用 `AcquireGeneration` 拿 `GenerationLease`，用完 `Close()`。
- `Enabled: false` 的 server 直接跳过；`Required: true` 的 server 连接失败会让整次加载失败，`Required: false` 只产生一条 `Diagnostic` 并把状态标为 `StateDegraded`。
- server 必须在 `Initialize` 时声明 `tools` capability，否则拒绝（`ErrCapabilityInvalid`）；工具名不合法或重复同样拒绝整个 server。
- `ConnectTimeout` / `CallTimeout` / `MaxResultBytes` 为零时取默认值（10s / 30s / 4MB），但超过策略上限（5min / 10min）会被拒绝。

## 常见问题

**Q: 连 `http://localhost` 的本地 server 报 `ErrTransportDenied`？**
A: 默认策略拒绝非 HTTPS 和解析到回环/私网地址的端点（防 SSRF）。本地开发需显式传 `mcp.HTTPPolicy{AllowInsecureLoopback: true}`；连内网 server 需 `AllowPrivateNetworks: true`。生产环境建议配合 `AllowedHosts` 白名单。

**Q: stdio server 配了 `Args` 却被拒绝，报 "additional arguments are not allowed"？**
A: 白名单设计：租户传入的参数默认全部拒绝，只有注册项显式提供 `ValidateArgs` 校验函数才放行。固定参数应写在 `StdioServer.FixedArgs` 里，由平台侧掌控。

**Q: 为什么工具名变成了 `mcp__search__lookup` 这种形式？**
A: `ToolDefinition.Canonical` 把 server ID 和工具名拼接并把非 `[A-Za-z0-9_]` 字符替换为 `_`，保证多 server 工具在同一个模型工具列表里不冲突。`CallTool` 的 `Name` 用原始名或 canonical 名均可匹配。

**Q: `CallTool` 报 `ErrGenerationUnavailable`，但 server 明明活着？**
A: 你传的 `Generation` 已被新一代替换并回收，或与租约不匹配。正确做法是每轮对话开始时取一次 `Snapshot`，用该代完成本轮所有调用；长事务用 `AcquireGeneration` 显式续命。

**Q: 结果被 `ErrResultTooLarge` 拒绝怎么办？**
A: 单个 server 可在 `ServerConfig.MaxResultBytes` 调大限制（默认 4MB）。注意这是对文本、二进制、结构化内容序列化后的总量估算，目的是防止上游用超大响应耗尽内存。

**Q: header 里配 `Authorization` 和 `AuthRef` 有什么区别？**
A: `HTTPConfig.AuthRef` 是标准做法，值经 `SecretProvider` 解析后作为 `Authorization` 头发送。`HeaderRefs` 里再配一个 `authorization` 会因重复被拒绝；`Host`、`proxy-*`、`sec-*` 等永远禁止。所有 secret 都以引用（`SecretRef`）形式存在配置里，错误信息中也不会泄露解析后的值。
