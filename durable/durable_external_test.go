package durable_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
)

func TestSnapshotV1ExactJSONAndDigestParity(t *testing.T) {
	snapshot := durable.Snapshot{
		SchemaVersion: 1,
		Identity:      durable.Identity{RunKey: "run-fixture", AgentKey: "agent-fixture", SessionID: "session-fixture", RequestID: "request-fixture"},
		InputDigest:   "sha256:input", ConfigDigest: "sha256:config", Status: durable.StatusRunning,
		Phase: durable.PhaseToolsReady, Revision: 9, FenceToken: 4, LeaseOwner: "worker-北京",
		LeaseUntil: time.Date(2026, 8, 1, 1, 2, 3, 456000000, time.FixedZone("fixture", 8*60*60)),
		Checkpoint: agent.Checkpoint{
			History: []agent.Message{agent.NewUserMessage("你好")}, NewMessages: []agent.Message{}, CompletedSteps: []agent.StepResult{},
			PendingToolCalls: []agent.ToolCall{{ID: "call-一", Name: "first", Input: `{"value":1}`}, {ID: "call-二", Name: "second", Input: `{"value":2}`}},
			Outcome:          agent.RunResult{Messages: []agent.Message{}, Steps: []agent.StepResult{}},
		},
	}
	want := []byte(`{"schema_version":1,"identity":{"run_key":"run-fixture","agent_key":"agent-fixture","session_id":"session-fixture","request_id":"request-fixture"},"input_digest":"sha256:input","config_digest":"sha256:config","status":"running","phase":"tools_ready","revision":9,"fence_token":4,"lease_owner":"worker-北京","lease_until":"2026-08-01T01:02:03.456+08:00","checkpoint":{"history":[{"role":"user","parts":[{"type":"text","text":"你好"}]}],"new_messages":[],"completed_steps":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"repair_count":0,"next_step":0,"pending_tool_calls":[{"id":"call-一","name":"first","input":"{\"value\":1}"},{"id":"call-二","name":"second","input":"{\"value\":2}"}],"outcome":{"messages":[],"steps":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"text":"","stop_reason":"","outcome":""}}}`)
	got, err := durable.MarshalSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("snapshot JSON:\n%s\nwant:\n%s", got, want)
	}
	roundTrip, err := durable.UnmarshalSnapshot(got)
	if err != nil {
		t.Fatal(err)
	}
	roundTripJSON, err := durable.MarshalSnapshot(roundTrip)
	if err != nil || !bytes.Equal(roundTripJSON, got) {
		t.Fatalf("round trip JSON = %s, %v", roundTripJSON, err)
	}
	var generic map[string]any
	if err := json.Unmarshal(got, &generic); err != nil {
		t.Fatal(err)
	}
	if _, err := durable.UnmarshalSnapshot([]byte(`{"schema_version":1,"unknown":true}`)); !errors.Is(err, durable.ErrSnapshotSchema) {
		t.Fatalf("strict decode error = %v", err)
	}

	identity := durable.Identity{RunKey: "run-123", AgentKey: "assistant.support", SessionID: "session-7", RequestID: "request-9"}
	key := durable.ToolExecutionKey(identity, 2, 1, agent.ToolCall{ID: "call-4", Name: "score", Input: `{"a":1,"b":2}`})
	if key != "tool:a6fe12335d4ca867f31c23a04b960ffb5c0bdafe6f3607824e90c0a54a32669b" {
		t.Fatalf("execution key = %q", key)
	}
}

