package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

type definitionModel struct{}

func (definitionModel) Name() string                     { return "model" }
func (definitionModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (definitionModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	return nil, nil
}

type definitionTool struct {
	content string
}

func (t definitionTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "lookup", Parameters: map[string]any{"type": "object"}}
}

func (definitionTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }

func (t definitionTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	return agent.ToolResult{Content: t.content}, nil
}

type toolRunModel struct {
	step int
}

func (*toolRunModel) Name() string { return "tool-model" }

func (*toolRunModel) Capabilities() agent.Capabilities { return agent.Capabilities{Tools: true} }

func (m *toolRunModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	m.step++
	chunks := make(chan agent.StreamChunk, 2)
	if m.step == 1 {
		call := agent.ToolCall{ID: "call-1", Name: "lookup", Input: `{}`}
		message := agent.Message{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}, FinishReason: agent.FinishToolCalls}
		chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
		chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{Message: message, FinishReason: agent.FinishToolCalls}}
	} else {
		message := agent.NewAssistantMessage("done")
		chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: "done"}
		chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{Message: message, FinishReason: agent.FinishStop}}
	}
	close(chunks)
	return chunks, nil
}

func TestRuntimeDefinitionPublicContract(t *testing.T) {
	tools, err := agent.NewToolSet(agent.NewRegistry(), []string{})
	if err != nil {
		t.Fatal(err)
	}
	settings := agent.ExecutionSettings{MaxSteps: 4}
	definition, err := agent.NewRuntimeDefinition(agent.RuntimeDefinitionSpec{
		Key:       "assistant.support",
		Model:     agent.ModelMetadata{Name: "model-v1", Version: "provider/model-v1", ContextWindow: 128000},
		Execution: settings, PromptVersion: "sha256:prompt", PolicyVersion: "practice-policy-v1",
	}, definitionModel{}, tools)
	if err != nil {
		t.Fatalf("NewRuntimeDefinition() error = %v", err)
	}
	versions := definition.ArtifactVersions()
	if definition.Key() != "assistant.support" || versions.Definition == "" || versions.Tools != tools.Version() {
		t.Fatalf("definition = %q, %+v", definition.Key(), versions)
	}
	settings.MaxSteps = 1
	if definition.ExecutionSettings().MaxSteps != 4 {
		t.Fatal("definition retained caller-owned settings")
	}
	if _, err := definition.NewAgent(); err != nil {
		t.Fatalf("NewAgent() error = %v", err)
	}
}

func TestRuntimeDefinitionRejectsIncompleteOrInconsistentArtifacts(t *testing.T) {
	tools, err := agent.NewToolSet(agent.NewRegistry(), []string{})
	if err != nil {
		t.Fatal(err)
	}
	base := agent.RuntimeDefinitionSpec{
		Key: "assistant.support", Model: agent.ModelMetadata{Name: "model-v1", Version: "provider/model-v1"},
		Execution: agent.ExecutionSettings{MaxSteps: 4}, PromptVersion: "sha256:prompt", PolicyVersion: "policy-v1",
	}
	tests := []struct {
		name  string
		spec  agent.RuntimeDefinitionSpec
		model agent.Model
	}{
		{name: "missing prompt version", spec: func() agent.RuntimeDefinitionSpec { value := base; value.PromptVersion = ""; return value }(), model: definitionModel{}},
		{name: "missing policy version", spec: func() agent.RuntimeDefinitionSpec { value := base; value.PolicyVersion = ""; return value }(), model: definitionModel{}},
		{name: "capability mismatch", spec: func() agent.RuntimeDefinitionSpec { value := base; value.Model.Capabilities.Tools = true; return value }(), model: definitionModel{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			definition, err := agent.NewRuntimeDefinition(tt.spec, tt.model, tools)
			if definition != nil || !errors.Is(err, agent.ErrAgentConfigInvalid) {
				t.Fatalf("NewRuntimeDefinition() = %+v, %v", definition, err)
			}
		})
	}
}

func TestRuntimeDefinitionRetainsToolSnapshotAndStableVersions(t *testing.T) {
	registry := agent.NewRegistry()
	if err := registry.Register(definitionTool{content: "first"}); err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	model := &toolRunModel{}
	spec := agent.RuntimeDefinitionSpec{
		Key: "tool-run", Model: agent.ModelMetadata{Name: "tool-model", Version: "tool-model-v1", Capabilities: model.Capabilities()},
		Execution: agent.ExecutionSettings{MaxSteps: 4}, PromptVersion: "prompt-v1", PolicyVersion: "policy-v1",
	}
	definition, err := agent.NewRuntimeDefinition(spec, model, tools)
	if err != nil {
		t.Fatal(err)
	}
	matching, err := agent.NewRuntimeDefinition(spec, &toolRunModel{}, tools)
	if err != nil {
		t.Fatal(err)
	}
	if definition.ArtifactVersions() != matching.ArtifactVersions() {
		t.Fatalf("versions differ: %+v != %+v", definition.ArtifactVersions(), matching.ArtifactVersions())
	}
	if err := registry.Replace(definitionTool{content: "second"}); err != nil {
		t.Fatal(err)
	}
	runner, err := definition.NewAgent()
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{agent.NewUserMessage("run")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Steps) < 1 || len(result.Steps[0].ToolResults) != 1 || result.Steps[0].ToolResults[0].Content != "first" {
		t.Fatalf("tool snapshot changed: %+v", result.Steps)
	}
}
