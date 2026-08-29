// Command openai-compat runs one real model/tool loop against any
// OpenAI-compatible endpoint using the official providers/openaicompat
// adapter and a tool generated with agent.NewTool.
//
// Configuration comes from environment variables:
//
//	OPENAI_API_KEY   API key (required unless the endpoint needs none)
//	OPENAI_BASE_URL  API root, default https://api.openai.com/v1
//	OPENAI_MODEL     model name, default gpt-4o-mini
//
// Examples:
//
//	OPENAI_API_KEY=sk-... go run ./examples/openai-compat
//	OPENAI_BASE_URL=https://api.deepseek.com/v1 OPENAI_API_KEY=... OPENAI_MODEL=deepseek-chat go run ./examples/openai-compat
//	OPENAI_BASE_URL=http://localhost:11434/v1 OPENAI_MODEL=qwen2.5 go run ./examples/openai-compat
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
)

type timeInput struct {
	Timezone string `json:"timezone,omitempty" description:"IANA timezone name such as Asia/Shanghai; defaults to UTC"`
}

func main() {
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = "gpt-4o-mini"
	}
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" && baseURL == "https://api.openai.com/v1" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY is not set.")
		fmt.Fprintln(os.Stderr, "Usage: OPENAI_API_KEY=sk-... go run ./examples/openai-compat")
		fmt.Fprintln(os.Stderr, "Point OPENAI_BASE_URL at any OpenAI-compatible endpoint (DeepSeek, Qwen, vLLM, Ollama, ...).")
		os.Exit(1)
	}

	clock := agent.MustNewTool("get_time", "Get the current date and time.",
		func(ctx context.Context, input timeInput) (agent.ToolResult, error) {
			location := time.UTC
			if input.Timezone != "" {
				loaded, err := time.LoadLocation(input.Timezone)
				if err != nil {
					return agent.ToolResult{
						Content: fmt.Sprintf("unknown timezone %q; use an IANA name such as Asia/Shanghai", input.Timezone),
						IsError: true,
					}, nil
				}
				location = loaded
			}
			return agent.ToolResult{Content: time.Now().In(location).Format(time.RFC1123)}, nil
		},
		agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent))

	registry := agent.NewRegistry()
	if err := registry.Register(clock); err != nil {
		fail(err)
	}

	runner, err := agent.New(agent.Config{
		Key:       "example.openai-compat",
		ModelName: modelName,
		MaxSteps:  6,
	}, openaicompat.New(baseURL, apiKey), registry)
	if err != nil {
		fail(err)
	}

	// Print streamed text deltas as they arrive.
	//
	// Lossless because this text is what the reader sees: the default emitter
	// drops observations when the consumer falls behind the model, which would
	// print a truncated answer while RunResult.Text stayed complete.
	emitter := agent.NewObservationEmitterWith(agent.ObservationOptions{QueueSize: 64, Lossless: true}, func(observation agent.Observation) {
		switch observation.Type {
		case agent.ObservationTextDelta:
			fmt.Print(observation.Text)
		case agent.ObservationToolCall:
			fmt.Printf("\n[tool call] %s %s\n", observation.ToolCall.Name, observation.ToolCall.Input)
		}
	})

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("You are a concise assistant. Use tools when they help."),
			agent.NewUserMessage("What is the current date and time in Shanghai?"),
		},
		ObservationEmitter: emitter,
	})
	emitter.Close()
	if err != nil {
		fail(err)
	}

	fmt.Printf("\n\n[%s] stop=%s steps=%d tokens=%d\n",
		result.ModelName, result.StopReason, len(result.Steps), result.Usage.TotalTokens)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
