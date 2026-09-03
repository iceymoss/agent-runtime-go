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
	"sync/atomic"
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
	if len(gotRequests) != 2 || len(gotRequests[0].Tools) != 17 {
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
	if err != nil || len(events) != 6 || events[0].Type != "agent.context.prepared" || events[1].Type != "agent.run.started" || events[2].Type != "agent.permission.checked" || events[3].Type != "agent.tool.started" || events[4].Type != "agent.tool.completed" || events[5].Type != "agent.run.completed" {
		t.Fatalf("events = %#v, error = %v", events, err)
	}

	reports, err := app.ContextReports(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("context reports = %#v", reports)
	}
	report := reports[0]
	if report.TokenizerID != "icoder/conservative-bytes-v1" || report.Compacted || report.Estimate.TotalInputTokens <= 0 || report.InputLimit <= 0 {
		t.Fatalf("context report plan side = %#v", report)
	}
	if report.Outcome != "completed" || report.StopReason != string(agent.StopReasonComplete) || report.Usage == nil || report.Usage.TotalTokens != 19 {
		t.Fatalf("context report outcome side = %#v", report)
	}
	if len(report.StepPromptTokens) != 2 || report.StepPromptTokens[0] != 5 || report.StepPromptTokens[1] != 8 {
		t.Fatalf("context report step prompt tokens = %#v", report.StepPromptTokens)
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
	if err != nil || len(events) != 3 || events[0].Type != "agent.context.prepared" || events[1].Type != "agent.run.started" || events[2].Type != "agent.run.failed" {
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
	if err != nil || stats.Claimed != 3 || stats.Delivered != 3 || len(publisher.events) != 3 {
		t.Fatalf("DispatchOutbox() = %#v, events = %#v, error = %v", stats, publisher.events, err)
	}
	stats, err = app.DispatchOutbox(context.Background(), publisher, 10)
	if err != nil || stats.Claimed != 0 || len(publisher.events) != 3 {
		t.Fatalf("second DispatchOutbox() = %#v, events = %#v, error = %v", stats, publisher.events, err)
	}
}

func TestAppSuspendsForApprovalAndResumesAfterDecision(t *testing.T) {
	workspace := t.TempDir()
	var (
		mu    sync.Mutex
		calls int
	)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			writeSSEResponse(t, response,
				`{"model":"fixture","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-write","function":{"name":"write_file","arguments":"{\"path\":\"note.txt\",\"content\":\"hello\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
				`[DONE]`,
			)
			return
		}
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"Wrote note.txt."},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()

	database := filepath.Join(t.TempDir(), "icoder.db")
	app, err := NewApp(context.Background(), Config{
		APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture",
		Workspace: workspace, Database: database, SessionID: "approval",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()

	// Nobody can answer an approval in this run, so the write must not happen and
	// the run must park instead of failing or proceeding.
	suspended, err := app.Run(ctx, "Create note.txt", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if suspended.Outcome != agent.OutcomeSuspended || suspended.StopReason != agent.StopReasonToolSuspended {
		t.Fatalf("suspended result = %+v", suspended)
	}
	if _, err := os.Stat(filepath.Join(workspace, "note.txt")); !os.IsNotExist(err) {
		t.Fatal("the file was written before the approval was granted")
	}
	snapshot, history, err := app.store.Load(ctx, "approval")
	if err != nil || snapshot.Revision != 0 || len(history) != 0 {
		t.Fatalf("a suspended run committed a turn: revision %d, %d messages, error %v", snapshot.Revision, len(history), err)
	}

	pending, err := app.PendingRuns(ctx)
	if err != nil || len(pending) != 1 || !pending[0].AwaitingApproval || pending[0].ApprovalRequestID == "" {
		t.Fatalf("PendingRuns() = %+v, error %v", pending, err)
	}
	runKey := pending[0].RunKey

	if err := app.ResolveRunApproval(ctx, runKey, true); err != nil {
		t.Fatalf("ResolveRunApproval() error = %v", err)
	}
	resumed, err := app.ResumeRun(ctx, runKey, nil, nil)
	if err != nil {
		t.Fatalf("ResumeRun() error = %v", err)
	}
	if resumed.Outcome != agent.OutcomeCompleted || resumed.Text != "Wrote note.txt." {
		t.Fatalf("resumed result = %+v", resumed)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "note.txt"))
	if err != nil || string(content) != "hello" {
		t.Fatalf("note.txt = %q, error %v", content, err)
	}
	snapshot, history, err = app.store.Load(ctx, "approval")
	if err != nil || snapshot.Revision != 1 || len(history) != 4 {
		t.Fatalf("committed session = %+v, %d messages, error %v", snapshot, len(history), err)
	}
	if history[len(history)-2].Role != agent.RoleTool && history[len(history)-1].Role != agent.RoleAssistant {
		t.Fatalf("resumed history is incoherent: %+v", history)
	}
	// The effect ledger must show the write as a completed effect exactly once.
	effects, err := app.RunEffects(ctx, runKey)
	if err != nil || len(effects) != 1 || effects[0].Status != "succeeded" {
		t.Fatalf("RunEffects() = %+v, error %v", effects, err)
	}
	after, err := app.PendingRuns(ctx)
	if err != nil || len(after) != 0 {
		t.Fatalf("PendingRuns() after completion = %+v, error %v", after, err)
	}
}

func TestAppAbandonRunClearsPendingWork(t *testing.T) {
	workspace := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-write","function":{"name":"write_file","arguments":"{\"path\":\"note.txt\",\"content\":\"hello\"}"}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()
	app, err := NewApp(context.Background(), Config{
		APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture",
		Workspace: workspace, Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "abandon",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	if _, err := app.Run(ctx, "Create note.txt", nil); err != nil {
		t.Fatal(err)
	}
	pending, err := app.PendingRuns(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("PendingRuns() = %+v, error %v", pending, err)
	}
	if err := app.ResolveRunApproval(ctx, pending[0].RunKey, false); err != nil {
		t.Fatalf("ResolveRunApproval() error = %v", err)
	}
	if err := app.AbandonRun(ctx, pending[0].RunKey); err != nil {
		t.Fatalf("AbandonRun() error = %v", err)
	}
	remaining, err := app.PendingRuns(ctx)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("PendingRuns() after abandon = %+v, error %v", remaining, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "note.txt")); !os.IsNotExist(err) {
		t.Fatal("an abandoned run still wrote to the workspace")
	}
}

func TestAppParksOnADelegateAndResumesWithTheChildResult(t *testing.T) {
	workspace := t.TempDir()
	var (
		mu           sync.Mutex
		parentCalls  int
		childPrompts []string
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
		// A child is read-only, so it never carries the delegate tools. That is a
		// stronger signal than call ordering and keeps the fixture honest about
		// which agent is talking.
		var parent bool
		for _, tool := range decoded.Tools {
			parent = parent || tool.Function.Name == "delegate_explore"
		}
		if !parent {
			mu.Lock()
			childPrompts = append(childPrompts, decoded.Messages[len(decoded.Messages)-1].Content)
			mu.Unlock()
			writeSSEResponse(t, response,
				`{"model":"fixture","choices":[{"delta":{"content":"The entry point is cmd/icoder/main.go."},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":6,"total_tokens":9}}`,
				`[DONE]`,
			)
			return
		}
		mu.Lock()
		parentCalls++
		call := parentCalls
		mu.Unlock()
		if call == 1 {
			writeSSEResponse(t, response,
				`{"model":"fixture","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-explore","function":{"name":"delegate_explore","arguments":"{\"task\":\"where is the entry point\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
				`[DONE]`,
			)
			return
		}
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"The explorer says cmd/icoder/main.go."},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()

	app, err := NewApp(context.Background(), Config{
		APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture",
		Workspace: workspace, Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "delegation",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()

	// The parent parks while the child works and is resumed from its checkpoint,
	// so the caller still sees one completed turn rather than a suspension it has
	// to drive itself.
	result, err := app.Run(ctx, "Where does this program start?", nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome != agent.OutcomeCompleted || result.Text != "The explorer says cmd/icoder/main.go." {
		t.Fatalf("result = %+v", result)
	}
	mu.Lock()
	prompts := append([]string(nil), childPrompts...)
	mu.Unlock()
	if len(prompts) != 1 || !strings.Contains(prompts[0], "where is the entry point") {
		t.Fatalf("child prompts = %#v", prompts)
	}

	// The delegation is auditable after the fact: the wake is an event on the
	// parent's stream and the child's answer is stored, not only replayed into
	// the model.
	events, err := app.store.ReplayEvents(ctx, "delegation", 0, 32)
	if err != nil {
		t.Fatal(err)
	}
	var awaited, completed bool
	for _, envelope := range events {
		awaited = awaited || envelope.Type == "agent.subagent.awaited"
		completed = completed || envelope.Type == "agent.subagent.completed"
	}
	if !awaited || !completed {
		t.Fatalf("delegation events awaited=%v completed=%v: %+v", awaited, completed, events)
	}
	delegations, err := app.store.ListDelegations(ctx, 10)
	if err != nil || len(delegations) != 1 || delegations[0].AgentKey != "icoder.explorer" {
		t.Fatalf("ListDelegations() = %+v, error %v", delegations, err)
	}
	if !strings.Contains(delegations[0].Result, "cmd/icoder/main.go") {
		t.Fatalf("stored child result = %q", delegations[0].Result)
	}

	// A parked-then-resumed run still commits exactly one turn.
	snapshot, history, err := app.store.Load(ctx, "delegation")
	if err != nil || snapshot.Revision != 1 || len(history) != 4 {
		t.Fatalf("committed session = %+v, %d messages, error %v", snapshot, len(history), err)
	}
	pending, err := app.PendingRuns(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("PendingRuns() = %+v, error %v", pending, err)
	}
}

func TestAppRecoversARunParkedOnADelegate(t *testing.T) {
	workspace := t.TempDir()
	var childSucceeds atomic.Bool
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
		var parent bool
		for _, tool := range decoded.Tools {
			parent = parent || tool.Function.Name == "delegate_review"
		}
		if !parent {
			if !childSucceeds.Load() {
				// An unavailable provider is exactly the case that leaves a parent
				// parked across a restart: the child is retryable, not failed.
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			writeSSEResponse(t, response,
				`{"model":"fixture","choices":[{"delta":{"content":"finding: the loop is unbounded."},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":6,"total_tokens":9}}`,
				`[DONE]`,
			)
			return
		}
		var answered bool
		for _, message := range decoded.Messages {
			answered = answered || message.Role == "tool"
		}
		if !answered {
			writeSSEResponse(t, response,
				`{"model":"fixture","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-review","function":{"name":"delegate_review","arguments":"{\"task\":\"review the loop\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
				`[DONE]`,
			)
			return
		}
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"The reviewer says the loop is unbounded."},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`,
			`[DONE]`,
		)
	}))
	defer server.Close()

	database := filepath.Join(t.TempDir(), "icoder.db")
	config := Config{
		APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture",
		Workspace: workspace, Database: database, SessionID: "recover-delegate",
	}
	ctx := context.Background()
	app, err := NewApp(ctx, config)
	if err != nil {
		t.Fatal(err)
	}

	// The child cannot finish, so the parent runs out of re-entries and stays
	// parked rather than pretending the delegation produced an answer.
	if _, err := app.Run(ctx, "Review the main loop", nil); err == nil {
		t.Fatal("Run() reported success while the child never finished")
	}
	snapshot, history, err := app.store.Load(ctx, "recover-delegate")
	if err != nil || snapshot.Revision != 0 || len(history) != 0 {
		t.Fatalf("a parked run committed a turn: revision %d, %d messages, error %v", snapshot.Revision, len(history), err)
	}
	if err := app.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// A new process reads the parked run from the database. It needs work driven,
	// not a human decision, so it must not be offered as an approval.
	restarted, err := NewApp(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := restarted.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	pending, err := restarted.PendingRuns(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("PendingRuns() = %+v, error %v", pending, err)
	}
	if !pending[0].AwaitingDelegation || pending[0].AwaitingApproval {
		t.Fatalf("parked run = %+v, want awaiting delegation", pending[0])
	}

	childSucceeds.Store(true)
	resumed, err := restarted.ResumeRun(ctx, pending[0].RunKey, nil, nil)
	if err != nil {
		t.Fatalf("ResumeRun() error = %v", err)
	}
	if resumed.Outcome != agent.OutcomeCompleted || resumed.Text != "The reviewer says the loop is unbounded." {
		t.Fatalf("resumed result = %+v", resumed)
	}
	snapshot, history, err = restarted.store.Load(ctx, "recover-delegate")
	if err != nil || snapshot.Revision != 1 || len(history) != 4 {
		t.Fatalf("committed session = %+v, %d messages, error %v", snapshot, len(history), err)
	}
	after, err := restarted.PendingRuns(ctx)
	if err != nil || len(after) != 0 {
		t.Fatalf("PendingRuns() after recovery = %+v, error %v", after, err)
	}
}
