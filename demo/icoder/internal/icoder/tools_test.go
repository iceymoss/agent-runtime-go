package icoder

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	toollifecycle "github.com/iceymoss/agent-runtime-go/tool"
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

type toolTestClock struct{ now time.Time }

func (c *toolTestClock) Now() time.Time { return c.now }

// newTestCatalog builds a real frozen generation over a real SQLite execution
// ledger, so these tests exercise the same path the application does rather than
// a simplified stand-in.
func newTestCatalog(t *testing.T, gate *PermissionGate, entries []lifecycleEntry) *ToolCatalog {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	catalog, err := NewToolCatalog(ToolCatalogOptions{Entries: entries, Permissions: gate, Ledger: store.ToolLedger()})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func writeEntry(tool agent.Tool) []lifecycleEntry {
	return []lifecycleEntry{{tool: tool, metadata: writeMetadata("workspace.write", "workspace.file"), resource: permission.Resource{Kind: "file", Key: "file.go"}}}
}

// testRunContext supplies the run identity the executor requires. Tools are
// always invoked inside a run, so a test that omits it would not be testing the
// real path.
func testRunContext(ctx context.Context, runKey, session string, approve ApprovalFunc) context.Context {
	return withRunContext(ctx, runContext{id: runKey, attempt: runKey + ":attempt", session: session, approve: approve})
}

func executeCatalogTool(t *testing.T, ctx context.Context, catalog *ToolCatalog, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	t.Helper()
	tool, ok := catalog.Registry().Get(invocation.Name)
	if !ok {
		t.Fatalf("tool %q is not in the frozen generation", invocation.Name)
	}
	return tool.Execute(ctx, invocation)
}

func newTestGate(t *testing.T, allowWrites bool) *PermissionGate {
	t.Helper()
	gate, err := NewPermissionGate(allowWrites, nil)
	if err != nil {
		t.Fatal(err)
	}
	return gate
}

func TestToolSuspendsWhenNobodyCanApprove(t *testing.T) {
	tests := []struct {
		name    string
		entries []lifecycleEntry
		call    agent.ToolInvocation
	}{
		{
			name:    "workspace write",
			entries: writeEntry(&fixedTool{}),
			call:    agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{}`},
		},
		{
			name: "unclassified MCP tool",
			entries: []lifecycleEntry{{
				tool:     &fixedTool{},
				metadata: externalMetadata("network.tool", "mcp.server", false),
				resource: permission.Resource{Kind: "mcp-tool", Key: "server/tool"},
			}},
			call: agent.ToolInvocation{CallID: "mcp-call", Name: "write_file", RawInput: `{}`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inner := test.entries[0].tool.(*fixedTool)
			catalog := newTestCatalog(t, newTestGate(t, false), test.entries)
			ctx := testRunContext(context.Background(), "run-1", "session", nil)
			_, err := executeCatalogTool(t, ctx, catalog, test.call)
			suspension, ok := agent.AsToolSuspension(err)
			if !ok {
				t.Fatalf("error = %v, want a tool suspension", err)
			}
			if suspension.Kind != agent.ToolSuspensionApproval || suspension.ExecutionKey == "" || suspension.ResumeToken == "" {
				t.Fatalf("suspension = %#v", suspension)
			}
			if inner.calls != 0 {
				t.Fatalf("tool ran %d times before approval", inner.calls)
			}
		})
	}
}

func TestToolExecutesAfterApproval(t *testing.T) {
	tests := []struct {
		name     string
		decision ApprovalDecision
	}{
		{name: "approve once", decision: ApprovalApproveOnce},
		{name: "approve for the session", decision: ApprovalApproveAuto},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inner := &fixedTool{}
			catalog := newTestCatalog(t, newTestGate(t, false), writeEntry(inner))
			var prompt ApprovalPrompt
			ctx := testRunContext(context.Background(), "run-1", "session", func(_ context.Context, value ApprovalPrompt) (ApprovalDecision, error) {
				prompt = value
				return test.decision, nil
			})
			result, err := executeCatalogTool(t, ctx, catalog, agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{"path":"file.go"}`})
			if err != nil || result.Content != "written" || inner.calls != 1 {
				t.Fatalf("result = %#v, calls = %d, error = %v", result, inner.calls, err)
			}
			if prompt.ToolName != "write_file" || prompt.Resource != "file.go" || !strings.Contains(prompt.Input, "file.go") {
				t.Fatalf("prompt = %#v", prompt)
			}
		})
	}
}

