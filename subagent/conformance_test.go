package subagent_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

var _ subagent.Store = (*subagent.MemoryStore)(nil)

type runnerFunc func(context.Context, subagent.RunRequest) (subagent.RunResult, error)

func (f runnerFunc) Run(ctx context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
	return f(ctx, request)
}

type recordingWaker struct {
	mu       sync.Mutex
	requests map[subagent.WakeKey]int
	fail     bool
}

func (w *recordingWaker) Wake(_ context.Context, request subagent.WakeRequest) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		w.fail = false
		return errors.New("injected wake failure")
	}
	if w.requests == nil {
		w.requests = make(map[subagent.WakeKey]int)
	}
	w.requests[request.WakeKey]++
	return nil
}

func TestMemoryStoreConformance(t *testing.T) {
	runStoreConformance(t, func() subagent.Store { return subagent.NewMemoryStore() })
}

func runStoreConformance(t *testing.T, newStore func() subagent.Store) {
	t.Helper()
	t.Run("idempotency isolation and deep copies", func(t *testing.T) {
		store := newStore()
		now := time.Unix(1_700_000_000, 0).UTC()
		request := rootSpawn("same", "root", defaultLimits(), subagent.Reservation{InputTokens: 10})
		first, created, err := store.Spawn(context.Background(), request, now)
		if err != nil || !created {
			t.Fatalf("spawn first: created=%v err=%v", created, err)
		}
		second, created, err := store.Spawn(context.Background(), request, now)
		if err != nil || created || first != second {
			t.Fatalf("spawn retry: created=%v err=%v first=%+v second=%+v", created, err, first, second)
		}
		request.Input[0] = 'X'
		if _, _, err = store.Spawn(context.Background(), request, now); !errors.Is(err, subagent.ErrIdempotencyConflict) {
			t.Fatalf("immutable mismatch: %v", err)
		}
		snapshot, err := store.Get(context.Background(), tenantA, first.Child.RelationshipKey)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Input[0] = 'Y'
		again, err := store.Get(context.Background(), tenantA, first.Child.RelationshipKey)
		if err != nil || string(again.Input) != "task" {
			t.Fatalf("deep copy: input=%q err=%v", again.Input, err)
		}
		if _, err = store.Get(context.Background(), tenantB, first.Child.RelationshipKey); !errors.Is(err, subagent.ErrNotFound) {
			t.Fatalf("cross-tenant get: %v", err)
		}
	})

	t.Run("parallel budget reservation", func(t *testing.T) {
		store := newStore()
		limits := defaultLimits()
		limits.MaxFanout = 20
		limits.MaxCostMicros = 100
		var accepted atomic.Int64
		var unexpected atomic.Value
		var wait sync.WaitGroup
		for i := 0; i < 10; i++ {
			wait.Add(1)
			go func(index int) {
				defer wait.Done()
				request := rootSpawn(subagent.RequestKey(fmt.Sprintf("sibling-%d", index)), "root", limits, subagent.Reservation{CostMicros: 30})
				_, _, err := store.Spawn(context.Background(), request, time.Unix(1_700_000_000, 0).UTC())
				if err == nil {
					accepted.Add(1)
					return
				}
				if !errors.Is(err, subagent.ErrCostBudgetExceeded) {
					unexpected.Store(err)
				}
			}(i)
		}
		wait.Wait()
		if value := unexpected.Load(); value != nil {
			t.Fatalf("unexpected reservation error: %v", value)
		}
		if accepted.Load() != 3 {
			t.Fatalf("accepted %d reservations, want 3", accepted.Load())
		}
	})

	t.Run("usage settles once", func(t *testing.T) {
		store := newStore()
		receipt, _, err := store.Spawn(context.Background(), rootSpawn("usage", "root", defaultLimits(), subagent.Reservation{InputTokens: 10, CostMicros: 20}), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		command := subagent.SettleCommand{TenantKey: tenantA, RelationshipKey: receipt.Child.RelationshipKey, UsageFactKey: "usage-1", Usage: subagent.Usage{InputTokens: 4, CostMicros: 7}, OccurredAt: time.Now()}
		budget, applied, err := store.SettleUsage(context.Background(), command)
		if err != nil || !applied || budget.Settled.CostMicros != 7 || budget.Released.CostMicros != 13 {
			t.Fatalf("first settlement: applied=%v budget=%+v err=%v", applied, budget, err)
		}
		budget, applied, err = store.SettleUsage(context.Background(), command)
		if err != nil || applied || budget.Settled.CostMicros != 7 {
			t.Fatalf("duplicate settlement: applied=%v budget=%+v err=%v", applied, budget, err)
		}
		command.Usage.CostMicros = 8
		if _, _, err = store.SettleUsage(context.Background(), command); !errors.Is(err, subagent.ErrUsageConflict) {
			t.Fatalf("usage mismatch: %v", err)
		}
	})
}

func TestServiceDynamicTwoLevelAndParallelSiblings(t *testing.T) {
	store := subagent.NewMemoryStore()
	waker := &recordingWaker{}
	now := time.Unix(1_700_000_000, 0).UTC()
	var service *subagent.Service
	runner := runnerFunc(func(ctx context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
		if request.AgentKey == "delegator" {
			_, err := service.Spawn(ctx, subagent.SpawnRequest{
				RequestKey: "grandchild",
				Parent: subagent.ParentRef{
					TenantKey:       request.Child.TenantKey,
					SessionKey:      request.Child.SessionKey,
					RunKey:          request.Child.RunKey,
					RelationshipKey: request.Child.RelationshipKey,
				},
				AgentKey: "worker",
				Input:    []byte("nested task"),
				Limits:   request.Limits,
				Reserve:  subagent.Reservation{CostMicros: 10},
			})
			if err != nil {
				return subagent.RunResult{}, err
			}
		}
		return subagent.RunResult{State: subagent.ChildCompleted, ResultRef: subagent.ResultRef("result/" + request.Child.RunKey), UsageFactKey: subagent.UsageFactKey("usage/" + request.Child.RunKey), Usage: subagent.Usage{CostMicros: 5}}, nil
	})
	var err error
	service, err = subagent.New(subagent.Options{Store: store, Runner: runner, ParentWaker: waker, WorkerID: "worker-1", LeaseDuration: time.Minute, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	limits := defaultLimits()
	limits.MaxFanout = 3
	limits.MaxCostMicros = 100
	for _, key := range []subagent.RequestKey{"child-a", "child-b"} {
		agentKey := "worker"
		if key == "child-a" {
			agentKey = "delegator"
		}
		request := rootSpawn(key, "root", limits, subagent.Reservation{CostMicros: 20})
		request.AgentKey = agentKey
		if _, err = service.Spawn(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, ok, runErr := service.RunNext(context.Background(), tenantA); runErr != nil || !ok {
			t.Fatalf("run %d: ok=%v err=%v", i, ok, runErr)
		}
	}
	report, err := service.Reconcile(context.Background(), subagent.ReconcileRequest{TenantKey: tenantA, Now: now, Limit: 10})
	if err != nil || report.WakesDelivered != 3 {
		t.Fatalf("reconcile wakes: report=%+v err=%v", report, err)
	}
}

func TestCommitBeforeWakeCrashAndReconcile(t *testing.T) {
	store := subagent.NewMemoryStore()
	waker := &recordingWaker{fail: true}
	now := time.Unix(1_700_000_000, 0).UTC()
	service, err := subagent.New(subagent.Options{
		Store: store,
		Runner: runnerFunc(func(context.Context, subagent.RunRequest) (subagent.RunResult, error) {
			return subagent.RunResult{State: subagent.ChildCompleted, ResultRef: "result", UsageFactKey: "usage", Usage: subagent.Usage{CostMicros: 1}}, nil
		}),
		ParentWaker: waker, WorkerID: "worker", LeaseDuration: time.Minute, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := service.Spawn(context.Background(), rootSpawn("wake", "root", defaultLimits(), subagent.Reservation{CostMicros: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := service.RunNext(context.Background(), tenantA); err != nil || !ok {
		t.Fatalf("run: ok=%v err=%v", ok, err)
	}
	report, err := service.Reconcile(context.Background(), subagent.ReconcileRequest{TenantKey: tenantA, Now: now, Limit: 1})
	if err != nil || report.WakeFailures != 1 {
		t.Fatalf("first reconcile: %+v err=%v", report, err)
	}
	snapshot, err := service.Get(context.Background(), tenantA, receipt.Child.RelationshipKey)
	if err != nil || !snapshot.WakePending || snapshot.State != subagent.ChildCompleted {
		t.Fatalf("committed before wake: %+v err=%v", snapshot, err)
	}
	report, err = service.Reconcile(context.Background(), subagent.ReconcileRequest{TenantKey: tenantA, Now: now, Limit: 1})
	if err != nil || report.WakesDelivered != 1 {
		t.Fatalf("second reconcile: %+v err=%v", report, err)
	}
	report, err = service.Reconcile(context.Background(), subagent.ReconcileRequest{TenantKey: tenantA, Now: now, Limit: 1})
	if err != nil || report.WakesDelivered != 0 {
		t.Fatalf("idempotent wake: %+v err=%v", report, err)
	}
}

func TestCancelPropagationPrecedenceAndBound(t *testing.T) {
	store := subagent.NewMemoryStore()
	now := time.Now()
	limits := defaultLimits()
	parent, _, err := store.Spawn(context.Background(), rootSpawn("parent", "root", limits, subagent.Reservation{CostMicros: 10}), now)
	if err != nil {
		t.Fatal(err)
	}
	childRequest := rootSpawn("child", parent.Child.RunKey, limits, subagent.Reservation{CostMicros: 10})
	childRequest.Parent.SessionKey = parent.Child.SessionKey
	childRequest.Parent.RelationshipKey = parent.Child.RelationshipKey
	childRequest.Parent.TreeKey = ""
	child, _, err := store.Spawn(context.Background(), childRequest, now)
	if err != nil {
		t.Fatal(err)
	}
	bounded := subagent.CancelRequest{TenantKey: tenantA, RequestKey: "bounded", RunKey: "root", Mode: subagent.CancelAbandon, MaxTraversal: 1}
	if _, err = store.RequestCancel(context.Background(), bounded, now); !errors.Is(err, subagent.ErrTraversalLimit) {
		t.Fatalf("bounded cancel: %v", err)
	}
	unchanged, err := store.Get(context.Background(), tenantA, parent.Child.RelationshipKey)
	if err != nil || unchanged.State != subagent.ChildQueued {
		t.Fatalf("bounded cancel partially mutated: %+v err=%v", unchanged, err)
	}
	request := subagent.CancelRequest{TenantKey: tenantA, RequestKey: "cancel", RunKey: "root", Mode: subagent.CancelAbandon, MaxTraversal: 10}
	result, err := store.RequestCancel(context.Background(), request, now)
	if err != nil || result.Affected != 2 {
		t.Fatalf("cancel tree: %+v err=%v", result, err)
	}
	for _, relationshipKey := range []subagent.RelationshipKey{parent.Child.RelationshipKey, child.Child.RelationshipKey} {
		snapshot, getErr := store.Get(context.Background(), tenantA, relationshipKey)
		if getErr != nil || snapshot.State != subagent.ChildCanceled || !snapshot.WakePending {
			t.Fatalf("canceled child: %+v err=%v", snapshot, getErr)
		}
	}
	duplicate, err := store.RequestCancel(context.Background(), request, now)
	if err != nil || duplicate != result {
		t.Fatalf("idempotent cancel: %+v err=%v", duplicate, err)
	}
}

func TestDepthAndFanoutLimits(t *testing.T) {
	store := subagent.NewMemoryStore()
	now := time.Now()
	limits := defaultLimits()
	limits.MaxDepth = 2
	limits.MaxFanout = 1
	first, _, err := store.Spawn(context.Background(), rootSpawn("first", "root", limits, subagent.Reservation{}), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.Spawn(context.Background(), rootSpawn("fanout", "root", limits, subagent.Reservation{}), now); !errors.Is(err, subagent.ErrFanoutExceeded) {
		t.Fatalf("fanout: %v", err)
	}
	nested := rootSpawn("nested", first.Child.RunKey, limits, subagent.Reservation{})
	nested.Parent.SessionKey = first.Child.SessionKey
	nested.Parent.RelationshipKey = first.Child.RelationshipKey
	nested.Parent.TreeKey = ""
	second, _, err := store.Spawn(context.Background(), nested, now)
	if err != nil {
		t.Fatal(err)
	}
	tooDeep := rootSpawn("too-deep", second.Child.RunKey, limits, subagent.Reservation{})
	tooDeep.Parent.SessionKey = second.Child.SessionKey
	tooDeep.Parent.RelationshipKey = second.Child.RelationshipKey
	tooDeep.Parent.TreeKey = ""
	if _, _, err = store.Spawn(context.Background(), tooDeep, now); !errors.Is(err, subagent.ErrDepthExceeded) {
		t.Fatalf("depth: %v", err)
	}
	cycleKey := subagent.RequestKey("cycle")
	cycleRun := derivedRunKeyForTest(tenantA, cycleKey)
	if _, _, err = subagent.NewMemoryStore().Spawn(context.Background(), rootSpawn(cycleKey, cycleRun, limits, subagent.Reservation{}), now); !errors.Is(err, subagent.ErrCycle) {
		t.Fatalf("cycle: %v", err)
	}
}

const (
	tenantA agent.TenantKey = "tenant-a"
	tenantB agent.TenantKey = "tenant-b"
)

func defaultLimits() subagent.Limits {
	return subagent.Limits{
		MaxDepth:        4,
		MaxFanout:       4,
		MaxInputTokens:  1000,
		MaxOutputTokens: 1000,
		MaxCostMicros:   1000,
		MaxToolCalls:    100,
		MaxRuntime:      time.Hour,
	}
}

func rootSpawn(key subagent.RequestKey, parentRun subagent.RunKey, limits subagent.Limits, reserve subagent.Reservation) subagent.SpawnRequest {
	return subagent.SpawnRequest{
		RequestKey: key,
		Parent: subagent.ParentRef{
			TenantKey:  tenantA,
			SessionKey: "parent-session",
			RunKey:     parentRun,
			TreeKey:    "tree/root",
		},
		AgentKey: "worker",
		Input:    []byte("task"),
		Limits:   limits,
		Reserve:  reserve,
	}
}

func derivedRunKeyForTest(tenant agent.TenantKey, key subagent.RequestKey) subagent.RunKey {
	sum := sha256.Sum256([]byte(fmt.Sprintf("run\x00%s\x00%s", tenant, key)))
	return subagent.RunKey("run/" + hex.EncodeToString(sum[:16]))
}
