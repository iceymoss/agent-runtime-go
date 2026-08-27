package agenttest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

// SubagentStoreFactory returns a fresh, empty child-run store for one subtest.
type SubagentStoreFactory func(t *testing.T) subagent.Store

// TestSubagentStore runs the child-run conformance suite.
//
// Depth, fan-out, cycle, and budget are safety limits: an adapter that enforces
// them slightly differently does not produce wrong data, it produces an agent
// that delegates further, wider, or more expensively than it was authorized to.
// The suite drives each limit to its boundary and past it, and it also checks the
// parts that only matter after a crash - claims that expire, terminals that
// commit before their wake, and usage that must settle exactly once.
func TestSubagentStore(t *testing.T, factory SubagentStoreFactory) {
	t.Helper()
	t.Run("spawn is idempotent and isolated", func(t *testing.T) { testSubagentSpawn(t, factory) })
	t.Run("the tree budget is reserved before a child runs", func(t *testing.T) { testSubagentBudget(t, factory) })
	t.Run("depth, fan-out, and cycles are refused", func(t *testing.T) { testSubagentLimits(t, factory) })
	t.Run("a claimed child runs once and can be recovered", func(t *testing.T) { testSubagentClaim(t, factory) })
	t.Run("usage settles exactly once", func(t *testing.T) { testSubagentSettlement(t, factory) })
	t.Run("cancellation is idempotent and ordered", func(t *testing.T) { testSubagentCancel(t, factory) })
	t.Run("a terminal child is woken at least once", func(t *testing.T) { testSubagentWake(t, factory) })
}

const (
	subagentTenantA = agent.TenantKey("tenant-a")
	subagentTenantB = agent.TenantKey("tenant-b")
)

func subagentLimits() subagent.Limits {
	return subagent.Limits{
		MaxDepth: 4, MaxFanout: 4, MaxInputTokens: 1000, MaxOutputTokens: 1000,
		MaxCostMicros: 1000, MaxToolCalls: 100, MaxRuntime: time.Hour,
	}
}

func subagentSpawn(key subagent.RequestKey, parentRun subagent.RunKey, limits subagent.Limits, reserve subagent.Reservation) subagent.SpawnRequest {
	return subagent.SpawnRequest{
		RequestKey: key,
		Parent: subagent.ParentRef{
			TenantKey: subagentTenantA, SessionKey: "parent-session", RunKey: parentRun, TreeKey: "tree/root",
		},
		AgentKey: "worker", Input: []byte("task"), Limits: limits, Reserve: reserve,
	}
}

func subagentNow() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

func testSubagentSpawn(t *testing.T, factory SubagentStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	request := subagentSpawn("same", "root", subagentLimits(), subagent.Reservation{InputTokens: 10})
	first, created, err := store.Spawn(ctx, request, subagentNow())
	if err != nil || !created {
		t.Fatalf("Spawn() created = %v, error = %v", created, err)
	}
	// The child's identity is derived, so a retry must address the same child
	// rather than starting a second one.
	if first.Child.RunKey != subagent.DeriveRunKey(subagentTenantA, "same") {
		t.Fatalf("Spawn() run key = %q, want the derived key", first.Child.RunKey)
	}
	second, created, err := store.Spawn(ctx, request, subagentNow())
	if err != nil || created || first != second {
		t.Fatalf("replayed Spawn() created = %v, error = %v, first = %+v, second = %+v", created, err, first, second)
	}

	request.Input[0] = 'X'
	if _, _, err := store.Spawn(ctx, request, subagentNow()); !errors.Is(err, subagent.ErrIdempotencyConflict) {
		t.Fatalf("Spawn() with different input error = %v, want subagent.ErrIdempotencyConflict", err)
	}
	if _, _, err := store.Spawn(ctx, subagent.SpawnRequest{RequestKey: "incomplete"}, subagentNow()); !errors.Is(err, subagent.ErrInvalidRequest) {
		t.Fatalf("Spawn() with an incomplete request error = %v, want subagent.ErrInvalidRequest", err)
	}

	snapshot, err := store.Get(ctx, subagentTenantA, first.Child.RelationshipKey)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	snapshot.Input[0] = 'Y'
	again, err := store.Get(ctx, subagentTenantA, first.Child.RelationshipKey)
	if err != nil || string(again.Input) != "task" {
		t.Fatalf("mutating a returned snapshot changed stored state: input = %q, error = %v", again.Input, err)
	}
	if _, err := store.Get(ctx, subagentTenantB, first.Child.RelationshipKey); !errors.Is(err, subagent.ErrNotFound) {
		t.Fatalf("Get() leaked across tenants: %v", err)
	}
}

