package icoder

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

func TestSummarizeRunTracksChangesAndChecks(t *testing.T) {
	patch := agent.ToolCall{ID: "patch", Name: "apply_patch", Input: `{"operations":[{"operation":"update","path":"b.go"},{"operation":"create","path":"a.go"}]}`}
	check := agent.ToolCall{ID: "check", Name: "run_command", Input: `{"program":"go","args":["test","./..."]}`}
	messages := []agent.Message{
		{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &patch}}},
		agent.NewToolMessage(agent.ToolResult{ToolCallID: "patch", Name: "apply_patch", Content: `[]`}),
		{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &check}}},
		agent.NewToolMessage(agent.ToolResult{ToolCallID: "check", Name: "run_command", Content: `{"exit_code":0}`}),
	}
	summary := summarizeRun(messages)
	if summary.Verification != "verified" || len(summary.ChangedFiles) != 2 || summary.ChangedFiles[0] != "a.go" || len(summary.Checks) != 1 || !summary.Checks[0].Passed {
		t.Fatalf("summarizeRun() = %#v", summary)
	}
}

func TestSummarizeRunMarksChangesAfterCheckUnverified(t *testing.T) {
	check := agent.ToolCall{ID: "check", Name: "run_command", Input: `{"program":"go","args":["test","./..."]}`}
	write := agent.ToolCall{ID: "write", Name: "write_file", Input: `{"path":"main.go"}`}
	messages := []agent.Message{
		{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &check}}},
		agent.NewToolMessage(agent.ToolResult{ToolCallID: "check", Name: "run_command", Content: `{"exit_code":0}`}),
		{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &write}}},
		agent.NewToolMessage(agent.ToolResult{ToolCallID: "write", Name: "write_file", Content: `{}`}),
	}
	if summary := summarizeRun(messages); summary.Verification != "unverified" {
		t.Fatalf("summarizeRun() = %#v", summary)
	}
}
