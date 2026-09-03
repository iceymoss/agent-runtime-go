package advanced

import (
	"context"
	"encoding/json"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/mcp"
	"github.com/iceymoss/agent-runtime-go/permission"
)

// remoteTool exposes one MCP tool to the runtime as an ordinary agent.Tool.
//
// Adapting it here rather than giving the runtime a second kind of tool is what
// puts remote tools through the same permission decision, the same allowlist,
// and the same effect accounting as local ones. An MCP tool is defined by a
// remote service, so you do not know in advance what it will ask to do — the
// one thing you must not do is trust it more than your own code.
type remoteTool struct {
	manager    mcp.Manager
	scope      mcp.Scope
	generation mcp.Generation
	server     mcp.ServerID
	definition agent.ToolDefinition
}

func (t *remoteTool) Definition() agent.ToolDefinition { return t.definition }

// ReplayPolicy is Never because the remote side did not tell us whether the
// call is idempotent. Assuming it is would silently authorize a duplicate.
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
		// A transport failure is not something the model can correct.
		return agent.ToolResult{}, fmt.Errorf("mcp call %s: %w", t.definition.Name, err)
	}
	return agent.ToolResult{Content: joinContent(result.Content), IsError: result.IsError}, nil
}

// NewRemoteTool wraps one discovered MCP tool for the registry.
func NewRemoteTool(manager mcp.Manager, scope mcp.Scope, generation mcp.Generation, server mcp.ServerID, definition agent.ToolDefinition) agent.Tool {
	return &remoteTool{manager: manager, scope: scope, generation: generation, server: server, definition: definition}
}

// RemoteToolPolicy decides what a remote tool may do.
//
// Ask, not allow, is the right default here: the tool set comes from a service
// you do not control and can change between deployments, so "unknown network
// side effect" is the honest classification until someone says otherwise.
func RemoteToolPolicy(known map[string]permission.CheckDecision) permission.Policy {
	return permission.PolicyFunc{
		PolicyVersion: "mcp/v1",
		EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
			decision, ok := known[request.ToolName]
			if !ok {
				decision = permission.DecisionAsk
			}
			return permission.CheckResult{
				Decision: decision, PolicyVersion: "mcp/v1",
				InputDigest: request.InputDigest, RuleKey: "mcp-tool",
			}, nil
		},
	}
}

func joinContent(parts []mcp.Content) string {
	var out string
	for _, part := range parts {
		out += part.Text
	}
	return out
}