func TestMemoryStoreBeginDeepCopyLeaseCASAndTerminal(t *testing.T) {
	ctx := context.Background()
	store := durable.NewMemoryStore()
	request := beginRequest("run-store")
	started, created, err := store.Begin(ctx, request)
	if err != nil || !created {
		t.Fatalf("Begin = %#v, %v, %v", started, created, err)
	}
	request.Checkpoint.History[0].Parts[0].Text = "mutated"
	loaded, _ := store.Load(ctx, "run-store")
	if loaded.Checkpoint.History[0].Text() != "hello" {
		t.Fatal("Begin retained caller alias")
	}
	if _, created, err := store.Begin(ctx, beginRequest("run-store")); err != nil || created {
		t.Fatalf("idempotent Begin = %v, %v", created, err)
	}
	conflict := beginRequest("run-store")
	conflict.ConfigDigest = "sha256:different"
	if _, _, err := store.Begin(ctx, conflict); !errors.Is(err, durable.ErrRunConflict) {
		t.Fatalf("mismatch error = %v", err)
	}

	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	leased, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "run-store", Owner: "one", Now: now, LeaseUntil: now.Add(time.Minute)})
	if err != nil || leased.FenceToken != 1 || leased.Revision != 1 {
		t.Fatalf("Acquire = %#v, %v", leased, err)
	}
	if _, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "run-store", Owner: "two", Now: now, LeaseUntil: now.Add(time.Minute)}); !errors.Is(err, durable.ErrLeaseHeld) {
		t.Fatalf("active race error = %v", err)
	}
	renewed, err := store.Renew(ctx, leased.Guard(), now.Add(time.Second), now.Add(2*time.Minute))
	if err != nil || renewed.Revision != 2 {
		t.Fatalf("Renew = %#v, %v", renewed, err)
	}
	if _, err := store.Save(ctx, durable.SaveRequest{Guard: leased.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight}); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("stale revision error = %v", err)
	}
	finalizing, err := store.Save(ctx, durable.SaveRequest{Guard: renewed.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseModelInflight, Checkpoint: renewed.Checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	finalizing, err = store.Save(ctx, durable.SaveRequest{Guard: finalizing.Guard(), Status: durable.StatusRunning, Phase: durable.PhaseFinalizing, Checkpoint: finalizing.Checkpoint})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := store.Save(ctx, durable.SaveRequest{Guard: finalizing.Guard(), Status: durable.StatusCompleted, Phase: durable.PhaseTerminal, Checkpoint: finalizing.Checkpoint})
	if err != nil || !completed.Terminal() || completed.LeaseOwner != "" {
		t.Fatalf("complete = %#v, %v", completed, err)
	}
	if _, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "run-store", Owner: "three", Now: now, LeaseUntil: now.Add(time.Minute)}); !errors.Is(err, durable.ErrTerminal) {
		t.Fatalf("terminal acquire error = %v", err)
	}
}

func TestLeaseRaceAndExpiredFence(t *testing.T) {
	ctx := context.Background()
	store := durable.NewMemoryStore()
	if _, _, err := store.Begin(ctx, beginRequest("race")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var wg sync.WaitGroup
	winners := make(chan durable.Snapshot, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			snapshot, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "race", Owner: owner, Now: now, LeaseUntil: now.Add(time.Second)})
			if err == nil {
				winners <- snapshot
			} else if !errors.Is(err, durable.ErrLeaseHeld) {
				t.Errorf("Acquire error = %v", err)
			}
		}(string(rune('a' + i)))
	}
	wg.Wait()
	close(winners)
	var first durable.Snapshot
	count := 0
	for winner := range winners {
		first, count = winner, count+1
	}
	if count != 1 {
		t.Fatalf("winners = %d", count)
	}
	second, err := store.Acquire(ctx, durable.AcquireRequest{RunKey: "race", Owner: "new", Now: now.Add(2 * time.Second), LeaseUntil: now.Add(3 * time.Second)})
	if err != nil || second.FenceToken != first.FenceToken+1 {
		t.Fatalf("takeover = %#v, %v", second, err)
	}
	if _, err := store.Release(ctx, first.Guard(), durable.PhaseModelReady); !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("stale release error = %v", err)
	}
}

