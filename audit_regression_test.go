package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestRunEnforcesEffectiveToolChoiceOnTerminalResponse(t *testing.T) {
	tests := []struct {
		name     string
		choice   ToolChoice
		response *Response
	}{
		{
			name:   "none rejects calls",
			choice: ToolChoice{Mode: ToolChoiceNone},
			response: terminalResponse(Message{Role: RoleAssistant, Parts: []ContentPart{
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "first", Input: `{}`}},
			}, FinishReason: FinishToolCalls}),
		},
		{
			name:     "required rejects zero calls",
			choice:   ToolChoice{Mode: ToolChoiceRequired},
			response: terminalResponse(NewAssistantMessage("not a tool call")),
		},
		{
			name:   "named rejects every different call",
			choice: ToolChoice{Mode: ToolChoiceNamed, Name: "first"},
			response: terminalResponse(Message{Role: RoleAssistant, Parts: []ContentPart{
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "first", Input: `{}`}},
				{Type: PartToolCall, ToolCall: &ToolCall{ID: "c2", Name: "second", Input: `{}`}},
			}, FinishReason: FinishToolCalls}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &fakeTool{name: "first"}
			second := &fakeTool{name: "second"}
			model := fixedStreamModel(tt.response)
			a := newTestAgent(t, Config{Key: "test"}, model, first, second)

			result, err := a.Run(context.Background(), RunRequest{
				Messages:   []Message{NewUserMessage("hi")},
				ToolChoice: &tt.choice,
			})
			if !isModelErrorKind(err, ModelErrorKindProtocol, false) {
				t.Fatalf("Run() error = %v, want protocol ModelError", err)
			}
			if first.calls != 0 || second.calls != 0 || len(result.Messages) != 0 || len(result.Steps) != 0 {
				t.Fatalf("provider violation changed state: result=%+v calls=%d/%d", result, first.calls, second.calls)
			}
		})
	}
}

func TestRunLengthFinishSuspendsAndPreservesResponse(t *testing.T) {
	message := Message{Role: RoleAssistant, Parts: []ContentPart{{Type: PartText, Text: "partial"}}, FinishReason: FinishLength}
	response := terminalResponse(message)
	response.Usage = Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	a := newTestAgent(t, Config{Key: "test"}, fixedStreamModel(response))

	result, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome != OutcomeSuspended || result.StopReason != StopReasonOutputLimit {
		t.Fatalf("outcome/reason = %q/%q", result.Outcome, result.StopReason)
	}
	if result.Text != "partial" || result.Usage != response.Usage || !reflect.DeepEqual(result.Messages, []Message{message}) {
		t.Fatalf("response was not preserved: %+v", result)
	}
}

func TestRepairBudgetStopsLaterSiblingTools(t *testing.T) {
	tool := &schemaTool{definition: ToolDefinition{
		Name: "score",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"value": map[string]any{"type": "integer"}},
			"required":   []any{"value"},
		},
	}}
	model := &fakeModel{steps: []scriptedStep{{calls: []ToolCall{
		{ID: "bad-1", Name: "score", Input: `{"value":"bad"}`},
		{ID: "bad-2", Name: "score", Input: `{"value":"bad"}`},
		{ID: "valid", Name: "score", Input: `{"value":1}`},
	}}}}
	a := newTestAgent(t, Config{Key: "test", ToolRepairLimit: 1}, model, tool)

	result, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if !errors.Is(err, ErrToolInputInvalid) {
		t.Fatalf("Run() error = %v, want ErrToolInputInvalid", err)
	}
	if tool.calls != 0 {
		t.Fatalf("valid sibling executed after repair exhaustion: calls=%d", tool.calls)
	}
	if len(result.Steps) != 1 || len(result.Steps[0].ToolResults) != 2 {
		t.Fatalf("partial exhausted step was not recorded: %+v", result.Steps)
	}
}

