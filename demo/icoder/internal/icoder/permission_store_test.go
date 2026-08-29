package icoder

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/permission"
)

func newPermissionStore(t *testing.T, clock func() time.Time) *SQLitePermissionStore {
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
	return NewSQLitePermissionStore(store.db, clock)
}

func approvalCheck() permission.CheckRequest {
	return permission.CheckRequest{
		RequestKey: "request-1",
		Subject:    permission.Subject{TenantKey: tenantKey, PrincipalKey: principalKey, ActorType: "human"},
		Resource:   permission.Resource{Kind: "workspace", Key: "/repo"}, SessionRef: "session-1",
		RunRef: "run-1", AttemptRef: "attempt-1", ExecutionRef: "execution-1", FenceToken: 3,
		ToolCallID: "call-1", ToolName: "write_file", Action: "workspace.write",
		InputDigest: "sha256:input", ToolGeneration: "tools-v1", PolicyVersion: policyVersion,
	}
}

func createApproval(t *testing.T, store *SQLitePermissionStore, expiresAt time.Time) permission.Snapshot {
	t.Helper()
	snapshot, created, err := store.CreateAndSuspend(context.Background(), permission.CreateApprovalCommand{
		Request:     permission.ApprovalRequest{RequestKey: "request-1", Check: approvalCheck(), ExpiresAt: expiresAt},
		ResumeToken: "resume-token",
	})
	if err != nil || !created || snapshot.Request.State != permission.ApprovalPending {
		t.Fatalf("CreateAndSuspend() = %+v, created %v, error %v", snapshot, created, err)
	}
	return snapshot
}

func TestSQLitePermissionStoreCreateIsIdempotentByRequestKey(t *testing.T) {
	store := newPermissionStore(t, nil)
	first := createApproval(t, store, time.Time{})
	second, created, err := store.CreateAndSuspend(context.Background(), permission.CreateApprovalCommand{
		Request:     permission.ApprovalRequest{RequestKey: "request-1", Check: approvalCheck()},
		ResumeToken: "resume-token",
	})
	if err != nil || created || second.Request.Revision != first.Request.Revision {
		t.Fatalf("replayed CreateAndSuspend() = %+v, created %v, error %v", second, created, err)
	}
	changed := approvalCheck()
	changed.InputDigest = "sha256:other"
	if _, _, err := store.CreateAndSuspend(context.Background(), permission.CreateApprovalCommand{
		Request:     permission.ApprovalRequest{RequestKey: "request-1", Check: changed},
		ResumeToken: "resume-token",
	}); !errors.Is(err, permission.ErrRequestConflict) {
		t.Fatalf("conflicting CreateAndSuspend() error = %v, want permission.ErrRequestConflict", err)
	}
}

