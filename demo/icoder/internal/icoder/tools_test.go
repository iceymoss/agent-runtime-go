package icoder

import (
	"context"
	"strings"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
)

type fixedTool struct{ calls int }

func (*fixedTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "write_file", Parameters: map[string]any{"type": "object"}}
}
func (*fixedTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t *fixedTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	t.calls++
	return agent.ToolResult{Content: "written"}, nil
}

func TestAuthorizedToolDeniesWhenApprovalUnavailable(t *testing.T) {
	service, err := NewPermissionService(false)
	if err != nil {
		t.Fatal(err)
	}
	inner := &fixedTool{}
	tool := &authorizedTool{tool: inner, permission: service, action: "workspace.write", sessionID: func() string { return "session" }, resource: func(string) permission.Resource {
		return permission.Resource{Kind: "workspace", Key: "file.go"}
	}}
	result, err := tool.Execute(context.Background(), agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !result.StopTurn || !strings.Contains(result.Content, "approval is unavailable") || inner.calls != 0 {
		t.Fatalf("result = %#v, calls = %d", result, inner.calls)
	}
}

func TestAuthorizedToolExecutesAfterApproval(t *testing.T) {
	service, err := NewPermissionService(false)
	if err != nil {
		t.Fatal(err)
	}
	inner := &fixedTool{}
	tool := &authorizedTool{tool: inner, permission: service, action: "workspace.write", sessionID: func() string { return "session" }, resource: func(string) permission.Resource { return permission.Resource{Kind: "file", Key: "file.go"} }}
	var prompt ApprovalPrompt
	ctx := withRunContext(context.Background(), "run-1", "session", func(_ context.Context, value ApprovalPrompt) (ApprovalDecision, error) {
		prompt = value
		return ApprovalApproveOnce, nil
	})
	result, err := tool.Execute(ctx, agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{"path":"file.go"}`})
	if err != nil || result.Content != "written" || inner.calls != 1 {
		t.Fatalf("result = %#v, calls = %d, error = %v", result, inner.calls, err)
	}
	if prompt.ToolName != "write_file" || prompt.Resource != "file.go" || !strings.Contains(prompt.Input, "file.go") {
		t.Fatalf("prompt = %#v", prompt)
	}
}

func TestAuthorizedToolDoesNotExecuteAfterRejection(t *testing.T) {
	service, err := NewPermissionService(false)
	if err != nil {
		t.Fatal(err)
	}
	inner := &fixedTool{}
	tool := &authorizedTool{tool: inner, permission: service, action: "workspace.write", sessionID: func() string { return "session" }, resource: func(string) permission.Resource { return permission.Resource{Kind: "file", Key: "file.go"} }}
	ctx := withRunContext(context.Background(), "run-2", "session", func(context.Context, ApprovalPrompt) (ApprovalDecision, error) { return ApprovalDeny, nil })
	result, err := tool.Execute(ctx, agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{}`})
	if err != nil || !result.IsError || !result.StopTurn || inner.calls != 0 {
		t.Fatalf("result = %#v, calls = %d, error = %v", result, inner.calls, err)
	}
}

func TestAuthorizedToolAllowsWriteFlag(t *testing.T) {
	service, err := NewPermissionService(true)
	if err != nil {
		t.Fatal(err)
	}
	inner := &fixedTool{}
	tool := &authorizedTool{tool: inner, permission: service, action: "workspace.write", sessionID: func() string { return "session" }, resource: func(string) permission.Resource {
		return permission.Resource{Kind: "workspace", Key: "file.go"}
	}}
	result, err := tool.Execute(context.Background(), agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{}`})
	if err != nil || result.Content != "written" || inner.calls != 1 {
		t.Fatalf("result = %#v, calls = %d, error = %v", result, inner.calls, err)
	}
}

func TestAuthorizedToolUsesFrozenRunSession(t *testing.T) {
	var checkedSession string
	policy := permission.PolicyFunc{PolicyVersion: policyVersion, EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
		checkedSession = request.SessionRef
		return permission.CheckResult{Decision: permission.DecisionAllow, PolicyVersion: request.PolicyVersion, InputDigest: request.InputDigest}, nil
	}}
	service, err := permission.NewService(permission.ServiceOptions{Policy: policy, Store: permission.NewMemoryStore()})
	if err != nil {
		t.Fatal(err)
	}
	inner := &fixedTool{}
	tool := &authorizedTool{tool: inner, permission: service, action: "workspace.write", sessionID: func() string { return "ui-session" }, resource: func(string) permission.Resource { return permission.Resource{Kind: "file", Key: "file.go"} }}
	ctx := withRunContext(context.Background(), "run", "run-session", nil)
	if _, err := tool.Execute(ctx, agent.ToolInvocation{CallID: "call", Name: "write_file", RawInput: `{}`}); err != nil {
		t.Fatal(err)
	}
	if checkedSession != "run-session" {
		t.Fatalf("permission session = %q", checkedSession)
	}
}

func TestValidateCommandAllowlist(t *testing.T) {
	for _, test := range []struct {
		program string
		args    []string
		valid   bool
	}{
		{program: "go", args: []string{"test", "./..."}, valid: true},
		{program: "gofmt", args: []string{"-w", "main.go"}, valid: true},
		{program: "git", args: []string{"diff"}, valid: true},
		{program: "npm", args: []string{"test"}, valid: true},
		{program: "pnpm", args: []string{"lint"}, valid: true},
		{program: "pytest", args: []string{"tests"}, valid: true},
		{program: "ruff", args: []string{"check", "."}, valid: true},
		{program: "cargo", args: []string{"clippy"}, valid: true},
		{program: "rustfmt", args: []string{"main.rs"}, valid: true},
		{program: "make", args: []string{"test"}, valid: true},
		{program: "git", args: []string{"push"}},
		{program: "npm", args: []string{"exec", "tool"}},
		{program: "cargo", args: []string{"publish"}},
		{program: "sh", args: []string{"-c", "true"}},
		{program: "go"},
	} {
		err := validateCommand(test.program, test.args)
		if (err == nil) != test.valid {
			t.Errorf("validateCommand(%q, %#v) error = %v", test.program, test.args, err)
		}
	}
}

func TestApplyPatchToolRequiresWritePermission(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewPermissionService(false)
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewRegistry()
	if err := registerTools(registry, workspace, nil, service, func() string { return "session" }); err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Get("apply_patch")
	if !ok {
		t.Fatal("apply_patch was not registered")
	}
	result, err := tool.Execute(context.Background(), agent.ToolInvocation{CallID: "call-patch", Name: "apply_patch", RawInput: `{"operations":[{"operation":"create","path":"new.txt","content":"new"}]}`})
	if err != nil || !result.IsError || !result.StopTurn || !strings.Contains(result.Content, "approval is unavailable") {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}
