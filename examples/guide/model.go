package main

import (
	"context"
	"os"
	"strings"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
	"github.com/iceymoss/agent-runtime-go/providers/retry"
)

// NewModel returns the model this application talks to.
//
// Credentials come from the application, never from the library: the runtime
// reads no environment variables, so this function is the one place that knows
// where the key lives. With no key configured it falls back to a scripted model
// so the example stays runnable.
func NewModel() (model agent.Model, name string) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return &scriptedModel{}, "scripted-v1"
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = "gpt-4o-mini"
	}
	// retry wraps the adapter rather than the other way round: it needs the
	// adapter's error classification to decide what is worth repeating.
	return retry.New(openaicompat.New(baseURL, key), retry.Options{}), modelName
}

// scriptedModel replays a fixed conversation so the example runs with no API
// key. It is also the shape of the smallest possible Model implementation.
type scriptedModel struct{ calls int }

func (*scriptedModel) Name() string { return "scripted" }
func (*scriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true, StructuredOutput: true}
}

func (m *scriptedModel) Stream(_ context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	m.calls++
	// Only this turn matters. History also holds earlier turns, including their
	// tool results, and treating those as "already called" would answer the
	// wrong question — a mistake worth seeing in a fake as well as a real one.
	turn := request.Messages
	for i := len(turn) - 1; i >= 0; i-- {
		if turn[i].Role == agent.RoleUser {
			turn = turn[i:]
			break
		}
	}
	question := turn[0].Text()
	answered := false
	for _, message := range turn[1:] {
		answered = answered || message.Role == agent.RoleTool
	}

	switch {
	case answered && strings.Contains(question, "重启"):
		return say("已经重启 checkout，稍后确认恢复情况。"), nil
	case answered:
		return say("checkout 的支付网关连续超时，连接池也被打满了。建议重启 checkout。"), nil
	case strings.Contains(question, "重启"):
		return toolCall("call-restart", "restart_service",
			`{"service":"checkout","reason":"payment gateway timeouts"}`), nil
	case len(request.Tools) > 0:
		return toolCall("call-logs", "read_logs", `{"service":"checkout","level":"WARN"}`), nil
	}
	return say("我可以帮你查日志和重启服务。"), nil
}

func toolCall(id, name, input string) <-chan agent.StreamChunk {
	call := agent.ToolCall{ID: id, Name: name, Input: input}
	message := agent.Message{Role: agent.RoleAssistant, FinishReason: agent.FinishToolCalls,
		Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}}
	return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishToolCalls})
}

// say builds a complete response and hands it to StreamResponse, which emits the
// deltas and the terminal chunk consistently. An adapter that streams token by
// token emits its own deltas instead.
func say(text string) <-chan agent.StreamChunk {
	message := agent.NewAssistantMessage(text)
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{
		Message: message, FinishReason: agent.FinishStop, ModelName: "scripted-v1",
		Usage: agent.Usage{PromptTokens: 20, CompletionTokens: 12, TotalTokens: 32},
	})
}
