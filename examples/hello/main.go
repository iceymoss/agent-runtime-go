package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

type helloModel struct{}

func (helloModel) Name() string { return "hello" }

func (helloModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }

func (helloModel) Stream(ctx context.Context, _ *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	chunks := make(chan agent.StreamChunk, 2)
	go func() {
		defer close(chunks)
		message := agent.NewAssistantMessage("Hello from Agent Runtime for Go.")
		select {
		case chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: message.Text()}:
		case <-ctx.Done():
			return
		}
		select {
		case chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
			Message: message, FinishReason: agent.FinishStop, ModelName: "hello-v1",
		}}:
		case <-ctx.Done():
		}
	}()
	return chunks, nil
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
