package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

type helloModel struct{}

func (helloModel) Name() string { return "hello" }

func (helloModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }

// Stream returns the model's answer as the runtime's chunk stream.
//
// An adapter that already holds the whole response - a non-streaming endpoint,
// a cached reply, or a fake like this one - hands it to agent.StreamResponse,
// which emits the deltas and the terminal chunk consistently. An adapter that
// genuinely streams emits its own deltas instead and finishes with one
// ChunkFinish whose response matches them.
func (helloModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	return agent.StreamResponse(&agent.Response{
		Message:      agent.NewAssistantMessage("Hello from Agent Runtime for Go."),
		FinishReason: agent.FinishStop,
		ModelName:    "hello-v1",
	}), nil
}

func main() {
	runner, err := agent.New(agent.Config{
		Key:          "example.hello",
		ModelName:    "hello-v1",
		MaxSteps:     4,
		AllowedTools: []string{},
	}, helloModel{}, agent.NewRegistry())
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{
		agent.NewSystemMessage("Answer clearly."),
		agent.NewUserMessage("Say hello."),
	}})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
}
