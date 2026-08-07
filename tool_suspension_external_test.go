package agent_test

import (
	"context"
	"errors"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
)

type suspensionModel struct{}

func (suspensionModel) Name() string                     { return "suspension-model" }
func (suspensionModel) Capabilities() agent.Capabilities { return agent.Capabilities{Tools: true} }
func (suspensionModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	call := agent.ToolCall{ID: "call-1", Name: "approval_tool", Input: `{}`}
	message := agent.Message{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}, FinishReason: agent.FinishToolCalls}
	stream := make(chan agent.StreamChunk, 2)
	stream <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
	stream <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{Message: message, FinishReason: agent.FinishToolCalls}}
	close(stream)
	return stream, nil
}

type suspensionTool struct{ calls int }

func (*suspensionTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "approval_tool", Parameters: map[string]any{"type": "object"}}
}
func (*suspensionTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t *suspensionTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	t.calls++
	return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{Kind: agent.ToolSuspensionApproval, ExecutionKey: "exec", RequestRef: "request", ResumeToken: "opaque", Revision: 2}, Cause: errors.New("approval pending")}
}

func TestAgentReturnsTypedToolSuspensionWithoutToolMessage(t *testing.T) {
	tool := &suspensionTool{}
	registry := agent.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	runtime, err := agent.New(agent.Config{Key: "suspension", ModelName: "model", MaxSteps: 4}, suspensionModel{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{agent.NewUserMessage("run")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != agent.OutcomeSuspended || result.StopReason != agent.StopReasonToolSuspended || result.Suspension == nil || result.Suspension.Tool == nil || result.Suspension.Tool.RequestRef != "request" {
		t.Fatalf("Run() = %#v", result)
	}
	if len(result.Messages) != 1 || result.Messages[0].Role != agent.RoleAssistant || len(result.Steps) != 1 || len(result.Steps[0].ToolResults) != 0 || tool.calls != 1 {
		t.Fatalf("messages = %#v, steps = %#v, calls = %d", result.Messages, result.Steps, tool.calls)
	}
}

func TestAsToolSuspensionRejectsOrdinaryError(t *testing.T) {
	if _, ok := agent.AsToolSuspension(errors.New("ordinary")); ok {
		t.Fatal("AsToolSuspension() accepted ordinary error")
	}
}
