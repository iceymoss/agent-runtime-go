# mcp

The `mcp` package is a tenant-isolated MCP (Model Context Protocol) capability runtime: it maintains per-tenant connections to external MCP servers (stdio or streamable HTTP), normalizes upstream tools into a namespaced tool catalog, and proxies tool calls after strict validation.

## What it is

The core is the `Manager` interface, constructed by `NewManager(source ConfigSource, connector Connector)`. `ConfigSource` supplies each tenant's server config (implemented by the application, typically from a database or config center). `Connector` establishes the real connection—built-ins are `NewStdioConnector` (subprocess) and `NewStreamableHTTPConnector` (HTTP):

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

After a successful config load and connect, Manager produces an immutable connection generation (`Snapshot.Generation`, content digest + connection sequence). Each tool in the snapshot is a `ToolDefinition` whose `Canonical` field is a globally unique name of the form `mcp__<serverID>__<toolName>`, convertible via `AgentDefinition()` to a root-package `agent.ToolDefinition` for model registration.

Manager holds only live transport connections and does no persistence; the source of truth for config always stays in the application's `ConfigSource`.

## Why you need it

MCP server config often comes from tenant input (command args, HTTP endpoints, headers). Passing it through unchecked is an injection and SSRF surface. Without this package you would need: pre-connect allowlist validation, secret/config separation, connection lifecycle and generation switching (when to safely close old connections on config change), and call boundary checks. This package's security boundary (locked by external security tests):

- **Validate before connect**: illegal config is rejected in `normalizeConfigSnapshot` and never reaches `Connector`—stdio args with newlines, illegal `ServerKey`, HTTP headers on the deny list all return errors immediately.
- **stdio is allowlist-based**: executables in `StdioRegistry` must be absolute paths and actually executable; tenant config may only reference registry entries by `ServerKey`; extra args are rejected by default (unless the entry provides `ValidateArgs`); env vars may only set names in `AllowedEnv`, with values always resolved per tenant via `SecretProvider`—no plaintext secrets.
- **HTTP defaults to minimal exposure**: endpoints must be absolute URLs without userinfo, query, or fragment; HTTPS is required by default, and resolved IPs must not be loopback/private (`HTTPPolicy.AllowInsecureLoopback` / `AllowPrivateNetworks` can opt in); headers such as `Host`, `Content-Length`, `proxy-*`, `sec-*` are permanently banned.
- **Call boundaries**: `CallTool` validates that generation, tenant, server, and tool name exist in that generation's snapshot; timeouts use `CallTimeout` (default 30s); results over `MaxResultBytes` (default 4MB) return `ErrResultTooLarge`.

When you do not need it: if you only call your own Go tool functions in-process (root-package `agent.Registry` is enough), or you connect a single fully controlled MCP server with no multi-tenant needs, a plain MCP client library is sufficient.

## How to use it

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

Sample output (the example `https://mcp.example.com` is not a real endpoint—you need a reachable MCP server to run this; the output shape matches behavior locked by package tests):

```text
mcp__search__lookup
Go is an open source programming language that makes it easy to build simple, reliable, and efficient software.
```

Key behavior:

- `NewManager` connects nothing; `StartScope` fetches config and connects for that tenant. Tenants are fully independent (external tests verify the constructor does not enumerate tenants).
- `ToolCall.Generation` is required and must be a currently held generation; config changes use `Reload` to produce a new generation; old generations close connections only after all leases release, so in-flight calls are not cut off. To pin a generation across calls, use `AcquireGeneration` for a `GenerationLease`, then `Close()`.
- Servers with `Enabled: false` are skipped; `Required: true` servers that fail to connect fail the whole load; `Required: false` produces one `Diagnostic` and marks state `StateDegraded`.
- Servers must declare the `tools` capability at `Initialize`, or they are rejected (`ErrCapabilityInvalid`); illegal or duplicate tool names likewise reject the whole server.
- Zero `ConnectTimeout` / `CallTimeout` / `MaxResultBytes` take defaults (10s / 30s / 4MB); values above policy caps (5min / 10min) are rejected.

## FAQ

**Q: Connecting to a local `http://localhost` server returns `ErrTransportDenied`?**
A: Default policy rejects non-HTTPS and endpoints that resolve to loopback/private addresses (SSRF protection). For local development pass `mcp.HTTPPolicy{AllowInsecureLoopback: true}`; for private-network servers set `AllowPrivateNetworks: true`. In production prefer an `AllowedHosts` allowlist.

**Q: stdio server with `Args` is rejected with "additional arguments are not allowed"?**
A: Allowlist design: tenant-supplied args are rejected by default; only registry entries that provide a `ValidateArgs` function may accept them. Fixed args belong in `StdioServer.FixedArgs`, controlled by the platform.

**Q: Why do tool names become forms like `mcp__search__lookup`?**
A: `ToolDefinition.Canonical` joins server ID and tool name, replacing non-`[A-Za-z0-9_]` characters with `_`, so tools from multiple servers do not collide in one model tool list. `CallTool`'s `Name` matches either the original or canonical name.

**Q: `CallTool` returns `ErrGenerationUnavailable`, but the server is still alive?**
A: The `Generation` you passed was replaced and reclaimed, or does not match the lease. Take a `Snapshot` at the start of each conversation turn and use that generation for all calls in the turn; for long transactions use `AcquireGeneration` to keep it alive.

**Q: Result rejected with `ErrResultTooLarge`?**
A: Raise the limit per server via `ServerConfig.MaxResultBytes` (default 4MB). This estimates total serialized text, binary, and structured content to stop oversized upstream responses from exhausting memory.

**Q: Difference between `Authorization` in headers and `AuthRef`?**
A: `HTTPConfig.AuthRef` is the standard approach: the value is resolved via `SecretProvider` and sent as the `Authorization` header. Adding another `authorization` in `HeaderRefs` is rejected as a duplicate; `Host`, `proxy-*`, `sec-*`, etc. are always banned. All secrets exist in config as references (`SecretRef`); error messages never leak resolved values.
