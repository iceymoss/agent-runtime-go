package icoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/mcp"
	"github.com/iceymoss/agent-runtime-go/permission"
)

type staticMCPConfig struct{ endpoint string }

func (s staticMCPConfig) Snapshot(context.Context, mcp.Scope) (mcp.ConfigSnapshot, error) {
	return mcp.ConfigSnapshot{Generation: "icoder-mcp-v1", Servers: []mcp.ServerConfig{{
		ID: "remote", Version: "v1", Enabled: true, Required: true,
		Transport:      mcp.TransportConfig{Kind: mcp.TransportStreamableHTTP, HTTP: &mcp.HTTPConfig{Endpoint: s.endpoint}},
		ConnectTimeout: 10 * time.Second, CallTimeout: time.Minute, MaxResultBytes: maxToolOutput,
	}}}, nil
}

type mcpAgentTool struct {
	manager    mcp.Manager
	scope      mcp.Scope
	generation mcp.Generation
	serverID   mcp.ServerID
	definition agent.ToolDefinition
	upstream   string
}

func (t *mcpAgentTool) Definition() agent.ToolDefinition { return t.definition }
func (t *mcpAgentTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t *mcpAgentTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var arguments map[string]any
	if err := json.Unmarshal([]byte(invocation.RawInput), &arguments); err != nil {
		return agent.ToolResult{}, err
	}
	result, err := t.manager.CallTool(ctx, mcp.ToolCall{Scope: t.scope, Generation: t.generation, ServerID: t.serverID, CallID: invocation.CallID, Name: t.upstream, Arguments: arguments})
	if err != nil {
		return agent.ToolResult{}, err
	}
	data, err := marshalString(result)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data, IsError: result.IsError}, nil
}

// mcpToolEntries connects to the configured MCP endpoint and maps every remote
// tool into the same lifecycle the local tools use.
//
// A remote tool's behavior is defined by someone else, so it is classified as an
// external effect with no idempotency and never replayed. That is what makes an
// unclassified remote tool ask for approval instead of running by default.
func mcpToolEntries(ctx context.Context, endpoint string) (mcp.Manager, []lifecycleEntry, error) {
	if endpoint == "" {
		return nil, nil, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		return nil, nil, fmt.Errorf("invalid MCP endpoint")
	}
	connector, err := mcp.NewStreamableHTTPConnector(&http.Client{Timeout: time.Minute}, mcp.HTTPPolicy{
		AllowInsecureLoopback: true,
		AllowedHosts:          []string{parsed.Host},
		MaxResponseBytes:      maxToolOutput,
	}, nil)
	if err != nil {
		return nil, nil, err
	}
	manager, err := mcp.NewManager(staticMCPConfig{endpoint: endpoint}, connector)
	if err != nil {
		return nil, nil, err
	}
	scope := mcp.Scope{TenantKey: "local"}
	if err := manager.StartScope(ctx, scope); err != nil {
		_, closeErr := manager.Close(context.Background())
		return nil, nil, errorsJoin(err, closeErr)
	}
	snapshot, ok := manager.Snapshot(scope)
	if !ok {
		return nil, nil, fmt.Errorf("MCP manager produced no snapshot")
	}
	var entries []lifecycleEntry
	for _, server := range snapshot.Servers {
		for _, definition := range server.Tools {
			tool := &mcpAgentTool{manager: manager, scope: scope, generation: snapshot.Generation, serverID: server.ID, definition: definition.AgentDefinition(), upstream: definition.Name}
			entries = append(entries, lifecycleEntry{
				tool:     tool,
				metadata: externalMetadata("network.tool", "mcp."+string(server.ID), false),
				resource: permission.Resource{Kind: "mcp-tool", Key: string(server.ID) + "/" + definition.Name},
			})
		}
	}
	return manager, entries, nil
}

func errorsJoin(left, right error) error {
	return errors.Join(left, right)
}