func testSubagentBudget(t *testing.T, factory SubagentStoreFactory) {
	store := factory(t)
	limits := subagentLimits()
	limits.MaxFanout = 20
	limits.MaxCostMicros = 100

	// Reservation happens before a child runs, so concurrent siblings compete for
	// the tree budget up front. Exactly three of ten can fit in 100 at 30 each.
	var accepted atomic.Int64
	var unexpected atomic.Value
	var wait sync.WaitGroup
	for index := range 10 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			request := subagentSpawn(subagent.RequestKey(fmt.Sprintf("sibling-%d", index)), "root", limits, subagent.Reservation{CostMicros: 30})
			if _, _, err := store.Spawn(context.Background(), request, subagentNow()); err == nil {
				accepted.Add(1)
			} else if !errors.Is(err, subagent.ErrCostBudgetExceeded) {
				unexpected.Store(err)
			}
		}(index)
	}
	wait.Wait()
	if value := unexpected.Load(); value != nil {
		t.Fatalf("unexpected reservation error: %v", value)
	}
	if accepted.Load() != 3 {
		t.Fatalf("accepted %d reservations, want 3", accepted.Load())
	}

	// Changing the limits of an existing tree would silently rewrite what the
	// earlier siblings were admitted against.
	widened := limits
	widened.MaxCostMicros = 10_000
	if _, _, err := store.Spawn(context.Background(), subagentSpawn("widened", "root", widened, subagent.Reservation{}), subagentNow()); !errors.Is(err, subagent.ErrIdempotencyConflict) {
		t.Fatalf("Spawn() redefining the tree limits error = %v, want subagent.ErrIdempotencyConflict", err)
	}
}

func testSubagentLimits(t *testing.T, factory SubagentStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	limits := subagentLimits()
	limits.MaxDepth, limits.MaxFanout = 2, 1

	first, _, err := store.Spawn(ctx, subagentSpawn("first", "root", limits, subagent.Reservation{}), subagentNow())
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	if _, _, err := store.Spawn(ctx, subagentSpawn("fanout", "root", limits, subagent.Reservation{}), subagentNow()); !errors.Is(err, subagent.ErrFanoutExceeded) {
		t.Fatalf("Spawn() past the fan-out limit error = %v, want subagent.ErrFanoutExceeded", err)
	}

	nested := subagentSpawn("nested", first.Child.RunKey, limits, subagent.Reservation{})
	nested.Parent.SessionKey, nested.Parent.RelationshipKey, nested.Parent.TreeKey = first.Child.SessionKey, first.Child.RelationshipKey, ""
	second, _, err := store.Spawn(ctx, nested, subagentNow())
	if err != nil {
		t.Fatalf("Spawn(nested) error = %v", err)
	}
	tooDeep := subagentSpawn("too-deep", second.Child.RunKey, limits, subagent.Reservation{})
	tooDeep.Parent.SessionKey, tooDeep.Parent.RelationshipKey, tooDeep.Parent.TreeKey = second.Child.SessionKey, second.Child.RelationshipKey, ""
	if _, _, err := store.Spawn(ctx, tooDeep, subagentNow()); !errors.Is(err, subagent.ErrDepthExceeded) {
		t.Fatalf("Spawn() past the depth limit error = %v, want subagent.ErrDepthExceeded", err)
	}

	// A request whose derived child key equals its own parent would delegate to
	// itself, which is the degenerate case cycle detection exists for.
	cycle := factory(t)
	selfKey := subagent.RequestKey("cycle")
	selfRun := subagent.DeriveRunKey(subagentTenantA, selfKey)
	if _, _, err := cycle.Spawn(ctx, subagentSpawn(selfKey, selfRun, limits, subagent.Reservation{}), subagentNow()); !errors.Is(err, subagent.ErrCycle) {
		t.Fatalf("Spawn() closing a cycle error = %v, want subagent.ErrCycle", err)
	}
}