func TestToolDenialIsModelVisibleAndStopsTheTurn(t *testing.T) {
	inner := &fixedTool{}
	catalog := newTestCatalog(t, newTestGate(t, false), writeEntry(inner))
	ctx := testRunContext(context.Background(), "run-2", "session", func(context.Context, ApprovalPrompt) (ApprovalDecision, error) {
		return ApprovalDeny, nil
	})
	result, err := executeCatalogTool(t, ctx, catalog, agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{}`})
	if err != nil {
		t.Fatalf("denial returned an error instead of a correctable result: %v", err)
	}
	if !result.IsError || !result.StopTurn || inner.calls != 0 {
		t.Fatalf("result = %#v, calls = %d", result, inner.calls)
	}
}

func TestToolAllowsWriteFlagWithoutPrompting(t *testing.T) {
	inner := &fixedTool{}
	catalog := newTestCatalog(t, newTestGate(t, true), writeEntry(inner))
	ctx := testRunContext(context.Background(), "run-3", "session", func(context.Context, ApprovalPrompt) (ApprovalDecision, error) {
		t.Fatal("approval was requested even though writes are pre-authorized")
		return ApprovalDeny, nil
	})
	result, err := executeCatalogTool(t, ctx, catalog, agent.ToolInvocation{CallID: "call-1", Name: "write_file", RawInput: `{}`})
	if err != nil || result.Content != "written" || inner.calls != 1 {
		t.Fatalf("result = %#v, calls = %d, error = %v", result, inner.calls, err)
	}
}

func TestToolAuthorizationUsesTheFrozenRunSession(t *testing.T) {
	// The UI can switch sessions mid-run. The authorization must still name the
	// session that started the run, or an approval would be attributed elsewhere.
	var sessions []string
	gate := newTestGate(t, true)
	original := gate.policy
	gate.policy = permission.PolicyFunc{PolicyVersion: policyVersion, EvaluateFunc: func(ctx context.Context, request permission.CheckRequest, grants []permission.Grant) (permission.CheckResult, error) {
		sessions = append(sessions, request.SessionRef)
		return original.Evaluate(ctx, request, grants)
	}}
	catalog := newTestCatalog(t, gate, writeEntry(&fixedTool{}))
	ctx := testRunContext(context.Background(), "run", "run-session", nil)
	if _, err := executeCatalogTool(t, ctx, catalog, agent.ToolInvocation{CallID: "call", Name: "write_file", RawInput: `{}`}); err != nil {
		t.Fatal(err)
	}
	if len(sessions) == 0 {
		t.Fatal("policy was never consulted")
	}
	for _, session := range sessions {
		if session != "run-session" {
			t.Fatalf("permission session = %q, want the run session", session)
		}
	}
}

func TestApprovalTerminalMessageRejectsInfrastructureErrors(t *testing.T) {
	if _, ok := approvalTerminalMessage(errors.New("database unavailable")); ok {
		t.Fatal("infrastructure error was converted to a user-facing approval message")
	}
	if _, ok := approvalTerminalMessage(permission.ErrRequestExpired); !ok {
		t.Fatal("expired approval was not recognized as terminal")
	}
}

func TestPermissionGateEvaluateDoesNotCreatePendingRequests(t *testing.T) {
	gate := newTestGate(t, false)
	request := permission.CheckRequest{
		RequestKey: "request", Subject: permission.Subject{TenantKey: tenantKey, PrincipalKey: principalKey},
		Resource: permission.Resource{Kind: "file", Key: "file.go"}, SessionRef: "session",
		RunRef: "run", AttemptRef: "attempt", ExecutionRef: "execution", ToolName: "write_file",
		Action: "workspace.write", InputDigest: "sha256:input", PolicyVersion: policyVersion,
	}
	result, err := gate.Evaluate(context.Background(), request)
	if err != nil || result.Decision != permission.DecisionAsk {
		t.Fatalf("Evaluate() = %#v, error %v", result, err)
	}
	pending, err := gate.PendingApprovals(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("PendingApprovals() = %d, error %v; a dry run must not create requests", len(pending), err)
	}
}

func TestToolCatalogFreezesMetadataIntoOneGeneration(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog := newTestCatalog(t, newTestGate(t, false), workspaceToolEntries(workspace, nil))
	if catalog.GenerationDigest() == "" {
		t.Fatal("generation digest is empty")
	}
	names := catalog.Names()
	if len(names) == 0 {
		t.Fatal("no tools were bridged into the root registry")
	}
	for _, name := range []string{"read_file", "apply_patch", "run_command", "git_commit"} {
		if _, ok := catalog.Registry().Get(name); !ok {
			t.Fatalf("tool %q is missing from the bridged registry", name)
		}
	}
	if _, ok := catalog.Registry().Get("get_weather"); ok {
		t.Fatal("weather tool was registered without a provider")
	}
	if _, ok := any(catalog.Registry()).(*agent.Registry); !ok {
		t.Fatal("catalog did not expose a root registry")
	}
	var _ toollifecycle.Metadata = readMetadata("workspace.read")
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
		{program: "git", args: []string{"commit", "-m", "message"}},
		{program: "git", args: []string{"add", "--", "file.go"}},
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

func TestRunCommandToolReturnsInvalidCWDAsToolError(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := runCommandTool{workspace: workspace}
	result, err := tool.Execute(context.Background(), agent.ToolInvocation{RawInput: fmt.Sprintf(`{"program":"git","args":["status"],"cwd":%q}`, workspace.root)})
	if err != nil || !result.IsError || !strings.Contains(result.Content, "absolute paths are not allowed") {
		t.Fatalf("result=%#v error=%v", result, err)
	}
}

func TestWorkspaceWriteToolsRequireApproval(t *testing.T) {
	// Every tool that changes the workspace must stop for a decision when no one
	// can give it, rather than proceeding on the model's say-so.
	tests := []struct {
		name  string
		call  agent.ToolInvocation
		token string
	}{
		{name: "git_commit", call: agent.ToolInvocation{CallID: "commit", Name: "git_commit", RawInput: `{"message":"test: commit","paths":["file.go"]}`}},
		{name: "apply_patch", call: agent.ToolInvocation{CallID: "patch", Name: "apply_patch", RawInput: `{"operations":[{"operation":"create","path":"new.txt","content":"new"}]}`}},
		{name: "run_command", call: agent.ToolInvocation{CallID: "command", Name: "run_command", RawInput: `{"program":"go","args":["test","./..."]}`}},
	}
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog := newTestCatalog(t, newTestGate(t, false), workspaceToolEntries(workspace, nil))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := testRunContext(context.Background(), "run-"+test.name, "session", nil)
			_, err := executeCatalogTool(t, ctx, catalog, test.call)
			if _, ok := agent.AsToolSuspension(err); !ok {
				t.Fatalf("error = %v, want a tool suspension awaiting approval", err)
			}
		})
	}
}

func TestWorkspaceReadToolsNeedNoApproval(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog := newTestCatalog(t, newTestGate(t, false), workspaceToolEntries(workspace, nil))
	ctx := testRunContext(context.Background(), "run-read", "session", nil)
	result, err := executeCatalogTool(t, ctx, catalog, agent.ToolInvocation{CallID: "cwd", Name: "get_working_directory", RawInput: `{}`})
	if err != nil || result.IsError || result.Content != workspace.WorkingDirectory() {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
}
