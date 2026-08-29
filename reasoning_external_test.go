package agent_test

import (
	"context"
	"strings"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
)

// thinkingModel is a reasoning model's shape: it reports its thinking separately
// from its answer.
type thinkingModel struct {
	caps      agent.Capabilities
	reasoning string
	answer    string
	requests  []*agent.GenerateRequest
}

func (thinkingModel) Name() string                        { return "thinking" }
func (m *thinkingModel) Capabilities() agent.Capabilities { return m.caps }
func (m *thinkingModel) Stream(_ context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	m.requests = append(m.requests, request)
	message := agent.Message{Role: agent.RoleAssistant, FinishReason: agent.FinishStop, Parts: []agent.ContentPart{
		{Type: agent.PartReasoning, Text: m.reasoning},
		{Type: agent.PartText, Text: m.answer},
	}}
	return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishStop}), nil
}

func reasoningCaps() agent.Capabilities {
	return agent.Capabilities{Tools: true, Reasoning: true}
}

// TestReasoningIsKeptSeparateFromTheAnswer is what makes a reasoning model
// usable: the application can show or store the thinking, and RunResult.Text
// stays the answer alone.
func TestReasoningIsKeptSeparateFromTheAnswer(t *testing.T) {
	model := &thinkingModel{caps: reasoningCaps(), reasoning: "the user asked for 2+2", answer: "4"}
	runner, err := agent.New(agent.Config{Key: "think", ModelName: "thinking", MaxSteps: 4, AllowedTools: []string{}},
		model, agent.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}

	var streamedReasoning, streamedText strings.Builder
	emitter := agent.NewObservationEmitterWith(agent.ObservationOptions{QueueSize: 8, Lossless: true}, func(o agent.Observation) {
		switch o.Type {
		case agent.ObservationReasoningDelta:
			streamedReasoning.WriteString(o.Text)
		case agent.ObservationTextDelta:
			streamedText.WriteString(o.Text)
		}
	})
	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("2+2?")}, ObservationEmitter: emitter,
	})
	emitter.Close()
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "4" {
		t.Fatalf("RunResult.Text = %q, want the answer without the thinking", result.Text)
	}
	if len(result.Messages) == 0 || result.Messages[0].Reasoning() != "the user asked for 2+2" {
		t.Fatalf("the reasoning was not preserved on the message: %+v", result.Messages)
	}
	// A UI must be able to tell the two streams apart.
	if streamedReasoning.String() != "the user asked for 2+2" || streamedText.String() != "4" {
		t.Fatalf("streamed reasoning = %q, text = %q", streamedReasoning.String(), streamedText.String())
	}
}

// TestReasoningIsNotFedBackToTheModel is the rule that keeps a reasoning model
// working across turns: providers reject their own thinking as assistant input,
// and replaying a chain of thought would change the question being answered.
func TestReasoningIsNotFedBackToTheModel(t *testing.T) {
	tool := agent.MustNewTool("noop", "Do nothing.",
		func(context.Context, struct{}) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "ok"}, nil
		})
	registry := agent.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	model := &twoStepThinkingModel{}
	runner, err := agent.New(agent.Config{Key: "think", ModelName: "thinking", MaxSteps: 4}, model, registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("go")},
	}); err != nil {
		t.Fatal(err)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model was called %d times", len(model.requests))
	}
	// The runtime hands the assistant message back with its reasoning intact;
	// dropping it is the adapter's job, and the history must still carry it so an
	// application can store what actually happened.
	var carried bool
	for _, message := range model.requests[1].Messages {
		carried = carried || message.Reasoning() != ""
	}
	if !carried {
		t.Fatal("the runtime dropped reasoning from the history it replays")
	}
}

type twoStepThinkingModel struct{ requests []*agent.GenerateRequest }

func (twoStepThinkingModel) Name() string { return "thinking" }
func (twoStepThinkingModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true, Reasoning: true}
}
func (m *twoStepThinkingModel) Stream(_ context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	m.requests = append(m.requests, request)
	if len(m.requests) == 1 {
		call := agent.ToolCall{ID: "c1", Name: "noop", Input: `{}`}
		message := agent.Message{Role: agent.RoleAssistant, FinishReason: agent.FinishToolCalls, Parts: []agent.ContentPart{
			{Type: agent.PartReasoning, Text: "I should call noop"},
			{Type: agent.PartToolCall, ToolCall: &call},
		}}
		return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishToolCalls}), nil
	}
	message := agent.NewAssistantMessage("done")
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishStop}), nil
}

// TestReasoningRequiresTheDeclaredCapability keeps Capabilities.Reasoning from
// being decorative: an application decides whether to render or redact thinking
// from the capability, before the first token arrives.
func TestReasoningRequiresTheDeclaredCapability(t *testing.T) {
	model := &thinkingModel{caps: agent.Capabilities{Tools: true}, reasoning: "thinking", answer: "4"}
	runner, err := agent.New(agent.Config{Key: "think", ModelName: "thinking", MaxSteps: 4, AllowedTools: []string{}},
		model, agent.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("2+2?")},
	}); err == nil {
		t.Fatal("a model produced reasoning without declaring the capability")
	}
}
