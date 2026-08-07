package icoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
)

// wireChatRequest decodes the provider requests captured by the fixture server.
type wireChatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func writeSSEResponse(t *testing.T, response http.ResponseWriter, events ...string) {
	t.Helper()
	response.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		if _, err := fmt.Fprintf(response, "data: %s\n\n", event); err != nil {
			t.Error(err)
		}
	}
}

func TestAppRunsToolLoopAndCommitsTurn(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "go.mod"), []byte("module example.com/icoder-test\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu       sync.Mutex
		requests []wireChatRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		var decoded wireChatRequest
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Error(err)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, decoded)
		call := len(requests)
		mu.Unlock()
		if call == 1 {
			writeSSEResponse(t, response,
				`{"model":"fixture","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-read","function":{"name":"read_file","arguments":"{\"path\":\"go.mod\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
				`[DONE]`,
			)
			return
		}
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"The module is example.com/icoder-test."},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()

	app, err := NewApp(context.Background(), Config{
		APIKey:    "fixture-key",
		BaseURL:   server.URL,
		Model:     "fixture",
		Workspace: workspace,
		Database:  filepath.Join(t.TempDir(), "icoder.db"),
		SessionID: "integration",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()

	var observations []agent.ObservationType
	result, err := app.Run(context.Background(), "Which module is this?", func(observation agent.Observation) {
		observations = append(observations, observation.Type)
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != "The module is example.com/icoder-test." || result.Outcome != agent.OutcomeCompleted || len(result.Steps) != 2 || result.Usage.TotalTokens != 19 {
		t.Fatalf("result = %#v", result)
	}
	if len(observations) == 0 {
		t.Fatal("run emitted no observations")
	}

	mu.Lock()
	gotRequests := append([]wireChatRequest(nil), requests...)
	mu.Unlock()
	if len(gotRequests) != 2 || len(gotRequests[0].Tools) != 15 {
		t.Fatalf("provider requests = %#v", gotRequests)
	}
	second := gotRequests[1]
	if len(second.Messages) < 2 || second.Messages[len(second.Messages)-1].Role != "tool" {
		t.Fatalf("second provider request = %#v", second)
	}
	var readResult FileContent
	if err := json.Unmarshal([]byte(second.Messages[len(second.Messages)-1].Content), &readResult); err != nil || readResult.Path != "go.mod" || readResult.TotalLines != 2 || !strings.Contains(readResult.Content, "module example.com/icoder-test") {
		t.Fatalf("read_file result = %#v, error = %v", readResult, err)
	}

	snapshot, history, err := app.store.Load(context.Background(), "integration")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != 1 || snapshot.Usage.TotalTokens != 19 || len(history) != 4 {
		t.Fatalf("stored session = %#v, history = %#v", snapshot, history)
	}
	events, err := app.store.ReplayEvents(context.Background(), "integration", 0, 10)
	if err != nil || len(events) != 5 || events[0].Type != "agent.run.started" || events[1].Type != "agent.permission.checked" || events[2].Type != "agent.tool.started" || events[3].Type != "agent.tool.completed" || events[4].Type != "agent.run.completed" {
		t.Fatalf("events = %#v, error = %v", events, err)
	}
}

func TestAppHistoryFollowsActiveSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()
	app, err := NewApp(context.Background(), Config{APIKey: "fixture", BaseURL: server.URL, Model: "fixture", Workspace: t.TempDir(), Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if _, err := app.Run(context.Background(), "alpha-question", nil); err != nil {
		t.Fatal(err)
	}
	if err := app.UseSession(context.Background(), "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Run(context.Background(), "beta-question", nil); err != nil {
		t.Fatal(err)
	}
	beta, err := app.History(context.Background())
	if err != nil || len(beta) != 2 || beta[0].Text() != "beta-question" {
		t.Fatalf("beta history = %#v, %v", beta, err)
	}
	if err := app.UseSession(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	alpha, err := app.History(context.Background())
	if err != nil || len(alpha) != 2 || alpha[0].Text() != "alpha-question" {
		t.Fatalf("alpha history = %#v, %v", alpha, err)
	}
}

func TestAppPersistsFailedRunTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
		_, _ = response.Write([]byte(`{"error":{"message":"temporarily unavailable"}}`))
	}))
	defer server.Close()
	app, err := NewApp(context.Background(), Config{APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture", Workspace: t.TempDir(), Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "failed-run"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close(context.Background()) }()
	if _, err := app.Run(context.Background(), "fail", nil); err == nil {
		t.Fatal("Run() returned no error")
	}
	events, err := app.store.ReplayEvents(context.Background(), "failed-run", 0, 10)
	if err != nil || len(events) != 2 || events[0].Type != "agent.run.started" || events[1].Type != "agent.run.failed" {
		t.Fatalf("ReplayEvents() = %#v, %v", events, err)
	}
}

func TestAppCanSucceedAfterSamePromptFailedAttempt(t *testing.T) {
	var fail bool = true
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if fail {
			response.WriteHeader(http.StatusBadRequest)
			_, _ = response.Write([]byte(`{"error":{"message":"rejected"}}`))
			return
		}
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"recovered"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()
	app, err := NewApp(context.Background(), Config{APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture", Workspace: t.TempDir(), Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "retry-run"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close(context.Background()) }()
	if _, err := app.Run(context.Background(), "same prompt", nil); err == nil {
		t.Fatal("first Run() returned no error")
	}
	fail = false
	result, err := app.Run(context.Background(), "same prompt", nil)
	if err != nil || result.Text != "recovered" {
		t.Fatalf("second Run() = %#v, %v", result, err)
	}
	events, err := app.store.ReplayEvents(context.Background(), "retry-run", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	terminal := 0
	for _, item := range events {
		if item.Type == "agent.run.failed" || item.Type == "agent.run.completed" {
			terminal++
		}
	}
	if terminal != 2 {
		t.Fatalf("terminal events = %d: %#v", terminal, events)
	}
}

type recordingPublisher struct{ events []event.Envelope }

func (p *recordingPublisher) Publish(_ context.Context, envelope event.Envelope) error {
	p.events = append(p.events, envelope)
	return nil
}

func TestAppDispatchOutboxDeliversPendingEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()
	app, err := NewApp(context.Background(), Config{APIKey: "fixture", BaseURL: server.URL, Model: "fixture", Workspace: t.TempDir(), Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = app.Close(context.Background()) }()
	if _, err := app.Run(context.Background(), "answer", nil); err != nil {
		t.Fatal(err)
	}
	publisher := &recordingPublisher{}
	stats, err := app.DispatchOutbox(context.Background(), publisher, 10)
	if err != nil || stats.Claimed != 2 || stats.Delivered != 2 || len(publisher.events) != 2 {
		t.Fatalf("DispatchOutbox() = %#v, events = %#v, error = %v", stats, publisher.events, err)
	}
	stats, err = app.DispatchOutbox(context.Background(), publisher, 10)
	if err != nil || stats.Claimed != 0 || len(publisher.events) != 2 {
		t.Fatalf("second DispatchOutbox() = %#v, events = %#v, error = %v", stats, publisher.events, err)
	}
}
