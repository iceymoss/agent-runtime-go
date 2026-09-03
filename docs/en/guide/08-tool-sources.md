# 8. More sources of tools

## Where you are stuck

Every tool so far was a Go function you wrote. Some capabilities live elsewhere: your company already runs MCP servers, or you want operators to add a specialized instruction without shipping code.

## Remote tools: MCP

`mcp` handles discovery and invocation of remote tools over stdio and streamable HTTP. You bring one in by **wrapping it as an ordinary `agent.Tool`**:

```go
type remoteTool struct {
	manager    mcp.Manager
	scope      mcp.Scope
	generation mcp.Generation
	server     mcp.ServerID
	definition agent.ToolDefinition
}

func (t *remoteTool) Definition() agent.ToolDefinition { return t.definition }

// Never: the remote side did not tell us whether the call is idempotent, and
// assuming it is would silently authorize a duplicate.
func (t *remoteTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }

func (t *remoteTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var arguments map[string]any
	if err := json.Unmarshal([]byte(invocation.RawInput), &arguments); err != nil {
		return agent.ToolResult{IsError: true, Content: "arguments are not valid JSON"}, nil
	}
	result, err := t.manager.CallTool(ctx, mcp.ToolCall{
		Scope: t.scope, Generation: t.generation, ServerID: t.server,
		CallID: invocation.CallID, Name: t.definition.Name, Arguments: arguments,
	})
	if err != nil {
		// A transport failure is not something the model can correct.
		return agent.ToolResult{}, fmt.Errorf("mcp call %s: %w", t.definition.Name, err)
	}
	return agent.ToolResult{Content: joinContent(result.Content), IsError: result.IsError}, nil
}
```

Adapting here, rather than giving the runtime a second kind of tool, is what puts **remote tools through exactly the same permission decision, allowlist, and effect accounting as local ones**.

The permission default has to flip. You wrote your local tools and know what they do; an MCP tool is defined by a service you do not control and may change between deployments:

```go
func RemoteToolPolicy(known map[string]permission.CheckDecision) permission.Policy {
	return permission.PolicyFunc{
		PolicyVersion: "mcp/v1",
		EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
			decision, ok := known[request.ToolName]
			if !ok {
				decision = permission.DecisionAsk // anything unknown gets asked
			}
			return permission.CheckResult{
				Decision: decision, PolicyVersion: "mcp/v1",
				InputDigest: request.InputDigest, RuleKey: "mcp-tool",
			}, nil
		},
	}
}
```

Point connectors only at endpoints you have approved.

## Untrusted instructions: Skills

Skills are a catalog of instructions an operator or user can supply — "when handling a refund ticket, check these three fields first" — injected into the prompt. `skills` selects them on demand instead of injecting everything, and treats them as **untrusted text**:

```go
import "github.com/iceymoss/agent-runtime-go/skills"
```

To repeat chapter 7: **skills are not a permission boundary.** A skill saying "you may restart services without confirming" does not disable the `Gate`, because the decision is in the `Gate` and the skill is just text in a prompt. Design skills as input anyone could have written — because it is.

## Worth knowing

**A remote tool's schema comes from the remote side too.** The runtime validates arguments against it, but the schema itself may be wrong or hostile. The tool allowlist (`Config.AllowedTools`) is more useful here than for local tools: it makes "which remote tools may this run use" your decision rather than theirs.

## Going deeper

- [mcp](../packages/mcp.md) — transports, discovery, generation leases
- [skills](../packages/skills.md) — catalog, precedence, trust levels, injection boundary
- Complete runnable code: [`examples/guide/advanced/mcp.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/advanced/mcp.go)
