package agenttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
)

// DurableStore is the full durable surface an adapter is expected to provide.
// The three ports are separate interfaces because they can be owned separately,
// but a store that only implements some of them cannot support recovery.
type DurableStore interface {
	durable.Store
	durable.ExecutionLedger
	durable.UsageLedger
}

// DurableStoreFactory returns a fresh, empty durable store for one subtest.
type DurableStoreFactory func(t *testing.T) DurableStore

// TestDurableStore runs the durable authority conformance suite.
//
// Everything here exists because getting it wrong is invisible until a worker
// dies at the wrong moment: a guard that compares three fields instead of four
// lets a zombie worker overwrite a live one, an effect ledger that forgets a
// fence lets a retry double-charge, and a usage fact that is not idempotent
// double-bills. The suite drives each of those situations deliberately.
//
// An adapter that also implements durable.EffectReplayer has that extension
// checked too; one that does not is held to the conservative default instead.
func TestDurableStore(t *testing.T, factory DurableStoreFactory) {
	t.Helper()
	t.Run("begin is idempotent by immutable input", func(t *testing.T) { testDurableBegin(t, factory) })
	t.Run("leases fence concurrent workers", func(t *testing.T) { testDurableLeases(t, factory) })
	t.Run("save enforces the status and phase machine", func(t *testing.T) { testDurableSave(t, factory) })
	t.Run("revoking a lease marks running effects unknown", func(t *testing.T) { testDurableRevoke(t, factory) })
	t.Run("scan pages recoverable work", func(t *testing.T) { testDurableScan(t, factory) })
	t.Run("the effect ledger records one outcome per execution", func(t *testing.T) { testDurableEffects(t, factory) })
	t.Run("usage facts are immutable and idempotent", func(t *testing.T) { testDurableUsage(t, factory) })
}

func durableIdentity(runKey string) durable.Identity {
	return durable.Identity{RunKey: runKey, AgentKey: "conformance", SessionID: "session-1", RequestID: "request-" + runKey}
}

func durableBeginRequest(runKey string) durable.BeginRequest {
	return durable.BeginRequest{
		Identity: durableIdentity(runKey), InputDigest: "sha256:input", ConfigDigest: "sha256:config",
		Checkpoint: agent.Checkpoint{History: []agent.Message{agent.NewUserMessage("input")}},
	}
}

// durableBegin creates a run and returns nothing; callers acquire it separately
// because acquiring is the step that grants write authority.
func durableBegin(t *testing.T, store DurableStore, runKey string) {
	t.Helper()
	snapshot, created, err := store.Begin(context.Background(), durableBeginRequest(runKey))
	if err != nil || !created || snapshot.Status != durable.StatusClaimed || snapshot.Phase != durable.PhaseModelReady {
		t.Fatalf("Begin() = %+v, created %v, error %v", snapshot, created, err)
	}
}

