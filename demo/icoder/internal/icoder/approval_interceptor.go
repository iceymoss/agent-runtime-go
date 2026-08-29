package icoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	toollifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

// approvalInterceptor is the preflight stage that decides whether a tool call is
// allowed to reach the effect boundary at all.
//
// It sits in front of the executor's own authorization for one reason: when a
// human is present, a refusal should be something the model can see and correct,
// not a failed run. The executor treats a denial as fatal to the attempt, which
// is right for an unattended worker and wrong for an interactive coding session.
// So this interceptor short-circuits a denial with a model-visible tool result,
// and lets everything it approves fall through to the executor, which
// re-authorizes it against the grant the approval created.
//
// When no approval callback is available - a scripted `icoder run` - the
// interceptor deliberately does nothing on an ask decision. The executor then
// produces a suspension blocker and the run suspends durably, so a human can
// approve it later instead of the agent silently proceeding or silently failing.
type approvalInterceptor struct {
	permissions *PermissionGate
	catalog     *ToolCatalog
	actions     map[string]string
	now         func() time.Time
}

func (*approvalInterceptor) Name() string { return "icoder.approval" }

// Version is the policy version, so changing the policy changes the frozen
// generation digest and therefore invalidates approvals granted under the old one.
func (*approvalInterceptor) Version() string { return string(policyVersion) }

func (i *approvalInterceptor) Before(ctx context.Context, invocation toollifecycle.InvocationIdentity) (toollifecycle.PreflightResult, error) {
	action, ok := i.actions[invocation.ToolName]
	if !ok {
		return toollifecycle.PreflightResult{}, fmt.Errorf("tool %q has no declared action", invocation.ToolName)
	}
	check, err := i.checkRequest(invocation, action)
	if err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	decision, err := i.permissions.Evaluate(ctx, check)
	if err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	run := currentRunContext(ctx)
	if err := recordRunFact(ctx, run, invocation.CallID+":permission-checked", "agent.permission.checked", map[string]any{
		"tool": invocation.ToolName, "action": action, "resource": check.Resource,
		"input_digest": check.InputDigest, "decision": decision.Decision, "reason_code": decision.ReasonCode,
	}); err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	switch decision.Decision {
	case permission.DecisionAllow:
		return toollifecycle.PreflightResult{Decision: toollifecycle.PreflightContinue}, nil
	case permission.DecisionDeny:
		return denied("permission denied: "+decision.ReasonCode, invocation), nil
	case permission.DecisionAsk:
		if run.approve == nil {
			// Nobody can answer right now. Falling through lets the executor
			// create the durable blocker that suspends the run for later approval.
			return toollifecycle.PreflightResult{Decision: toollifecycle.PreflightContinue}, nil
		}
		return i.ask(ctx, invocation, check, run)
	default:
		return denied("permission denied: unknown decision", invocation), nil
	}
}

// After and OnError exist to satisfy the interceptor contract. Authorization has
// nothing left to say once the effect has happened; the audit interceptor owns
// the record of what did happen.
func (*approvalInterceptor) After(_ context.Context, _ toollifecycle.PreparedExecution, result agent.ToolResult) (agent.ToolResult, error) {
	return result, nil
}

func (*approvalInterceptor) OnError(context.Context, toollifecycle.PreparedExecution, error) error {
	return nil
}

