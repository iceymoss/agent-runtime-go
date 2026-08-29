package openaicompat_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
)

// TestModelConformance holds the library's own adapter to the suite it tells
// third-party adapters to run.
//
// An adapter and the suite that describes adapters drift apart unless one is
// checked against the other, and the reference implementation is exactly where
// that drift is least visible and most costly: it is what everyone copies.
func TestModelConformance(t *testing.T) {
	agenttest.TestModel(t, func(t *testing.T, testCase agenttest.ModelCase) agent.Model {
		server := httptest.NewServer(fixtureHandler(t, testCase))
		t.Cleanup(server.Close)
		return openaicompat.New(server.URL, "conformance-key")
	})
}

// fixtureHandler serves one upstream behavior per conformance case.
func fixtureHandler(t *testing.T, testCase agenttest.ModelCase) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch testCase {
		case agenttest.ModelCaseValidStream:
			// Cached prompt tokens are reported inside prompt_tokens upstream, so
			// the normalized split is prompt 5 + cache 2 + completion 4 = 11.
			streamSSE(t, w,
				`{"model":"conformance-model","choices":[{"delta":{"content":"hello "},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"content":"world"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"first","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"0}"}},{"index":1,"id":"call_b","function":{"name":"second","arguments":"{\"b\":1}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":2}}}`,
				`[DONE]`,
			)
		case agenttest.ModelCaseMissingTerminal:
			// A stream that closes without ever producing content or a tool call
			// has no terminal response to report.
			streamSSE(t, w, `{"choices":[{"delta":{},"finish_reason":null}]}`, `[DONE]`)
		case agenttest.ModelCaseAfterTerminal:
			// The model said it was finished and then kept talking.
			streamSSE(t, w,
				`{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`,
				`{"choices":[{"delta":{"content":" and more"},"finish_reason":null}]}`,
				`[DONE]`,
			)
		case agenttest.ModelCaseInvalidUsage:
			streamSSE(t, w,
				`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":3}}`,
				`[DONE]`,
			)
		case agenttest.ModelCaseRejected:
			writeStatus(t, w, http.StatusBadRequest, `{"error":{"message":"bad request","type":"invalid_request_error"}}`)
		case agenttest.ModelCaseAuth:
			writeStatus(t, w, http.StatusUnauthorized, `{"error":{"message":"invalid api key","type":"invalid_request_error"}}`)
		case agenttest.ModelCaseRateLimit:
			w.Header().Set("Retry-After", "2")
			writeStatus(t, w, http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
		case agenttest.ModelCaseTransport:
			writeStatus(t, w, http.StatusServiceUnavailable, `{"error":{"message":"upstream unavailable"}}`)
		case agenttest.ModelCaseProviderDetail:
			// The provider's own message must reach SafeDetail; nothing from the
			// transport layer may leak into it.
			writeStatus(t, w, http.StatusBadRequest, `{"error":{"message":"safe-provider-detail","type":"invalid_request_error"}}`)
		case agenttest.ModelCaseCancellation:
			// Send nothing at all, so the request is still in flight when the
			// caller cancels and the cancellation surfaces from Stream itself.
			// The caller cancels well before this fallback; it only keeps a
			// misbehaving run from hanging the suite.
			select {
			case <-r.Context().Done():
			case <-time.After(300 * time.Millisecond):
			}
		default:
			t.Fatalf("unhandled conformance case %q", testCase)
		}
	})
}

func streamSSE(t *testing.T, w http.ResponseWriter, events ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", event); err != nil {
			t.Errorf("write SSE event: %v", err)
			return
		}
		flush(w)
	}
}

func writeStatus(t *testing.T, w http.ResponseWriter, status int, body string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil {
		t.Errorf("write error body: %v", err)
	}
}

func flush(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