func durableAcquire(t *testing.T, store DurableStore, runKey, owner string) durable.Snapshot {
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

// durableToolsPhase drives a run to the phase where effects may be prepared,
// following the same edges the runtime takes.
func durableToolsPhase(t *testing.T, store DurableStore, runKey string, call agent.ToolCall) durable.Snapshot {
	t.Helper()
	ctx := context.Background()
	durableBegin(t, store, runKey)
	snapshot := durableAcquire(t, store, runKey, "worker-1")
	snapshot, err := store.Save(ctx, durable.SaveRequest{Guard: snapshot.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight})
	if err != nil {
		t.Fatalf("Save(model inflight) error = %v", err)
	}
	snapshot, err = store.Save(ctx, durable.SaveRequest{
		Guard: snapshot.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseToolsReady,
		Checkpoint: agent.Checkpoint{PendingToolCalls: []agent.ToolCall{call}},
	})
	if err != nil {
		t.Fatalf("Save(tools ready) error = %v", err)
	}
	return snapshot
}

func testDurableBegin(t *testing.T, factory DurableStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	request := durableBeginRequest("run-1")
	first, created, err := store.Begin(ctx, request)
	if err != nil || !created {
		t.Fatalf("Begin() created = %v, error = %v", created, err)
	}
	replayed, created, err := store.Begin(ctx, request)
	if err != nil || created || replayed.Revision != first.Revision {
		t.Fatalf("replayed Begin() = %+v, created %v, error %v", replayed, created, err)
	}

	tests := []struct {
		name   string
		mutate func(durable.BeginRequest) durable.BeginRequest
	}{
		{name: "a different config digest", mutate: func(r durable.BeginRequest) durable.BeginRequest {
			r.ConfigDigest = "sha256:other"
			return r
		}},
		{name: "a different input digest", mutate: func(r durable.BeginRequest) durable.BeginRequest {
			r.InputDigest = "sha256:other"
			return r
		}},
		{name: "the same session and request under a new run key", mutate: func(r durable.BeginRequest) durable.BeginRequest {
			r.Identity.RunKey = "run-2"
			return r
		}},
		{name: "a missing immutable field", mutate: func(r durable.BeginRequest) durable.BeginRequest {
			r.Identity.AgentKey = ""
			return r
		}},
	}
	for _, test := range tests {
		t.Run(test.name+" is a conflict", func(t *testing.T) {
			if _, _, err := store.Begin(ctx, test.mutate(request)); !errors.Is(err, durable.ErrRunConflict) {
				t.Fatalf("Begin() error = %v, want durable.ErrRunConflict", err)
			}
		})
	}
	if _, err := store.Load(ctx, "absent"); !errors.Is(err, durable.ErrRunNotFound) {
		t.Fatalf("Load(absent) error = %v, want durable.ErrRunNotFound", err)
	}
}

func testDurableLeases(t *testing.T, factory DurableStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	durableBegin(t, store, "run-1")
	leased := durableAcquire(t, store, "run-1", "worker-1")
	if leased.FenceToken == 0 || leased.Revision == 0 {
		t.Fatalf("Acquire() did not advance the fence and revision: %+v", leased)
	}
	now := time.Now().UTC()
	if _, err := store.Acquire(ctx, durable.AcquireRequest{
		RunKey: "run-1", Owner: "worker-2", Now: now, LeaseUntil: now.Add(time.Minute),
	}); !errors.Is(err, durable.ErrLeaseHeld) {
		t.Fatalf("Acquire() while a lease is live error = %v, want durable.ErrLeaseHeld", err)
	}

	renewed, err := store.Renew(ctx, leased.Guard(), now, now.Add(2*time.Minute))
	if err != nil || renewed.Revision != leased.Revision+1 {
		t.Fatalf("Renew() = %+v, error %v", renewed, err)
	}
	// Renewing with the guard that was already spent is exactly what a duplicated
	// worker would do, and it must not succeed.
	if _, err := store.Renew(ctx, leased.Guard(), now, now.Add(2*time.Minute)); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("Renew() with a stale guard error = %v, want durable.ErrLeaseLost", err)
	}

	released, err := store.Release(ctx, renewed.Guard(), durable.PhaseModelReady)
	if err != nil || released.Status != durable.StatusSuspended || released.LeaseOwner != "" {
		t.Fatalf("Release() = %+v, error %v", released, err)
	}
	// Suspension is only safe where nothing is in flight.
	durableBegin(t, store, "run-2")
	inflight := durableAcquire(t, store, "run-2", "worker-1")
	if _, err := store.Release(ctx, inflight.Guard(), durable.PhaseModelInflight); !errors.Is(err, durable.ErrInvalidTransition) {
		t.Fatalf("Release(model inflight) error = %v, want durable.ErrInvalidTransition", err)
	}
}

