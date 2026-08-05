package icoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type OpenAIModel struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

func NewOpenAIModel(apiKey, baseURL string) *OpenAIModel {
	return &OpenAIModel{apiKey: apiKey, baseURL: strings.TrimRight(baseURL, "/"), client: &http.Client{Timeout: 2 * time.Minute}}
}

func (m *OpenAIModel) Name() string { return "openai-compatible" }

func (m *OpenAIModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true, ToolChoiceNone: true, ToolChoiceRequired: true, ToolChoiceNamed: true}
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Tools       []chatTool    `json:"tools,omitempty"`
	ToolChoice  any           `json:"tool_choice,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	TopP        *float64      `json:"top_p,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
	Arguments   string         `json:"arguments,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

func (m *OpenAIModel) Stream(ctx context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	if err := agent.ValidateGenerateRequestCapabilities(request, m.Capabilities()); err != nil {
		return nil, err
	}
	wire, err := projectChatRequest(request)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal provider request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create provider request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+m.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpResponse, err := m.client.Do(httpRequest)
	if err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindTransport, true, 0, 0, "provider request failed", err)
	}
	defer httpResponse.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, 8<<20))
	if err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindTransport, true, httpResponse.StatusCode, 0, "read provider response", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		kind, retryable := agent.ModelErrorKindRejected, false
		if httpResponse.StatusCode == http.StatusUnauthorized || httpResponse.StatusCode == http.StatusForbidden {
			kind = agent.ModelErrorKindAuth
		} else if httpResponse.StatusCode == http.StatusTooManyRequests {
			kind, retryable = agent.ModelErrorKindRateLimit, true
		} else if httpResponse.StatusCode >= 500 {
			kind, retryable = agent.ModelErrorKindTransport, true
		}
		return nil, agent.NewModelError(kind, retryable, httpResponse.StatusCode, 0, "provider rejected request", fmt.Errorf("provider status %d", httpResponse.StatusCode))
	}
	var decoded chatResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindProtocol, false, httpResponse.StatusCode, 0, "invalid provider response", err)
	}
	response, err := projectChatResponse(decoded)
	if err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindProtocol, false, httpResponse.StatusCode, 0, "invalid provider response", err)
	}
	chunks := make(chan agent.StreamChunk, len(response.Message.Parts)+1)
	for _, part := range response.Message.Parts {
		switch part.Type {
		case agent.PartText:
			chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: part.Text}
		case agent.PartToolCall:
			call := *part.ToolCall
			chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
		}
	}
	chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: response}
	close(chunks)
	return chunks, nil
}

func projectChatRequest(request *agent.GenerateRequest) (chatRequest, error) {
	wire := chatRequest{Model: request.Model, Temperature: request.Temperature, MaxTokens: request.MaxTokens, TopP: request.TopP}
	for _, message := range request.Messages {
		if message.Role == agent.RoleTool {
			for _, result := range message.ToolResults() {
				wire.Messages = append(wire.Messages, chatMessage{Role: "tool", Content: result.Content, ToolCallID: result.ToolCallID, Name: result.Name})
			}
			continue
		}
		projected := chatMessage{Role: string(message.Role), Content: message.Text()}
		for _, call := range message.ToolCalls() {
			projected.ToolCalls = append(projected.ToolCalls, chatToolCall{ID: call.ID, Type: "function", Function: chatFunction{Name: call.Name, Arguments: call.Input}})
		}
		wire.Messages = append(wire.Messages, projected)
	}
	for _, definition := range request.Tools {
		wire.Tools = append(wire.Tools, chatTool{Type: "function", Function: chatFunction{Name: definition.Name, Description: definition.Description, Parameters: definition.Parameters}})
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case agent.ToolChoiceAuto, agent.ToolChoiceNone, agent.ToolChoiceRequired:
			wire.ToolChoice = request.ToolChoice.Mode
		case agent.ToolChoiceNamed:
			wire.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": request.ToolChoice.Name}}
		default:
			return chatRequest{}, fmt.Errorf("unsupported tool choice %q", request.ToolChoice.Mode)
		}
	}
	return wire, nil
}

func projectChatResponse(response chatResponse) (*agent.Response, error) {
	if len(response.Choices) != 1 {
		return nil, fmt.Errorf("expected one choice, got %d", len(response.Choices))
	}
	choice := response.Choices[0]
	message := agent.Message{Role: agent.RoleAssistant}
	if choice.Message.Content != "" {
		message.Parts = append(message.Parts, agent.ContentPart{Type: agent.PartText, Text: choice.Message.Content})
	}
	for _, upstream := range choice.Message.ToolCalls {
		call := agent.ToolCall{ID: upstream.ID, Name: upstream.Function.Name, Input: upstream.Function.Arguments}
		message.Parts = append(message.Parts, agent.ContentPart{Type: agent.PartToolCall, ToolCall: &call})
	}
	reason := agent.FinishReason(choice.FinishReason)
	message.FinishReason = reason
	result := &agent.Response{Message: message, FinishReason: reason, ModelName: response.Model, Usage: agent.Usage{PromptTokens: response.Usage.PromptTokens, CompletionTokens: response.Usage.CompletionTokens, TotalTokens: response.Usage.TotalTokens}}
	if err := agent.ValidateResponse(result); err != nil {
		return nil, err
	}
	if err := result.Usage.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}
