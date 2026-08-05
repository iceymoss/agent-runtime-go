package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRunOrdinaryAndDurableParity(t *testing.T) {
	strictScore := ToolDefinition{Name: "score", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{
			"score": map[string]any{"type": "integer"},
		}, "required": []any{"score"},
	}, Strict: true}
	tests := []struct {
		name  string
		cfg   Config
		setup func() (Model, []Tool)
		ctx   func() context.Context
	}{
		{name: "final stop completed", cfg: Config{Key: "runtime-test", ModelName: "configured-model", MaxSteps: 4}, setup: func() (Model, []Tool) {
			return &fakeModel{steps: []scriptedStep{{text: "done", usage: Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}}}, nil
		}},
		{name: "finish length suspended", cfg: Config{Key: "runtime-test", ModelName: "configured-model", MaxSteps: 4}, setup: func() (Model, []Tool) {
			message := Message{Role: RoleAssistant, Parts: []ContentPart{{Type: PartText, Text: "partial"}}, FinishReason: FinishLength}
			response := terminalResponse(message)
			response.Usage = Usage{PromptTokens: 4, CompletionTokens: 3, TotalTokens: 7}
			response.ModelName = "response-model"
			return fixedStreamModel(response), nil
		}},
		{name: "valid tool batch then answer", cfg: Config{Key: "runtime-test", ModelName: "configured-model", MaxSteps: 4}, setup: func() (Model, []Tool) {
			model := &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "first-call", Name: "first", Input: `{"value":1}`}, {ID: "second-call", Name: "second", Input: `{"value":2}`}}, usage: Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}}, {text: "tools complete", usage: Usage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}}}}
			return model, []Tool{&fakeTool{name: "first", result: ToolResult{Content: "one"}}, &fakeTool{name: "second", result: ToolResult{Content: "two"}}}
		}},
		{name: "malformed tool input repaired", cfg: Config{Key: "runtime-test", ModelName: "configured-model", MaxSteps: 4, ToolRepairLimit: 1}, setup: func() (Model, []Tool) {
			return &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "bad", Name: "score", Input: `{"score":`}}, usage: Usage{TotalTokens: 2}}, {text: "repaired", usage: Usage{TotalTokens: 1}}}}, []Tool{&schemaTool{definition: strictScore}}
		}},
		{name: "schema invalid tool input repaired", cfg: Config{Key: "runtime-test", ModelName: "configured-model", MaxSteps: 4, ToolRepairLimit: 1}, setup: func() (Model, []Tool) {
			return &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "bad", Name: "score", Input: `{"score":"high"}`}}, usage: Usage{TotalTokens: 2}}, {text: "repaired", usage: Usage{TotalTokens: 1}}}}, []Tool{&schemaTool{definition: strictScore}}
		}},
		{name: "canceled before execution", cfg: Config{Key: "runtime-test", ModelName: "configured-model", MaxSteps: 4}, setup: func() (Model, []Tool) {
			return &fakeModel{steps: []scriptedStep{{text: "must not run"}}}, nil
		}, ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ordinaryModel, ordinaryTools := tt.setup()
			ordinary := newTestAgent(t, tt.cfg, ordinaryModel, ordinaryTools...)
			ordinaryContext := context.Background()
			if tt.ctx != nil {
				ordinaryContext = tt.ctx()
			}
			ordinaryResult, ordinaryErr := ordinary.Run(ordinaryContext, RunRequest{Messages: []Message{NewUserMessage("input")}})
			durableModel, durableTools := tt.setup()
			durable := newTestAgent(t, tt.cfg, durableModel, durableTools...)
			durableContext := context.Background()
			if tt.ctx != nil {
				durableContext = tt.ctx()
			}
			durableResult, durableErr := durable.Run(durableContext, durableRuntimeRequest(newRuntimeMemoryStore(), "parity-"+tt.name))
			if got, want := classifyParityError(durableErr), classifyParityError(ordinaryErr); got != want {
				t.Fatalf("error classification differs: durable=%+v (%v), ordinary=%+v (%v)", got, durableErr, want, ordinaryErr)
			}
			normalizeDurableResult(durableResult)
			if !reflect.DeepEqual(durableResult, ordinaryResult) {
				t.Fatalf("normalized results differ:\ndurable: %#v\nordinary: %#v", durableResult, ordinaryResult)
			}
		})
	}
}

type parityErrorClassification struct {
	present, canceled, deadlineExceeded, toolInputInvalid bool
	modelKind                                             ModelErrorKind
	modelRetryable                                        bool
}

func classifyParityError(err error) parityErrorClassification {
	classification := parityErrorClassification{present: err != nil, canceled: errors.Is(err, context.Canceled), deadlineExceeded: errors.Is(err, context.DeadlineExceeded), toolInputInvalid: errors.Is(err, ErrToolInputInvalid)}
	var modelErr *ModelError
	if errors.As(err, &modelErr) {
		classification.modelKind, classification.modelRetryable = modelErr.Kind, modelErr.Retryable
	}
	return classification
}

func normalizeDurableResult(result *RunResult) {
	if result == nil {
		return
	}
	result.DurableCompletion = nil
	result.DurableFence = 0
	if len(result.Messages) == 0 {
		result.Messages = nil
	}
	if len(result.Steps) == 0 {
		result.Steps = nil
	}
}
