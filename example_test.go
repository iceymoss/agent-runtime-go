package agent_test

import (
	"context"
	"fmt"
	"strings"

	agent "github.com/iceymoss/agent-runtime-go"
)

// exampleModel stands in for a provider adapter so these examples are
// deterministic. A real application passes providers/openaicompat, or its own
// implementation of the three-method agent.Model port.
type exampleModel struct{ replies []string }

func (exampleModel) Name() string { return "example" }
func (exampleModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}

func (m *exampleModel) Stream(_ context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	// Answer once the tool has reported back; otherwise call it.
	for _, message := range request.Messages {
		if message.Role == agent.RoleTool {
			text := message.ToolResults()[0].Content
			return reply("The weather is " + text + "."), nil
		}
	}
	if len(request.Tools) == 0 {
		return reply(m.replies[0]), nil
	}
	call := agent.ToolCall{ID: "call-1", Name: "get_weather", Input: `{"city":"Hangzhou"}`}
	message := agent.Message{Role: agent.RoleAssistant, FinishReason: agent.FinishToolCalls,
		Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}}
	return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishToolCalls}), nil
}

func reply(text string) <-chan agent.StreamChunk {
	message := agent.NewAssistantMessage(text)
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishStop, ModelName: "example-v1"})
}

// ExampleNew assembles an agent and runs one turn. This is the whole API a first
// agent needs.
func ExampleNew() {
	runner, err := agent.New(agent.Config{
		Key:          "example.assistant",
		ModelName:    "example-v1",
		MaxSteps:     4,
		AllowedTools: []string{}, // no tools for this run
	}, &exampleModel{replies: []string{"Hello."}}, agent.NewRegistry())
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{
		agent.NewSystemMessage("Answer in one word."),
		agent.NewUserMessage("Say hello."),
	}})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
	fmt.Println(result.Outcome, result.StopReason)
	// Output:
	// Hello.
	// completed complete
}

// ExampleNewTool defines a tool from a plain function and lets the model call
// it. The JSON Schema is generated from the input struct.
func ExampleNewTool() {
	type weatherInput struct {
		City string `json:"city" description:"City name"`
	}

	weather := agent.MustNewTool("get_weather", "Get the current weather for a city.",
		func(_ context.Context, input weatherInput) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "sunny in " + input.City}, nil
		})

	registry := agent.NewRegistry()
	if err := registry.Register(weather); err != nil {
		panic(err)
	}
	runner, err := agent.New(agent.Config{Key: "example.weather", ModelName: "example-v1", MaxSteps: 4},
		&exampleModel{}, registry)
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("What is the weather in Hangzhou?")},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
	// The run took two model steps: one to call the tool, one to answer.
	fmt.Println("steps:", len(result.Steps))
	// Output:
	// The weather is sunny in Hangzhou.
	// steps: 2
}

// ExampleNewObservationEmitterWith streams text to a reader as it arrives.
//
// Lossless is what makes the streamed text trustworthy: the default emitter
// drops observations when the consumer falls behind the model, which would show
// a truncated answer while RunResult.Text stayed complete.
func ExampleNewObservationEmitterWith() {
	runner, err := agent.New(agent.Config{Key: "example.stream", ModelName: "example-v1", MaxSteps: 4, AllowedTools: []string{}},
		&exampleModel{replies: []string{"Streaming works."}}, agent.NewRegistry())
	if err != nil {
		panic(err)
	}

	var streamed strings.Builder
	emitter := agent.NewObservationEmitterWith(
		agent.ObservationOptions{QueueSize: 64, Lossless: true},
		func(observation agent.Observation) {
			if observation.Type == agent.ObservationTextDelta {
				streamed.WriteString(observation.Text)
			}
		})

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages:           []agent.Message{agent.NewUserMessage("stream")},
		ObservationEmitter: emitter,
	})
	emitter.Close()
	if err != nil {
		panic(err)
	}
	fmt.Println(streamed.String())
	fmt.Println("dropped:", emitter.Dropped(), "matches result:", streamed.String() == result.Text)
	// Output:
	// Streaming works.
	// dropped: 0 matches result: true
}