func testSubagentClaim(t *testing.T, factory SubagentStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	now := subagentNow()
	receipt, _, err := store.Spawn(ctx, subagentSpawn("claim", "root", subagentLimits(), subagent.Reservation{InputTokens: 10}), now)
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}

	claimed, ok, err := store.ClaimNext(ctx, subagent.ClaimRequest{TenantKey: subagentTenantA, Owner: "worker-1", Now: now, Lease: time.Minute})
	if err != nil || !ok || claimed.State != subagent.ChildRunning {
		t.Fatalf("ClaimNext() = %+v, ok %v, error %v", claimed, ok, err)
	}
	// A live claim is what stops two workers from running one child twice.
	if _, ok, err := store.ClaimNext(ctx, subagent.ClaimRequest{TenantKey: subagentTenantA, Owner: "worker-2", Now: now, Lease: time.Minute}); err != nil || ok {
		t.Fatalf("ClaimNext() handed out a second claim: ok %v, error %v", ok, err)
	}
	if _, ok, err := store.ClaimNext(ctx, subagent.ClaimRequest{TenantKey: subagentTenantA, Owner: "", Now: now, Lease: time.Minute}); !errors.Is(err, subagent.ErrInvalidRequest) {
		t.Fatalf("ClaimNext() without an owner: ok %v, error %v", ok, err)
	}

	// A worker that died leaves an expired claim. Recovery must return the child
	// to the queue rather than leaving it stuck as running forever.
	recovered, err := store.Recover(ctx, subagent.ReconcileRequest{TenantKey: subagentTenantA, Now: now.Add(2 * time.Minute), Limit: 10})
	if err != nil || recovered == 0 {
		t.Fatalf("Recover() = %d, error %v", recovered, err)
	}
	requeued, ok, err := store.ClaimNext(ctx, subagent.ClaimRequest{TenantKey: subagentTenantA, Owner: "worker-2", Now: now.Add(3 * time.Minute), Lease: time.Minute})
	if err != nil || !ok || requeued.Receipt.Child.RelationshipKey != receipt.Child.RelationshipKey {
		t.Fatalf("ClaimNext() after recovery = %+v, ok %v, error %v", requeued, ok, err)
	}

	// Committing under a version the store has moved past is a stale worker
	// writing, which must be refused.
	if _, err := store.CommitTerminal(ctx, subagent.TerminalCommand{
		TenantKey: subagentTenantA, RelationshipKey: receipt.Child.RelationshipKey,
		ExpectedVersion: requeued.Version + 7, Owner: "worker-2",
		Result:      subagent.RunResult{State: subagent.ChildCompleted, ResultRef: "result-1", UsageFactKey: "usage-claim"},
		CompletedAt: now.Add(4 * time.Minute),
	}); err == nil {
		t.Fatal("CommitTerminal() accepted a stale version")
	}
	terminal, err := store.CommitTerminal(ctx, subagent.TerminalCommand{
		TenantKey: subagentTenantA, RelationshipKey: receipt.Child.RelationshipKey,
		ExpectedVersion: requeued.Version, Owner: "worker-2",
		Result:      subagent.RunResult{State: subagent.ChildCompleted, ResultRef: "result-1", UsageFactKey: "usage-claim"},
		CompletedAt: now.Add(4 * time.Minute),
	})
	if err != nil || terminal.State != subagent.ChildCompleted || !terminal.State.Terminal() {
		t.Fatalf("CommitTerminal() = %+v, error %v", terminal, err)
	}
}

func testSubagentSettlement(t *testing.T, factory SubagentStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	receipt, _, err := store.Spawn(ctx, subagentSpawn("usage", "root", subagentLimits(), subagent.Reservation{InputTokens: 10, CostMicros: 20}), subagentNow())
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	command := subagent.SettleCommand{
		TenantKey: subagentTenantA, RelationshipKey: receipt.Child.RelationshipKey,
		UsageFactKey: "usage-1", Usage: subagent.Usage{InputTokens: 4, CostMicros: 7}, OccurredAt: subagentNow(),
	}
	budget, applied, err := store.SettleUsage(ctx, command)
	if err != nil || !applied || budget.Settled.CostMicros != 7 {
		t.Fatalf("SettleUsage() = %+v, applied %v, error %v", budget, applied, err)
	}
	// The unused part of a reservation returns to the tree, or one cheap child
	// would permanently hold budget its siblings could have used.
	if budget.Released.CostMicros != 13 {
		t.Fatalf("SettleUsage() released %d, want the unused 13", budget.Released.CostMicros)
	}
	if budget, applied, err := store.SettleUsage(ctx, command); err != nil || applied || budget.Settled.CostMicros != 7 {
		t.Fatalf("replayed SettleUsage() = %+v, applied %v, error %v", budget, applied, err)
	}
	conflicting := command
	conflicting.Usage.CostMicros = 8
	if _, _, err := store.SettleUsage(ctx, conflicting); !errors.Is(err, subagent.ErrUsageConflict) {
		t.Fatalf("conflicting SettleUsage() error = %v, want subagent.ErrUsageConflict", err)
	}

	// Settling more than the child reserved would escape the tree budget.
	other, _, err := store.Spawn(ctx, subagentSpawn("overspend", "root", subagentLimits(), subagent.Reservation{CostMicros: 5}), subagentNow())
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	if _, _, err := store.SettleUsage(ctx, subagent.SettleCommand{
		TenantKey: subagentTenantA, RelationshipKey: other.Child.RelationshipKey,
		UsageFactKey: "usage-2", Usage: subagent.Usage{CostMicros: 50}, OccurredAt: subagentNow(),
	}); !errors.Is(err, subagent.ErrUsageExceedsReserve) {
		t.Fatalf("SettleUsage() beyond the reservation error = %v, want subagent.ErrUsageExceedsReserve", err)
	}
}

