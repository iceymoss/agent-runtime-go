package icoder

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
)

func TestStoreCommitTurnAndIdempotency(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	snapshot, history, err := store.Load(ctx, "session")
	if err != nil || len(history) != 0 {
		t.Fatalf("Load() = %#v, %d, %v", snapshot, len(history), err)
	}
	result := agent.RunResult{Messages: []agent.Message{agent.NewAssistantMessage("answer")}, Text: "answer", Outcome: agent.OutcomeCompleted, StopReason: agent.StopReasonComplete, Usage: agent.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3}}
	if err := store.CommitTurn(ctx, snapshot, "request-1", "run-1", "input-1", agent.NewUserMessage("question"), result, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitTurn(ctx, snapshot, "request-1", "run-1", "input-1", agent.NewUserMessage("question"), result, nil); err != nil {
		t.Fatalf("idempotent CommitTurn() error = %v", err)
	}
	updated, history, err := store.Load(ctx, "session")
	if err != nil || updated.Revision != 1 || updated.Usage.TotalTokens != 3 || len(history) != 2 {
		t.Fatalf("Load() = %#v, %#v, %v", updated, history, err)
	}
	events, err := store.ReplayEvents(ctx, "session", 0, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 1 || events[0].Type != "agent.run.completed" {
		t.Fatalf("ReplayEvents() = %#v, %v", events, err)
	}
}

func TestStoreRejectsStaleRevision(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	snapshot, _, err := store.Load(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	result := agent.RunResult{Messages: []agent.Message{agent.NewAssistantMessage("one")}, Outcome: agent.OutcomeCompleted, StopReason: agent.StopReasonComplete}
	if err := store.CommitTurn(ctx, snapshot, "one", "run-one", "one", agent.NewUserMessage("one"), result, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitTurn(ctx, snapshot, "two", "run-two", "two", agent.NewUserMessage("two"), result, nil); err == nil {
		t.Fatal("CommitTurn() accepted stale revision")
	}
}

func TestStoreListsAndClearsSessions(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	snapshot, _, err := store.Load(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	result := agent.RunResult{Messages: []agent.Message{agent.NewAssistantMessage("answer")}, Outcome: agent.OutcomeCompleted, StopReason: agent.StopReasonComplete}
	if err := store.CommitTurn(ctx, snapshot, "request", "run", "input", agent.NewUserMessage("question"), result, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListSessions(ctx)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("ListSessions() = %#v, %v", sessions, err)
	}
	if err := store.ClearSession(ctx, "alpha"); err != nil {
		t.Fatal(err)
	}
	cleared, history, err := store.Load(ctx, "alpha")
	if err != nil || cleared.Revision != 0 || len(history) != 0 {
		t.Fatalf("Load(cleared) = %#v, %#v, %v", cleared, history, err)
	}
	events, err := store.ReplayEvents(ctx, "alpha", 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("ReplayEvents(cleared) = %#v, %v", events, err)
	}
}

func TestStoreHistoryIsIsolatedBySession(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx := context.Background()
	for _, session := range []string{"alpha", "beta"} {
		snapshot, _, err := store.Load(ctx, session)
		if err != nil {
			t.Fatal(err)
		}
		result := agent.RunResult{Messages: []agent.Message{agent.NewAssistantMessage("answer-" + session)}, Outcome: agent.OutcomeCompleted, StopReason: agent.StopReasonComplete}
		if err := store.CommitTurn(ctx, snapshot, "request-"+session, "run-"+session, "input-"+session, agent.NewUserMessage("question-"+session), result, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, alpha, err := store.Load(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	_, beta, err := store.Load(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if len(alpha) != 2 || len(beta) != 2 || alpha[0].Text() != "question-alpha" || beta[0].Text() != "question-beta" || alpha[1].Text() == beta[1].Text() {
		t.Fatalf("alpha = %#v, beta = %#v", alpha, beta)
	}
}

func TestStorePersistsTaskStateWithTurn(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	snapshot, _, err := store.Load(ctx, "task-state")
	if err != nil {
		t.Fatal(err)
	}
	write := agent.ToolCall{ID: "write", Name: "write_file", Input: `{"path":"main.go"}`}
	result := agent.RunResult{Messages: []agent.Message{
		{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &write}}},
		agent.NewToolMessage(agent.ToolResult{ToolCallID: "write", Name: "write_file", Content: `{}`}),
	}}
	if err := store.CommitTurn(ctx, snapshot, "request", "run", "input", agent.NewUserMessage("fix main"), result, nil); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadTaskState(ctx, "task-state")
	if err != nil || state == nil || state.Goal != "fix main" || state.Status != "needs_validation" || state.Verification != "unverified" || len(state.ChangedFiles) != 1 || state.ChangedFiles[0] != "main.go" {
		t.Fatalf("LoadTaskState() = %#v, %v", state, err)
	}
	if err := store.ClearSession(ctx, "task-state"); err != nil {
		t.Fatal(err)
	}
	state, err = store.LoadTaskState(ctx, "task-state")
	if err != nil || state != nil {
		t.Fatalf("LoadTaskState() after clear = %#v, %v", state, err)
	}
}

func TestStoreAppendRunEventIsIdempotentAndOrdered(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, _, err := store.Load(ctx, "events"); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRunEvent(ctx, "events", "run:started", "agent.run.started", map[string]any{"value": 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRunEvent(ctx, "events", "run:started", "agent.run.started", map[string]any{"value": 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendRunEvent(ctx, "events", "run:terminal", "agent.run.failed", map[string]any{"error": "failed"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReplayEvents(ctx, "events", 0, 10)
	if err != nil || len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 || events[1].Type != "agent.run.failed" {
		t.Fatalf("ReplayEvents() = %#v, %v", events, err)
	}
}

func TestStoreCommitTurnCreatesPendingOutboxAtomically(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	snapshot, _, err := store.Load(ctx, "outbox")
	if err != nil {
		t.Fatal(err)
	}
	result := agent.RunResult{Messages: []agent.Message{agent.NewAssistantMessage("answer")}, Outcome: agent.OutcomeCompleted}
	if err := store.CommitTurn(ctx, snapshot, "request", "run", "input", agent.NewUserMessage("question"), result, nil); err != nil {
		t.Fatal(err)
	}
	var state event.OutboxState
	if err := store.db.QueryRow(`SELECT outbox_state FROM event_records WHERE event_id = 'request:completed'`).Scan(&state); err != nil || state != event.OutboxPending {
		t.Fatalf("completed outbox state = %q, %v", state, err)
	}
	if err := store.CommitTurn(ctx, snapshot, "stale", "stale-run", "input", agent.NewUserMessage("question"), result, nil); err == nil {
		t.Fatal("CommitTurn() accepted stale snapshot")
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM event_records WHERE event_id = 'stale:completed'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back outbox count = %d, %v", count, err)
	}
}

func TestStoreMigratesLegacyEventsAsDelivered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := store.Load(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO icoder_events(session_id, sequence, event_id, event_type, payload, occurred_at) VALUES(?, ?, ?, ?, ?, ?)`, "legacy", 1, "legacy:event", "agent.run.completed", `{}`, time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var state event.OutboxState
	if err := store.db.QueryRow(`SELECT outbox_state FROM event_records WHERE event_id = 'legacy:event'`).Scan(&state); err != nil || state != event.OutboxDelivered {
		t.Fatalf("legacy outbox state = %q, %v", state, err)
	}
	claimed, err := store.events.Claim(ctx, event.ClaimCommand{TenantKey: "local", Owner: "worker", Limit: 10, LeaseDuration: time.Minute})
	if err != nil || len(claimed) != 0 {
		t.Fatalf("Claim() legacy events = %#v, %v", claimed, err)
	}
}
