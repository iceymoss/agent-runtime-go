package icoder

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go/permission"
)

const (
	// tenantKey is untyped on purpose so it satisfies both agent.TenantKey and
	// permission.TenantKey without a conversion at every call site.
	tenantKey = "local"
	// principalKey identifies the human running the CLI. iCoder is single-user,
	// but the permission records still name a principal so an exported audit
	// trail is meaningful.
	principalKey = "cli-user"
	// grantTTL is short by design. The grant created when a user approves a call
	// exists only to carry that decision into the same execution's authorization
	// check; it is not a standing permission.
	grantTTL = 2 * time.Minute
)

// PermissionGate owns iCoder's local authorization policy and the store behind
// it. It exists as a type rather than a bare permission.Service because the
// approval interceptor needs one capability the Service port deliberately does
// not offer: evaluating a decision without creating an approval request.
//
// That distinction matters. A non-interactive run must be able to discover that
// a call needs approval and suspend, rather than leaving a stray pending request
// behind for every preflight it performs.
type PermissionGate struct {
	service permission.Service
	store   permission.Store
	policy  permission.Policy
}

// NewPermissionGate builds the policy and service iCoder shares between the
// approval interceptor, the tool executor, and the CLI.
//
// allowWrites is a process-level authorization for the whole workspace. It is
// not a sandbox: tools still run as the current user, and path confinement is
// enforced inside the tools themselves.
//
// store may be nil, which falls back to the in-memory reference implementation.
// The application passes a persistent store so an approval a run is waiting on
// can still be answered after the process that asked for it has exited.
func NewPermissionGate(allowWrites bool, store permission.Store) (*PermissionGate, error) {
	policy := permission.PolicyFunc{PolicyVersion: policyVersion, EvaluateFunc: func(_ context.Context, request permission.CheckRequest, grants []permission.Grant) (permission.CheckResult, error) {
		result := permission.CheckResult{PolicyVersion: request.PolicyVersion, InputDigest: request.InputDigest}
		// FindGrants only returns grants that already match this exact request,
		// so any grant here is an approval the user gave for this exact call.
		if len(grants) > 0 {
			result.Decision, result.RuleKey = permission.DecisionAllow, "approved-grant"
			return result, nil
		}
		switch request.Action {
		case "workspace.read", "workspace.search", "network.read", "subagent.spawn":
			result.Decision, result.RuleKey = permission.DecisionAllow, "safe-local-operation"
		case "workspace.write", "workspace.command", "workspace.commit", "network.tool":
			if allowWrites {
				result.Decision, result.RuleKey = permission.DecisionAllow, "cli-write-flag"
			} else {
				result.Decision, result.RuleKey = permission.DecisionAsk, "workspace-write-approval"
				result.Constraint = permission.GrantConstraint{Scope: permission.ScopeInvocation, ExpiresAt: request.ApprovalExpiresAt}
			}
		default:
			result.Decision, result.ReasonCode = permission.DecisionDeny, "no-matching-rule"
		}
		return result, nil
	}}
	if store == nil {
		store = permission.NewMemoryStore()
	}
	service, err := permission.NewService(permission.ServiceOptions{Policy: policy, Store: store})
	if err != nil {
		return nil, err
	}
	return &PermissionGate{service: service, store: store, policy: policy}, nil
}

// Service returns the port the tool executor and the CLI share.
func (g *PermissionGate) Service() permission.Service { return g.service }

// Evaluate answers "what would Check decide?" without the side effect Check has
// for an ask decision. It reads the same grants and runs the same policy, so a
// preflight can never disagree with the authorization that follows it.
func (g *PermissionGate) Evaluate(ctx context.Context, request permission.CheckRequest) (permission.CheckResult, error) {
	grants, err := g.store.FindGrants(ctx, permission.GrantQuery{TenantKey: request.Subject.TenantKey, Check: request})
	if err != nil {
		return permission.CheckResult{}, err
	}
	result, err := g.policy.Evaluate(ctx, request, grants)
	if err != nil {
		return permission.CheckResult{}, err
	}
	if result.PolicyVersion == "" {
		result.PolicyVersion = g.policy.Version()
	}
	if result.InputDigest == "" {
		result.InputDigest = request.InputDigest
	}
	return result, nil
}

// PendingApprovals lists the approval requests a suspended run is waiting on, so
// a non-interactive workflow can show a human what needs a decision.
func (g *PermissionGate) PendingApprovals(ctx context.Context) ([]permission.Snapshot, error) {
	return g.store.ListPending(ctx, permission.ListPendingQuery{TenantKey: tenantKey})
}