func testSubagentCancel(t *testing.T, factory SubagentStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	now := subagentNow()
	receipt, _, err := store.Spawn(ctx, subagentSpawn("cancel", "root", subagentLimits(), subagent.Reservation{}), now)
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	// Cancellation walks down from a run to everything it delegated, so the
	// parent's run key is what cancels this child and its own descendants.
	request := subagent.CancelRequest{
		TenantKey: subagentTenantA, RequestKey: "cancel-1", RunKey: "root",
		Mode: subagent.CancelAbandon, Reason: "shutdown", MaxTraversal: 10,
	}
	result, err := store.RequestCancel(ctx, request, now)
	if err != nil || result.Affected == 0 {
		t.Fatalf("RequestCancel() = %+v, error %v", result, err)
	}
	// A retried cancel command is the same command, not a second cancellation.
	duplicate, err := store.RequestCancel(ctx, request, now)
	if err != nil || duplicate != result {
		t.Fatalf("replayed RequestCancel() = %+v, first = %+v, error %v", duplicate, result, err)
	}
	snapshot, err := store.Get(ctx, subagentTenantA, receipt.Child.RelationshipKey)
	if err != nil || snapshot.CancelMode != subagent.CancelAbandon {
		t.Fatalf("Get() after cancel = %+v, error %v", snapshot, err)
	}
}

func testSubagentWake(t *testing.T, factory SubagentStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	now := subagentNow()
	receipt, _, err := store.Spawn(ctx, subagentSpawn("wake", "root", subagentLimits(), subagent.Reservation{}), now)
	if err != nil {
		t.Fatalf("Spawn() error = %v", err)
	}
	claimed, ok, err := store.ClaimNext(ctx, subagent.ClaimRequest{TenantKey: subagentTenantA, Owner: "worker-1", Now: now, Lease: time.Minute})
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok = %v, error = %v", ok, err)
	}
	if _, err := store.CommitTerminal(ctx, subagent.TerminalCommand{
		TenantKey: subagentTenantA, RelationshipKey: receipt.Child.RelationshipKey,
		ExpectedVersion: claimed.Version, Owner: "worker-1",
		Result:      subagent.RunResult{State: subagent.ChildCompleted, ResultRef: "result-1", UsageFactKey: "usage-wake"},
		CompletedAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("CommitTerminal() error = %v", err)
	}

	// The terminal state commits before the parent is told about it, so the wake
	// survives a crash in between and is delivered at least once.
	wake, ok, err := store.ClaimWake(ctx, subagentTenantA)
	if err != nil || !ok || wake.Request.Child.RelationshipKey != receipt.Child.RelationshipKey {
		t.Fatalf("ClaimWake() = %+v, ok %v, error %v", wake, ok, err)
	}
	if err := store.CompleteWake(ctx, subagentTenantA, wake.Request.WakeKey, wake.Version, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("CompleteWake() error = %v", err)
	}
	if _, ok, err := store.ClaimWake(ctx, subagentTenantA); err != nil || ok {
		t.Fatalf("ClaimWake() after completion: ok %v, error %v", ok, err)
	}

	// The fact log is what an audit reads, so it must record the whole lifecycle
	// in a stable order.
	facts, err := store.Facts(ctx, subagentTenantA, 0, 100)
	if err != nil || len(facts) == 0 {
		t.Fatalf("Facts() = %d, error %v", len(facts), err)
	}
	kinds := map[subagent.FactKind]bool{}
	for index, fact := range facts {
		kinds[fact.Kind] = true
		if index > 0 && facts[index-1].Revision >= fact.Revision {
			t.Fatalf("Facts() is not ordered by revision: %+v", facts)
		}
	}
	for _, kind := range []subagent.FactKind{subagent.FactChildAccepted, subagent.FactChildRunning, subagent.FactChildTerminal} {
		if !kinds[kind] {
			t.Fatalf("Facts() is missing %q: %+v", kind, facts)
		}
	}
	if tail, err := store.Facts(ctx, subagentTenantA, facts[len(facts)-1].Revision, 100); err != nil || len(tail) != 0 {
		t.Fatalf("Facts() after the last revision = %d, error %v", len(tail), err)
	}
}