func testDurableSave(t *testing.T, factory DurableStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	durableBegin(t, store, "run-1")
	leased := durableAcquire(t, store, "run-1", "worker-1")

	tests := []struct {
		name    string
		request durable.SaveRequest
	}{
		{name: "model ready cannot jump straight to tools ready",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseToolsReady}},
		{name: "completion requires the finalizing phase",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusCompleted, Phase: durable.PhaseTerminal}},
		{name: "a running run cannot carry a failure",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight, Failure: &durable.Failure{Message: "boom"}}},
		{name: "a claimed status cannot be saved",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusClaimed, Phase: durable.PhaseModelReady}},
		{name: "a suspended run cannot sit in an inflight phase",
			request: durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusSuspended, Phase: durable.PhaseModelInflight}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.Save(ctx, test.request); !errors.Is(err, durable.ErrInvalidTransition) {
				t.Fatalf("Save() error = %v, want durable.ErrInvalidTransition", err)
			}
		})
	}

	inflight, err := store.Save(ctx, durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight})
	if err != nil || inflight.Revision != leased.Revision+1 {
		t.Fatalf("Save() = %+v, error %v", inflight, err)
	}
	if _, err := store.Save(ctx, durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight}); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("Save() with a spent guard error = %v, want durable.ErrLeaseLost", err)
	}
	foreign := inflight.Guard()
	foreign.LeaseOwner = "worker-2"
	if _, err := store.Save(ctx, durable.SaveRequest{Guard: foreign, Status: durable.StatusRunning, Phase: durable.PhaseModelInflight}); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("Save() with a foreign owner error = %v, want durable.ErrLeaseLost", err)
	}

	finalizing, err := store.Save(ctx, durable.SaveRequest{Guard: inflight.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseFinalizing})
	if err != nil {
		t.Fatalf("Save(finalizing) error = %v", err)
	}
	done, err := store.Save(ctx, durable.SaveRequest{Guard: finalizing.Guard(), Status: durable.StatusCompleted, Phase: durable.PhaseTerminal})
	if err != nil || !done.Terminal() || done.LeaseOwner != "" {
		t.Fatalf("Save(completed) = %+v, error %v", done, err)
	}
	// A terminal run is immutable, and its lease can never be re-acquired.
	if _, err := store.Save(ctx, durable.SaveRequest{Guard: done.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelReady}); !errors.Is(err, durable.ErrTerminal) {
		t.Fatalf("Save() on a terminal run error = %v, want durable.ErrTerminal", err)
	}
	now := time.Now().UTC()
	if _, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "run-1", Owner: "worker-2", Now: now, LeaseUntil: now.Add(time.Minute)}); !errors.Is(err, durable.ErrTerminal) {
		t.Fatalf("Acquire() on a terminal run error = %v, want durable.ErrTerminal", err)
	}
}

func testDurableRevoke(t *testing.T, factory DurableStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	call := agent.ToolCall{ID: "call-1", Name: "probe", Input: `{}`}
	snapshot := durableToolsPhase(t, store, "run-1", call)
	record, _, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", ToolCall: call, PreparedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("PrepareEffect() error = %v", err)
	}
	if _, err := store.BeginEffect(ctx, snapshot.Guard(), record.ExecutionKey, time.Now().UTC()); err != nil {
		t.Fatalf("BeginEffect() error = %v", err)
	}

	now := time.Now().UTC()
	if _, err := store.RevokeLease(ctx, durable.RevokeLeaseRequest{
		RunKey: "run-1", ExpectedRevision: snapshot.Revision, ExpectedFence: snapshot.FenceToken,
		Now: now, RequireExpired: true,
	}); !errors.Is(err, durable.ErrLeaseHeld) {
		t.Fatalf("RevokeLease(require expired) on a live lease error = %v, want durable.ErrLeaseHeld", err)
	}
	revoked, err := store.RevokeLease(ctx, durable.RevokeLeaseRequest{
		RunKey: "run-1", ExpectedRevision: snapshot.Revision, ExpectedFence: snapshot.FenceToken, Now: now,
	})
	if err != nil {
		t.Fatalf("RevokeLease() error = %v", err)
	}
	if revoked.FenceToken != snapshot.FenceToken+1 {
		t.Fatalf("RevokeLease() did not raise the fence: %d then %d", snapshot.FenceToken, revoked.FenceToken)
	}
	if revoked.Status != durable.StatusSuspended || revoked.Phase != durable.PhaseToolsReady {
		t.Fatalf("RevokeLease() = %s/%s, want suspended/tools_ready", revoked.Status, revoked.Phase)
	}
	// The whole point of revoking atomically: an effect that was running is now
	// ambiguous, and must be recorded as such rather than silently retried.
	unknown, err := store.LoadEffect(ctx, record.ExecutionKey)
	if err != nil || unknown.Status != durable.EffectUnknown {
		t.Fatalf("LoadEffect() after revoke = %+v, error %v", unknown, err)
	}
	resumed := durableAcquire(t, store, "run-1", "worker-2")
	if _, err := store.BeginEffect(ctx, resumed.Guard(), record.ExecutionKey, time.Now().UTC()); !errors.Is(err, durable.ErrToolEffectUnknown) {
		t.Fatalf("BeginEffect() on an unknown effect error = %v, want durable.ErrToolEffectUnknown", err)
	}
	if replayer, ok := store.(durable.EffectReplayer); ok {
		replayed, err := replayer.ReplayEffect(ctx, resumed.Guard(), record.ExecutionKey, time.Now().UTC())
		if err != nil || replayed.Status != durable.EffectRunning || replayed.FenceToken != resumed.FenceToken {
			t.Fatalf("ReplayEffect() = %+v, error %v", replayed, err)
		}
	}
	if _, err := store.RevokeLease(ctx, durable.RevokeLeaseRequest{
		RunKey: "run-1", ExpectedRevision: snapshot.Revision, ExpectedFence: snapshot.FenceToken, Now: now,
	}); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("RevokeLease() with a stale expectation error = %v, want durable.ErrLeaseLost", err)
	}
}

