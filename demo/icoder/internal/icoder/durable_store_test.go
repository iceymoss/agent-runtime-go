package icoder

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/durable"
)

func newDurableStore(t *testing.T) *SQLiteDurableStore {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store.Durable()
}

func beginRun(t *testing.T, store *SQLiteDurableStore, runKey string) durable.Snapshot {
	t.Helper()
	snapshot, created, err := store.Begin(context.Background(), durable.BeginRequest{
		Identity:    durable.Identity{RunKey: runKey, AgentKey: "icoder", SessionID: "session", RequestID: "request-" + runKey},
		InputDigest: "sha256:input", ConfigDigest: "sha256:config",
		Checkpoint: agent.Checkpoint{History: []agent.Message{agent.NewUserMessage("task")}},
	})
	if err != nil || !created {
		t.Fatalf("Begin() = created %v, error %v", created, err)
	}
	return snapshot
}

func acquireRun(t *testing.T, store *SQLiteDurableStore, runKey, owner string) durable.Snapshot {
	t.Helper()
	now := time.Now().UTC()
	snapshot, err := store.Acquire(context.Background(), durable.AcquireRequest{
		RunKey: durable.RunKey(runKey), Owner: owner, Now: now, LeaseUntil: now.Add(time.Minute), AllowSuspended: true,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	return snapshot
}

func TestSQLiteDurableStoreBeginIsIdempotentByImmutableInput(t *testing.T) {
	store := newDurableStore(t)
	ctx := context.Background()
	request := durable.BeginRequest{
		Identity:    durable.Identity{RunKey: "run-1", AgentKey: "icoder", SessionID: "session", RequestID: "request-1"},
		InputDigest: "sha256:input", ConfigDigest: "sha256:config",
	}
	first, created, err := store.Begin(ctx, request)
	if err != nil || !created || first.Status != durable.StatusClaimed || first.Phase != durable.PhaseModelReady {
		t.Fatalf("Begin() = %+v, created %v, error %v", first, created, err)
	}
	second, created, err := store.Begin(ctx, request)
	if err != nil || created || second.Revision != first.Revision {
		t.Fatalf("replayed Begin() = %+v, created %v, error %v", second, created, err)
	}

	tests := []struct {
		name    string
		mutate  func(durable.BeginRequest) durable.BeginRequest
		wantErr error
	}{
		{
			name:    "different config digest is a conflict",
			mutate:  func(r durable.BeginRequest) durable.BeginRequest { r.ConfigDigest = "sha256:other"; return r },
			wantErr: durable.ErrRunConflict,
		},
		{
			name: "reusing the session and request pair is a conflict",
			mutate: func(r durable.BeginRequest) durable.BeginRequest {
				r.Identity.RunKey = "run-2"
				return r
			},
			wantErr: durable.ErrRunConflict,
		},
		{
			name:    "missing immutable field is rejected",
			mutate:  func(r durable.BeginRequest) durable.BeginRequest { r.Identity.AgentKey = ""; return r },
			wantErr: durable.ErrRunConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := store.Begin(ctx, test.mutate(request)); !errors.Is(err, test.wantErr) {
				t.Fatalf("Begin() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestSQLiteDurableStoreEnforcesGuardAndPhaseEdges(t *testing.T) {
	store := newDurableStore(t)
	ctx := context.Background()
	beginRun(t, store, "run-1")
	leased := acquireRun(t, store, "run-1", "worker-1")

	tests := []struct {
		name    string
		request durable.SaveRequest
		wantErr error
	}{
		{
			name:    "model ready cannot jump to tools ready",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseToolsReady},
			wantErr: durable.ErrInvalidTransition,
		},
		{
			name:    "completion requires finalizing",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusCompleted, Phase: durable.PhaseTerminal},
			wantErr: durable.ErrInvalidTransition,
		},
		{
			name:    "running cannot carry a failure",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight, Failure: &durable.Failure{Message: "boom"}},
			wantErr: durable.ErrInvalidTransition,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.Save(ctx, test.request); !errors.Is(err, test.wantErr) {
				t.Fatalf("Save() error = %v, want %v", err, test.wantErr)
			}
		})
	}

	inflight, err := store.Save(ctx, durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight})
	if err != nil || inflight.Revision != leased.Revision+1 {
		t.Fatalf("Save() = %+v, error %v", inflight, err)
	}
	if _, err := store.Save(ctx, durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight}); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("stale guard error = %v, want durable.ErrLeaseLost", err)
	}
	finalizing, err := store.Save(ctx, durable.SaveRequest{Guard: inflight.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseFinalizing})
	if err != nil {
		t.Fatalf("Save(finalizing) error = %v", err)
	}
	done, err := store.Save(ctx, durable.SaveRequest{Guard: finalizing.Guard(), Status: durable.StatusCompleted, Phase: durable.PhaseTerminal})
	if err != nil || !done.Terminal() || done.LeaseOwner != "" {
		t.Fatalf("Save(completed) = %+v, error %v", done, err)
	}
	if _, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "run-1", Owner: "worker-2", Now: time.Now(), LeaseUntil: time.Now().Add(time.Minute)}); !errors.Is(err, durable.ErrTerminal) {
		t.Fatalf("Acquire(terminal) error = %v, want durable.ErrTerminal", err)
	}
}

