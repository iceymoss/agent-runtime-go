package agenttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/permission"
)

// PermissionStoreFactory returns a fresh, empty approval store driven by the
// supplied clock. The clock is a parameter because expiry is part of the
// contract, and a suite that could only use wall-clock time could not test it.
type PermissionStoreFactory func(t *testing.T, clock permission.Clock) permission.Store

// TestPermissionStore runs the approval and grant conformance suite.
//
// An approval store decides whether a side effect is allowed to happen, so its
// failure modes are not "wrong data" but "the wrong thing ran". The suite pins
// the properties that prevent that: a decision belongs to exactly one request,
// an approval that lapsed cannot be used, and a grant authorizes only the exact
// call it was given for.
func TestPermissionStore(t *testing.T, factory PermissionStoreFactory) {
	t.Helper()
	t.Run("creating an approval is idempotent by request key", func(t *testing.T) {
		testApprovalCreate(t, factory)
	})
	t.Run("resolving is guarded by revision and terminal state", func(t *testing.T) {
		testApprovalResolve(t, factory)
	})
	t.Run("canceling requires the attempt that is waiting", func(t *testing.T) {
		testApprovalCancel(t, factory)
	})
	t.Run("a lapsed approval can no longer be used", func(t *testing.T) {
		testApprovalExpiry(t, factory)
	})
	t.Run("a grant authorizes only the call it was given for", func(t *testing.T) {
		testGrantBinding(t, factory)
	})
	t.Run("grants are consumed and revoked under revision control", func(t *testing.T) {
		testGrantLifecycle(t, factory)
	})
}

// fixedClock makes expiry deterministic. Tests advance it explicitly rather than
// sleeping, so they stay fast and cannot flake.
type fixedClock struct{ now time.Time }

func (c *fixedClock) Now() time.Time { return c.now }