func TestSQLitePermissionStoreResolveCreatesAnApplicableGrant(t *testing.T) {
	store := newPermissionStore(t, nil)
	pending := createApproval(t, store, time.Time{})
	check := approvalCheck()
	spec := permission.GrantSpec{
		Scope: permission.ScopeInvocation, PrincipalKey: principalKey, SessionRef: check.SessionRef,
		ToolName: check.ToolName, Action: check.Action, ResourceKind: check.Resource.Kind, ResourceKey: check.Resource.Key,
		InputDigest: check.InputDigest, PolicyVersion: check.PolicyVersion, ToolGeneration: check.ToolGeneration,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	command := permission.ResolveCommand{
		TenantKey: tenantKey, RequestKey: "request-1", CommandKey: "command", DecisionKey: "decision",
		GrantKey: "grant-1", ApproverKey: principalKey, ExpectedRevision: pending.Request.Revision,
		Kind: permission.ResolutionApprove, ReasonCode: "user-approved-once", Grant: &spec,
	}
	resolved, applied, err := store.Resolve(context.Background(), command)
	if err != nil || !applied || resolved.Request.State != permission.ApprovalApproved || resolved.Grant == nil {
		t.Fatalf("Resolve() = %+v, applied %v, error %v", resolved, applied, err)
	}
	if _, applied, err := store.Resolve(context.Background(), command); err != nil || applied {
		t.Fatalf("replayed Resolve() applied = %v, error = %v", applied, err)
	}
	grants, err := store.FindGrants(context.Background(), permission.GrantQuery{TenantKey: tenantKey, Check: check})
	if err != nil || len(grants) != 1 || grants[0].GrantKey != "grant-1" {
		t.Fatalf("FindGrants() = %+v, error %v", grants, err)
	}
	// A grant is bound to the exact input it authorized; a different input must
	// not inherit the decision.
	other := check
	other.InputDigest = "sha256:different"
	if grants, err := store.FindGrants(context.Background(), permission.GrantQuery{TenantKey: tenantKey, Check: other}); err != nil || len(grants) != 0 {
		t.Fatalf("FindGrants(other input) = %+v, error %v", grants, err)
	}
}

func TestSQLitePermissionStoreRejectsStaleRevisionsAndTerminalStates(t *testing.T) {
	store := newPermissionStore(t, nil)
	pending := createApproval(t, store, time.Time{})
	deny := permission.ResolveCommand{
		TenantKey: tenantKey, RequestKey: "request-1", CommandKey: "deny", DecisionKey: "deny",
		ApproverKey: principalKey, ExpectedRevision: pending.Request.Revision, Kind: permission.ResolutionDeny,
	}
	if _, _, err := store.Resolve(context.Background(), deny); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		call    func() error
		wantErr error
	}{
		{
			name: "resolving again with a different decision",
			call: func() error {
				other := deny
				other.CommandKey, other.DecisionKey = "approve", "approve"
				other.Kind = permission.ResolutionApprove
				_, _, err := store.Resolve(context.Background(), other)
				return err
			},
			wantErr: permission.ErrAlreadyResolved,
		},
		{
			name: "canceling a resolved request",
			call: func() error {
				_, _, err := store.Cancel(context.Background(), permission.CancelCommand{
					TenantKey: tenantKey, RequestKey: "request-1", ExpectedRevision: 2, AttemptRef: "attempt-1", FenceToken: 3,
				})
				return err
			},
			wantErr: permission.ErrAlreadyResolved,
		},
		{
			name: "reading a request that does not exist",
			call: func() error {
				_, err := store.GetRequest(context.Background(), permission.GetRequestQuery{TenantKey: tenantKey, RequestKey: "missing"})
				return err
			},
			wantErr: permission.ErrRequestNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestSQLitePermissionStoreExpiresPendingRequestsLazily(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC)
	clock := &toolTestClock{now: now}
	store := newPermissionStore(t, clock.Now)
	createApproval(t, store, now.Add(time.Minute))
	if pending, err := store.ListPending(context.Background(), permission.ListPendingQuery{TenantKey: tenantKey}); err != nil || len(pending) != 1 {
		t.Fatalf("ListPending() = %d, error %v", len(pending), err)
	}
	// Every read applies expiry, so a lapsed approval can never be answered even
	// if no sweep has run.
	clock.now = now.Add(2 * time.Minute)
	snapshot, err := store.GetRequest(context.Background(), permission.GetRequestQuery{TenantKey: tenantKey, RequestKey: "request-1"})
	if err != nil || snapshot.Request.State != permission.ApprovalExpired {
		t.Fatalf("GetRequest() = %+v, error %v", snapshot.Request.State, err)
	}
	if pending, err := store.ListPending(context.Background(), permission.ListPendingQuery{TenantKey: tenantKey}); err != nil || len(pending) != 0 {
		t.Fatalf("ListPending() after expiry = %d, error %v", len(pending), err)
	}
}

func TestSQLitePermissionStoreCancelRequiresTheOwningFence(t *testing.T) {
	store := newPermissionStore(t, nil)
	pending := createApproval(t, store, time.Time{})
	_, _, err := store.Cancel(context.Background(), permission.CancelCommand{
		TenantKey: tenantKey, RequestKey: "request-1", ExpectedRevision: pending.Request.Revision,
		AttemptRef: "attempt-1", FenceToken: 99,
	})
	if !errors.Is(err, permission.ErrStaleFence) {
		t.Fatalf("Cancel() with a foreign fence error = %v, want permission.ErrStaleFence", err)
	}
	snapshot, applied, err := store.Cancel(context.Background(), permission.CancelCommand{
		TenantKey: tenantKey, RequestKey: "request-1", ExpectedRevision: pending.Request.Revision,
		AttemptRef: "attempt-1", FenceToken: 3,
	})
	if err != nil || !applied || snapshot.Request.State != permission.ApprovalCanceled {
		t.Fatalf("Cancel() = %+v, applied %v, error %v", snapshot.Request.State, applied, err)
	}
}

func TestSQLitePermissionStoreRevokeAndConsumeGrant(t *testing.T) {
	store := newPermissionStore(t, nil)
	pending := createApproval(t, store, time.Time{})
	check := approvalCheck()
	limit := uint64(1)
	spec := permission.GrantSpec{
		Scope: permission.ScopeSession, PrincipalKey: principalKey, SessionRef: check.SessionRef,
		ToolName: check.ToolName, Action: check.Action, ResourceKind: check.Resource.Kind, ResourceKey: check.Resource.Key,
		PolicyVersion: check.PolicyVersion, ToolGeneration: check.ToolGeneration,
		ExpiresAt: time.Now().UTC().Add(time.Minute), MaxUses: &limit,
	}
	resolved, _, err := store.Resolve(context.Background(), permission.ResolveCommand{
		TenantKey: tenantKey, RequestKey: "request-1", CommandKey: "command", DecisionKey: "decision",
		GrantKey: "grant-1", ApproverKey: principalKey, ExpectedRevision: pending.Request.Revision,
		Kind: permission.ResolutionApprove, Grant: &spec,
	})
	if err != nil || resolved.Grant == nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	consumed, err := store.ConsumeGrant(context.Background(), permission.ConsumeGrantCommand{
		TenantKey: tenantKey, GrantKey: "grant-1", Check: check, ExpectedRevision: resolved.Grant.Revision,
	})
	if err != nil || consumed.State != permission.GrantExhausted {
		t.Fatalf("ConsumeGrant() = %+v, error %v", consumed.State, err)
	}
	if _, err := store.RevokeGrant(context.Background(), permission.RevokeGrantCommand{
		TenantKey: tenantKey, GrantKey: "grant-1", ExpectedRevision: consumed.Revision,
	}); !errors.Is(err, permission.ErrGrantNotApplicable) {
		t.Fatalf("RevokeGrant() on an exhausted grant error = %v, want permission.ErrGrantNotApplicable", err)
	}
}

// TestSQLitePermissionStoreConformance holds this adapter to the same approval
// contract as the library's reference implementation.
func TestSQLitePermissionStoreConformance(t *testing.T) {
	agenttest.TestPermissionStore(t, func(t *testing.T, clock permission.Clock) permission.Store {
		return newPermissionStore(t, clock.Now)
	})
}