func TestSQLiteDurableStoreRefusesLiveLeaseTakeover(t *testing.T) {
	store := newDurableStore(t)
	beginRun(t, store, "run-1")
	acquireRun(t, store, "run-1", "worker-1")
	now := time.Now().UTC()
	_, err := store.Acquire(context.Background(), durable.AcquireRequest{RunKey: "run-1", Owner: "worker-2", Now: now, LeaseUntil: now.Add(time.Minute)})
	if !errors.Is(err, durable.ErrLeaseHeld) {
		t.Fatalf("Acquire() error = %v, want durable.ErrLeaseHeld", err)
	}
}

// prepareToolsPhase drives a run to the phase where effects may be prepared,
// following the same edges the runtime takes.
func prepareToolsPhase(t *testing.T, store *SQLiteDurableStore, runKey string, call agent.ToolCall) durable.Snapshot {
	t.Helper()
	ctx := context.Background()
	beginRun(t, store, runKey)
	leased := acquireRun(t, store, runKey, "worker-1")
	inflight, err := store.Save(ctx, durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight})
	if err != nil {
		t.Fatalf("Save(model inflight) error = %v", err)
	}
	tools, err := store.Save(ctx, durable.SaveRequest{
		Guard: inflight.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseToolsReady,
		Checkpoint: agent.Checkpoint{PendingToolCalls: []agent.ToolCall{call}},
	})
	if err != nil {
		t.Fatalf("Save(tools ready) error = %v", err)
	}
	return tools
}

func TestSQLiteDurableStoreEffectLedgerLifecycle(t *testing.T) {
	store := newDurableStore(t)
	ctx := context.Background()
	call := agent.ToolCall{ID: "call-1", Name: "write_file", Input: `{"path":"a.go"}`}
	snapshot := prepareToolsPhase(t, store, "run-1", call)

	record, created, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", StepNumber: 0, Ordinal: 0, ToolCall: call, PreparedAt: time.Now().UTC(),
	})
	if err != nil || !created || record.Status != durable.EffectPrepared {
		t.Fatalf("PrepareEffect() = %+v, created %v, error %v", record, created, err)
	}
	// The execution key must match the runtime's idempotency key exactly, or the
	// two layers would deduplicate on different anchors.
	execution, err := agent.NewToolExecution(agent.RunIdentity{RunKey: "run-1", AgentKey: "icoder", SessionID: "session", RequestID: "request-run-1"}, 0, 0, call)
	if err != nil {
		t.Fatal(err)
	}
	if execution.IdempotencyKey != string(record.ExecutionKey) {
		t.Fatalf("execution key = %q, runtime idempotency key = %q", record.ExecutionKey, execution.IdempotencyKey)
	}
	if _, created, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", StepNumber: 0, Ordinal: 0, ToolCall: call, PreparedAt: time.Now().UTC(),
	}); err != nil || created {
		t.Fatalf("replayed PrepareEffect() created = %v, error = %v", created, err)
	}
	if _, _, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", StepNumber: 0, Ordinal: 0,
		ToolCall: agent.ToolCall{ID: "call-2", Name: "write_file", Input: `{"path":"b.go"}`}, PreparedAt: time.Now().UTC(),
	}); !errors.Is(err, durable.ErrEffectConflict) {
		t.Fatalf("duplicate ordinal error = %v, want durable.ErrEffectConflict", err)
	}

	started, err := store.BeginEffect(ctx, snapshot.Guard(), record.ExecutionKey, time.Now().UTC())
	if err != nil || started.Status != durable.EffectRunning || started.FenceToken != snapshot.FenceToken {
		t.Fatalf("BeginEffect() = %+v, error %v", started, err)
	}
	finishedAt := time.Now().UTC()
	result := agent.ToolResult{ToolCallID: "call-1", Name: "write_file", Content: `{"digest":"sha256:x"}`}
	completed, err := store.CompleteEffect(ctx, durable.CompleteEffectRequest{
		Guard: snapshot.Guard(), ExecutionKey: record.ExecutionKey, Result: &result, FinishedAt: finishedAt,
	})
	if err != nil || completed.Status != durable.EffectSucceeded || completed.Result == nil {
		t.Fatalf("CompleteEffect() = %+v, error %v", completed, err)
	}
	if _, err := store.CompleteEffect(ctx, durable.CompleteEffectRequest{
		Guard: snapshot.Guard(), ExecutionKey: record.ExecutionKey, Result: &result, FinishedAt: finishedAt,
	}); err != nil {
		t.Fatalf("replayed CompleteEffect() error = %v", err)
	}
	if _, err := store.CompleteEffect(ctx, durable.CompleteEffectRequest{
		Guard: snapshot.Guard(), ExecutionKey: record.ExecutionKey, Failure: &durable.EffectFailure{Message: "different"}, FinishedAt: finishedAt,
	}); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("conflicting CompleteEffect() error = %v, want durable.ErrLeaseLost", err)
	}
	effects, err := store.ListEffects(ctx, "run-1")
	if err != nil || len(effects) != 1 || effects[0].Status != durable.EffectSucceeded {
		t.Fatalf("ListEffects() = %+v, error %v", effects, err)
	}
}

