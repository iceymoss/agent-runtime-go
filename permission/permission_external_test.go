package permission_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/permission"
)

type testClock struct {
	mu  sync.RWMutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func testCheck() permission.CheckRequest {
	return permission.CheckRequest{
		RequestKey: "request-1", Subject: permission.Subject{TenantKey: "tenant-1", PrincipalKey: "principal-1"},
		Resource: permission.Resource{Kind: "file", Key: "resource-1"}, SessionRef: "session-1", RunRef: "run-1",
		AttemptRef: "attempt-1", ExecutionRef: "execution-1", FenceToken: 4, ToolCallID: "call-1",
		ToolName: "write", Action: "update", InputDigest: "sha256:input", ToolGeneration: "tools-v1",
		DefinitionDigest: "sha256:definition", PolicyVersion: "policy-v1",
	}
}

func createPending(t *testing.T, store *permission.MemoryStore, expiresAt time.Time) permission.Snapshot {
	t.Helper()
	check := testCheck()
	snapshot, created, err := store.CreateAndSuspend(context.Background(), permission.CreateApprovalCommand{
		Request: permission.ApprovalRequest{RequestKey: check.RequestKey, Check: check, ExpiresAt: expiresAt}, ResumeToken: "opaque-token",
	})
	if err != nil || !created {
		t.Fatalf("CreateAndSuspend() = (%+v, %v, %v)", snapshot, created, err)
	}
	return snapshot
}

func TestMemoryStoreResolveFirstWins(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	store := permission.NewMemoryStore(clock)
	createPending(t, store, clock.Now().Add(time.Hour))
	commands := []permission.ResolveCommand{
		{TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "approve", DecisionKey: "approve", ApproverKey: "a", ExpectedRevision: 1, Kind: permission.ResolutionApprove},
		{TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "deny", DecisionKey: "deny", ApproverKey: "b", ExpectedRevision: 1, Kind: permission.ResolutionDeny},
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, command := range commands {
		wg.Add(1)
		go func(command permission.ResolveCommand) {
			defer wg.Done()
			<-start
			_, _, err := store.Resolve(context.Background(), command)
			errs <- err
		}(command)
	}
	close(start)
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, permission.ErrAlreadyResolved) {
			conflicts++
		} else {
			t.Fatalf("unexpected Resolve error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}

	winner, err := store.GetRequest(context.Background(), permission.GetRequestQuery{TenantKey: "tenant-1", RequestKey: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	retry := commands[0]
	if winner.Resolution.Kind == permission.ResolutionDeny {
		retry = commands[1]
	}
	_, changed, err := store.Resolve(context.Background(), retry)
	if err != nil || changed {
		t.Fatalf("idempotent Resolve() = changed %v, err %v", changed, err)
	}
}

func TestMemoryStoreExpiryAndTenantIsolation(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	store := permission.NewMemoryStore(clock)
	createPending(t, store, clock.Now().Add(time.Minute))
	if _, err := store.GetRequest(context.Background(), permission.GetRequestQuery{TenantKey: "other", RequestKey: "request-1"}); !errors.Is(err, permission.ErrRequestNotFound) {
		t.Fatalf("cross-tenant GetRequest error = %v", err)
	}
	clock.advance(time.Minute)
	result, err := store.ExpireDue(context.Background(), permission.ExpireCommand{TenantKey: "tenant-1"})
	if err != nil || len(result.Approvals) != 1 || result.Approvals[0].Request.State != permission.ApprovalExpired {
		t.Fatalf("ExpireDue() = (%+v, %v)", result, err)
	}
	_, _, err = store.Resolve(context.Background(), permission.ResolveCommand{TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "late", DecisionKey: "late", ApproverKey: "approver", ExpectedRevision: 1, Kind: permission.ResolutionApprove})
	if !errors.Is(err, permission.ErrRequestExpired) {
		t.Fatalf("expired Resolve error = %v", err)
	}
}

func TestMemoryStoreGrantConsumeIsAtomic(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	store := permission.NewMemoryStore(clock)
	createPending(t, store, clock.Now().Add(time.Hour))
	one := uint64(1)
	check := testCheck()
	resolved, _, err := store.Resolve(context.Background(), permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "approve", DecisionKey: "decision-1", GrantKey: "grant-1",
		ApproverKey: "approver", ExpectedRevision: 1, Kind: permission.ResolutionApprove,
		Grant: &permission.GrantSpec{Scope: permission.ScopeInvocation, PrincipalKey: check.Subject.PrincipalKey, ToolName: check.ToolName,
			Action: check.Action, ResourceKind: check.Resource.Kind, ResourceKey: check.Resource.Key, InputDigest: check.InputDigest,
			PolicyVersion: check.PolicyVersion, ToolGeneration: check.ToolGeneration, ExpiresAt: clock.Now().Add(time.Hour), MaxUses: &one},
	})
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, consumeErr := store.ConsumeGrant(context.Background(), permission.ConsumeGrantCommand{TenantKey: "tenant-1", GrantKey: "grant-1", Check: check, ExpectedRevision: resolved.Grant.Revision})
			errs <- consumeErr
		}()
	}
	wg.Wait()
	close(errs)
	success := 0
	for consumeErr := range errs {
		if consumeErr == nil {
			success++
		} else if !errors.Is(consumeErr, permission.ErrRevisionConflict) && !errors.Is(consumeErr, permission.ErrGrantExhausted) {
			t.Fatalf("unexpected ConsumeGrant error: %v", consumeErr)
		}
	}
	if success != 1 {
		t.Fatalf("successful consumes=%d", success)
	}
	if resolved.Grant.RemainingUses == nil || *resolved.Grant.RemainingUses != 1 {
		t.Fatalf("grant alias mutated: %+v", resolved.Grant)
	}
}

func TestServicePersistsAskAndFencesAttempts(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	store := permission.NewMemoryStore(clock)
	policy := permission.PolicyFunc{PolicyVersion: "policy-v1", EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
		return permission.CheckResult{Decision: permission.DecisionAsk, PolicyVersion: request.PolicyVersion, InputDigest: request.InputDigest}, nil
	}}
	service, err := permission.NewService(permission.ServiceOptions{Policy: policy, Store: store, Clock: clock, Fences: testFence{}})
	if err != nil {
		t.Fatal(err)
	}
	check := testCheck()
	check.ApprovalExpiresAt = clock.Now().Add(time.Hour)
	result, err := service.Check(context.Background(), check)
	if err != nil || result.Blocker == nil || result.Blocker.ResumeToken == "" {
		t.Fatalf("Check() = (%+v, %v)", result, err)
	}
	persisted, err := service.GetRequest(context.Background(), permission.GetRequestQuery{TenantKey: "tenant-1", RequestKey: "request-1"})
	if err != nil || persisted.Request.State != permission.ApprovalPending || persisted.ResumeToken != result.Blocker.ResumeToken {
		t.Fatalf("persisted approval = (%+v, %v)", persisted, err)
	}
	check.AttemptRef = "stale"
	if _, err := service.Check(context.Background(), check); !errors.Is(err, permission.ErrStaleFence) {
		t.Fatalf("stale Check error = %v", err)
	}
}

func TestServiceCancelAndResumeBinding(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)}
	store := permission.NewMemoryStore(clock)
	policy := permission.PolicyFunc{PolicyVersion: "policy-v1", EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
		return permission.CheckResult{Decision: permission.DecisionAsk, PolicyVersion: request.PolicyVersion, InputDigest: request.InputDigest}, nil
	}}
	service, err := permission.NewService(permission.ServiceOptions{Policy: policy, Store: store, Clock: clock, Fences: testFence{}})
	if err != nil {
		t.Fatal(err)
	}
	check := testCheck()
	check.ApprovalExpiresAt = clock.Now().Add(time.Hour)
	result, err := service.Check(context.Background(), check)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Cancel(context.Background(), permission.CancelCommand{TenantKey: "tenant-1", RequestKey: "request-1", ExpectedRevision: 1, AttemptRef: "stale", FenceToken: 4})
	if !errors.Is(err, permission.ErrStaleFence) {
		t.Fatalf("stale Cancel error = %v", err)
	}
	resolved, changed, err := service.Resolve(context.Background(), permission.ResolveCommand{TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "approve", DecisionKey: "decision", ApproverKey: "approver", ExpectedRevision: 1, Kind: permission.ResolutionApprove})
	if err != nil || !changed || resolved.Request.State != permission.ApprovalApproved {
		t.Fatalf("Resolve() = (%+v, %v, %v)", resolved, changed, err)
	}
	_, err = service.Revalidate(context.Background(), permission.RevalidateCommand{TenantKey: "tenant-1", RequestKey: "request-1", ResumeToken: result.Blocker.ResumeToken, AttemptRef: "attempt-1", FenceToken: 4, InputDigest: "changed", PolicyVersion: "policy-v1", ToolGeneration: "tools-v1"})
	if !errors.Is(err, permission.ErrResumeRevalidation) {
		t.Fatalf("changed digest Revalidate error = %v", err)
	}
	allowed, err := service.Revalidate(context.Background(), permission.RevalidateCommand{TenantKey: "tenant-1", RequestKey: "request-1", ResumeToken: result.Blocker.ResumeToken, AttemptRef: "attempt-1", FenceToken: 4, InputDigest: check.InputDigest, PolicyVersion: check.PolicyVersion, ToolGeneration: check.ToolGeneration})
	if err != nil || allowed.Decision != permission.DecisionAllow {
		t.Fatalf("approved Revalidate() = (%+v, %v)", allowed, err)
	}
}

type testFence struct{}

func (testFence) ValidateFence(_ context.Context, _ permission.TenantKey, _ permission.RunRef, attempt permission.AttemptRef, fence uint64) error {
	if attempt != "attempt-1" || fence != 4 {
		return permission.ErrStaleFence
	}
	return nil
}