// ask creates the durable approval request, puts it in front of the human, and
// records the answer. Approving creates a short-lived invocation grant so the
// executor's own authorization allows exactly this call and nothing else.
func (i *approvalInterceptor) ask(ctx context.Context, invocation toollifecycle.InvocationIdentity, check permission.CheckRequest, run runContext) (toollifecycle.PreflightResult, error) {
	service := i.permissions.Service()
	result, err := service.Check(ctx, check)
	if err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	if result.Decision != permission.DecisionAsk || result.Approval == nil {
		// The decision changed between the dry run and the request, which means
		// another approval already settled it. Let the executor authorize again.
		return toollifecycle.PreflightResult{Decision: toollifecycle.PreflightContinue}, nil
	}
	if err := recordRunFact(ctx, run, invocation.CallID+":approval-requested", "agent.approval.requested", map[string]any{
		"tool": invocation.ToolName, "action": check.Action, "resource": check.Resource, "input_digest": check.InputDigest,
	}); err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	choice, err := run.approve(ctx, ApprovalPrompt{
		ToolName: invocation.ToolName, Action: check.Action, Resource: check.Resource.Key,
		Input: approvalInput(invocation.RawInput), ExpiresAt: check.ApprovalExpiresAt,
	})
	if err != nil {
		i.cancel(ctx, check, result.Approval.Request.Revision)
		return toollifecycle.PreflightResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	kind, reason := permission.ResolutionDeny, "user-rejected"
	switch choice {
	case ApprovalApproveOnce:
		kind, reason = permission.ResolutionApprove, "user-approved-once"
	case ApprovalApproveAuto:
		kind, reason = permission.ResolutionApprove, "user-approved-auto"
	}
	command := permission.ResolveCommand{
		TenantKey: tenantKey, RequestKey: check.RequestKey,
		CommandKey: string(check.RequestKey) + ":" + string(kind), ApproverKey: principalKey,
		ExpectedRevision: result.Approval.Request.Revision, Kind: kind, ReasonCode: reason,
	}
	command.DecisionKey = permission.DecisionKey(command.CommandKey)
	if kind == permission.ResolutionApprove {
		command.GrantKey = permission.GrantKey(string(check.RequestKey) + ":grant")
		command.Grant = &permission.GrantSpec{
			Scope: permission.ScopeInvocation, PrincipalKey: principalKey, SessionRef: check.SessionRef,
			ToolName: check.ToolName, Action: check.Action,
			ResourceKind: check.Resource.Kind, ResourceKey: check.Resource.Key,
			InputDigest: check.InputDigest, PolicyVersion: check.PolicyVersion,
			ToolGeneration: check.ToolGeneration, ExpiresAt: i.now().Add(grantTTL),
		}
	}
	if _, _, err := service.Resolve(ctx, command); err != nil {
		if message, terminal := approvalTerminalMessage(err); terminal {
			return denied(message, invocation), nil
		}
		return toollifecycle.PreflightResult{}, err
	}
	if err := recordRunFact(ctx, run, invocation.CallID+":approval-resolved", "agent.approval.resolved", map[string]any{
		"tool": invocation.ToolName, "resolution": kind, "reason_code": reason,
	}); err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	if kind == permission.ResolutionDeny {
		return denied("permission denied by user", invocation), nil
	}
	return toollifecycle.PreflightResult{Decision: toollifecycle.PreflightContinue}, nil
}

// cancel releases an approval request the user never answered, on a context that
// outlives the canceled run so the record does not stay pending forever.
func (i *approvalInterceptor) cancel(ctx context.Context, check permission.CheckRequest, revision uint64) {
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer stop()
	_, _, _ = i.permissions.Service().Cancel(cleanup, permission.CancelCommand{
		TenantKey: tenantKey, RequestKey: check.RequestKey, ExpectedRevision: revision,
		AttemptRef: check.AttemptRef, FenceToken: check.FenceToken,
	})
}

// checkRequest binds the authorization decision to the exact call. The request
// key, input digest, and tool generation are all derived, never supplied, so two
// different calls can never share one decision.
func (i *approvalInterceptor) checkRequest(invocation toollifecycle.InvocationIdentity, action string) (permission.CheckRequest, error) {
	inputDigest, err := canonicalToolInputDigest(invocation.RawInput)
	if err != nil {
		return permission.CheckRequest{}, err
	}
	requestKey := permission.RequestKey(digest([]byte(invocation.RunKey + "\x00" + invocation.CallID + "\x00" + invocation.ToolName + "\x00" + string(inputDigest))))
	return permission.CheckRequest{
		RequestKey: requestKey,
		Subject:    permission.Subject{TenantKey: tenantKey, PrincipalKey: principalKey, ActorType: "human"},
		Resource:   i.catalog.resource(invocation.ToolName), SessionRef: invocation.SessionRef,
		RunRef: permission.RunRef(invocation.RunKey), AttemptRef: permission.AttemptRef(invocation.AttemptKey),
		ExecutionRef: permission.ExecutionRef(requestKey), FenceToken: invocation.FenceToken,
		ToolCallID: invocation.CallID, ToolName: invocation.ToolName, Action: action,
		InputDigest: inputDigest, ToolGeneration: i.catalog.GenerationDigest(),
		PolicyVersion: policyVersion, ApprovalExpiresAt: i.now().Add(approvalTTL),
	}, nil
}

// canonicalToolInputDigest reproduces the digest the executor computes from the
// canonical form of a tool's input. Both sides must agree, because that digest
// is what binds an approval to one specific set of arguments.
func canonicalToolInputDigest(raw string) (permission.InputDigest, error) {
	if raw == "" {
		raw = "{}"
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return "", fmt.Errorf("tool input is not a JSON object: %w", err)
	}
	value2, err := agent.CanonicalDigest(value)
	if err != nil {
		return "", err
	}
	return permission.InputDigest(value2), nil
}

// denied builds the model-visible refusal. StopTurn ends the turn so the agent
// reports what it could not do instead of trying the same thing again.
func denied(message string, invocation toollifecycle.InvocationIdentity) toollifecycle.PreflightResult {
	return toollifecycle.PreflightResult{
		Decision: toollifecycle.PreflightReturn,
		Result:   &agent.ToolResult{ToolCallID: invocation.CallID, Name: invocation.ToolName, Content: message, IsError: true, StopTurn: true},
	}
}

// approvalTerminalMessage turns a permission lifecycle error into a sentence a
// user can act on, for the cases where retrying the same approval is pointless.
func approvalTerminalMessage(err error) (string, bool) {
	switch {
	case errors.Is(err, permission.ErrRequestExpired), errors.Is(err, permission.ErrGrantExpired):
		return "Approval expired before it was confirmed. Run the task again to request a new approval.", true
	case errors.Is(err, permission.ErrRequestCanceled):
		return "Approval was canceled. Run the task again if you still want to continue.", true
	case errors.Is(err, permission.ErrPermissionDenied):
		return "Permission was denied. The requested action was not performed.", true
	default:
		return "", false
	}
}