func TestSQLiteDurableStoreRevokeLeaseMarksRunningEffectsUnknown(t *testing.T) {
	store := newDurableStore(t)
	ctx := context.Background()
	call := agent.ToolCall{ID: "call-1", Name: "run_command", Input: `{"program":"go"}`}
	snapshot := prepareToolsPhase(t, store, "run-1", call)
	record, _, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", ToolCall: call, PreparedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginEffect(ctx, snapshot.Guard(), record.ExecutionKey, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	revoked, err := store.RevokeLease(ctx, durable.RevokeLeaseRequest{
		RunKey: "run-1", ExpectedRevision: snapshot.Revision, ExpectedFence: snapshot.FenceToken, Now: time.Now().UTC(),
	})
	if err != nil || revoked.Status != durable.StatusSuspended || revoked.Phase != durable.PhaseToolsReady || revoked.FenceToken != snapshot.FenceToken+1 {
		t.Fatalf("RevokeLease() = %+v, error %v", revoked, err)
	}
	unknown, err := store.LoadEffect(ctx, record.ExecutionKey)
	if err != nil || unknown.Status != durable.EffectUnknown {
		t.Fatalf("LoadEffect() = %+v, error %v", unknown, err)
	}

	resumed := acquireRun(t, store, "run-1", "worker-2")
	if _, err := store.BeginEffect(ctx, resumed.Guard(), record.ExecutionKey, time.Now().UTC()); !errors.Is(err, durable.ErrToolEffectUnknown) {
		t.Fatalf("BeginEffect(unknown) error = %v, want durable.ErrToolEffectUnknown", err)
	}
	replayed, err := store.ReplayEffect(ctx, resumed.Guard(), record.ExecutionKey, time.Now().UTC())
	if err != nil || replayed.Status != durable.EffectRunning || replayed.FenceToken != resumed.FenceToken {
		t.Fatalf("ReplayEffect() = %+v, error %v", replayed, err)
	}
}

