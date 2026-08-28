package agent_test

import (
	"context"
	"errors"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
)

// responseModel is the adapter shape most people write first: it already has the
// whole answer and only needs to hand it to the runtime.
type responseModel struct{ response *agent.Response }

func (responseModel) Name() string                     { return "response" }
func (responseModel) Capabilities() agent.Capabilities { return agent.Capabilities{Tools: true} }
func (m responseModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	return agent.StreamResponse(m.response), nil
}

// TestStreamResponseSatisfiesTheChunkConsistencyRule is the point of the helper:
// an adapter that holds a complete response must not have to rediscover, from a
// protocol error, that the runtime also wants it re-emitted as deltas.
func TestStreamResponseSatisfiesTheChunkConsistencyRule(t *testing.T) {
	message := agent.NewAssistantMessage("the answer")
	message.FinishReason = agent.FinishStop
	model := responseModel{response: &agent.Response{
		Message: message, FinishReason: agent.FinishStop, ModelName: "response-v1",
	}}
	runner, err := agent.New(agent.Config{Key: "test", ModelName: "response-v1", MaxSteps: 4, AllowedTools: []string{}},
		model, agent.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("ask")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "the answer" || result.Outcome != agent.OutcomeCompleted {
		t.Fatalf("result = %+v", result)
	}
}

// TestStreamResponseCarriesToolCalls keeps the tool path working through the
// same helper, which is what a non-streaming provider needs.
func TestStreamResponseCarriesToolCalls(t *testing.T) {
	call := agent.ToolCall{ID: "call-1", Name: "noop", Input: `{}`}
	message := agent.Message{Role: agent.RoleAssistant, Parts: []agent.ContentPart{
		{Type: agent.PartToolCall, ToolCall: &call},
	}, FinishReason: agent.FinishToolCalls}

	var got []agent.StreamChunk
	for chunk := range agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishToolCalls}) {
		got = append(got, chunk)
	}
	if len(got) != 2 || got[0].Type != agent.ChunkToolCall || got[0].ToolCall.ID != "call-1" || got[1].Type != agent.ChunkFinish {
		t.Fatalf("chunks = %+v", got)
	}
	// The emitted call must be a copy: a caller that mutates it must not reach
	// into the response the runtime is about to validate.
	got[0].ToolCall.Name = "mutated"
	if message.Parts[0].ToolCall.Name != "noop" {
		t.Fatal("StreamResponse shared the tool call with its caller")
	}
}

// TestStreamResponseReportsAMissingResponse turns an adapter bug into a message
// that names the adapter, rather than into "stream closed before terminal
// response" from somewhere deep in the runtime.
func TestStreamResponseReportsAMissingResponse(t *testing.T) {
	var got []agent.StreamChunk
	for chunk := range agent.StreamResponse(nil) {
		got = append(got, chunk)
	}
	if len(got) != 1 || got[0].Type != agent.ChunkError {
		t.Fatalf("chunks = %+v", got)
	}
	var modelErr *agent.ModelError
	if !errors.As(got[0].Err, &modelErr) || modelErr.Kind != agent.ModelErrorKindProtocol {
		t.Fatalf("error = %v", got[0].Err)
	}
}