func TestRegistryStoresNormalizedImmutableDefinition(t *testing.T) {
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"nested": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"value": map[string]any{"type": "string"}},
				"additionalProperties": true,
			},
		},
		"$defs": map[string]any{
			"node": map[string]any{
				"type":       "object",
				"properties": map[string]any{"next": map[string]any{"$ref": "#/$defs/node"}},
			},
		},
	}
	tool := &schemaTool{definition: ToolDefinition{Name: "tree", Parameters: parameters, Strict: true}}
	registry := NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	parameters["type"] = "string"
	parameters["properties"].(map[string]any)["nested"] = map[string]any{"type": "string"}
	tool.definition.Parameters = map[string]any{"type": "string"}

	set, err := NewToolSet(registry, nil)
	if err != nil {
		t.Fatalf("NewToolSet() error = %v", err)
	}
	definition := set.Definitions()[0]
	if definition.Parameters["additionalProperties"] != false {
		t.Fatalf("root strict schema was not normalized: %#v", definition.Parameters)
	}
	nested := definition.Parameters["properties"].(map[string]any)["nested"].(map[string]any)
	if nested["additionalProperties"] != true {
		t.Fatalf("explicit nested additionalProperties was overwritten: %#v", nested)
	}
	node := definition.Parameters["$defs"].(map[string]any)["node"].(map[string]any)
	if node["additionalProperties"] != false {
		t.Fatalf("$defs object was not normalized: %#v", node)
	}

	definition.Parameters["type"] = "array"
	definition.Parameters["properties"].(map[string]any)["nested"] = nil
	again := set.Definitions()[0]
	if again.Parameters["type"] != "object" || again.Parameters["properties"].(map[string]any)["nested"] == nil {
		t.Fatalf("Definitions leaked mutable policy state: %#v", again.Parameters)
	}
	if err := set.validate("tree", `{"nested":{"value":"ok","extra":1}}`); err != nil {
		t.Fatalf("compiled schema differs from normalized definition: %v", err)
	}
	if err := set.validate("tree", `{"unknown":true}`); err == nil {
		t.Fatal("compiled strict schema accepted an unknown root property")
	}
}

func TestRegistryRejectsExternalRefsAndSupportsLocalRecursiveRefs(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]any
		wantErr bool
	}{
		{
			name: "local recursive",
			params: map[string]any{
				"$ref": "#/$defs/node",
				"$defs": map[string]any{"node": map[string]any{
					"type": "object", "properties": map[string]any{"next": map[string]any{"$ref": "#/$defs/node"}},
				}},
			},
		},
		{name: "external https", params: map[string]any{"$ref": "https://attacker.invalid/schema.json"}, wantErr: true},
		{name: "external relative", params: map[string]any{"$ref": "other.json#/$defs/x"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewRegistry().Register(&schemaTool{definition: ToolDefinition{Name: "ref", Parameters: tt.params, Strict: true}})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Register() error = %v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestRunObserverReceivesDeepCopies(t *testing.T) {
	tool := &schemaTool{definition: ToolDefinition{Name: "tool", Parameters: map[string]any{"type": "object"}}, result: ToolResult{Content: "original"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "tool", Input: `{}`}}},
		{text: "done"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)
	emitter := NewObservationEmitter(16, func(observation Observation) {
		if observation.ToolCall != nil {
			observation.ToolCall.Name = "mutated"
		}
		if observation.ToolResult != nil {
			observation.ToolResult.Content = "mutated"
		}
		if observation.Step != nil {
			if len(observation.Step.ToolCalls) > 0 {
				observation.Step.Message.Parts[0].ToolCall.Name = "mutated"
				observation.Step.ToolCalls[0].Name = "mutated"
				observation.Step.ToolResults[0].Content = "mutated"
			}
		}
	})

	result, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter})
	emitter.Close()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "done" || result.Messages[0].ToolCalls()[0].Name != "tool" || result.Steps[0].ToolResults[0].Content != "original" {
		t.Fatalf("observer mutated runtime result: %+v", result)
	}
}

