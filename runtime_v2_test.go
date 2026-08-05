package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestToolChoiceValidate(t *testing.T) {
	tests := []struct {
		name    string
		choice  ToolChoice
		tools   []ToolDefinition
		caps    Capabilities
		wantErr bool
	}{
		{name: "auto", choice: ToolChoice{Mode: ToolChoiceAuto}, caps: Capabilities{Tools: true}},
		{name: "none", choice: ToolChoice{Mode: ToolChoiceNone}, caps: Capabilities{Tools: true, ToolChoiceNone: true}},
		{name: "required", choice: ToolChoice{Mode: ToolChoiceRequired}, tools: []ToolDefinition{{Name: "score"}}, caps: Capabilities{Tools: true, ToolChoiceRequired: true}},
		{name: "named", choice: ToolChoice{Mode: ToolChoiceNamed, Name: "score"}, tools: []ToolDefinition{{Name: "score"}}, caps: Capabilities{Tools: true, ToolChoiceNamed: true}},
		{name: "named missing name", choice: ToolChoice{Mode: ToolChoiceNamed}, caps: Capabilities{Tools: true, ToolChoiceNamed: true}, wantErr: true},
		{name: "named inactive tool", choice: ToolChoice{Mode: ToolChoiceNamed, Name: "other"}, tools: []ToolDefinition{{Name: "score"}}, caps: Capabilities{Tools: true, ToolChoiceNamed: true}, wantErr: true},
		{name: "none unsupported", choice: ToolChoice{Mode: ToolChoiceNone}, caps: Capabilities{Tools: true}, wantErr: true},
		{name: "required without tools", choice: ToolChoice{Mode: ToolChoiceRequired}, caps: Capabilities{Tools: true, ToolChoiceRequired: true}, wantErr: true},
		{name: "unknown", choice: ToolChoice{Mode: "sometimes"}, caps: Capabilities{Tools: true}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.choice.Validate(tt.tools, tt.caps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCapabilitiesValidate(t *testing.T) {
	tests := []struct {
		name string
		caps Capabilities
		bad  bool
	}{
		{name: "empty"},
		{name: "coherent", caps: Capabilities{Tools: true, ToolChoiceNamed: true, UsageDetails: true}},
		{name: "choice without tools", caps: Capabilities{ToolChoiceRequired: true}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.caps.Validate(); (err != nil) != tt.bad {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestRegistryCompilesAndValidatesDraft202012Schema(t *testing.T) {
	tests := []struct {
		name    string
		tool    *schemaTool
		wantErr bool
	}{
		{name: "valid draft 2020-12", tool: &schemaTool{definition: ToolDefinition{Name: "valid", Parameters: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"}}}},
		{name: "malformed schema", tool: &schemaTool{definition: ToolDefinition{Name: "bad", Parameters: map[string]any{"type": 42}}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewRegistry().Register(tt.tool)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Register() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRunSchemaValidationAndRepairBudget(t *testing.T) {
	definition := ToolDefinition{
		Name: "score",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"score": map[string]any{"type": "integer"}},
			"required":   []any{"score"},
		},
		Strict: true,
	}
	tests := []struct {
		name       string
		limit      int
		inputs     []string
		wantErr    error
		wantCalls  int
		wantModels int
	}{
		{name: "malformed corrected on normal next step", limit: 1, inputs: []string{`{"score":`, `{"score":1}`}, wantCalls: 1, wantModels: 3},
		{name: "schema invalid corrected", limit: 1, inputs: []string{`{"score":"bad"}`, `{"score":1}`}, wantCalls: 1, wantModels: 3},
		{name: "strict rejects additional property", limit: 1, inputs: []string{`{"score":1,"extra":true}`, `{"score":1}`}, wantCalls: 1, wantModels: 3},
		{name: "budget exhausted", limit: 1, inputs: []string{`{"score":"bad"}`, `{"score":"still bad"}`}, wantErr: ErrToolInputInvalid, wantCalls: 0, wantModels: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &schemaTool{definition: definition}
			steps := make([]scriptedStep, 0, len(tt.inputs)+1)
			for i, input := range tt.inputs {
				steps = append(steps, scriptedStep{calls: []ToolCall{{ID: string(rune('a' + i)), Name: "score", Input: input}}})
			}
			if tt.wantErr == nil {
				steps = append(steps, scriptedStep{text: "corrected"})
			}
			model := &fakeModel{steps: steps}
			a := newTestAgent(t, Config{Key: "test", ToolRepairLimit: tt.limit}, model, tool)

			res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Run() error = %v, want %v", err, tt.wantErr)
			}
			if tool.calls != tt.wantCalls || model.calls != tt.wantModels {
				t.Fatalf("calls tool/model = %d/%d, want %d/%d", tool.calls, model.calls, tt.wantCalls, tt.wantModels)
			}
			if tt.wantErr != nil && res.Outcome != OutcomeFailed {
				t.Fatalf("Outcome = %q, want failed", res.Outcome)
			}
		})
	}
}

func TestStrictSchemaDoesNotChangeNonStrictAdditionalProperties(t *testing.T) {
	tool := &schemaTool{definition: ToolDefinition{
		Name:       "loose",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"known": map[string]any{"type": "string"}}},
	}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c", Name: "loose", Input: `{"known":"yes","extra":true}`}}},
		{text: "ok"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)
	if _, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tool.calls != 1 {
		t.Fatalf("tool calls = %d", tool.calls)
	}
}

func TestRunStepPolicyControlsOnlyRequest(t *testing.T) {
	first := &schemaTool{definition: ToolDefinition{Name: "first", Parameters: map[string]any{"type": "object"}}}
	second := &schemaTool{definition: ToolDefinition{Name: "second", Parameters: map[string]any{"type": "object"}}}
	model := &recordingModel{fakeModel: fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "second", Input: `{}`}}},
		{text: "ok"},
	}}}
	temp, topP, maxTokens := 0.25, 0.8, 123
	policyCalls := 0
	policy := func(input StepPolicyInput) (StepPolicyDecision, error) {
		policyCalls++
		if len(input.AvailableTools) != 2 {
			t.Fatalf("policy input = %+v", input)
		}
		if len(input.CompletedSteps) > 0 {
			return StepPolicyDecision{ActiveTools: []string{}, ToolChoice: &ToolChoice{Mode: ToolChoiceNone}}, nil
		}
		return StepPolicyDecision{
			ActiveTools: []string{"second"},
			ToolChoice:  &ToolChoice{Mode: ToolChoiceNamed, Name: "second"},
			Model:       "policy-model",
			Generation:  GenerationOptions{Temperature: &temp, TopP: &topP, MaxTokens: &maxTokens},
		}, nil
	}
	a := newTestAgent(t, Config{Key: "test", ModelName: "default"}, model, first, second)
	history := []Message{NewUserMessage("unchanged")}
	_, err := a.Run(context.Background(), RunRequest{Messages: history, StepPolicy: policy})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if policyCalls != 2 {
		t.Fatalf("policy calls = %d", policyCalls)
	}
	req := model.requests[0]
	if req.Model != "policy-model" || len(req.Tools) != 1 || req.Tools[0].Name != "second" || req.ToolChoice == nil || req.ToolChoice.Name != "second" {
		t.Fatalf("request policy fields = %+v", req)
	}
	if req.Temperature != &temp || req.TopP != &topP || req.MaxTokens != &maxTokens {
		t.Fatalf("generation pointers were not preserved: %+v", req)
	}
	if !reflect.DeepEqual(history, []Message{NewUserMessage("unchanged")}) {
		t.Fatalf("history mutated: %+v", history)
	}
}

func TestRunOutcomeSemantics(t *testing.T) {
	tests := []struct {
		name       string
		cfg        Config
		steps      []scriptedStep
		tool       Tool
		want       Outcome
		wantReason StopReason
	}{
		{name: "terminal model response completes", cfg: Config{MaxSteps: 4}, steps: []scriptedStep{{text: "done"}}, want: OutcomeCompleted, wantReason: StopReasonComplete},
		{name: "max steps with pending continuation suspends", cfg: Config{MaxSteps: 2, LoopDetectWindow: 2, LoopDetectThreshold: 1}, steps: []scriptedStep{{calls: []ToolCall{{ID: "c1", Name: "tool", Input: `{"step":1}`}}}, {calls: []ToolCall{{ID: "c2", Name: "tool", Input: `{"step":2}`}}}}, tool: &schemaTool{definition: ToolDefinition{Name: "tool", Parameters: map[string]any{"type": "object"}}, result: ToolResult{Content: "ok"}}, want: OutcomeSuspended, wantReason: StopReasonMaxSteps},
		{name: "context budget with pending continuation suspends", cfg: Config{MaxSteps: 2, LoopDetectWindow: 2, LoopDetectThreshold: 1, ContextWindow: 100}, steps: []scriptedStep{{calls: []ToolCall{{ID: "c", Name: "tool", Input: `{}`}}, usage: Usage{PromptTokens: 90, TotalTokens: 90}}}, tool: &schemaTool{definition: ToolDefinition{Name: "tool", Parameters: map[string]any{"type": "object"}}, result: ToolResult{Content: "ok"}}, want: OutcomeSuspended, wantReason: StopReasonContextBudget},
		{name: "tool StopTurn explicitly completes", cfg: Config{MaxSteps: 2, LoopDetectWindow: 2, LoopDetectThreshold: 1}, steps: []scriptedStep{{calls: []ToolCall{{ID: "c", Name: "tool", Input: `{}`}}}}, tool: &schemaTool{definition: ToolDefinition{Name: "tool", Parameters: map[string]any{"type": "object"}}, result: ToolResult{Content: "stop", StopTurn: true}}, want: OutcomeCompleted, wantReason: StopReasonToolStopTurn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tools []Tool
			if tt.tool != nil {
				tools = append(tools, tt.tool)
			}
			a := newTestAgent(t, tt.cfg, &fakeModel{steps: tt.steps}, tools...)
			res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if res.Outcome != tt.want || res.StopReason != tt.wantReason {
				t.Fatalf("outcome/reason = %q/%q, want %q/%q", res.Outcome, res.StopReason, tt.want, tt.wantReason)
			}
		})
	}
}

func TestUsageValidateAndAddDetails(t *testing.T) {
	tests := []struct {
		name string
		use  Usage
		bad  bool
	}{
		{name: "valid", use: Usage{PromptTokens: 10, CompletionTokens: 8, TotalTokens: 24, ReasoningTokens: 3, CacheCreationTokens: 2, CacheReadTokens: 4}},
		{name: "reasoning exceeds completion", use: Usage{CompletionTokens: 2, TotalTokens: 2, ReasoningTokens: 3}, bad: true},
		{name: "cache total mismatch", use: Usage{PromptTokens: 5, TotalTokens: 5, CacheCreationTokens: 3, CacheReadTokens: 3}, bad: true},
		{name: "total mismatch", use: Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 4}, bad: true},
		{name: "negative", use: Usage{PromptTokens: -1}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.use.Validate(); (err != nil) != tt.bad {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
	u := Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 6, ReasoningTokens: 1, CacheReadTokens: 1}
	u.Add(Usage{PromptTokens: 4, CompletionTokens: 5, TotalTokens: 12, ReasoningTokens: 2, CacheCreationTokens: 1, CacheReadTokens: 2})
	want := Usage{PromptTokens: 6, CompletionTokens: 8, TotalTokens: 18, ReasoningTokens: 3, CacheCreationTokens: 1, CacheReadTokens: 3}
	if u != want {
		t.Fatalf("Add() = %+v, want %+v", u, want)
	}
}

func TestCentralRequestAndContentValidation(t *testing.T) {
	tests := []struct {
		name string
		req  RunRequest
	}{
		{name: "empty history", req: RunRequest{}},
		{name: "unknown role", req: RunRequest{Messages: []Message{{Role: "alien", Parts: []ContentPart{{Type: PartText, Text: "x"}}}}}},
		{name: "empty parts", req: RunRequest{Messages: []Message{{Role: RoleUser}}}},
		{name: "wrong payload for text", req: RunRequest{Messages: []Message{{Role: RoleUser, Parts: []ContentPart{{Type: PartText, ToolCall: &ToolCall{ID: "c", Name: "x"}}}}}}},
		{name: "user tool call", req: RunRequest{Messages: []Message{{Role: RoleUser, Parts: []ContentPart{{Type: PartToolCall, ToolCall: &ToolCall{ID: "c", Name: "x", Input: `{}`}}}}}}},
		{name: "tool result missing pair id", req: RunRequest{Messages: []Message{{Role: RoleTool, Parts: []ContentPart{{Type: PartToolResult, ToolResult: &ToolResult{Name: "x"}}}}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateRunRequest(tt.req); err == nil {
				t.Fatal("ValidateRunRequest() error = nil")
			}
		})
	}
	if err := ValidateRunRequest(RunRequest{Messages: []Message{NewSystemMessage("sys"), NewUserMessage("hi")}}); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestValidateGenerateRequest(t *testing.T) {
	tests := []struct {
		name string
		req  *GenerateRequest
		bad  bool
	}{
		{name: "valid", req: &GenerateRequest{Model: "m", Messages: []Message{NewUserMessage("hi")}}},
		{name: "nil", bad: true},
		{name: "missing messages", req: &GenerateRequest{Model: "m"}, bad: true},
		{name: "invalid message", req: &GenerateRequest{Model: "m", Messages: []Message{{Role: RoleUser}}}, bad: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateGenerateRequest(tt.req); (err != nil) != tt.bad {
				t.Fatalf("ValidateGenerateRequest() error = %v", err)
			}
		})
	}
}

func TestStepPolicyCannotMutateCompletedSteps(t *testing.T) {
	tool := &schemaTool{definition: ToolDefinition{Name: "tool", Parameters: map[string]any{"type": "object"}}, result: ToolResult{Content: "ok"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c", Name: "tool", Input: `{}`}}},
		{text: "done"},
	}}
	policy := func(input StepPolicyInput) (StepPolicyDecision, error) {
		if len(input.CompletedSteps) > 0 {
			input.CompletedSteps[0].Message.Parts[0].ToolCall.Name = "mutated"
			input.CompletedSteps[0].ToolCalls[0].Name = "mutated"
			input.CompletedSteps[0].ToolResults[0].Content = "mutated"
		}
		return StepPolicyDecision{}, nil
	}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)
	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, StepPolicy: policy})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	step := res.Steps[0]
	if step.ToolCalls[0].Name != "tool" || step.ToolResults[0].Content != "ok" || step.Message.ToolCalls()[0].Name != "tool" {
		t.Fatalf("policy mutated completed step: %+v", step)
	}
}

func TestRunRejectsStreamTerminalMismatch(t *testing.T) {
	tests := []struct {
		name     string
		streamed []StreamChunk
		message  Message
	}{
		{name: "text mismatch", streamed: []StreamChunk{{Type: ChunkText, TextDelta: "stream"}}, message: NewAssistantMessage("terminal")},
		{name: "tool calls mismatch", streamed: []StreamChunk{{Type: ChunkToolCall, ToolCall: &ToolCall{ID: "a", Name: "x", Input: `{}`}}}, message: Message{Role: RoleAssistant, Parts: []ContentPart{{Type: PartToolCall, ToolCall: &ToolCall{ID: "b", Name: "x", Input: `{}`}}}, FinishReason: FinishToolCalls}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := modelFunc(func(context.Context, *GenerateRequest) (<-chan StreamChunk, error) {
				ch := make(chan StreamChunk, len(tt.streamed)+1)
				for _, chunk := range tt.streamed {
					ch <- chunk
				}
				finish := tt.message.FinishReason
				ch <- StreamChunk{Type: ChunkFinish, Response: &Response{Message: tt.message, FinishReason: finish}}
				close(ch)
				return ch, nil
			})
			a := newTestAgent(t, Config{Key: "test"}, model)
			res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
			if !isModelErrorKind(err, ModelErrorKindProtocol, false) || res.Outcome != OutcomeFailed {
				t.Fatalf("result/error = %+v/%v", res, err)
			}
		})
	}
}

func TestConfigRejectsRepairLimitAboveHardMax(t *testing.T) {
	_, err := New(Config{MaxSteps: 2, ToolRepairLimit: 3}, &fakeModel{}, NewRegistry())
	if !errors.Is(err, ErrAgentConfigInvalid) || !strings.Contains(err.Error(), "tool repair") {
		t.Fatalf("New() error = %v", err)
	}
}

func TestNewValidatesConfiguredToolChoiceAgainstCapabilities(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register(&schemaTool{definition: ToolDefinition{Name: "score", Parameters: map[string]any{"type": "object"}}}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	choice := ToolChoice{Mode: ToolChoiceNamed, Name: "missing"}
	_, err := New(Config{MaxSteps: 4, ToolChoice: &choice}, &fakeModel{}, registry)
	if !errors.Is(err, ErrAgentConfigInvalid) {
		t.Fatalf("New() error = %v, want ErrAgentConfigInvalid", err)
	}
}

func TestRunUsesImmutableToolSnapshotAfterAgentAssembly(t *testing.T) {
	registry := NewRegistry()
	original := &fakeTool{name: "existing", desc: "original", result: ToolResult{Content: "original"}}
	if err := registry.Register(original); err != nil {
		t.Fatal(err)
	}
	model := &recordingModel{fakeModel: fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "existing-call", Name: "existing", Input: `{}`}}},
		{text: "done"},
	}}}
	a, err := New(Config{Key: "test", MaxSteps: 4}, model, registry)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &fakeTool{name: "existing", desc: "replacement", result: ToolResult{Content: "replacement"}}
	late := &fakeTool{name: "late", desc: "late", result: ToolResult{Content: "late"}}
	if err := registry.Replace(replacement); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(late); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if original.calls != 1 || replacement.calls != 0 || late.calls != 0 {
		t.Fatalf("tool calls = original %d, replacement %d, late %d", original.calls, replacement.calls, late.calls)
	}
	definitions := model.requests[0].Tools
	if len(definitions) != 1 || definitions[0].Name != "existing" || definitions[0].Description != "original" {
		t.Fatalf("definitions = %#v", definitions)
	}
}

type schemaTool struct {
	definition ToolDefinition
	result     ToolResult
	calls      int
}

func (t *schemaTool) Definition() ToolDefinition { return t.definition }

func (t *schemaTool) ReplayPolicy() ReplayPolicy { return ReplayPolicyNever }

func (t *schemaTool) Execute(context.Context, ToolInvocation) (ToolResult, error) {
	t.calls++
	return t.result, nil
}

type recordingModel struct {
	fakeModel
	requests []*GenerateRequest
}

func (m *recordingModel) Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamChunk, error) {
	m.requests = append(m.requests, req)
	return m.fakeModel.Stream(ctx, req)
}
