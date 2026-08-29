package advanced

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

type analystModel struct{}

func (analystModel) Name() string                     { return "analyst" }
func (analystModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (analystModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	message := agent.NewAssistantMessage("支付网关超时占了全部错误。")
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{
		Message: message, FinishReason: agent.FinishStop,
		Usage: agent.Usage{PromptTokens: 30, CompletionTokens: 10, TotalTokens: 40},
	}), nil
}

func TestDelegationReturnsAStructuredResult(t *testing.T) {
	analyst, err := agent.New(agent.Config{
		Key: "ops.analyst", ModelName: "analyst", MaxSteps: 4, AllowedTools: []string{},
	}, analystModel{}, agent.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewDelegation(analyst)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	receipt, err := Delegate(ctx, service, "run-1", "分析 checkout 的错误日志")
	if err != nil {
		t.Fatalf("Delegate() error = %v", err)
	}
	snapshot, err := Collect(ctx, service, receipt)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if snapshot.State != subagent.ChildCompleted {
		t.Fatalf("child state = %s", snapshot.State)
	}
	// The parent gets the answer, not the child's transcript.
	if !strings.Contains(string(snapshot.ResultRef), "支付网关") {
		t.Fatalf("result = %q", snapshot.ResultRef)
	}
	// Usage is settled against the reservation, so the tree budget is accurate.
	if snapshot.Usage.OutputTokens != 10 {
		t.Fatalf("usage = %+v", snapshot.Usage)
	}
}

// TestDelegationRefusesToExceedTheTreeBudget is why the budget is a safety
// limit rather than a report: the third child is refused before it runs, not
// discovered to have overspent afterwards.
func TestDelegationRefusesToExceedTheTreeBudget(t *testing.T) {
	analyst, err := agent.New(agent.Config{
		Key: "ops.analyst", ModelName: "analyst", MaxSteps: 4, AllowedTools: []string{},
	}, analystModel{}, agent.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewDelegation(analyst)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// One tree, 50k input tokens; each child reserves 20k. Two fit, the third
	// does not.
	spawn := func(index int) error {
		_, err := service.Spawn(ctx, subagent.SpawnRequest{
			RequestKey: subagent.RequestKey(fmt.Sprintf("run-budget:child-%d", index)),
			Parent: subagent.ParentRef{
				TenantKey: "local", SessionKey: "local",
				RunKey: "run-budget", TreeKey: "tree-budget",
			},
			AgentKey: "ops.analyst", Input: []byte("task"),
			Limits: subagent.Limits{
				MaxDepth: 2, MaxFanout: 8,
				MaxInputTokens: 50_000, MaxOutputTokens: 50_000,
				MaxCostMicros: 1_000_000, MaxToolCalls: 100, MaxRuntime: time.Hour,
			},
			Reserve: subagent.Reservation{InputTokens: 20_000},
		})
		return err
	}
	for index := range 2 {
		if err := spawn(index); err != nil {
			t.Fatalf("child %d was refused: %v", index, err)
		}
	}
	if err := spawn(2); !errors.Is(err, subagent.ErrTokenBudgetExceeded) {
		t.Fatalf("third child error = %v, want subagent.ErrTokenBudgetExceeded", err)
	}
}

func TestRunFinishedEventIsReplayable(t *testing.T) {
	store := event.NewMemoryStore()
	ctx := context.Background()
	result := &agent.RunResult{
		Outcome: agent.OutcomeCompleted, StopReason: agent.StopReasonComplete,
		Usage: agent.Usage{TotalTokens: 42},
	}
	if _, err := RecordRunFinished(ctx, store, "session-1", result); err != nil {
		t.Fatalf("RecordRunFinished() error = %v", err)
	}
	events, err := ReplayStream(ctx, store, "session-1", 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("ReplayStream() = %d events, error %v", len(events), err)
	}
	if events[0].Type != "agent.run.finished" || !strings.Contains(string(events[0].Payload), `"tokens":42`) {
		t.Fatalf("event = %+v", events[0])
	}
}
