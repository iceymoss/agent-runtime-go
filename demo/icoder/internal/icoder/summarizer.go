package icoder

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
	agentcontext "github.com/iceymoss/agent-runtime-go/context"
)

type modelSummarizer struct {
	generator agent.Generator
	model     string
}

func (s *modelSummarizer) Summarize(ctx context.Context, request agentcontext.SummaryRequest) (agentcontext.SummaryResult, error) {
	history, err := marshalString(request.Messages)
	if err != nil {
		return agentcontext.SummaryResult{}, err
	}
	maxTokens, temperature := 1024, 0.0
	response, err := s.generator.Generate(ctx, &agent.GenerateRequest{Model: s.model, Messages: []agent.Message{
		agent.NewSystemMessage("Summarize the coding conversation as concise durable context. Preserve user goals, decisions, changed files, commands and results, unresolved work, and important constraints. Do not include tool calls. Return plain text only."),
		agent.NewUserMessage(history),
	}, MaxTokens: &maxTokens, Temperature: &temperature})
	if err != nil {
		return agentcontext.SummaryResult{}, err
	}
	text := response.Message.Text()
	if text == "" {
		return agentcontext.SummaryResult{}, fmt.Errorf("summarizer returned empty text")
	}
	return agentcontext.SummaryResult{Messages: []agent.Message{agent.NewAssistantMessage(text)}, Generation: "icoder-summary/" + s.model + "/v1"}, nil
}
