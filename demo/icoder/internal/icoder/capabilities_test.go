package icoder

import (
	"context"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

type reviewModel struct{}

func (reviewModel) Name() string                     { return "fixture" }
func (reviewModel) Capabilities() agent.Capabilities { return agent.Capabilities{Tools: true} }
func (reviewModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	stream := make(chan agent.StreamChunk, 2)
	stream <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: "finding: main.go:10 is incorrect"}
	stream <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{Message: agent.NewAssistantMessage("finding: main.go:10 is incorrect"), FinishReason: agent.FinishStop, Usage: agent.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}}
	close(stream)
	return stream, nil
}

func TestReviewRunnerExecutesRealChildAgent(t *testing.T) {
	registry := agent.NewRegistry()
	reviewAgent, err := agent.New(agent.Config{Key: "review", ModelName: "fixture", MaxSteps: 4, AllowedTools: []string{}}, reviewModel{}, registry)
	if err != nil {
		t.Fatal(err)
	}
	runner := &reviewRunner{agent: reviewAgent, results: make(map[subagent.ResultRef]string)}
	result, err := runner.Run(context.Background(), subagent.RunRequest{Input: []byte("review")})
	if err != nil || result.State != subagent.ChildCompleted || result.Usage.InputTokens != 2 || result.Usage.OutputTokens != 3 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	review, ok := runner.Result(result.ResultRef)
	if !ok || review != "finding: main.go:10 is incorrect" {
		t.Fatalf("Result() = %q, %t", review, ok)
	}
}
