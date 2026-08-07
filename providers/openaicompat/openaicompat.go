// Package openaicompat provides an agent.Model adapter for OpenAI-compatible
// chat completion APIs, including OpenAI, DeepSeek, Qwen, Kimi, vLLM, and
// Ollama's OpenAI endpoint.
//
// The adapter speaks the /chat/completions wire protocol in streaming (SSE)
// mode by default, assembles upstream fragments into the canonical
// agent.StreamChunk contract, normalizes usage, and classifies failures as
// agent.ModelError. Providers with unreliable SSE support can opt into
// non-streaming requests with WithoutStreaming.
//
//	model := openaicompat.New("https://api.openai.com/v1", apiKey)
//	runner, err := agent.New(agent.Config{
//		Key:       "example.assistant",
//		ModelName: "gpt-4o-mini",
//		MaxSteps:  8,
//	}, model, registry)
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
)

// DefaultName is the provider identifier reported by Model.Name unless
// overridden with WithName.
const DefaultName = "openai-compatible"

const maxErrorBodyBytes = 64 << 10

// Model is an agent.Model backed by an OpenAI-compatible chat completions API.
// It is safe for concurrent use.
type Model struct {
	baseURL       string
	apiKey        string
	name          string
	client        *http.Client
	caps          agent.Capabilities
	headers       map[string]string
	nonStreaming  bool
	noStreamUsage bool
}

// Option customizes a Model.
type Option func(*Model)

// WithName overrides the provider identifier returned by Name.
func WithName(name string) Option {
	return func(m *Model) { m.name = name }
}

// WithHTTPClient replaces the default HTTP client. Use it to configure
// proxies, timeouts, or transport-level retries.
func WithHTTPClient(client *http.Client) Option {
	return func(m *Model) { m.client = client }
}

// WithCapabilities overrides the declared model capabilities. Use it when the
// upstream provider does not support every default capability (for example a
// provider without named tool choice).
func WithCapabilities(caps agent.Capabilities) Option {
	return func(m *Model) { m.caps = caps }
}

// WithHeader adds a custom header to every request, for example
// OpenRouter attribution headers or organization identifiers.
func WithHeader(key, value string) Option {
	return func(m *Model) {
		if m.headers == nil {
			m.headers = make(map[string]string)
		}
		m.headers[key] = value
	}
}

// WithoutStreaming makes the adapter issue non-streaming requests and
// synthesize the chunk stream from the complete response. Use it for
// providers with broken or unavailable SSE endpoints.
func WithoutStreaming() Option {
	return func(m *Model) { m.nonStreaming = true }
}

// WithoutStreamUsage omits stream_options.include_usage from streaming
// requests. Use it for providers that reject the stream_options field.
// Usage totals will be zero for streamed steps.
func WithoutStreamUsage() Option {
	return func(m *Model) { m.noStreamUsage = true }
}

// New builds a Model for an OpenAI-compatible endpoint. baseURL is the API
// root (for example "https://api.openai.com/v1"); the adapter appends
// /chat/completions. An empty apiKey omits the Authorization header, which
// suits local servers such as Ollama or vLLM.
func New(baseURL, apiKey string, opts ...Option) *Model {
	m := &Model{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		name:    DefaultName,
		client:  &http.Client{Timeout: 5 * time.Minute},
		caps: agent.Capabilities{
			Tools:              true,
			ToolChoiceNone:     true,
			ToolChoiceRequired: true,
			ToolChoiceNamed:    true,
		},
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Name returns the provider identifier.
func (m *Model) Name() string { return m.name }

// Capabilities returns the declared provider capabilities.
func (m *Model) Capabilities() agent.Capabilities { return m.caps }

// Stream generates one step. In streaming mode it consumes SSE fragments and
// emits canonical chunks as they arrive; in non-streaming mode it synthesizes
// the chunk sequence from one complete response.
func (m *Model) Stream(ctx context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	if err := agent.ValidateGenerateRequestCapabilities(request, m.caps); err != nil {
		return nil, err
	}
	if m.nonStreaming {
		response, err := m.generate(ctx, request)
		if err != nil {
			return nil, err
		}
		return synthesizeChunks(response), nil
	}
	wire, err := projectChatRequest(request)
	if err != nil {
		return nil, err
	}
	wire.Stream = true
	if !m.noStreamUsage {
		wire.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	httpResponse, err := m.post(ctx, wire)
	if err != nil {
		return nil, err
	}
	contentType := strings.ToLower(httpResponse.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "application/json") {
		response, err := decodeChatResponse(httpResponse)
		if err != nil {
			return nil, err
		}
		return synthesizeChunks(response), nil
	}
	if contentType != "" && !strings.HasPrefix(contentType, "text/event-stream") {
		httpResponse.Body.Close()
		return nil, agent.NewModelError(agent.ModelErrorKindProtocol, false, httpResponse.StatusCode, 0,
			fmt.Sprintf("unexpected provider content type %q; expected text/event-stream or application/json (check the provider base URL)", httpResponse.Header.Get("Content-Type")), nil)
	}
	chunks := make(chan agent.StreamChunk)
	go m.consumeSSE(ctx, httpResponse.Body, chunks)
	return chunks, nil
}

// Generate implements the optional agent.Generator interface with one
// non-streaming request.
func (m *Model) Generate(ctx context.Context, request *agent.GenerateRequest) (*agent.Response, error) {
	if err := agent.ValidateGenerateRequestCapabilities(request, m.caps); err != nil {
		return nil, err
	}
	return m.generate(ctx, request)
}

func (m *Model) generate(ctx context.Context, request *agent.GenerateRequest) (*agent.Response, error) {
	wire, err := projectChatRequest(request)
	if err != nil {
		return nil, err
	}
	httpResponse, err := m.post(ctx, wire)
	if err != nil {
		return nil, err
	}
	return decodeChatResponse(httpResponse)
}

func decodeChatResponse(httpResponse *http.Response) (*agent.Response, error) {
	defer httpResponse.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(httpResponse.Body, 32<<20))
	if err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindTransport, true, httpResponse.StatusCode, 0, "read provider response", err)
	}
	var decoded chatResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindProtocol, false, httpResponse.StatusCode, 0, "invalid provider response", err)
	}
	response, err := projectChatResponse(decoded)
	if err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindProtocol, false, httpResponse.StatusCode, 0, "invalid provider response", err)
	}
	return response, nil
}

