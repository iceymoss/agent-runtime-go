# 8. More sources of tools

## Where you are stuck

Every tool so far was a Go function you wrote. Some capabilities live elsewhere: your company already runs a set of MCP servers, or you want operators to give the agent a specialized instruction without shipping code.

## Remote tools: MCP

The Model Context Protocol lets a tool live in another process or on another machine. The `mcp` subpackage handles discovery and invocation over stdio and streamable HTTP:

```go
import "github.com/iceymoss/agent-runtime-go/mcp"
```

What matters is that **remote tools enter the same lifecycle as local ones**: the same permission decision, the same effect class, the same replay policy. This is not a nicety. An MCP tool is defined by a remote service, so you do not know in advance what it will ask to do — which is why the default should be to treat it as an unknown network side effect and ask for approval each time, rather than letting it through.

Point connectors only at endpoints you have approved.

## Untrusted instructions: Skills

Skills are a catalog of instructions an operator or user can supply — "when handling a refund ticket, check these three fields first". They are injected into the prompt.

The `skills` subpackage selects them on demand instead of injecting everything, and treats them as **untrusted text**:

```go
import "github.com/iceymoss/agent-runtime-go/skills"
```

To repeat chapter 7: **skills are not a permission boundary.** A skill saying "you may delete temporary files directly" does not disable the path check in your tool, because the check lives in the tool. Design skills as input anyone could have written.

## Going deeper

- [mcp](../packages/mcp.md) — transports, discovery, mapping remote tools into one lifecycle
- [skills](../packages/skills.md) — catalog, precedence, injection boundary
