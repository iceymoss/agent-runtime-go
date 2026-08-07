package icoder

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	agentcontext "github.com/iceymoss/agent-runtime-go/context"
)

func TestSQLiteContextPlanStorePersistsExactPlans(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	plans := contextPlanStore{store: store}
	planner, err := agentcontext.NewPlanner(byteCounter{}, plans)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Prepare(context.Background(), contextPrepareRequest("session", 1, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := plans.Get(context.Background(), plan.Ref())
	if err != nil || loaded.Ref() != plan.Ref() || len(loaded.Messages()) != len(plan.Messages()) {
		t.Fatalf("Get() = %#v, %v", loaded.Ref(), err)
	}
	if _, err := plans.Get(context.Background(), agentcontext.PlanRef{TenantKey: "local", PlanKey: plan.Ref().PlanKey, PlanDigest: "wrong"}); !errors.Is(err, agentcontext.ErrPlanDrift) {
		t.Fatalf("Get(drifted ref) error = %v", err)
	}
}

func TestSQLiteContextPivotAndMessageTail(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, _, err := store.Load(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	first := agent.RunResult{Messages: []agent.Message{agent.NewAssistantMessage("first")}}
	if err := store.CommitTurn(ctx, snapshot, "request-1", "input-1", agent.NewUserMessage("one"), first); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err = store.Load(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	second := agent.RunResult{Messages: []agent.Message{agent.NewAssistantMessage("second")}}
	if err := store.CommitTurn(ctx, snapshot, "request-2", "input-2", agent.NewUserMessage("two"), second); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err = store.Load(ctx, "session")
	if err != nil {
		t.Fatal(err)
	}
	tail, err := store.MessagesAfterRevision(ctx, "session", 1)
	if err != nil || len(tail) != 2 || tail[0].Text() != "two" || tail[1].Text() != "second" {
		t.Fatalf("MessagesAfterRevision() = %#v, %v", tail, err)
	}
	pivot := agentcontext.PivotRef{Artifact: agentcontext.ArtifactRef{Kind: "summary", Key: "sum", Digest: "digest", Schema: "context-summary/v1", Generation: "test"}, CoveredThrough: 1, SourceDigest: "source", FactSetDigest: "facts"}
	if err := store.SavePivot(ctx, snapshot, pivot); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := store.Load(ctx, "session")
	if err != nil || loaded.Pivot == nil || *loaded.Pivot != pivot {
		t.Fatalf("Load() pivot = %#v, %v", loaded.Pivot, err)
	}
	if err := store.SavePivot(ctx, SessionSnapshot{ID: "session", Revision: 0}, pivot); err == nil {
		t.Fatal("SavePivot() accepted a stale revision")
	}
}

func contextPrepareRequest(session string, revision uint64, instruction string) agentcontext.PrepareRequest {
	return agentcontext.PrepareRequest{
		Source:             agentcontext.SourceRef{TenantKey: "local", SessionKey: session, SessionRevision: revision},
		Runtime:            agentcontext.RuntimeArtifacts{DefinitionDigest: "runtime", ProjectionVersion: "test/v1", TokenizerID: byteCounter{}.ID(), SystemMessages: []agent.Message{agent.NewSystemMessage("system")}},
		InvocationMessages: []agent.Message{agent.NewUserMessage(instruction)},
		Budget:             agentcontext.Budget{ContextTokens: 1000, ReservedOutputTokens: 100, SafetyMarginTokens: 10},
	}
}