func testDurableScan(t *testing.T, factory DurableStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	for _, key := range []string{"run-1", "run-2"} {
		durableBegin(t, store, key)
	}
	past := time.Now().UTC().Add(-time.Hour)
	if _, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "run-1", Owner: "worker-1", Now: past, LeaseUntil: past.Add(time.Second)}); err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	durableAcquire(t, store, "run-2", "worker-2")

	expired, err := store.Scan(ctx, durable.ScanRequest{Limit: 10, ExpiredBefore: time.Now().UTC()})
	if err != nil || len(expired.Snapshots) != 1 || expired.Snapshots[0].Identity.RunKey != "run-1" {
		t.Fatalf("Scan(expired) = %+v, error %v", expired, err)
	}
	running, err := store.Scan(ctx, durable.ScanRequest{Limit: 10, Statuses: []durable.Status{durable.StatusRunning}})
	if err != nil || len(running.Snapshots) != 2 {
		t.Fatalf("Scan(running) = %+v, error %v", running, err)
	}
	page, err := store.Scan(ctx, durable.ScanRequest{Limit: 1})
	if err != nil || len(page.Snapshots) != 1 || page.Next == "" {
		t.Fatalf("Scan(limit 1) = %+v, error %v", page, err)
	}
	next, err := store.Scan(ctx, durable.ScanRequest{Limit: 1, Cursor: page.Next})
	if err != nil || len(next.Snapshots) != 1 || next.Snapshots[0].Identity.RunKey == page.Snapshots[0].Identity.RunKey {
		t.Fatalf("Scan(cursor) = %+v, error %v", next, err)
	}
	if _, err := store.Scan(ctx, durable.ScanRequest{Limit: 0}); !errors.Is(err, durable.ErrLimitExceeded) {
		t.Fatalf("Scan(limit 0) error = %v, want durable.ErrLimitExceeded", err)
	}
}

func testDurableEffects(t *testing.T, factory DurableStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	call := agent.ToolCall{ID: "call-1", Name: "probe", Input: `{"path":"a.go"}`}
	snapshot := durableToolsPhase(t, store, "run-1", call)

	record, created, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", ToolCall: call, PreparedAt: time.Now().UTC(),
	})
	if err != nil || !created || record.Status != durable.EffectPrepared {
		t.Fatalf("PrepareEffect() = %+v, created %v, error %v", record, created, err)
	}
	// The ledger key must be the runtime's key, or the two layers would dedupe on
	// different anchors and a retry would run the tool twice.
	execution, err := agent.NewToolExecution(agent.RunIdentity(durableIdentity("run-1")), 0, 0, call)
	if err != nil {
		t.Fatalf("NewToolExecution() error = %v", err)
	}
	if execution.IdempotencyKey != string(record.ExecutionKey) {
		t.Fatalf("execution key = %q, runtime idempotency key = %q", record.ExecutionKey, execution.IdempotencyKey)
	}
	if _, created, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", ToolCall: call, PreparedAt: time.Now().UTC().Add(time.Second),
	}); err != nil || created {
		t.Fatalf("replayed PrepareEffect() created = %v, error %v", created, err)
	}
	if _, _, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: snapshot.Guard(), AttemptKey: "attempt-1", StepNumber: 0, Ordinal: 0,
		ToolCall: agent.ToolCall{ID: "call-2", Name: "probe", Input: `{"path":"b.go"}`}, PreparedAt: time.Now().UTC(),
	}); !errors.Is(err, durable.ErrEffectConflict) {
		t.Fatalf("PrepareEffect() reusing a step ordinal error = %v, want durable.ErrEffectConflict", err)
	}

	started, err := store.BeginEffect(ctx, snapshot.Guard(), record.ExecutionKey, time.Now().UTC())
	if err != nil || started.Status != durable.EffectRunning || started.FenceToken != snapshot.FenceToken {
		t.Fatalf("BeginEffect() = %+v, error %v", started, err)
	}
	if _, err := store.BeginEffect(ctx, snapshot.Guard(), record.ExecutionKey, time.Now().UTC()); err != nil {
		t.Fatalf("BeginEffect() is not idempotent under the same fence: %v", err)
	}
	if _, err := store.BeginEffect(ctx, snapshot.Guard(), "tool:absent", time.Now().UTC()); !errors.Is(err, durable.ErrEffectNotFound) {
		t.Fatalf("BeginEffect(absent) error = %v, want durable.ErrEffectNotFound", err)
	}

	finishedAt := time.Now().UTC()
	result := agent.ToolResult{ToolCallID: "call-1", Name: "probe", Content: "ok"}
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
		Guard: snapshot.Guard(), ExecutionKey: record.ExecutionKey,
		Failure: &durable.EffectFailure{Message: "different"}, FinishedAt: finishedAt,
	}); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("conflicting CompleteEffect() error = %v, want durable.ErrLeaseLost", err)
	}
	if _, err := store.CompleteEffect(ctx, durable.CompleteEffectRequest{
		Guard: snapshot.Guard(), ExecutionKey: record.ExecutionKey, FinishedAt: finishedAt,
	}); !errors.Is(err, durable.ErrInvalidTransition) {
		t.Fatalf("CompleteEffect() without a result or failure error = %v, want durable.ErrInvalidTransition", err)
	}
	if _, err := store.MarkEffectUnknown(ctx, snapshot.Guard(), record.ExecutionKey, finishedAt); !errors.Is(err, durable.ErrInvalidTransition) {
		t.Fatalf("MarkEffectUnknown() on a settled effect error = %v, want durable.ErrInvalidTransition", err)
	}
	effects, err := store.ListEffects(ctx, "run-1")
	if err != nil || len(effects) != 1 || effects[0].Status != durable.EffectSucceeded {
		t.Fatalf("ListEffects() = %+v, error %v", effects, err)
	}
}

