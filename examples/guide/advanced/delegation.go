package advanced

import (
	"context"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

const tenant = agent.TenantKey("local")

// childRunner executes one child run. The child is a full agent with its own
// context and its own — deliberately read-only — tool set.
type childRunner struct{ analyst *agent.Agent }

func (r *childRunner) Run(ctx context.Context, request subagent.RunRequest) (subagent.RunResult, error) {
	result, err := r.analyst.Run(ctx, agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("你是日志分析助手，只回答看到了什么。"),
			agent.NewUserMessage(string(request.Input)),
		},
	})
	if err != nil {
		// Retryable failures come back as a suspension so the child can be
		// picked up again rather than being written off.
		return subagent.RunResult{
			State:   subagent.ChildSuspended,
			Failure: &subagent.Failure{Code: "attempt_failed", Message: err.Error(), Retryable: true},
		}, nil
	}
	return subagent.RunResult{
		State:        subagent.ChildCompleted,
		ResultRef:    subagent.ResultRef(result.Text),
		UsageFactKey: subagent.UsageFactKey(request.Child.RunKey),
		Usage: subagent.Usage{
			InputTokens:  int64(result.Usage.PromptTokens),
			OutputTokens: int64(result.Usage.CompletionTokens),
		},
	}, nil
}

// parentWaker is told when a child reaches a terminal state. It must be
// idempotent by WakeKey: a committed wake can be delivered twice.
type parentWaker struct{ woken map[subagent.WakeKey]bool }

func (w *parentWaker) Wake(_ context.Context, request subagent.WakeRequest) error {
	w.woken[request.WakeKey] = true
	return nil
}

// NewDelegation assembles the child-run service.
func NewDelegation(analyst *agent.Agent) (*subagent.Service, error) {
	return subagent.New(subagent.Options{
		Store:         subagent.NewMemoryStore(),
		Runner:        &childRunner{analyst: analyst},
		ParentWaker:   &parentWaker{woken: map[subagent.WakeKey]bool{}},
		WorkerID:      "worker-1",
		LeaseDuration: 5 * time.Minute,
	})
}

// Delegate hands one task to a child and returns the receipt the parent parks on.
//
// The limits are not bookkeeping. Without MaxDepth an agent that can delegate
// can delegate a tree with no bottom; without the token reservation one child
// can spend the whole tree's budget. Reserve is taken before the child runs,
// which is what makes the limit binding rather than advisory.
func Delegate(ctx context.Context, service *subagent.Service, parentRunKey, task string) (subagent.SpawnReceipt, error) {
	return service.Spawn(ctx, subagent.SpawnRequest{
		RequestKey: subagent.RequestKey(parentRunKey + ":analyze"),
		Parent: subagent.ParentRef{
			TenantKey: tenant, SessionKey: "local", RunKey: subagent.RunKey(parentRunKey),
			TreeKey: subagent.TreeKey(parentRunKey),
		},
		AgentKey: "ops.analyst",
		Input:    []byte(task),
		Limits: subagent.Limits{
			MaxDepth: 2, MaxFanout: 4,
			MaxInputTokens: 50_000, MaxOutputTokens: 10_000,
			MaxCostMicros: 1_000_000, MaxToolCalls: 20, MaxRuntime: 5 * time.Minute,
		},
		Reserve: subagent.Reservation{
			InputTokens: 20_000, OutputTokens: 4_000,
			CostMicros: 200_000, ToolCalls: 8, Runtime: time.Minute,
		},
	})
}

// Collect drives the child and reads its answer.
//
// A real application does not busy-wait: the parent parks with a
// ToolSuspensionExternal and a worker calls RunNext, so a crash while the child
// works leaves a resumable parent rather than a lost delegation.
func Collect(ctx context.Context, service *subagent.Service, receipt subagent.SpawnReceipt) (subagent.Snapshot, error) {
	if _, _, err := service.RunNext(ctx, tenant); err != nil {
		return subagent.Snapshot{}, err
	}
	if _, err := service.Reconcile(ctx, subagent.ReconcileRequest{TenantKey: tenant, Limit: 10}); err != nil {
		return subagent.Snapshot{}, err
	}
	return service.Get(ctx, tenant, receipt.Child.RelationshipKey)
}
