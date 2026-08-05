package icoder

import (
	"context"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	jsoncodec "github.com/iceymoss/agent-runtime-go/demo/icoder/internal/jsoncodec"
	"github.com/iceymoss/agent-runtime-go/permission"
)

const policyVersion permission.PolicyVersion = "icoder-policy-v1"

type authorizedTool struct {
	tool       agent.Tool
	permission permission.Service
	action     string
	resource   func(string) permission.Resource
	sessionID  func() string
}

func (t *authorizedTool) Definition() agent.ToolDefinition { return t.tool.Definition() }
func (t *authorizedTool) ReplayPolicy() agent.ReplayPolicy { return t.tool.ReplayPolicy() }

func (t *authorizedTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	inputDigest := permission.InputDigest(digest([]byte(invocation.RawInput)))
	sessionID := t.sessionID()
	requestKey := permission.RequestKey(digest([]byte(sessionID + "\x00" + invocation.CallID + "\x00" + invocation.Name + "\x00" + invocation.RawInput)))
	decision, err := t.permission.Check(ctx, permission.CheckRequest{
		RequestKey: requestKey,
		Subject:    permission.Subject{TenantKey: "local", PrincipalKey: "cli-user", ActorType: "human"},
		Resource:   t.resource(invocation.RawInput), SessionRef: sessionID,
		RunRef: permission.RunRef(sessionID), AttemptRef: permission.AttemptRef(invocation.CallID),
		ExecutionRef: permission.ExecutionRef(invocation.CallID), FenceToken: 1,
		ToolCallID: invocation.CallID, ToolName: invocation.Name, Action: t.action,
		InputDigest: inputDigest, ToolGeneration: "icoder-tools-v1", DefinitionDigest: "icoder-v1",
		PolicyVersion: policyVersion, ApprovalExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		return agent.ToolResult{}, err
	}
	switch decision.Decision {
	case permission.DecisionAllow:
		return t.tool.Execute(ctx, invocation)
	case permission.DecisionAsk:
		return agent.ToolResult{IsError: true, StopTurn: true, Content: fmt.Sprintf("approval required: request=%s token=%s", decision.Blocker.RequestRef, decision.Blocker.ResumeToken)}, nil
	default:
		return agent.ToolResult{IsError: true, StopTurn: true, Content: "permission denied: " + decision.ReasonCode}, nil
	}
}

func NewPermissionService(allowWrites bool) (permission.Service, error) {
	policy := permission.PolicyFunc{PolicyVersion: policyVersion, EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
		result := permission.CheckResult{PolicyVersion: request.PolicyVersion, InputDigest: request.InputDigest}
		switch request.Action {
		case "workspace.read", "workspace.search", "network.read", "subagent.spawn":
			result.Decision, result.RuleKey = permission.DecisionAllow, "safe-local-operation"
		case "workspace.write", "workspace.command":
			if allowWrites {
				result.Decision, result.RuleKey = permission.DecisionAllow, "cli-write-flag"
			} else {
				result.Decision, result.RuleKey = permission.DecisionAsk, "workspace-write-approval"
				result.Constraint = permission.GrantConstraint{Scope: permission.ScopeInvocation, ExpiresAt: request.ApprovalExpiresAt}
			}
		default:
			result.Decision, result.ReasonCode = permission.DecisionDeny, "no-matching-rule"
		}
		return result, nil
	}}
	return permission.NewService(permission.ServiceOptions{Policy: policy, Store: permission.NewMemoryStore()})
}

type readFileTool struct{ workspace *Workspace }

func (t readFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "read_file", Description: "Read a UTF-8 text file inside the workspace.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}}
}

type listFilesTool struct{ workspace *Workspace }

type workingDirectoryTool struct{ workspace *Workspace }

func (t workingDirectoryTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "get_working_directory", Description: "Return the current working directory used by local workspace tools.", Strict: true, Parameters: map[string]any{"type": "object"}}
}
func (workingDirectoryTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t workingDirectoryTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	return agent.ToolResult{Content: t.workspace.WorkingDirectory()}, nil
}

