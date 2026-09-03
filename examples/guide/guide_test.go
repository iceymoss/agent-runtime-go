package main

import (
	"context"
	"strings"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
)

// TestGuideApplicationRuns keeps the guide honest.
//
// Every code block in docs/guide comes from this package, so a change that
// breaks the application breaks the documentation with it — which is the point
// of the example existing at all.
func TestGuideApplicationRuns(t *testing.T) {
	if err := run(context.Background()); err != nil {
		t.Fatalf("run() error = %v", err)
	}
}

// TestApprovalActuallyGatesTheSideEffect is the claim chapter 7 makes: the
// restart does not happen until someone approves it, and the tool is not
// reachable around the gate.
func TestApprovalActuallyGatesTheSideEffect(t *testing.T) {
	ops := NewOps()
	gate := NewGate(RestartTool(ops), Policy(), func(string) bool { return false })

	// Without a resume handle the call parks instead of executing.
	_, err := gate.Execute(context.Background(), toolInvocation())
	if _, ok := asSuspension(err); !ok {
		t.Fatalf("Execute() error = %v, want a suspension", err)
	}
	if len(ops.restarted) != 0 {
		t.Fatalf("the service was restarted without approval: %v", ops.restarted)
	}

	// A resume handle without an approval is a refusal, not a bypass.
	denied := NewGate(RestartTool(ops), Policy(), func(string) bool { return false })
	result, err := denied.Execute(context.Background(), resumedInvocation())
	if err != nil || !result.IsError {
		t.Fatalf("denied resume = %+v, error %v", result, err)
	}
	if len(ops.restarted) != 0 {
		t.Fatalf("a denied approval still restarted: %v", ops.restarted)
	}

	// Approved, it runs.
	allowed := NewGate(RestartTool(ops), Policy(), func(string) bool { return true })
	if result, err := allowed.Execute(context.Background(), resumedInvocation()); err != nil || result.IsError {
		t.Fatalf("approved resume = %+v, error %v", result, err)
	}
	if len(ops.restarted) != 1 {
		t.Fatalf("restarted %v, want exactly one", ops.restarted)
	}
}

// TestReadOnlyToolNeedsNoApproval keeps the gate from turning every call into a
// question, which is how approval fatigue trains people to click yes.
func TestReadOnlyToolNeedsNoApproval(t *testing.T) {
	ops := NewOps()
	gate := NewGate(ReadLogsTool(ops), Policy(), func(string) bool { return false })
	result, err := gate.Execute(context.Background(), agent.ToolInvocation{
		CallID: "call-1", Name: "read_logs", RawInput: `{"service":"checkout"}`,
	})
	if err != nil || result.IsError {
		t.Fatalf("read_logs = %+v, error %v", result, err)
	}
	if !strings.Contains(result.Content, "payment gateway") {
		t.Fatalf("read_logs content = %q", result.Content)
	}
}

// TestUnknownToolIsDeniedByDefault pins the policy's default: a tool nobody has
// decided about is refused rather than quietly permitted, so adding a tool
// cannot silently widen what the agent may do.
func TestUnknownToolIsDeniedByDefault(t *testing.T) {
	ops := NewOps()
	unlisted := agent.MustNewTool("drop_database", "Delete everything.",
		func(context.Context, struct{}) (agent.ToolResult, error) {
			t.Fatal("an unlisted tool was executed")
			return agent.ToolResult{}, nil
		})
	gate := NewGate(unlisted, Policy(), func(string) bool { return true })
	result, err := gate.Execute(context.Background(), agent.ToolInvocation{
		CallID: "call-1", Name: "drop_database", RawInput: `{}`,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError || !result.StopTurn || !strings.Contains(result.Content, "unknown-tool") {
		t.Fatalf("unlisted tool result = %+v", result)
	}
	_ = ops
}

func toolInvocation() agent.ToolInvocation {
	return agent.ToolInvocation{CallID: "call-1", Name: "restart_service", RawInput: `{"service":"checkout","reason":"test"}`}
}

func resumedInvocation() agent.ToolInvocation {
	invocation := toolInvocation()
	invocation.Resume = &agent.ToolSuspension{Kind: agent.ToolSuspensionApproval, RequestRef: invocation.CallID}
	return invocation
}

func asSuspension(err error) (agent.ToolSuspension, bool) { return agent.AsToolSuspension(err) }
