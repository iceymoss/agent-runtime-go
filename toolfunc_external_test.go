package agent_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
)

type forecastInput struct {
	City    string  `json:"city" description:"City name"`
	Days    int     `json:"days,omitempty" description:"Forecast days"`
	Verbose *bool   `json:"verbose"`
	Scale   float64 `json:"scale,omitempty"`
}

func TestNewToolGeneratesSchema(t *testing.T) {
	tool, err := agent.NewTool("forecast", "Get a weather forecast.",
		func(ctx context.Context, input forecastInput) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "ok"}, nil
		})
	if err != nil {
		t.Fatalf("new tool: %v", err)
	}
	definition := tool.Definition()
	if definition.Name != "forecast" || definition.Description != "Get a weather forecast." {
		t.Errorf("unexpected definition metadata %+v", definition)
	}
	if !definition.Strict {
		t.Errorf("expected strict schema by default")
	}
	expected := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city":    map[string]any{"type": "string", "description": "City name"},
			"days":    map[string]any{"type": "integer", "description": "Forecast days"},
			"verbose": map[string]any{"type": "boolean"},
			"scale":   map[string]any{"type": "number"},
		},
		"required": []any{"city"},
	}
	if !reflect.DeepEqual(definition.Parameters, expected) {
		t.Errorf("unexpected schema:\n got %#v\nwant %#v", definition.Parameters, expected)
	}
}

func TestNewToolRejectsUnsupportedTypes(t *testing.T) {
	type badInput struct {
		Signal chan int `json:"signal"`
	}
	_, err := agent.NewTool("bad", "Broken tool.",
		func(ctx context.Context, input badInput) (agent.ToolResult, error) {
			return agent.ToolResult{}, nil
		})
	if !errors.Is(err, agent.ErrAgentConfigInvalid) {
		t.Fatalf("expected ErrAgentConfigInvalid, got %v", err)
	}

	type nonStruct = string
	_, err = agent.NewTool("bad2", "Broken tool.",
		func(ctx context.Context, input nonStruct) (agent.ToolResult, error) {
			return agent.ToolResult{}, nil
		})
	if !errors.Is(err, agent.ErrAgentConfigInvalid) {
		t.Fatalf("expected ErrAgentConfigInvalid for non-struct input, got %v", err)
	}
}

func TestFuncToolDecodeFailureIsModelCorrectable(t *testing.T) {
	tool := agent.MustNewTool("forecast", "Get a weather forecast.",
		func(ctx context.Context, input forecastInput) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "ok"}, nil
		})
	result, err := tool.Execute(context.Background(), agent.ToolInvocation{
		CallID:   "call-1",
		Name:     "forecast",
		RawInput: `{"city":"Hangzhou","days":"three"}`,
	})
	if err != nil {
		t.Fatalf("decode failure must not be a fatal error: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected IsError result, got %+v", result)
	}
}

func TestFuncToolOptions(t *testing.T) {
	tool := agent.MustNewTool("forecast", "Get a weather forecast.",
		func(ctx context.Context, input forecastInput) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "ok"}, nil
		},
		agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent),
		agent.WithToolExecutableVersion("v1"),
		agent.WithoutStrictSchema(),
	)
	if tool.ReplayPolicy() != agent.ReplayPolicyIdempotent {
		t.Errorf("unexpected replay policy %q", tool.ReplayPolicy())
	}
	if tool.ExecutableVersion() != "v1" {
		t.Errorf("unexpected executable version %q", tool.ExecutableVersion())
	}
	if tool.Definition().Strict {
		t.Errorf("expected strict disabled")
	}
}

// funcToolScriptedModel drives a repair loop: the first step sends input that
// violates the strict schema, the second step sends valid input, and the
// third step returns the final answer.
type funcToolScriptedModel struct{}

func (funcToolScriptedModel) Name() string { return "scripted" }

func (funcToolScriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}

func (funcToolScriptedModel) Stream(ctx context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	toolSteps := 0
	sawError := false
	for _, message := range req.Messages {
		for _, result := range message.ToolResults() {
			toolSteps++
			if result.IsError {
				sawError = true
			}
		}
	}
	chunks := make(chan agent.StreamChunk, 2)
	defer close(chunks)
	switch {
	case toolSteps == 0:
		call := agent.ToolCall{ID: "call-1", Name: "forecast", Input: `{"town":"Hangzhou"}`}
		chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
		chunks <- finishWithToolCall(call)
	case toolSteps == 1 && sawError:
		call := agent.ToolCall{ID: "call-2", Name: "forecast", Input: `{"city":"Hangzhou"}`}
		chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
		chunks <- finishWithToolCall(call)
	default:
		message := agent.NewAssistantMessage("Hangzhou is sunny.")
		chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: message.Text()}
		chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
			Message: message, FinishReason: agent.FinishStop, ModelName: "scripted-v1",
		}}
	}
	return chunks, nil
}

func finishWithToolCall(call agent.ToolCall) agent.StreamChunk {
	message := agent.Message{
		Role:         agent.RoleAssistant,
		Parts:        []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}},
		FinishReason: agent.FinishToolCalls,
	}
	return agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
		Message: message, FinishReason: agent.FinishToolCalls, ModelName: "scripted-v1",
	}}
}

func TestFuncToolRunWithRepair(t *testing.T) {
	var received forecastInput
	tool := agent.MustNewTool("forecast", "Get a weather forecast.",
		func(ctx context.Context, input forecastInput) (agent.ToolResult, error) {
			received = input
			return agent.ToolResult{Content: `{"condition":"sunny"}`}, nil
		})

	registry := agent.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	runner, err := agent.New(agent.Config{
		Key:       "test.forecast",
		ModelName: "scripted-v1",
		MaxSteps:  4,
	}, funcToolScriptedModel{}, registry)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{
		agent.NewUserMessage("What is the weather in Hangzhou?"),
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "Hangzhou is sunny." {
		t.Errorf("unexpected text %q", result.Text)
	}
	if received.City != "Hangzhou" {
		t.Errorf("tool function received %+v", received)
	}
	// Step 1 must have produced a model-correctable schema error.
	if len(result.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(result.Steps))
	}
	firstResults := result.Steps[0].ToolResults
	if len(firstResults) != 1 || !firstResults[0].IsError {
		t.Errorf("expected schema violation error in step 1, got %+v", firstResults)
	}
}