func (m *Model) post(ctx context.Context, wire chatRequest) (*http.Response, error) {
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal provider request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create provider request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if m.apiKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	for key, value := range m.headers {
		httpRequest.Header.Set(key, value)
	}
	httpResponse, err := m.client.Do(httpRequest)
	if err != nil {
		return nil, agent.NewModelError(agent.ModelErrorKindTransport, true, 0, 0, "provider request failed", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		defer httpResponse.Body.Close()
		snippet, _ := io.ReadAll(io.LimitReader(httpResponse.Body, maxErrorBodyBytes))
		return nil, classifyStatus(httpResponse.StatusCode, snippet)
	}
	return httpResponse, nil
}

func classifyStatus(status int, body []byte) error {
	kind, retryable := agent.ModelErrorKindRejected, false
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		kind = agent.ModelErrorKindAuth
	case status == http.StatusTooManyRequests:
		kind, retryable = agent.ModelErrorKindRateLimit, true
	case status >= 500:
		kind, retryable = agent.ModelErrorKindTransport, true
	}
	cause := fmt.Errorf("provider status %d: %s", status, strings.TrimSpace(string(body)))
	return agent.NewModelError(kind, retryable, status, 0, "provider rejected request", cause)
}

// pendingToolCall accumulates streamed tool call fragments for one index.
type pendingToolCall struct {
	id   string
	name strings.Builder
	args strings.Builder
}

func (m *Model) consumeSSE(ctx context.Context, body io.ReadCloser, chunks chan<- agent.StreamChunk) {
	defer close(chunks)
	defer body.Close()

	emit := func(chunk agent.StreamChunk) bool {
		select {
		case chunks <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}
	fail := func(err error) {
		emit(agent.StreamChunk{Type: agent.ChunkError, Err: err})
	}

	var (
		text      strings.Builder
		pending   = make(map[int]*pendingToolCall)
		finish    string
		modelName string
		usage     *wireUsage
	)

	reader := bufio.NewReader(body)
	for {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				break
			}
			if payload != "" {
				var chunk wireStreamChunk
				if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
					fail(agent.NewModelError(agent.ModelErrorKindProtocol, false, 0, 0, "invalid stream chunk", err))
					return
				}
				if chunk.Error != nil {
					fail(agent.NewModelError(agent.ModelErrorKindRejected, false, 0, 0, "provider stream error", fmt.Errorf("%s", chunk.Error.Message)))
					return
				}
				if chunk.Model != "" {
					modelName = chunk.Model
				}
				if chunk.Usage != nil {
					usage = chunk.Usage
				}
				for _, choice := range chunk.Choices {
					if choice.Delta.Content != "" {
						text.WriteString(choice.Delta.Content)
						if !emit(agent.StreamChunk{Type: agent.ChunkText, TextDelta: choice.Delta.Content}) {
							return
						}
					}
					for _, fragment := range choice.Delta.ToolCalls {
						call, ok := pending[fragment.Index]
						if !ok {
							call = &pendingToolCall{}
							pending[fragment.Index] = call
						}
						if fragment.ID != "" {
							call.id = fragment.ID
						}
						call.name.WriteString(fragment.Function.Name)
						call.args.WriteString(fragment.Function.Arguments)
					}
					if choice.FinishReason != "" {
						finish = choice.FinishReason
					}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			fail(agent.NewModelError(agent.ModelErrorKindTransport, true, 0, 0, "provider stream failed", readErr))
			return
		}
	}

	calls := assembleToolCalls(pending)
	if text.Len() == 0 && len(calls) == 0 {
		fail(agent.NewModelError(agent.ModelErrorKindProtocol, false, 0, 0, "stream contained no content", nil))
		return
	}

	reason := mapFinishReason(finish, len(calls) > 0)
	message := agent.Message{Role: agent.RoleAssistant, FinishReason: reason}
	if text.Len() > 0 {
		message.Parts = append(message.Parts, agent.ContentPart{Type: agent.PartText, Text: text.String()})
	}
	for i := range calls {
		call := calls[i]
		message.Parts = append(message.Parts, agent.ContentPart{Type: agent.PartToolCall, ToolCall: &call})
		if !emit(agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}) {
			return
		}
	}
	emit(agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
		Message:      message,
		FinishReason: reason,
		ModelName:    modelName,
		Usage:        normalizeUsage(usage),
	}})
}

