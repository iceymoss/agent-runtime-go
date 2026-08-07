package icoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/mcp"
	"github.com/iceymoss/agent-runtime-go/permission"
	"github.com/iceymoss/agent-runtime-go/subagent"
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

func registerMCP(ctx context.Context, registry *agent.Registry, endpoint string, permissions permission.Service, sessionID func() string) (mcp.Manager, []string, error) {
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
	var names []string
	for _, server := range snapshot.Servers {
		for _, definition := range server.Tools {
			tool := &mcpAgentTool{manager: manager, scope: scope, generation: snapshot.Generation, serverID: server.ID, definition: definition.AgentDefinition(), upstream: definition.Name}
			wrapped := &authorizedTool{tool: tool, permission: permissions, action: "network.tool", sessionID: sessionID, resource: func(string) permission.Resource {
				return permission.Resource{Kind: "mcp-tool", Key: string(server.ID) + "/" + definition.Name}
			}}
			if err := registry.Register(wrapped); err != nil {
				return nil, nil, err
			}
			names = append(names, definition.Canonical)
		}
	}
	return manager, names, nil
}

type reviewRunner struct {
	agent   *agent.Agent
	mu      sync.RWMutex
	results map[subagent.ResultRef]string
}

func (r *reviewRunner) Run(ctx context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
	result, err := r.agent.Run(ctx, agent.RunRequest{Messages: []agent.Message{
		agent.NewSystemMessage("You are an independent read-only code reviewer. Inspect relevant files and git changes before reporting findings. Prioritize correctness, security, regressions, and missing tests. Return findings with file and line references; state explicitly when no findings are found."),
		agent.NewUserMessage(string(request.Input)),
	}})
	if err != nil {
		return subagent.RunResult{}, err
	}
	ref := subagent.ResultRef("review:" + digest(request.Input)[7:23])
	r.mu.Lock()
	r.results[ref] = result.Text
	r.mu.Unlock()
	return subagent.RunResult{State: subagent.ChildCompleted, ResultRef: ref, UsageFactKey: subagent.UsageFactKey(ref), Usage: subagent.Usage{InputTokens: int64(result.Usage.PromptTokens), OutputTokens: int64(result.Usage.CompletionTokens)}}, nil
}

func (r *reviewRunner) Result(ref subagent.ResultRef) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	value, ok := r.results[ref]
	return value, ok
}

type wakeRecorder struct{}

func (wakeRecorder) Wake(_ context.Context, request subagent.WakeRequest) error {
	return nil
}

type delegateReviewTool struct {
	service   *subagent.Service
	runner    *reviewRunner
	sessionID func() string
}

func (t *delegateReviewTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "delegate_review", Description: "Delegate a focused code review to an independent child run.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"task": map[string]any{"type": "string"}}, "required": []any{"task"}}}
}
func (*delegateReviewTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t *delegateReviewTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	limits := subagent.Limits{MaxDepth: 2, MaxFanout: 2, MaxInputTokens: 4096, MaxOutputTokens: 1024, MaxCostMicros: 1_000_000, MaxToolCalls: 10, MaxRuntime: time.Minute}
	reserve := subagent.Reservation{InputTokens: 4096, OutputTokens: 1024, CostMicros: 1_000_000, ToolCalls: 10, Runtime: time.Minute}
	sessionID := t.sessionID()
	receipt, err := t.service.Spawn(ctx, subagent.SpawnRequest{RequestKey: subagent.RequestKey(invocation.CallID), Parent: subagent.ParentRef{TenantKey: "local", SessionKey: subagent.SessionKey(sessionID), RunKey: subagent.RunKey(sessionID)}, AgentKey: "icoder.reviewer", Input: []byte(input.Task), Limits: limits, Reserve: reserve})
	if err != nil {
		return agent.ToolResult{}, err
	}
	snapshot, claimed, err := t.service.RunNext(ctx, "local")
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !claimed {
		return agent.ToolResult{Content: "child accepted but not claimed", IsError: true}, nil
	}
	if _, err := t.service.Reconcile(ctx, subagent.ReconcileRequest{TenantKey: "local", Limit: 10}); err != nil {
		return agent.ToolResult{}, err
	}
	review, ok := t.runner.Result(snapshot.ResultRef)
	if !ok {
		return agent.ToolResult{Content: "child completed without a readable result", IsError: true}, nil
	}
	data, err := marshalString(map[string]any{"child": receipt.Child.RunKey, "state": snapshot.State, "result_ref": snapshot.ResultRef, "review": review})
	return agent.ToolResult{Content: data}, err
}

func registerSubagent(registry *agent.Registry, model agent.Model, modelName string, workspace *Workspace, sessionID func() string) error {
	reviewRegistry := agent.NewRegistry()
	for _, tool := range []agent.Tool{workingDirectoryTool{workspace}, listFilesTool{workspace}, globFilesTool{workspace}, readFileTool{workspace}, searchCodeTool{workspace}, gitStatusTool{workspace}, gitDiffTool{workspace}} {
		if err := reviewRegistry.Register(tool); err != nil {
			return err
		}
	}
	maxTokens := 2048
	reviewAgent, err := agent.New(agent.Config{Key: "icoder.reviewer", ModelName: modelName, MaxSteps: 10, MaxTokens: &maxTokens, AllowedTools: reviewRegistry.Names()}, model, reviewRegistry)
	if err != nil {
		return err
	}
	runner := &reviewRunner{agent: reviewAgent, results: make(map[subagent.ResultRef]string)}
	service, err := subagent.New(subagent.Options{Store: subagent.NewMemoryStore(), Runner: runner, ParentWaker: wakeRecorder{}, WorkerID: "icoder-worker", LeaseDuration: time.Minute})
	if err != nil {
		return err
	}
	return registry.Register(&delegateReviewTool{service: service, runner: runner, sessionID: sessionID})
}

func errorsJoin(left, right error) error {
	return errors.Join(left, right)
}
