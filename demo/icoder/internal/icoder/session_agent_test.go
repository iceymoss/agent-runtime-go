package icoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/session"
)

// newQueueFixture serves one answer per turn so a queued run has something
// deterministic to execute.
func newQueueFixture(t *testing.T, answers ...string) *httptest.Server {
	t.Helper()
	var (
		mu    sync.Mutex
		calls int
	)
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if _, err := io.ReadAll(request.Body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		index := calls
		calls++
		mu.Unlock()
		if index >= len(answers) {
			index = len(answers) - 1
		}
		payload, err := json.Marshal(answers[index])
		if err != nil {
			t.Error(err)
			return
		}
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":`+string(payload)+`},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
			`[DONE]`,
		)
	}))
}

func newQueueApp(t *testing.T, server *httptest.Server, workspace, database string) *App {
	t.Helper()
	application, err := NewApp(context.Background(), Config{
		APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture",
		Workspace: workspace, Database: database, SessionID: "queued",
	})
	if err != nil {
		t.Fatal(err)
	}
	return application
}

func TestAppQueuesRunsAndExecutesThemThroughTheSessionAgent(t *testing.T) {
	server := newQueueFixture(t, "First answer.", "Second answer.")
	defer server.Close()
	application := newQueueApp(t, server, t.TempDir(), filepath.Join(t.TempDir(), "icoder.db"))
	defer func() {
		if err := application.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()

	first, err := application.QueueRun(ctx, "What is this project?")
	if err != nil {
		t.Fatalf("QueueRun() error = %v", err)
	}
	// Submitting the same instruction at the same revision is the same request,
	// so it must join the queue once rather than twice.
	again, err := application.QueueRun(ctx, "What is this project?")
	if err != nil || again != first {
		t.Fatalf("replayed QueueRun() = %q, first = %q, error %v", again, first, err)
	}

	claimed, err := application.RunQueuedWork(ctx)
	if err != nil || !claimed {
		t.Fatalf("RunQueuedWork() claimed = %v, error = %v", claimed, err)
	}
	result, err := application.AwaitQueuedRun(ctx, first)
	if err != nil {
		t.Fatalf("AwaitQueuedRun() error = %v", err)
	}
	if result.Receipt.ExecutionState != session.ExecutionReleased {
		t.Fatalf("queued run = %+v", result.Receipt)
	}
	if result.CoreResult == nil || result.CoreResult.Outcome != agent.OutcomeCompleted || result.CoreResult.Text != "First answer." {
		t.Fatalf("queued core result = %+v", result.CoreResult)
	}
	// The merge advanced the queue's session revision exactly once, and the
	// attempt committed the conversation turn.
	if result.SessionRevision != 1 {
		t.Fatalf("session revision = %d, want 1", result.SessionRevision)
	}
	snapshot, history, err := application.store.Load(ctx, "queued")
	if err != nil || snapshot.Revision != 1 || len(history) != 2 {
		t.Fatalf("committed session = %+v, %d messages, error %v", snapshot, len(history), err)
	}

	// An empty queue is not an error; it is how a one-shot worker learns it is
	// done.
	if claimed, err := application.RunQueuedWork(ctx); err != nil || claimed {
		t.Fatalf("RunQueuedWork() on an empty queue: claimed %v, error %v", claimed, err)
	}
}

func TestAppQueuedRunSurvivesRestartAndCanBeAbandoned(t *testing.T) {
	server := newQueueFixture(t, "Answer.")
	defer server.Close()
	workspace := t.TempDir()
	database := filepath.Join(t.TempDir(), "icoder.db")
	ctx := context.Background()

	application := newQueueApp(t, server, workspace, database)
	runKey, err := application.QueueRun(ctx, "Summarize the workspace.")
	if err != nil {
		t.Fatalf("QueueRun() error = %v", err)
	}
	if err := application.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// The queue is durable, so the run is still waiting in the new process with
	// the context plan it was admitted against.
	restarted := newQueueApp(t, server, workspace, database)
	defer func() {
		if err := restarted.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	pending, err := restarted.queue.store.Get(ctx, runKey)
	if err != nil || pending.Receipt.ExecutionState != session.ExecutionQueued {
		t.Fatalf("queued run after restart = %+v, error %v", pending.Receipt, err)
	}

	// Abandoning is final: the run must not then be executed by a worker.
	cancelled, err := restarted.CancelQueuedRun(ctx, runKey, session.CancelAbandon, "operator")
	if err != nil || !cancelled.Requested {
		t.Fatalf("CancelQueuedRun() = %+v, error %v", cancelled, err)
	}
	if claimed, err := restarted.RunQueuedWork(ctx); err != nil || claimed {
		t.Fatalf("RunQueuedWork() executed an abandoned run: claimed %v, error %v", claimed, err)
	}
	after, err := restarted.queue.store.Get(ctx, runKey)
	if err != nil || after.Receipt.ExecutionState != session.ExecutionReleased {
		t.Fatalf("abandoned run = %+v, error %v", after.Receipt, err)
	}
	snapshot, _, err := restarted.store.Load(ctx, "queued")
	if err != nil || snapshot.Revision != 0 {
		t.Fatalf("an abandoned run committed a turn: %+v, error %v", snapshot, err)
	}
}
