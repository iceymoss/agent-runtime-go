package icoder

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

func newSubagentStore(t *testing.T) *SQLiteSubagentStore {
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
	return NewSQLiteSubagentStore(store.db)
}

// TestSQLiteSubagentStoreConformance holds the adapter to the same child-run
// contract as the library's reference store. Depth, fan-out, cycle and budget
// are safety limits, so an adapter that drifts here would authorize delegation
// the caller never allowed.
func TestSQLiteSubagentStoreConformance(t *testing.T) {
	agenttest.TestSubagentStore(t, func(t *testing.T) subagent.Store { return newSubagentStore(t) })
}

// TestSQLiteSubagentStoreSurvivesReopen is the reason this adapter exists: a
// parked parent is only resumable if the relationship it is waiting on is still
// there after a restart.
func TestSQLiteSubagentStoreSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "icoder.db")
	opened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()
	request := subagent.SpawnRequest{
		RequestKey: "restart",
		Parent: subagent.ParentRef{
			TenantKey: tenantKey, SessionKey: "session", RunKey: "parent-run", TreeKey: "tree/parent-run",
		},
		AgentKey: "icoder.explorer", Input: []byte("task"),
		Limits:  subagent.Limits{MaxDepth: 2, MaxFanout: 2, MaxInputTokens: 100, MaxOutputTokens: 100, MaxCostMicros: 100, MaxToolCalls: 10, MaxRuntime: time.Hour},
		Reserve: subagent.Reservation{InputTokens: 10, OutputTokens: 10, CostMicros: 10, ToolCalls: 1, Runtime: time.Minute},
	}
	receipt, created, err := opened.SubagentStore().Spawn(ctx, request, now)
	if err != nil || !created {
		t.Fatalf("Spawn() created = %v, error = %v", created, err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	}()
	store := reopened.SubagentStore()
	snapshot, err := store.Get(ctx, tenantKey, receipt.Child.RelationshipKey)
	if err != nil || snapshot.State != subagent.ChildQueued || snapshot.AgentKey != "icoder.explorer" {
		t.Fatalf("Get() after reopen = %+v, error %v", snapshot, err)
	}
	// The reservation survived too, so the restarted process cannot hand the same
	// budget out twice.
	if _, _, err := store.Spawn(ctx, request, now); err != nil {
		t.Fatalf("replayed Spawn() after reopen error = %v", err)
	}
	sibling := request
	sibling.RequestKey = "restart-sibling"
	sibling.Reserve.CostMicros = 95
	if _, _, err := store.Spawn(ctx, sibling, now); !errors.Is(err, subagent.ErrCostBudgetExceeded) {
		t.Fatalf("Spawn() over the surviving budget error = %v, want subagent.ErrCostBudgetExceeded", err)
	}

	// A worker in the new process can still pick the child up and finish it.
	claimed, ok, err := store.ClaimNext(ctx, subagent.ClaimRequest{TenantKey: tenantKey, Owner: "worker", Now: now, Lease: time.Minute})
	if err != nil || !ok || claimed.Receipt.Child.RelationshipKey != receipt.Child.RelationshipKey {
		t.Fatalf("ClaimNext() after reopen = %+v, ok %v, error %v", claimed, ok, err)
	}
	if _, err := store.CommitTerminal(ctx, subagent.TerminalCommand{
		TenantKey: tenantKey, RelationshipKey: receipt.Child.RelationshipKey,
		ExpectedVersion: claimed.Version, Owner: "worker",
		Result:      subagent.RunResult{State: subagent.ChildCompleted, ResultRef: "result", UsageFactKey: "usage", Usage: subagent.Usage{InputTokens: 3}},
		CompletedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("CommitTerminal() after reopen error = %v", err)
	}
	wake, ok, err := store.ClaimWake(ctx, tenantKey)
	if err != nil || !ok || wake.Request.Child.RelationshipKey != receipt.Child.RelationshipKey {
		t.Fatalf("ClaimWake() after reopen = %+v, ok %v, error %v", wake, ok, err)
	}
}
