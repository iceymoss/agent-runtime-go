package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
)

// Policy decides what may run without asking.
//
// It is a pure decision — allow, deny, or ask — separated from the tool so the
// rule is auditable and can be versioned. The version travels with every
// decision, so a recorded run says which rules produced it.
func Policy() permission.Policy {
	return permission.PolicyFunc{
		PolicyVersion: "ops/v1",
		EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
			result := permission.CheckResult{PolicyVersion: "ops/v1", InputDigest: request.InputDigest}
			switch request.ToolName {
			case "read_logs":
				result.Decision, result.RuleKey = permission.DecisionAllow, "read-only"
			case "restart_service":
				result.Decision, result.RuleKey = permission.DecisionAsk, "service-restart"
			default:
				// Refusing by default matters: a tool added later is denied until
				// someone decides about it, rather than silently permitted.
				result.Decision, result.RuleKey = permission.DecisionDeny, "unknown-tool"
			}
			return result, nil
		},
	}
}

// Gate wraps a tool so the policy runs before it does.
//
// This is where authorization has to live. A prompt saying "ask before
// restarting" is not a boundary — the model is probabilistic and user input can
// override it — so the check sits between the runtime and the side effect,
// where nothing can talk its way past.
type Gate struct {
	inner    agent.Tool
	policy   permission.Policy
	approved func(tool string) bool
}

func NewGate(inner agent.Tool, policy permission.Policy, approved func(string) bool) *Gate {
	return &Gate{inner: inner, policy: policy, approved: approved}
}

func (g *Gate) Definition() agent.ToolDefinition { return g.inner.Definition() }
func (g *Gate) ReplayPolicy() agent.ReplayPolicy { return g.inner.ReplayPolicy() }

// OwnsToolExecutionLifecycle is required for a tool that can suspend.
//
// Without it the runtime opens its own effect boundary around every call, and a
// suspended call leaves that effect running under a fence the resumed attempt no
// longer holds — the resume then fails with "effect is not prepared". Declaring
// ownership says this tool's backend accounts for its own effects, which here
// means it has none to account for: approval happens before anything executes.
// A tool with real side effects that must survive a crash should go through the
// tool subpackage's executor instead, which keeps a ledger.
func (g *Gate) OwnsToolExecutionLifecycle() bool { return true }

func (g *Gate) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	digest, err := agent.DigestToolInput(invocation.RawInput)
	if err != nil {
		return agent.ToolResult{}, err
	}
	decision, err := g.policy.Evaluate(ctx, permission.CheckRequest{
		RequestKey:    permission.RequestKey(invocation.CallID),
		ToolName:      g.inner.Definition().Name,
		ToolCallID:    invocation.CallID,
		InputDigest:   permission.InputDigest(digest),
		PolicyVersion: g.policy.Version(),
	}, nil)
	if err != nil {
		return agent.ToolResult{}, err
	}

	switch decision.Decision {
	case permission.DecisionAllow:
		return g.inner.Execute(ctx, invocation)

	case permission.DecisionDeny:
		// Denial is a result the model can see and work around, not a crash.
		return agent.ToolResult{IsError: true, StopTurn: true,
			Content: fmt.Sprintf("策略拒绝了 %s（规则 %s）", g.inner.Definition().Name, decision.RuleKey)}, nil

	case permission.DecisionAsk:
		// A resumed invocation carries the handle this tool issued last time.
		// Its presence is what says a human has since answered.
		if invocation.Resume != nil && g.approved(g.inner.Definition().Name) {
			return g.inner.Execute(ctx, invocation)
		}
		if invocation.Resume != nil {
			return agent.ToolResult{IsError: true, StopTurn: true, Content: "审批被拒绝"}, nil
		}
		// Park the whole run. The runtime checkpoints it, so the process can die
		// here and the decision can still be answered afterwards.
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{
			Kind:        agent.ToolSuspensionApproval,
			RequestRef:  invocation.CallID,
			ResumeToken: digest,
		}}
	}
	return agent.ToolResult{}, fmt.Errorf("未知的权限判定 %q", decision.Decision)
}