func (t listFilesTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "list_files", Description: "List files inside the workspace.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}}}
}
func (listFilesTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t listFilesTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Limit int `json:"limit"`
	}
	if err := jsoncodec.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	files, err := t.workspace.ListFiles(ctx, input.Limit)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := jsoncodec.MarshalString(files)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data}, nil
}
func (readFileTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t readFileTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := jsoncodec.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	content, err := t.workspace.ReadFile(ctx, input.Path)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: content}, nil
}

type searchCodeTool struct{ workspace *Workspace }

func (t searchCodeTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "search_code", Description: "Search literal text in workspace files.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200}}, "required": []any{"pattern"}}}
}
func (searchCodeTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t searchCodeTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Pattern string `json:"pattern"`
		Limit   int    `json:"limit"`
	}
	if err := jsoncodec.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	matches, err := t.workspace.Search(ctx, input.Pattern, input.Limit)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := jsoncodec.MarshalString(matches)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data}, nil
}

type writeFileTool struct{ workspace *Workspace }

func (t writeFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "write_file", Description: "Replace one file inside the workspace. Requires permission.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "expected_digest": map[string]any{"type": "string"}}, "required": []any{"path", "content"}}}
}

type runCommandTool struct{ workspace *Workspace }

func (t runCommandTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "run_command", Description: "Run an approved build, test, format, or Git inspection command in the workspace. Requires permission.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"program": map[string]any{"type": "string", "enum": []any{"go", "git"}}, "args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120}}, "required": []any{"program", "args"}}}
}
func (runCommandTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t runCommandTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Program        string   `json:"program"`
		Args           []string `json:"args"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := jsoncodec.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	if err := validateCommand(input.Program, input.Args); err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	result, err := t.workspace.RunCommand(ctx, input.Program, input.Args, time.Duration(input.TimeoutSeconds)*time.Second)
	if err != nil {
		return agent.ToolResult{}, err
	}
	data, err := jsoncodec.MarshalString(result)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data, IsError: result.ExitCode != 0}, nil
}

func validateCommand(program string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("command arguments are required")
	}
	switch program {
	case "go":
		switch args[0] {
		case "test", "vet", "build", "fmt":
			return nil
		}
	case "git":
		switch args[0] {
		case "status", "diff", "log", "show":
			return nil
		}
	}
	return fmt.Errorf("command is not approved")
}
func (writeFileTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t writeFileTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Path           string `json:"path"`
		Content        string `json:"content"`
		ExpectedDigest string `json:"expected_digest"`
	}
	if err := jsoncodec.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	result, err := t.workspace.WriteFile(ctx, input.Path, input.Content, input.ExpectedDigest)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: `{"digest":"` + result + `"}`}, nil
}

func registerTools(registry *agent.Registry, workspace *Workspace, weather WeatherProvider, permissions permission.Service, sessionID func() string) error {
	tools := []struct {
		tool   agent.Tool
		action string
	}{
		{workingDirectoryTool{workspace: workspace}, "workspace.read"},
		{listFilesTool{workspace: workspace}, "workspace.read"},
		{readFileTool{workspace: workspace}, "workspace.read"},
		{searchCodeTool{workspace: workspace}, "workspace.search"},
		{writeFileTool{workspace: workspace}, "workspace.write"},
		{runCommandTool{workspace: workspace}, "workspace.command"},
		{weatherTool{provider: weather}, "network.read"},
	}
	for _, entry := range tools {
		wrapped := &authorizedTool{tool: entry.tool, permission: permissions, action: entry.action, sessionID: sessionID, resource: func(string) permission.Resource {
			return permission.Resource{Kind: "workspace", Key: workspace.WorkingDirectory()}
		}}
		if err := registry.Register(wrapped); err != nil {
			return err
		}
	}
	return nil
}
