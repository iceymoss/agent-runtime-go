// Package agenttest provides reusable conformance tests for agent adapters.
package agenttest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// ModelCase identifies an upstream behavior supplied by an adapter fixture.
type ModelCase string

const (
	ModelCaseValidStream     ModelCase = "valid_stream"
	ModelCaseMissingTerminal ModelCase = "missing_terminal"
	ModelCaseAfterTerminal   ModelCase = "after_terminal"
	ModelCaseInvalidUsage    ModelCase = "invalid_usage"
	ModelCaseRejected        ModelCase = "rejected"
	ModelCaseAuth            ModelCase = "auth"
	ModelCaseRateLimit       ModelCase = "rate_limit"
	ModelCaseTransport       ModelCase = "transport"
	ModelCaseProviderDetail  ModelCase = "provider_detail"
	ModelCaseCancellation    ModelCase = "cancellation"
)

// ModelFactory constructs an adapter backed by one provider fixture.
type ModelFactory func(t *testing.T, testCase ModelCase) agent.Model

// TestModel runs the provider-neutral model conformance suite.
func TestModel(t *testing.T, factory ModelFactory) {
	t.Helper()
	t.Run("valid stream", func(t *testing.T) {
		model := factory(t, ModelCaseValidStream)
		if model.Name() == "" {
			t.Fatal("Model.Name() is empty")
		}
		caps := model.Capabilities()
		if err := caps.Validate(); err != nil || !caps.Tools || !caps.ToolChoiceNone || !caps.ToolChoiceRequired || !caps.ToolChoiceNamed || !caps.UsageDetails {
			t.Fatalf("Capabilities() = %+v, %v", caps, err)
		}
		chunks, err := openStream(t, model, context.Background())
		if err != nil {
			t.Fatalf("Stream() error = %v", err)
		}
		var text strings.Builder
		var calls []agent.ToolCall
		var response *agent.Response
		for _, chunk := range chunks {
			switch chunk.Type {
			case agent.ChunkText:
				text.WriteString(chunk.TextDelta)
			case agent.ChunkToolCall:
				if chunk.ToolCall == nil {
					t.Fatal("tool chunk has nil call")
				}
				calls = append(calls, *chunk.ToolCall)
			case agent.ChunkFinish:
				if response != nil || chunk.Response == nil {
					t.Fatalf("invalid terminal chunk: %#v", chunk)
				}
				response = chunk.Response
			case agent.ChunkError:
				t.Fatalf("unexpected error chunk: %v", chunk.Err)
			default:
				t.Fatalf("unknown chunk type %q", chunk.Type)
			}
		}
		wantCalls := []agent.ToolCall{{ID: "call_a", Name: "first", Input: `{"a":0}`}, {ID: "call_b", Name: "second", Input: `{"b":1}`}}
		wantUsage := agent.Usage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 11, CacheReadTokens: 2}
		if response == nil || text.String() != "hello world" || !equalCalls(calls, wantCalls) {
			t.Fatalf("stream = text %q, calls %#v, response %#v", text.String(), calls, response)
		}
		if err := agent.ValidateResponse(response); err != nil || response.Usage != wantUsage || response.Usage.Validate() != nil || response.FinishReason != agent.FinishToolCalls || response.ModelName == "" || response.Message.Text() != text.String() || !equalCalls(response.ToolCalls(), calls) {
			t.Fatalf("response = %#v, validation = %v", response, err)
		}
	})

	for _, testCase := range []ModelCase{ModelCaseMissingTerminal, ModelCaseAfterTerminal, ModelCaseInvalidUsage} {
		t.Run(string(testCase), func(t *testing.T) {
			err := streamFailure(t, factory(t, testCase), context.Background())
			assertModelError(t, err, agent.ModelErrorKindProtocol, false, 0, 0)
		})
	}

	for _, tt := range []struct {
		testCase   ModelCase
		kind       agent.ModelErrorKind
		retryable  bool
		status     int
		retryAfter time.Duration
	}{
		{testCase: ModelCaseRejected, kind: agent.ModelErrorKindRejected, status: 400},
		{testCase: ModelCaseAuth, kind: agent.ModelErrorKindAuth, status: 401},
		{testCase: ModelCaseRateLimit, kind: agent.ModelErrorKindRateLimit, retryable: true, status: 429, retryAfter: 2 * time.Second},
		{testCase: ModelCaseTransport, kind: agent.ModelErrorKindTransport, retryable: true, status: 503},
	} {
		t.Run(string(tt.testCase), func(t *testing.T) {
			err := streamFailure(t, factory(t, tt.testCase), context.Background())
			modelErr := assertModelError(t, err, tt.kind, tt.retryable, tt.status, tt.retryAfter)
			if modelErr.Cause == nil {
				t.Fatalf("HTTP error has nil cause: %+v", modelErr)
			}
		})
	}

	t.Run("provider detail", func(t *testing.T) {
		err := streamFailure(t, factory(t, ModelCaseProviderDetail), context.Background())
		modelErr := assertModelError(t, err, agent.ModelErrorKindRejected, false, 400, 0)
		if !strings.Contains(modelErr.SafeDetail, "safe-provider-detail") || !strings.Contains(err.Error(), "safe-provider-detail") || strings.Contains(err.Error(), "unsafe-cause-marker") || modelErr.Cause == nil {
			t.Fatalf("provider detail error = %v, typed = %+v", err, modelErr)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		model := factory(t, ModelCaseCancellation)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := model.Stream(ctx, request())
			done <- err
		}()
		time.Sleep(10 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Stream() error = %v", err)
			}
			var modelErr *agent.ModelError
			if errors.As(err, &modelErr) {
				t.Fatalf("cancellation classified as ModelError: %+v", modelErr)
			}
		case <-time.After(time.Second):
			t.Fatal("Stream() did not return after cancellation")
		}
	})
}

