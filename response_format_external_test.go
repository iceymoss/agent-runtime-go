package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
)

// formatModel records the request it was asked to satisfy, so a test can assert
// what actually reached the provider boundary.
type formatModel struct {
	caps agent.Capabilities
	last *agent.GenerateRequest
}

func (formatModel) Name() string                        { return "format" }
func (m *formatModel) Capabilities() agent.Capabilities { return m.caps }
func (m *formatModel) Stream(_ context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	m.last = request
	message := agent.NewAssistantMessage(`{"city":"Shanghai"}`)
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishStop}), nil
}

func structuredCaps() agent.Capabilities {
	return agent.Capabilities{Tools: true, StructuredOutput: true}
}

func extractionFormat() *agent.ResponseFormat {
	return &agent.ResponseFormat{
		Kind: agent.ResponseFormatJSONSchema, Name: "city",
		Schema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		Strict: true,
	}
}

// TestResponseFormatReachesTheModel is the whole point: an agent whose answer is
// parsed by code can say so, instead of hoping the prompt is obeyed.
func TestResponseFormatReachesTheModel(t *testing.T) {
	model := &formatModel{caps: structuredCaps()}
	runner, err := agent.New(agent.Config{
		Key: "extract", ModelName: "format", MaxSteps: 4, AllowedTools: []string{},
		ResponseFormat: extractionFormat(),
	}, model, agent.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("which city")},
	}); err != nil {
		t.Fatal(err)
	}
	got := model.last.ResponseFormat
	if got == nil || got.Kind != agent.ResponseFormatJSONSchema || got.Name != "city" || !got.Strict {
		t.Fatalf("request response format = %+v", got)
	}
	// The request must not share the schema bytes with the configured agent.
	got.Schema[0] = 'X'
	if _, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("again")},
	}); err != nil {
		t.Fatal(err)
	}
	if string(model.last.ResponseFormat.Schema) != string(extractionFormat().Schema) {
		t.Fatalf("the agent's schema was mutated through a request: %s", model.last.ResponseFormat.Schema)
	}
}

// TestRunResponseFormatOverridesTheAgentDefault lets one agent answer normally
// and produce machine-readable output only where a caller needs it.
func TestRunResponseFormatOverridesTheAgentDefault(t *testing.T) {
	model := &formatModel{caps: structuredCaps()}
	runner, err := agent.New(agent.Config{Key: "chat", ModelName: "format", MaxSteps: 4, AllowedTools: []string{}},
		model, agent.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("chat")},
	}); err != nil || model.last.ResponseFormat != nil {
		t.Fatalf("an unconstrained run sent %+v, error %v", model.last.ResponseFormat, err)
	}
	if _, err := runner.Run(context.Background(), agent.RunRequest{
		Messages:       []agent.Message{agent.NewUserMessage("extract")},
		ResponseFormat: &agent.ResponseFormat{Kind: agent.ResponseFormatJSON},
	}); err != nil {
		t.Fatal(err)
	}
	if model.last.ResponseFormat == nil || model.last.ResponseFormat.Kind != agent.ResponseFormatJSON {
		t.Fatalf("run override = %+v", model.last.ResponseFormat)
	}
}

// TestResponseFormatIsRefusedWhenTheModelCannotHonorIt fails at assembly rather
// than halfway through a conversation, which is the point of declaring
// capabilities at all.
func TestResponseFormatIsRefusedWhenTheModelCannotHonorIt(t *testing.T) {
	_, err := agent.New(agent.Config{
		Key: "extract", ModelName: "format", MaxSteps: 4, AllowedTools: []string{},
		ResponseFormat: extractionFormat(),
	}, &formatModel{caps: agent.Capabilities{Tools: true}}, agent.NewRegistry())
	if !errors.Is(err, agent.ErrAgentConfigInvalid) {
		t.Fatalf("New() error = %v, want agent.ErrAgentConfigInvalid", err)
	}
}

func TestResponseFormatValidation(t *testing.T) {
	tests := []struct {
		name    string
		format  *agent.ResponseFormat
		wantErr bool
	}{
		{name: "nil is unconstrained generation", format: nil},
		{name: "json needs nothing else", format: &agent.ResponseFormat{Kind: agent.ResponseFormatJSON}},
		{name: "json schema needs a name", format: &agent.ResponseFormat{Kind: agent.ResponseFormatJSONSchema, Schema: json.RawMessage(`{}`)}, wantErr: true},
		{name: "json schema needs a schema", format: &agent.ResponseFormat{Kind: agent.ResponseFormatJSONSchema, Name: "x"}, wantErr: true},
		{name: "a schema that is not JSON is refused", format: &agent.ResponseFormat{Kind: agent.ResponseFormatJSONSchema, Name: "x", Schema: json.RawMessage(`{`)}, wantErr: true},
		{name: "json takes no schema", format: &agent.ResponseFormat{Kind: agent.ResponseFormatJSON, Schema: json.RawMessage(`{}`)}, wantErr: true},
		{name: "an unknown kind is refused", format: &agent.ResponseFormat{Kind: "yaml"}, wantErr: true},
		{name: "a valid schema is accepted", format: extractionFormat()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.format.Validate(structuredCaps()); (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
