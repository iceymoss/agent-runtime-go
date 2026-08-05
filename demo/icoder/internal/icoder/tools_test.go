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

func TestAuthorizedToolAsksBeforeWrite(t *testing.T) {
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
	if !result.IsError || !result.StopTurn || !strings.Contains(result.Content, "approval required") || inner.calls != 0 {
		t.Fatalf("result = %#v, calls = %d", result, inner.calls)
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

func TestValidateCommandAllowlist(t *testing.T) {
	for _, test := range []struct {
		program string
		args    []string
		valid   bool
	}{
		{program: "go", args: []string{"test", "./..."}, valid: true},
		{program: "git", args: []string{"diff"}, valid: true},
		{program: "git", args: []string{"push"}},
		{program: "sh", args: []string{"-c", "true"}},
		{program: "go"},
	} {
		err := validateCommand(test.program, test.args)
		if (err == nil) != test.valid {
			t.Errorf("validateCommand(%q, %#v) error = %v", test.program, test.args, err)
		}
	}
}