func assembleToolCalls(pending map[int]*pendingToolCall) []agent.ToolCall {
	if len(pending) == 0 {
		return nil
	}
	indexes := make([]int, 0, len(pending))
	for index := range pending {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	calls := make([]agent.ToolCall, 0, len(indexes))
	for _, index := range indexes {
		call := pending[index]
		calls = append(calls, agent.ToolCall{ID: call.id, Name: call.name.String(), Input: call.args.String()})
	}
	return calls
}

func mapFinishReason(finish string, hasToolCalls bool) agent.FinishReason {
	if hasToolCalls {
		return agent.FinishToolCalls
	}
	switch finish {
	case "length":
		return agent.FinishLength
	default:
		return agent.FinishStop
	}
}

func synthesizeChunks(response *agent.Response) <-chan agent.StreamChunk {
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
	return chunks
}

// --- wire protocol ---

type chatRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []chatTool     `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	Stream        bool           `json:"stream,omitempty"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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
	Strict      bool           `json:"strict,omitempty"`
	Arguments   string         `json:"arguments,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type wireUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type wireError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *wireError `json:"error"`
}

type wireStreamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *wireError `json:"error"`
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
		for _, part := range message.Parts {
			if part.Type == agent.PartImage {
				return chatRequest{}, agent.NewModelError(agent.ModelErrorKindUnsupported, false, 0, 0, "image input is not supported by the openaicompat adapter", nil)
			}
		}
		projected := chatMessage{Role: string(message.Role), Content: message.Text()}
		for _, call := range message.ToolCalls() {
			projected.ToolCalls = append(projected.ToolCalls, chatToolCall{ID: call.ID, Type: "function", Function: chatFunction{Name: call.Name, Arguments: call.Input}})
		}
		wire.Messages = append(wire.Messages, projected)
	}
	for _, definition := range request.Tools {
		wire.Tools = append(wire.Tools, chatTool{Type: "function", Function: chatFunction{Name: definition.Name, Description: definition.Description, Parameters: definition.Parameters, Strict: definition.Strict}})
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case agent.ToolChoiceAuto, agent.ToolChoiceNone, agent.ToolChoiceRequired:
			wire.ToolChoice = string(request.ToolChoice.Mode)
		case agent.ToolChoiceNamed:
			wire.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": request.ToolChoice.Name}}
		default:
			return chatRequest{}, fmt.Errorf("unsupported tool choice %q", request.ToolChoice.Mode)
		}
	}
	return wire, nil
}

func projectChatResponse(response chatResponse) (*agent.Response, error) {
	if response.Error != nil {
		return nil, fmt.Errorf("provider error: %s", response.Error.Message)
	}
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
	reason := mapFinishReason(choice.FinishReason, len(choice.Message.ToolCalls) > 0)
	message.FinishReason = reason
	result := &agent.Response{Message: message, FinishReason: reason, ModelName: response.Model, Usage: normalizeUsage(response.Usage)}
	if err := agent.ValidateResponse(result); err != nil {
		return nil, err
	}
	if err := result.Usage.Validate(); err != nil {
		return nil, err
	}
	return result, nil
}

// normalizeUsage converts wire usage to the canonical contract: PromptTokens
// excludes cached tokens, while TotalTokens covers every component.
func normalizeUsage(wire *wireUsage) agent.Usage {
	if wire == nil {
		return agent.Usage{}
	}
	usage := agent.Usage{
		PromptTokens:     wire.PromptTokens,
		CompletionTokens: wire.CompletionTokens,
		TotalTokens:      wire.TotalTokens,
	}
	if cached := wire.PromptTokensDetails.CachedTokens; cached > 0 && cached <= usage.PromptTokens {
		usage.PromptTokens -= cached
		usage.CacheReadTokens = cached
	}
	if reasoning := wire.CompletionTokensDetails.ReasoningTokens; reasoning > 0 && reasoning <= usage.CompletionTokens {
		usage.ReasoningTokens = reasoning
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens + usage.CacheCreationTokens + usage.CacheReadTokens
	}
	return usage
}