func TestRunRejectsChunksAfterFinishAndUnknownChunks(t *testing.T) {
	tests := []struct {
		name  string
		extra StreamChunk
	}{
		{name: "text after finish", extra: StreamChunk{Type: ChunkText, TextDelta: "late"}},
		{name: "error after finish", extra: StreamChunk{Type: ChunkError, Err: errors.New("late")}},
		{name: "finish after finish", extra: StreamChunk{Type: ChunkFinish, Response: terminalResponse(NewAssistantMessage("duplicate"))}},
		{name: "unknown chunk", extra: StreamChunk{Type: "mystery"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := modelFunc(func(context.Context, *GenerateRequest) (<-chan StreamChunk, error) {
				ch := make(chan StreamChunk, 3)
				if tt.extra.Type == "mystery" {
					ch <- tt.extra
				}
				ch <- StreamChunk{Type: ChunkFinish, Response: terminalResponse(NewAssistantMessage("done"))}
				if tt.extra.Type != "mystery" {
					ch <- tt.extra
				}
				close(ch)
				return ch, nil
			})
			a := newTestAgent(t, Config{Key: "test"}, model)
			if _, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}}); !isModelErrorKind(err, ModelErrorKindProtocol, false) {
				t.Fatalf("Run() error = %v, want protocol ModelError", err)
			}
		})
	}
}

func TestRunReturnsTerminalStateForEveryOutcome(t *testing.T) {
	tests := []struct {
		name    string
		request RunRequest
		model   Model
		wantErr bool
	}{
		{name: "completed", request: RunRequest{Messages: []Message{NewUserMessage("hi")}}, model: fixedStreamModel(terminalResponse(NewAssistantMessage("done")))},
		{name: "suspended", request: RunRequest{Messages: []Message{NewUserMessage("hi")}}, model: fixedStreamModel(terminalResponse(Message{Role: RoleAssistant, Parts: []ContentPart{{Type: PartText, Text: "partial"}}, FinishReason: FinishLength}))},
		{name: "request validation failure", request: RunRequest{}, model: &fakeModel{}, wantErr: true},
		{name: "upstream failure", request: RunRequest{Messages: []Message{NewUserMessage("hi")}}, model: &fakeModel{steps: []scriptedStep{{openErr: errors.New("offline")}}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAgent(t, Config{Key: "test"}, tt.model)
			result, err := a.Run(context.Background(), tt.request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Run() error = %v, wantErr=%v", err, tt.wantErr)
			}
			if result == nil || tt.wantErr && result.Outcome != OutcomeFailed {
				t.Fatalf("terminal result = %+v, error = %v", result, err)
			}
		})
	}
}

func TestRunCancellationReturnsErrorAndFailedResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := newTestAgent(t, Config{Key: "test"}, &fakeModel{})
	result, err := a.Run(ctx, RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if !errors.Is(err, context.Canceled) || result.Outcome != OutcomeFailed {
		t.Fatalf("result/error = %+v/%v", result, err)
	}
}

func terminalResponse(message Message) *Response {
	return &Response{Message: message, FinishReason: message.FinishReason}
}

func fixedStreamModel(response *Response) Model {
	return capableModelFunc(func(context.Context, *GenerateRequest) (<-chan StreamChunk, error) {
		ch := make(chan StreamChunk, len(response.Message.Parts)+1)
		for _, part := range response.Message.Parts {
			switch part.Type {
			case PartText:
				ch <- StreamChunk{Type: ChunkText, TextDelta: part.Text}
			case PartToolCall:
				call := *part.ToolCall
				ch <- StreamChunk{Type: ChunkToolCall, ToolCall: &call}
			}
		}
		ch <- StreamChunk{Type: ChunkFinish, Response: response}
		close(ch)
		return ch, nil
	})
}

type capableModelFunc modelFunc

func (f capableModelFunc) Name() string { return "capable-func" }

func (f capableModelFunc) Capabilities() Capabilities {
	return Capabilities{Tools: true, ToolChoiceNone: true, ToolChoiceRequired: true, ToolChoiceNamed: true}
}

func (f capableModelFunc) Generate(context.Context, *GenerateRequest) (*Response, error) {
	return nil, errors.New("not implemented")
}

func (f capableModelFunc) Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamChunk, error) {
	return modelFunc(f)(ctx, req)
}

func TestRepairBudgetDocumentationDefinesInvalidCalls(t *testing.T) {
	if !strings.Contains(toolRepairLimitDocumentation, "invalid tool calls") {
		t.Fatalf("repair budget documentation is ambiguous: %q", toolRepairLimitDocumentation)
	}
}