func request() *agent.GenerateRequest {
	return &agent.GenerateRequest{
		Model:    "conformance-model",
		Messages: []agent.Message{agent.NewUserMessage("test")},
		Tools: []agent.ToolDefinition{
			{Name: "first", Parameters: map[string]any{"type": "object"}},
			{Name: "second", Parameters: map[string]any{"type": "object"}},
		},
	}
}

func openStream(t *testing.T, model agent.Model, ctx context.Context) ([]agent.StreamChunk, error) {
	t.Helper()
	stream, err := model.Stream(ctx, request())
	if err != nil {
		return nil, err
	}
	done := make(chan []agent.StreamChunk, 1)
	go func() {
		var chunks []agent.StreamChunk
		for chunk := range stream {
			chunks = append(chunks, chunk)
		}
		done <- chunks
	}()
	select {
	case chunks := <-done:
		return chunks, nil
	case <-time.After(time.Second):
		t.Fatal("model stream did not close")
		return nil, nil
	}
}

func streamFailure(t *testing.T, model agent.Model, ctx context.Context) error {
	t.Helper()
	chunks, err := openStream(t, model, ctx)
	if err != nil {
		return err
	}
	var failure error
	for _, chunk := range chunks {
		if chunk.Type == agent.ChunkFinish {
			t.Fatalf("invalid stream emitted finish: %#v", chunks)
		}
		if chunk.Type == agent.ChunkError {
			if failure != nil {
				t.Fatalf("stream emitted multiple errors: %#v", chunks)
			}
			failure = chunk.Err
		}
	}
	if failure == nil {
		t.Fatalf("stream did not report failure: %#v", chunks)
	}
	return failure
}

func assertModelError(t *testing.T, err error, kind agent.ModelErrorKind, retryable bool, status int, retryAfter time.Duration) *agent.ModelError {
	t.Helper()
	var modelErr *agent.ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != kind || modelErr.Retryable != retryable || modelErr.HTTPStatus != status || modelErr.RetryAfter != retryAfter {
		t.Fatalf("error = %v, typed = %+v", err, modelErr)
	}
	return modelErr
}

func equalCalls(a, b []agent.ToolCall) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
