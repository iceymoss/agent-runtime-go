package openaicompat_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
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

func (weatherTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	return agent.ToolResult{Content: `{"city":"Hangzhou","condition":"sunny","temperature_c":28}`}, nil
}

func writeSSE(t *testing.T, w http.ResponseWriter, events ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
			t.Fatalf("write SSE event: %v", err)
		}
	}
}

func requestHasToolMessage(t *testing.T, body []byte) bool {
	t.Helper()
	var decoded struct {
		Stream   bool `json:"stream"`
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if !decoded.Stream {
		t.Fatalf("expected stream:true request, got %s", body)
	}
	if len(decoded.Tools) != 1 || decoded.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("expected get_weather tool definition, got %s", body)
	}
	for _, message := range decoded.Messages {
		if message.Role == "tool" {
			return true
		}
	}
	return false
}

func TestStreamingToolLoop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("unexpected authorization header %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if !requestHasToolMessage(t, body) {
			// First step: the model requests a tool call, with the arguments
			// split across two fragments.
			writeSSE(t, w,
				`{"model":"fake-1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Hangzhou\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":40}}}`,
				`[DONE]`,
			)
			return
		}
		// Second step: the model answers with streamed text.
		writeSSE(t, w,
			`{"model":"fake-1","choices":[{"delta":{"content":"Hangzhou is "},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"sunny."},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":8,"total_tokens":128}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()

	registry := agent.NewRegistry()
	if err := registry.Register(weatherTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	model := openaicompat.New(server.URL, "test-key")
	runner, err := agent.New(agent.Config{
		Key:       "test.weather",
		ModelName: "fake-1",
		MaxSteps:  4,
	}, model, registry)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{
		agent.NewSystemMessage("Use tools when needed."),
		agent.NewUserMessage("What is the weather in Hangzhou?"),
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "Hangzhou is sunny." {
		t.Errorf("unexpected text %q", result.Text)
	}
	if result.StopReason != agent.StopReasonComplete {
		t.Errorf("unexpected stop reason %q", result.StopReason)
	}
	if result.ModelName != "fake-1" {
		t.Errorf("unexpected model name %q", result.ModelName)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(result.Steps))
	}
	if calls := result.Steps[0].ToolCalls; len(calls) != 1 || calls[0].Name != "get_weather" || calls[0].Input != `{"city":"Hangzhou"}` {
		t.Errorf("unexpected tool calls %+v", calls)
	}
	// Usage is normalized: cached tokens move out of PromptTokens into
	// CacheReadTokens while totals still add up.
	if result.Usage.TotalTokens != 238 {
		t.Errorf("unexpected total tokens %d", result.Usage.TotalTokens)
	}
	if result.Usage.CacheReadTokens != 40 {
		t.Errorf("unexpected cache read tokens %d", result.Usage.CacheReadTokens)
	}
	if result.Usage.PromptTokens != 180 {
		t.Errorf("unexpected prompt tokens %d", result.Usage.PromptTokens)
	}
}

func TestNonStreamingMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if strings.Contains(string(body), `"stream":true`) {
			t.Errorf("expected non-streaming request, got %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"fake-1","choices":[{"message":{"role":"assistant","content":"Hello."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer server.Close()

	model := openaicompat.New(server.URL, "", openaicompat.WithoutStreaming())
	runner, err := agent.New(agent.Config{
		Key:       "test.hello",
		ModelName: "fake-1",
		MaxSteps:  4,
	}, model, agent.NewRegistry())
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{
		agent.NewUserMessage("Say hello."),
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "Hello." {
		t.Errorf("unexpected text %q", result.Text)
	}
	if result.Usage.TotalTokens != 12 {
		t.Errorf("unexpected total tokens %d", result.Usage.TotalTokens)
	}
}

func TestStreamingModeAcceptsJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("expected streaming request, got %s", body)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = io.WriteString(w, `{"model":"fake-1","choices":[{"message":{"role":"assistant","content":"Hello."},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer server.Close()

	model := openaicompat.New(server.URL, "")
	runner, err := agent.New(agent.Config{
		Key:       "test.json-fallback",
		ModelName: "fake-1",
		MaxSteps:  4,
	}, model, agent.NewRegistry())
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{
		agent.NewUserMessage("Say hello."),
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Text != "Hello." {
		t.Errorf("unexpected text %q", result.Text)
	}
	if result.Usage.TotalTokens != 12 {
		t.Errorf("unexpected total tokens %d", result.Usage.TotalTokens)
	}
}

func TestStreamingModeRejectsUnexpectedContentType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><title>Provider home page</title>`)
	}))
	defer server.Close()

	model := openaicompat.New(server.URL, "")
	_, err := model.Stream(context.Background(), &agent.GenerateRequest{
		Model:    "fake-1",
		Messages: []agent.Message{agent.NewUserMessage("Say hello.")},
	})
	var modelErr *agent.ModelError
	if !errors.As(err, &modelErr) {
		t.Fatalf("expected ModelError, got %v", err)
	}
	if modelErr.Kind != agent.ModelErrorKindProtocol || !strings.Contains(modelErr.Error(), "check the provider base URL") {
		t.Fatalf("unexpected error: %v", modelErr)
	}
}

func TestStatusClassification(t *testing.T) {
	cases := []struct {
		status    int
		kind      agent.ModelErrorKind
		retryable bool
	}{
		{http.StatusUnauthorized, agent.ModelErrorKindAuth, false},
		{http.StatusTooManyRequests, agent.ModelErrorKindRateLimit, true},
		{http.StatusInternalServerError, agent.ModelErrorKindTransport, true},
		{http.StatusBadRequest, agent.ModelErrorKindRejected, false},
	}
	for _, testCase := range cases {
		t.Run(fmt.Sprintf("status_%d", testCase.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
			}))
			defer server.Close()

			model := openaicompat.New(server.URL, "key")
			_, err := model.Stream(context.Background(), &agent.GenerateRequest{
				Model:    "fake-1",
				Messages: []agent.Message{agent.NewUserMessage("hi")},
			})
			var modelErr *agent.ModelError
			if !errors.As(err, &modelErr) {
				t.Fatalf("expected ModelError, got %v", err)
			}
			if modelErr.Kind != testCase.kind {
				t.Errorf("expected kind %q, got %q", testCase.kind, modelErr.Kind)
			}
			if modelErr.Retryable != testCase.retryable {
				t.Errorf("expected retryable=%v, got %v", testCase.retryable, modelErr.Retryable)
			}
			if modelErr.HTTPStatus != testCase.status {
				t.Errorf("expected status %d, got %d", testCase.status, modelErr.HTTPStatus)
			}
		})
	}
}

func TestStreamErrorChunkFailsRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(t, w, `{"error":{"message":"model overloaded"}}`)
	}))
	defer server.Close()

	model := openaicompat.New(server.URL, "key")
	runner, err := agent.New(agent.Config{
		Key:       "test.error",
		ModelName: "fake-1",
		MaxSteps:  4,
	}, model, agent.NewRegistry())
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	_, err = runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{
		agent.NewUserMessage("hi"),
	}})
	var modelErr *agent.ModelError
	if !errors.As(err, &modelErr) {
		t.Fatalf("expected ModelError, got %v", err)
	}
	if modelErr.Kind != agent.ModelErrorKindRejected {
		t.Errorf("expected rejected kind, got %q", modelErr.Kind)
	}
}

// TestResponseFormatProjection pins how the portable constraint reaches an
// OpenAI-compatible endpoint, because an agent whose answer is parsed by code
// depends on the provider - not the runtime - actually enforcing it.
func TestResponseFormatProjection(t *testing.T) {
	tests := []struct {
		name       string
		format     *agent.ResponseFormat
		wantType   string
		wantSchema bool
		wantStrict bool
	}{
		{name: "unconstrained generation sends no field"},
		{name: "json mode", format: &agent.ResponseFormat{Kind: agent.ResponseFormatJSON}, wantType: "json_object"},
		{
			name: "json schema carries name, schema, and strict",
			format: &agent.ResponseFormat{
				Kind: agent.ResponseFormatJSONSchema, Name: "city",
				Schema: json.RawMessage(`{"type":"object"}`), Strict: true,
			},
			wantType: "json_schema", wantSchema: true, wantStrict: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var decoded struct {
				ResponseFormat *struct {
					Type       string `json:"type"`
					JSONSchema *struct {
						Name   string          `json:"name"`
						Schema json.RawMessage `json:"schema"`
						Strict bool            `json:"strict"`
					} `json:"json_schema"`
				} `json:"response_format"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatalf("read request: %v", err)
				}
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				writeSSE(t, w,
					`{"choices":[{"delta":{"content":"{}"},"finish_reason":null}]}`,
					`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
					`[DONE]`,
				)
			}))
			defer server.Close()

			model := openaicompat.New(server.URL, "key")
			runner, err := agent.New(agent.Config{
				Key: "extract", ModelName: "test-model", MaxSteps: 4, AllowedTools: []string{},
				ResponseFormat: test.format,
			}, model, agent.NewRegistry())
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if _, err := runner.Run(context.Background(), agent.RunRequest{
				Messages: []agent.Message{agent.NewUserMessage("extract")},
			}); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if test.wantType == "" {
				if decoded.ResponseFormat != nil {
					t.Fatalf("response_format = %+v, want absent", decoded.ResponseFormat)
				}
				return
			}
			if decoded.ResponseFormat == nil || decoded.ResponseFormat.Type != test.wantType {
				t.Fatalf("response_format = %+v, want type %q", decoded.ResponseFormat, test.wantType)
			}
			schema := decoded.ResponseFormat.JSONSchema
			if test.wantSchema {
				if schema == nil || schema.Name != "city" || string(schema.Schema) != `{"type":"object"}` || schema.Strict != test.wantStrict {
					t.Fatalf("json_schema = %+v", schema)
				}
			} else if schema != nil {
				t.Fatalf("json mode sent a schema: %+v", schema)
			}
		})
	}
}

// TestReasoningRoundTrip covers the two halves that make a reasoning model
// usable: the adapter surfaces the provider's thinking, and it never sends that
// thinking back - providers that emit reasoning reject it as assistant input.
func TestReasoningRoundTrip(t *testing.T) {
	var requests []struct {
		Messages []struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
		} `json:"messages"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		var decoded struct {
			Messages []struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requests = append(requests, decoded)
		if len(requests) == 1 {
			writeSSE(t, w,
				`{"choices":[{"delta":{"reasoning_content":"the city is in China"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"get_weather","arguments":"{\"city\":\"Hangzhou\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			)
			return
		}
		writeSSE(t, w,
			`{"choices":[{"delta":{"reasoning":"sunny and 28"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"It is sunny in Hangzhou."},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`[DONE]`,
		)
	}))
	defer server.Close()

	registry := agent.NewRegistry()
	if err := registry.Register(weatherTool{}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	runner, err := agent.New(agent.Config{Key: "weather", ModelName: "test-model", MaxSteps: 4},
		openaicompat.New(server.URL, "key"), registry)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("weather in Hangzhou?")},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "It is sunny in Hangzhou." {
		t.Fatalf("RunResult.Text = %q", result.Text)
	}
	// Both spellings are surfaced, and neither leaks into the answer.
	var reasoning []string
	for _, message := range result.Messages {
		if thinking := message.Reasoning(); thinking != "" {
			reasoning = append(reasoning, thinking)
		}
	}
	if len(reasoning) != 2 || reasoning[0] != "the city is in China" || reasoning[1] != "sunny and 28" {
		t.Fatalf("reasoning = %#v", reasoning)
	}
	// The second request replays the first turn; the thinking must not be in it.
	if len(requests) != 2 {
		t.Fatalf("provider was called %d times", len(requests))
	}
	for _, message := range requests[1].Messages {
		if message.ReasoningContent != "" || message.Reasoning != "" {
			t.Fatalf("the adapter sent reasoning back upstream: %+v", message)
		}
		if strings.Contains(message.Content, "the city is in China") {
			t.Fatalf("reasoning leaked into assistant content: %+v", message)
		}
	}
}