func newFixedClock() *fixedClock {
	return &fixedClock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func conformanceCheck() permission.CheckRequest {
	return permission.CheckRequest{
		RequestKey: "request-1",
		Subject:    permission.Subject{TenantKey: "tenant-1", PrincipalKey: "principal-1", ActorType: "human"},
		Resource:   permission.Resource{Kind: "workspace", Key: "/repo"}, SessionRef: "session-1",
		RunRef: "run-1", AttemptRef: "attempt-1", ExecutionRef: "execution-1", FenceToken: 3,
		ToolCallID: "call-1", ToolName: "write_file", Action: "workspace.write",
		InputDigest: "sha256:input", ToolGeneration: "tools-v1", PolicyVersion: "policy-v1",
	}
}

func conformanceGrantSpec(check permission.CheckRequest, scope permission.Scope, expiresAt time.Time) permission.GrantSpec {
	return permission.GrantSpec{
		Scope: scope, PrincipalKey: check.Subject.PrincipalKey, SessionRef: check.SessionRef,
		ToolName: check.ToolName, Action: check.Action,
		ResourceKind: check.Resource.Kind, ResourceKey: check.Resource.Key,
		InputDigest: check.InputDigest, PolicyVersion: check.PolicyVersion,
		ToolGeneration: check.ToolGeneration, ExpiresAt: expiresAt,
	}
}

func createApprovalRequest(t *testing.T, store permission.Store, expiresAt time.Time) permission.Snapshot {
	t.Helper()
	snapshot, created, err := store.CreateAndSuspend(context.Background(), permission.CreateApprovalCommand{
		Request:     permission.ApprovalRequest{RequestKey: "request-1", Check: conformanceCheck(), ExpiresAt: expiresAt},
		ResumeToken: "resume-token",
	})
	if err != nil || !created || snapshot.Request.State != permission.ApprovalPending {
		t.Fatalf("CreateAndSuspend() = %+v, created %v, error %v", snapshot, created, err)
	}
	if snapshot.ResumeToken != "resume-token" {
		t.Fatalf("CreateAndSuspend() lost the resume token: %+v", snapshot)
	}
	return snapshot
}

func testApprovalCreate(t *testing.T, factory PermissionStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock)
	ctx := context.Background()
	first := createApprovalRequest(t, store, time.Time{})

	replayed, created, err := store.CreateAndSuspend(ctx, permission.CreateApprovalCommand{
		Request:     permission.ApprovalRequest{RequestKey: "request-1", Check: conformanceCheck()},
		ResumeToken: "resume-token",
	})
	if err != nil || created || replayed.Request.Revision != first.Request.Revision {
		t.Fatalf("replayed CreateAndSuspend() = %+v, created %v, error %v", replayed, created, err)
	}

	changed := conformanceCheck()
	changed.InputDigest = "sha256:other"
	if _, _, err := store.CreateAndSuspend(ctx, permission.CreateApprovalCommand{
		Request:     permission.ApprovalRequest{RequestKey: "request-1", Check: changed},
		ResumeToken: "resume-token",
	}); !errors.Is(err, permission.ErrRequestConflict) {
		t.Fatalf("CreateAndSuspend() with different input error = %v, want permission.ErrRequestConflict", err)
	}
	// An approval that is already past its deadline must never become pending, or
	// a decision could be collected for a window that already closed.
	if _, _, err := store.CreateAndSuspend(ctx, permission.CreateApprovalCommand{
		Request: permission.ApprovalRequest{
			RequestKey: "request-late", Check: withRequestKey(conformanceCheck(), "request-late"),
			ExpiresAt: clock.now.Add(-time.Second),
		},
		ResumeToken: "resume-token",
	}); !errors.Is(err, permission.ErrRequestExpired) {
		t.Fatalf("CreateAndSuspend() with a past deadline error = %v, want permission.ErrRequestExpired", err)
	}
	if _, _, err := store.CreateAndSuspend(ctx, permission.CreateApprovalCommand{
		Request: permission.ApprovalRequest{RequestKey: "request-2", Check: withRequestKey(conformanceCheck(), "request-2")},
	}); !errors.Is(err, permission.ErrInvalidRequest) {
		t.Fatalf("CreateAndSuspend() without a resume token error = %v, want permission.ErrInvalidRequest", err)
	}
	if _, err := store.GetRequest(ctx, permission.GetRequestQuery{TenantKey: "tenant-1", RequestKey: "absent"}); !errors.Is(err, permission.ErrRequestNotFound) {
		t.Fatalf("GetRequest(absent) error = %v, want permission.ErrRequestNotFound", err)
	}
}

func withRequestKey(check permission.CheckRequest, key permission.RequestKey) permission.CheckRequest {
	check.RequestKey = key
	return check
}

func testApprovalResolve(t *testing.T, factory PermissionStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock)
	ctx := context.Background()
	pending := createApprovalRequest(t, store, time.Time{})
	approve := permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "command-approve",
		DecisionKey: "decision-approve", ApproverKey: "principal-1",
		ExpectedRevision: pending.Request.Revision, Kind: permission.ResolutionApprove, ReasonCode: "approved",
	}

	if _, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "stale", DecisionKey: "stale",
		ApproverKey: "principal-1", ExpectedRevision: pending.Request.Revision + 5, Kind: permission.ResolutionApprove,
	}); !errors.Is(err, permission.ErrRevisionConflict) {
		t.Fatalf("Resolve() with a stale revision error = %v, want permission.ErrRevisionConflict", err)
	}

	resolved, applied, err := store.Resolve(ctx, approve)
	if err != nil || !applied || resolved.Request.State != permission.ApprovalApproved || resolved.Resolution == nil {
		t.Fatalf("Resolve() = %+v, applied %v, error %v", resolved, applied, err)
	}
	// Replaying the identical decision is how a retried command stays safe.
	if _, applied, err := store.Resolve(ctx, approve); err != nil || applied {
		t.Fatalf("replayed Resolve() applied = %v, error %v", applied, err)
	}
	// A second, different decision on the same request must not overwrite the first.
	contradicting := approve
	contradicting.CommandKey, contradicting.DecisionKey = "command-deny", "decision-deny"
	contradicting.Kind = permission.ResolutionDeny
	if _, _, err := store.Resolve(ctx, contradicting); !errors.Is(err, permission.ErrAlreadyResolved) {
		t.Fatalf("contradicting Resolve() error = %v, want permission.ErrAlreadyResolved", err)
	}
	if _, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d", ApproverKey: "p",
	}); !errors.Is(err, permission.ErrInvalidRequest) {
		t.Fatalf("Resolve() without a decision kind error = %v, want permission.ErrInvalidRequest", err)
	}
}