func testDurableUsage(t *testing.T, factory DurableStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	occurredAt := time.Now().UTC().Truncate(time.Microsecond)
	fact := durable.UsageFact{
		TenantKey: "tenant-1", UsageKey: durable.UsageFactKey("tenant-1", "attempt-1", "", "model"),
		RunKey: "run-1", AttemptKey: "attempt-1", Kind: "model", Provider: "conformance",
		Model: "test-model", PricingVersion: "v1", InputTokens: 12, OutputTokens: 3,
		CostMicros: 40, Currency: "USD", OccurredAt: occurredAt,
	}
	stored, created, err := store.RecordUsage(ctx, fact)
	if err != nil || !created || stored.InputTokens != 12 {
		t.Fatalf("RecordUsage() = %+v, created %v, error %v", stored, created, err)
	}
	if _, created, err := store.RecordUsage(ctx, fact); err != nil || created {
		t.Fatalf("replayed RecordUsage() created = %v, error %v", created, err)
	}
	// A usage fact is evidence, not a counter. The same key with different
	// numbers means someone is about to bill twice.
	conflicting := fact
	conflicting.OutputTokens = 4
	if _, _, err := store.RecordUsage(ctx, conflicting); !errors.Is(err, durable.ErrUsageConflict) {
		t.Fatalf("conflicting RecordUsage() error = %v, want durable.ErrUsageConflict", err)
	}
	if _, _, err := store.RecordUsage(ctx, durable.UsageFact{TenantKey: "tenant-1"}); !errors.Is(err, durable.ErrUsageConflict) {
		t.Fatalf("incomplete RecordUsage() error = %v, want durable.ErrUsageConflict", err)
	}
	negative := fact
	negative.UsageKey = "usage-negative"
	negative.InputTokens = -1
	if _, _, err := store.RecordUsage(ctx, negative); !errors.Is(err, durable.ErrUsageConflict) {
		t.Fatalf("negative RecordUsage() error = %v, want durable.ErrUsageConflict", err)
	}
	loaded, err := store.LoadUsage(ctx, "tenant-1", fact.UsageKey)
	if err != nil || !loaded.OccurredAt.Equal(occurredAt) || loaded.CostMicros != 40 {
		t.Fatalf("LoadUsage() = %+v, error %v", loaded, err)
	}
	if _, err := store.LoadUsage(ctx, "tenant-1", "absent"); !errors.Is(err, durable.ErrUsageNotFound) {
		t.Fatalf("LoadUsage(absent) error = %v, want durable.ErrUsageNotFound", err)
	}
	facts, err := store.ListAttemptUsage(ctx, "tenant-1", "attempt-1")
	if err != nil || len(facts) != 1 {
		t.Fatalf("ListAttemptUsage() = %+v, error %v", facts, err)
	}
	if other, err := store.ListAttemptUsage(ctx, "tenant-2", "attempt-1"); err != nil || len(other) != 0 {
		t.Fatalf("ListAttemptUsage() leaked across tenants: %+v, error %v", other, err)
	}
}