func TestSQLiteDurableStoreScanFindsExpiredLeases(t *testing.T) {
	store := newDurableStore(t)
	ctx := context.Background()
	for _, key := range []string{"run-1", "run-2"} {
		beginRun(t, store, key)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "run-1", Owner: "worker-1", Now: past, LeaseUntil: past.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	acquireRun(t, store, "run-2", "worker-2")

	page, err := store.Scan(ctx, durable.ScanRequest{Limit: 10, ExpiredBefore: time.Now().UTC()})
	if err != nil || len(page.Snapshots) != 1 || page.Snapshots[0].Identity.RunKey != "run-1" {
		t.Fatalf("Scan(expired) = %+v, error %v", page, err)
	}
	claimed, err := store.Scan(ctx, durable.ScanRequest{Limit: 10, Statuses: []durable.Status{durable.StatusRunning}})
	if err != nil || len(claimed.Snapshots) != 2 {
		t.Fatalf("Scan(running) = %+v, error %v", claimed, err)
	}
	paged, err := store.Scan(ctx, durable.ScanRequest{Limit: 1})
	if err != nil || len(paged.Snapshots) != 1 || paged.Next != "run-1" {
		t.Fatalf("Scan(limit 1) = %+v, error %v", paged, err)
	}
	if _, err := store.Scan(ctx, durable.ScanRequest{Limit: 0}); !errors.Is(err, durable.ErrLimitExceeded) {
		t.Fatalf("Scan(limit 0) error = %v, want durable.ErrLimitExceeded", err)
	}
}

func TestSQLiteDurableStoreUsageLedgerIsIdempotent(t *testing.T) {
	store := newDurableStore(t)
	ctx := context.Background()
	occurredAt := time.Now().UTC().Truncate(time.Microsecond)
	fact := durable.UsageFact{
		TenantKey: "local", UsageKey: durable.UsageFactKey("local", "attempt-1", "", "model"),
		RunKey: "run-1", AttemptKey: "attempt-1", Kind: "model", Provider: "openai-compatible",
		Model: "test-model", PricingVersion: "v1", InputTokens: 12, OutputTokens: 3,
		CostMicros: 40, Currency: "USD", OccurredAt: occurredAt,
	}
	stored, created, err := store.RecordUsage(ctx, fact)
	if err != nil || !created || stored.InputTokens != 12 {
		t.Fatalf("RecordUsage() = %+v, created %v, error %v", stored, created, err)
	}
	if _, created, err := store.RecordUsage(ctx, fact); err != nil || created {
		t.Fatalf("replayed RecordUsage() created = %v, error = %v", created, err)
	}
	conflicting := fact
	conflicting.OutputTokens = 4
	if _, _, err := store.RecordUsage(ctx, conflicting); !errors.Is(err, durable.ErrUsageConflict) {
		t.Fatalf("conflicting RecordUsage() error = %v, want durable.ErrUsageConflict", err)
	}
	loaded, err := store.LoadUsage(ctx, "local", fact.UsageKey)
	if err != nil || !loaded.OccurredAt.Equal(occurredAt) {
		t.Fatalf("LoadUsage() = %+v, error %v", loaded, err)
	}
	if _, err := store.LoadUsage(ctx, "local", "missing"); !errors.Is(err, durable.ErrUsageNotFound) {
		t.Fatalf("LoadUsage(missing) error = %v, want durable.ErrUsageNotFound", err)
	}
	facts, err := store.ListAttemptUsage(ctx, "local", "attempt-1")
	if err != nil || len(facts) != 1 {
		t.Fatalf("ListAttemptUsage() = %+v, error %v", facts, err)
	}
	if _, _, err := store.RecordUsage(ctx, durable.UsageFact{TenantKey: "local"}); !errors.Is(err, durable.ErrUsageConflict) {
		t.Fatalf("RecordUsage(incomplete) error = %v, want durable.ErrUsageConflict", err)
	}
}

// TestSQLiteDurableStoreCheckpointConformance holds the SQLite adapter to the
// runtime's own definition of a correct CheckpointStore, so this implementation
// and the library's reference are verified against the same contract rather than
// against each other.
func TestSQLiteDurableStoreCheckpointConformance(t *testing.T) {
	agenttest.TestCheckpointStore(t, func(t *testing.T) agent.CheckpointStore {
		store := newDurableStore(t)
		adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
			Store: store, Ledger: store, AttemptKey: "conformance-attempt",
		})
		if err != nil {
			t.Fatalf("NewCheckpointAdapter() error = %v", err)
		}
		return adapter
	})
}

// TestSQLiteDurableStoreConformance holds this adapter to the same durable
// authority contract as the library's reference implementation.
func TestSQLiteDurableStoreConformance(t *testing.T) {
	agenttest.TestDurableStore(t, func(t *testing.T) agenttest.DurableStore { return newDurableStore(t) })
}