func testApprovalCancel(t *testing.T, factory PermissionStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock)
	ctx := context.Background()
	pending := createApprovalRequest(t, store, time.Time{})

	// The fence and attempt identify the worker that is actually blocked on this
	// decision. A different attempt must not be able to cancel it out from under it.
	if _, _, err := store.Cancel(ctx, permission.CancelCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", ExpectedRevision: pending.Request.Revision,
		AttemptRef: "attempt-1", FenceToken: 99,
	}); !errors.Is(err, permission.ErrStaleFence) {
		t.Fatalf("Cancel() with a foreign fence error = %v, want permission.ErrStaleFence", err)
	}
	canceled, applied, err := store.Cancel(ctx, permission.CancelCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", ExpectedRevision: pending.Request.Revision,
		AttemptRef: "attempt-1", FenceToken: 3,
	})
	if err != nil || !applied || canceled.Request.State != permission.ApprovalCanceled {
		t.Fatalf("Cancel() = %+v, applied %v, error %v", canceled, applied, err)
	}
	if _, applied, err := store.Cancel(ctx, permission.CancelCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", ExpectedRevision: canceled.Request.Revision,
		AttemptRef: "attempt-1", FenceToken: 3,
	}); err != nil || applied {
		t.Fatalf("replayed Cancel() applied = %v, error %v", applied, err)
	}
	if _, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d",
		ApproverKey: "p", ExpectedRevision: canceled.Request.Revision, Kind: permission.ResolutionApprove,
	}); !errors.Is(err, permission.ErrRequestCanceled) {
		t.Fatalf("Resolve() on a canceled request error = %v, want permission.ErrRequestCanceled", err)
	}
}

func testApprovalExpiry(t *testing.T, factory PermissionStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock)
	ctx := context.Background()
	pending := createApprovalRequest(t, store, clock.now.Add(time.Minute))

	listed, err := store.ListPending(ctx, permission.ListPendingQuery{TenantKey: "tenant-1"})
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListPending() = %d, error %v", len(listed), err)
	}
	if scoped, err := store.ListPending(ctx, permission.ListPendingQuery{TenantKey: "tenant-1", SessionRef: "other"}); err != nil || len(scoped) != 0 {
		t.Fatalf("ListPending() ignored the session filter: %d, error %v", len(scoped), err)
	}
	if other, err := store.ListPending(ctx, permission.ListPendingQuery{TenantKey: "tenant-2"}); err != nil || len(other) != 0 {
		t.Fatalf("ListPending() leaked across tenants: %d, error %v", len(other), err)
	}

	// Expiry has to apply on read. A store that only expires during a sweep would
	// let a lapsed approval be resolved by anyone who reads it before the sweep runs.
	clock.now = clock.now.Add(2 * time.Minute)
	expired, err := store.GetRequest(ctx, permission.GetRequestQuery{TenantKey: "tenant-1", RequestKey: "request-1"})
	if err != nil || expired.Request.State != permission.ApprovalExpired {
		t.Fatalf("GetRequest() after the deadline = %s, error %v", expired.Request.State, err)
	}
	if listed, err := store.ListPending(ctx, permission.ListPendingQuery{TenantKey: "tenant-1"}); err != nil || len(listed) != 0 {
		t.Fatalf("ListPending() still reports a lapsed approval: %d, error %v", len(listed), err)
	}
	if _, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d",
		ApproverKey: "p", ExpectedRevision: pending.Request.Revision, Kind: permission.ResolutionApprove,
	}); !errors.Is(err, permission.ErrRequestExpired) {
		t.Fatalf("Resolve() on a lapsed approval error = %v, want permission.ErrRequestExpired", err)
	}
	if _, err := store.ExpireDue(ctx, permission.ExpireCommand{TenantKey: "tenant-1", Limit: -1}); !errors.Is(err, permission.ErrInvalidRequest) {
		t.Fatalf("ExpireDue() with a negative limit error = %v, want permission.ErrInvalidRequest", err)
	}
}