func TestEffectLedgerCrashPointsIdempotencyAndUnknown(t *testing.T) {
	ctx := context.Background()
	store, leased := leasedStore(t, "effects")
	now := time.Now().UTC()
	prepare := durable.PrepareEffectRequest{
		Guard: leased.Guard(), AttemptKey: "attempt-1", StepNumber: 1, Ordinal: 0,
		ToolCall: agent.ToolCall{ID: "call-1", Name: "charge", Input: `{"amount":1}`}, PreparedAt: now,
	}
	record, created, err := store.PrepareEffect(ctx, prepare)
	if err != nil || !created || record.Status != durable.EffectPrepared {
		t.Fatalf("prepare = %#v, %v, %v", record, created, err)
	}
	if replay, created, err := store.PrepareEffect(ctx, prepare); err != nil || created || replay.Digest != record.Digest {
		t.Fatalf("prepare replay = %#v, %v, %v", replay, created, err)
	}
	running, err := store.BeginEffect(ctx, leased.Guard(), record.ExecutionKey, now.Add(time.Second))
	if err != nil || running.Status != durable.EffectRunning || running.FenceToken != leased.FenceToken {
		t.Fatalf("begin = %#v, %v", running, err)
	}
	if replay, err := store.BeginEffect(ctx, leased.Guard(), record.ExecutionKey, now.Add(2*time.Second)); err != nil || replay.StartedAt != running.StartedAt {
		t.Fatalf("begin replay = %#v, %v", replay, err)
	}
	unknown, err := store.MarkEffectUnknown(ctx, leased.Guard(), record.ExecutionKey, now.Add(3*time.Second))
	if err != nil || unknown.Status != durable.EffectUnknown {
		t.Fatalf("unknown = %#v, %v", unknown, err)
	}
	if _, err := store.BeginEffect(ctx, leased.Guard(), record.ExecutionKey, now.Add(4*time.Second)); !errors.Is(err, durable.ErrToolEffectUnknown) {
		t.Fatalf("unknown replay error = %v", err)
	}

	other := prepare
	other.Ordinal = 1
	other.ToolCall = agent.ToolCall{ID: "call-2", Name: "lookup", Input: `{}`}
	prepared, _, err := store.PrepareEffect(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginEffect(ctx, leased.Guard(), prepared.ExecutionKey, now); err != nil {
		t.Fatal(err)
	}
	result := &agent.ToolResult{ToolCallID: "call-2", Name: "lookup", Content: "ok"}
	completion := durable.CompleteEffectRequest{Guard: leased.Guard(), ExecutionKey: prepared.ExecutionKey, Result: result, FinishedAt: now.Add(time.Second)}
	succeeded, err := store.CompleteEffect(ctx, completion)
	if err != nil || succeeded.Status != durable.EffectSucceeded {
		t.Fatalf("complete = %#v, %v", succeeded, err)
	}
	if replay, err := store.CompleteEffect(ctx, completion); err != nil || replay.Status != durable.EffectSucceeded {
		t.Fatalf("complete replay = %#v, %v", replay, err)
	}
}

func TestUsageLedgerImmutableDelta(t *testing.T) {
	ctx := context.Background()
	store := durable.NewMemoryStore()
	fact := durable.UsageFact{
		TenantKey: "tenant", RunKey: "run", AttemptKey: "attempt", Kind: "model", Provider: "provider", Model: "model",
		PricingVersion: "v1", InputTokens: 10, OutputTokens: 2, CostMicros: 12, Currency: "USD", OccurredAt: time.Now().UTC(),
	}
	fact.UsageKey = durable.UsageFactKey(fact.TenantKey, fact.AttemptKey, fact.ExecutionKey, fact.Kind)
	if _, created, err := store.RecordUsage(ctx, fact); err != nil || !created {
		t.Fatalf("record = %v, %v", created, err)
	}
	if _, created, err := store.RecordUsage(ctx, fact); err != nil || created {
		t.Fatalf("replay = %v, %v", created, err)
	}
	changed := fact
	changed.OutputTokens++
	if _, _, err := store.RecordUsage(ctx, changed); !errors.Is(err, durable.ErrUsageConflict) {
		t.Fatalf("changed delta error = %v", err)
	}
	facts, err := store.ListAttemptUsage(ctx, "tenant", "attempt")
	if err != nil || len(facts) != 1 || facts[0].CostMicros != 12 {
		t.Fatalf("facts = %#v, %v", facts, err)
	}
}

func TestReconcilerRevokesExpiredAndSuspendsUnknown(t *testing.T) {
	ctx := context.Background()
	store, leased := leasedStore(t, "reconcile")
	now := leased.LeaseUntil.Add(time.Second)
	prepared, _, err := store.PrepareEffect(ctx, durable.PrepareEffectRequest{
		Guard: leased.Guard(), AttemptKey: "attempt", StepNumber: 1, ToolCall: agent.ToolCall{ID: "call", Name: "effect", Input: `{}`}, PreparedAt: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginEffect(ctx, leased.Guard(), prepared.ExecutionKey, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	reconciler, err := durable.NewReconciler(durable.ReconcilerOptions{Store: store, Effects: store, Work: sink, MaxBatch: 10})
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.ReconcileBatch(ctx, durable.ReconcileRequest{Now: now, Limit: 10})
	if err != nil || report.Revoked != 1 || report.UnknownEffects != 1 || report.Enqueued != 0 || report.Classifications[durable.ClassificationOperatorRequired] != 1 {
		t.Fatalf("report = %#v, %v", report, err)
	}
	snapshot, _ := store.Load(ctx, "reconcile")
	effect, _ := store.LoadEffect(ctx, prepared.ExecutionKey)
	if snapshot.Status != durable.StatusSuspended || snapshot.Phase != durable.PhaseToolsReady || snapshot.FenceToken != leased.FenceToken+1 || effect.Status != durable.EffectUnknown {
		t.Fatalf("recovered snapshot/effect = %#v / %#v", snapshot, effect)
	}
	second, err := reconciler.ReconcileBatch(ctx, durable.ReconcileRequest{Now: now.Add(time.Second), Limit: 10})
	if err != nil || second.Enqueued != 0 || len(sink.items) != 0 || second.Classifications[durable.ClassificationOperatorRequired] != 1 {
		t.Fatalf("second report = %#v, %v", second, err)
	}
}

func TestReconcilerBlockerAndBoundedShutdownReports(t *testing.T) {
	ctx := context.Background()
	store, leased := leasedStore(t, "blocked")
	if _, err := store.Release(ctx, leased.Guard(), durable.PhaseModelReady); err != nil {
		t.Fatal(err)
	}
	sink := &memorySink{}
	reconciler, err := durable.NewReconciler(durable.ReconcilerOptions{
		Store: store, Effects: store, Blockers: fixedBlocker{active: true}, Work: sink, MaxBatch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := reconciler.ReconcileBatch(ctx, durable.ReconcileRequest{Now: time.Now().UTC(), Limit: 1})
	if err != nil || report.Classifications[durable.ClassificationBlocked] != 1 || report.Enqueued != 0 {
		t.Fatalf("blocked report = %#v, %v", report, err)
	}
	refs := []durable.RunRef{{RunKey: "blocked"}, {RunKey: "other"}}
	checkpoint := reconciler.Checkpoint(ctx, refs, "shutdown")
	if !errors.Is(checkpoint.Err, durable.ErrLimitExceeded) || len(checkpoint.Remaining) != 2 {
		t.Fatalf("bounded checkpoint = %#v", checkpoint)
	}
	revoke := reconciler.Revoke(ctx, refs, "shutdown", time.Now().UTC())
	if !errors.Is(revoke.Err, durable.ErrLimitExceeded) || len(revoke.Remaining) != 2 {
		t.Fatalf("bounded revoke = %#v", revoke)
	}
}

func beginRequest(key string) durable.BeginRequest {
	return durable.BeginRequest{
		Identity:    durable.Identity{RunKey: key, AgentKey: "agent", SessionID: "session-" + key, RequestID: "request"},
		InputDigest: "sha256:input", ConfigDigest: "sha256:config", Checkpoint: agent.Checkpoint{History: []agent.Message{agent.NewUserMessage("hello")}},
	}
}

func leasedStore(t *testing.T, key string) (*durable.MemoryStore, durable.Snapshot) {
	t.Helper()
	store := durable.NewMemoryStore()
	if _, _, err := store.Begin(context.Background(), beginRequest(key)); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot, err := store.Acquire(context.Background(), durable.AcquireRequest{RunKey: durable.RunKey(key), Owner: "worker", Now: now, LeaseUntil: now.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return store, snapshot
}

type memorySink struct{ items []durable.WorkItem }

func (s *memorySink) Enqueue(_ context.Context, item durable.WorkItem) error {
	s.items = append(s.items, item)
	return nil
}

type fixedBlocker struct{ active bool }

func (b fixedBlocker) GetBlocker(context.Context, durable.RunRef) (durable.Blocker, bool, error) {
	return durable.Blocker{Kind: durable.BlockerApproval, Key: "approval", Digest: "sha256:blocker", Active: b.active}, true, nil
}
