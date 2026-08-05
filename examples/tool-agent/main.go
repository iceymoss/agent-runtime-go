package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

type weatherTool struct{}

func (weatherTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "get_weather",
		Description: "Get the current weather for a city.",
		Strict:      true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required": []any{"city"},
		},
	}
}

func (weatherTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }

func (weatherTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: `{"city":"Hangzhou","condition":"sunny","temperature_c":28}`}, nil
}

// scriptedModel makes the example deterministic: the first step requests a
// tool, and the second step turns the tool result into a final answer.
type scriptedModel struct{}

func (scriptedModel) Name() string { return "scripted" }

func (scriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}

func (scriptedModel) Stream(ctx context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	chunks := make(chan agent.StreamChunk, 2)
	go func() {
		defer close(chunks)
		if req.Messages[len(req.Messages)-1].Role != agent.RoleTool {
			call := agent.ToolCall{ID: "call-weather-1", Name: "get_weather", Input: `{"city":"Hangzhou"}`}
			message := agent.Message{
				Role:         agent.RoleAssistant,
				Parts:        []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}},
				FinishReason: agent.FinishToolCalls,
			}
			chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
			chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
				Message: message, FinishReason: agent.FinishToolCalls, ModelName: "scripted-v1",
			}}
			return
		}

		message := agent.NewAssistantMessage("Hangzhou is sunny and 28 C.")
		chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: message.Text()}
		chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
			Message: message, FinishReason: agent.FinishStop, ModelName: "scripted-v1",
		}}
	}()
	return chunks, nil
}

type WeatherAgent struct {
	runner *agent.Agent
}

func NewWeatherAgent(model agent.Model) (*WeatherAgent, error) {
	registry := agent.NewRegistry()
	if err := registry.Register(weatherTool{}); err != nil {
		return nil, err
	}
	runner, err := agent.New(agent.Config{
		Key:          "example.weather",
		ModelName:    "scripted-v1",
		MaxSteps:     4,
		AllowedTools: []string{"get_weather"},
	}, model, registry)
	if err != nil {
		return nil, err
	}
	return &WeatherAgent{runner: runner}, nil
}

func (a *WeatherAgent) Ask(ctx context.Context, question string) (string, error) {
	result, err := a.runner.Run(ctx, agent.RunRequest{Messages: []agent.Message{
		agent.NewSystemMessage("Use tools when current weather is needed."),
		agent.NewUserMessage(question),
	}})
	if err != nil {
		return "", err
	}
	return result.Text, nil
}

func main() {
	weatherAgent, err := NewWeatherAgent(scriptedModel{})
	if err != nil {
		panic(err)
	}
	answer, err := weatherAgent.Ask(context.Background(), "What is the weather in Hangzhou?")
	if err != nil {
		panic(err)
	}
	fmt.Println(answer)
}