func testGrantBinding(t *testing.T, factory PermissionStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock)
	ctx := context.Background()
	pending := createApprovalRequest(t, store, time.Time{})
	check := conformanceCheck()
	spec := conformanceGrantSpec(check, permission.ScopeInvocation, clock.now.Add(time.Minute))
	resolved, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d",
		GrantKey: "grant-1", ApproverKey: "principal-1", ExpectedRevision: pending.Request.Revision,
		Kind: permission.ResolutionApprove, Grant: &spec,
	})
	if err != nil || resolved.Grant == nil {
		t.Fatalf("Resolve() with a grant = %+v, error %v", resolved, err)
	}

	found, err := store.FindGrants(ctx, permission.GrantQuery{TenantKey: "tenant-1", Check: check})
	if err != nil || len(found) != 1 || found[0].GrantKey != "grant-1" {
		t.Fatalf("FindGrants() = %+v, error %v", found, err)
	}

	// Each of these is a different effect than the one the user approved.
	tests := []struct {
		name   string
		mutate func(permission.CheckRequest) permission.CheckRequest
	}{
		{name: "different input", mutate: func(c permission.CheckRequest) permission.CheckRequest {
			c.InputDigest = "sha256:other"
			return c
		}},
		{name: "different tool", mutate: func(c permission.CheckRequest) permission.CheckRequest {
			c.ToolName = "run_command"
			return c
		}},
		{name: "different action", mutate: func(c permission.CheckRequest) permission.CheckRequest {
			c.Action = "workspace.command"
			return c
		}},
		{name: "different resource", mutate: func(c permission.CheckRequest) permission.CheckRequest {
			c.Resource.Key = "/other"
			return c
		}},
		{name: "different tool generation", mutate: func(c permission.CheckRequest) permission.CheckRequest {
			c.ToolGeneration = "tools-v2"
			return c
		}},
		{name: "different policy version", mutate: func(c permission.CheckRequest) permission.CheckRequest {
			c.PolicyVersion = "policy-v2"
			return c
		}},
		{name: "different principal", mutate: func(c permission.CheckRequest) permission.CheckRequest {
			c.Subject.PrincipalKey = "principal-2"
			return c
		}},
	}
	for _, test := range tests {
		t.Run(test.name+" is not authorized", func(t *testing.T) {
			grants, err := store.FindGrants(ctx, permission.GrantQuery{TenantKey: "tenant-1", Check: test.mutate(check)})
			if err != nil || len(grants) != 0 {
				t.Fatalf("FindGrants() = %+v, error %v", grants, err)
			}
		})
	}
	if grants, err := store.FindGrants(ctx, permission.GrantQuery{TenantKey: "tenant-2", Check: check}); err != nil || len(grants) != 0 {
		t.Fatalf("FindGrants() leaked across tenants: %+v, error %v", grants, err)
	}

	// An expired grant stops authorizing even though nothing swept it.
	clock.now = clock.now.Add(2 * time.Minute)
	if grants, err := store.FindGrants(ctx, permission.GrantQuery{TenantKey: "tenant-1", Check: check}); err != nil || len(grants) != 0 {
		t.Fatalf("FindGrants() returned a lapsed grant: %+v, error %v", grants, err)
	}
}

func testGrantLifecycle(t *testing.T, factory PermissionStoreFactory) {
	clock := newFixedClock()
	store := factory(t, clock)
	ctx := context.Background()
	pending := createApprovalRequest(t, store, time.Time{})
	check := conformanceCheck()

	denyWithGrant := permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d",
		GrantKey: "grant-1", ApproverKey: "principal-1", ExpectedRevision: pending.Request.Revision,
		Kind: permission.ResolutionDeny,
	}
	spec := conformanceGrantSpec(check, permission.ScopeInvocation, clock.now.Add(time.Minute))
	denyWithGrant.Grant = &spec
	if _, _, err := store.Resolve(ctx, denyWithGrant); err == nil {
		t.Fatal("Resolve() created a grant while denying the request")
	}
	past := conformanceGrantSpec(check, permission.ScopeInvocation, clock.now.Add(-time.Minute))
	if _, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d",
		GrantKey: "grant-past", ApproverKey: "principal-1", ExpectedRevision: pending.Request.Revision,
		Kind: permission.ResolutionApprove, Grant: &past,
	}); !errors.Is(err, permission.ErrGrantExpired) {
		t.Fatalf("Resolve() with an already-expired grant error = %v, want permission.ErrGrantExpired", err)
	}
	mismatched := conformanceGrantSpec(check, permission.ScopeInvocation, clock.now.Add(time.Minute))
	mismatched.ToolName = "run_command"
	if _, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d",
		GrantKey: "grant-mismatch", ApproverKey: "principal-1", ExpectedRevision: pending.Request.Revision,
		Kind: permission.ResolutionApprove, Grant: &mismatched,
	}); !errors.Is(err, permission.ErrGrantNotApplicable) {
		t.Fatalf("Resolve() with a grant for another tool error = %v, want permission.ErrGrantNotApplicable", err)
	}

	limit := uint64(1)
	single := conformanceGrantSpec(check, permission.ScopeSession, clock.now.Add(time.Minute))
	single.MaxUses = &limit
	resolved, _, err := store.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1", CommandKey: "c", DecisionKey: "d",
		GrantKey: "grant-1", ApproverKey: "principal-1", ExpectedRevision: pending.Request.Revision,
		Kind: permission.ResolutionApprove, Grant: &single,
	})
	if err != nil || resolved.Grant == nil || resolved.Grant.RemainingUses == nil {
		t.Fatalf("Resolve() = %+v, error %v", resolved, err)
	}
	if _, err := store.ConsumeGrant(ctx, permission.ConsumeGrantCommand{
		TenantKey: "tenant-1", GrantKey: "grant-1", Check: check, ExpectedRevision: resolved.Grant.Revision + 9,
	}); !errors.Is(err, permission.ErrRevisionConflict) {
		t.Fatalf("ConsumeGrant() with a stale revision error = %v, want permission.ErrRevisionConflict", err)
	}
	consumed, err := store.ConsumeGrant(ctx, permission.ConsumeGrantCommand{
		TenantKey: "tenant-1", GrantKey: "grant-1", Check: check, ExpectedRevision: resolved.Grant.Revision,
	})
	if err != nil || consumed.State != permission.GrantExhausted {
		t.Fatalf("ConsumeGrant() = %+v, error %v", consumed, err)
	}
	if _, err := store.ConsumeGrant(ctx, permission.ConsumeGrantCommand{
		TenantKey: "tenant-1", GrantKey: "grant-1", Check: check, ExpectedRevision: consumed.Revision,
	}); !errors.Is(err, permission.ErrGrantExhausted) {
		t.Fatalf("ConsumeGrant() past its limit error = %v, want permission.ErrGrantExhausted", err)
	}
	if _, err := store.RevokeGrant(ctx, permission.RevokeGrantCommand{
		TenantKey: "tenant-1", GrantKey: "grant-1", ExpectedRevision: consumed.Revision,
	}); !errors.Is(err, permission.ErrGrantNotApplicable) {
		t.Fatalf("RevokeGrant() on an exhausted grant error = %v, want permission.ErrGrantNotApplicable", err)
	}
	if _, err := store.RevokeGrant(ctx, permission.RevokeGrantCommand{
		TenantKey: "tenant-1", GrantKey: "absent", ExpectedRevision: 1,
	}); !errors.Is(err, permission.ErrGrantNotFound) {
		t.Fatalf("RevokeGrant(absent) error = %v, want permission.ErrGrantNotFound", err)
	}
}
