package context_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	agentcontext "github.com/iceymoss/agent-runtime-go/context"
)

type textCounter struct{}

func (textCounter) ID() string { return "text/v1" }

func (textCounter) CountTokens(_ context.Context, messages []agent.Message) (int, error) {
	total := 0
	for _, message := range messages {
		for _, part := range message.Parts {
			total += len(part.Text)
			if part.ToolCall != nil {
				total += len(part.ToolCall.Input)
			}
			if part.ToolResult != nil {
				total += len(part.ToolResult.Content)
			}
		}
	}
	return total, nil
}

func TestNormalizeHistoryPairingRepairAndDeepCopy(t *testing.T) {
	call := agent.Message{Role: agent.RoleAssistant, FinishReason: agent.FinishToolCalls, Parts: []agent.ContentPart{
		{Type: agent.PartText, Text: "checking"},
		{Type: agent.PartToolCall, ToolCall: &agent.ToolCall{ID: "b", Name: "lookup", Input: "{"}},
		{Type: agent.PartToolCall, ToolCall: &agent.ToolCall{ID: "a", Name: "lookup", Input: `{}`}},
	}}
	result, err := agentcontext.NormalizeHistory(agentcontext.NormalizeRequest{
		Messages: []agent.Message{call}, Policy: agentcontext.RepairFromTerminal,
		TerminalFacts: []agentcontext.TerminalFact{
			{CallID: "a", Reason: "canceled", SourceRevision: 7},
			{CallID: "b", Reason: "worker lost", SourceRevision: 7, EffectUnknown: true},
		},
	})
	if err != nil {
		t.Fatalf("NormalizeHistory() error = %v", err)
	}
	if len(result.Messages) != 2 || len(result.Exchanges) != 1 || len(result.Messages[1].ToolResults()) != 2 {
		t.Fatalf("unexpected normalized result: %#v", result)
	}
	if result.Messages[1].ToolResults()[0].ToolCallID != "a" {
		t.Fatalf("synthetic results are not deterministic: %#v", result.Messages[1].ToolResults())
	}
	call.Parts[1].ToolCall.Name = "mutated"
	if result.Messages[0].ToolCalls()[0].Name != "lookup" {
		t.Fatal("normalizer result aliases input")
	}
	again, err := agentcontext.NormalizeHistory(agentcontext.NormalizeRequest{Messages: result.Messages})
	if err != nil || again.Digest != result.Digest {
		t.Fatalf("normalization is not idempotent: digest=%q error=%v", again.Digest, err)
	}
}

func TestNormalizeHistoryPreservesImagesInOrderAndDeepCopiesBytes(t *testing.T) {
	data := []byte{1, 2, 3}
	input := agent.Message{Role: agent.RoleUser, Parts: []agent.ContentPart{
		{Type: agent.PartText, Text: "before"},
		{Type: agent.PartImage, Image: &agent.ImageContent{MediaType: "image/png", Data: data}},
		{Type: agent.PartText, Text: "after"},
	}}
	result, err := agentcontext.NormalizeHistory(agentcontext.NormalizeRequest{Messages: []agent.Message{input}})
	if err != nil {
		t.Fatalf("NormalizeHistory() error = %v", err)
	}
	data[0] = 9
	if len(result.Messages[0].Parts) != 3 || result.Messages[0].Parts[1].Type != agent.PartImage || result.Messages[0].Parts[1].Image.Data[0] != 1 {
		t.Fatalf("normalized image was dropped, reordered, or aliased: %#v", result.Messages)
	}
}

func TestNormalizeHistoryRejectsInvalidPairingAndStripsExtensions(t *testing.T) {
	orphan := agent.NewToolMessage(agent.ToolResult{ToolCallID: "missing", Name: "lookup", Content: "x"})
	_, err := agentcontext.NormalizeHistory(agentcontext.NormalizeRequest{Messages: []agent.Message{orphan}})
	if !errors.Is(err, agentcontext.ErrToolPairing) {
		t.Fatalf("NormalizeHistory(orphan) error = %v", err)
	}

	message := agent.NewAssistantMessage("safe")
	message.Parts = append(message.Parts, agent.ContentPart{Type: agent.PartType("provider_reasoning"), Text: "secret"})
	result, err := agentcontext.NormalizeHistory(agentcontext.NormalizeRequest{Messages: []agent.Message{message}})
	if err != nil {
		t.Fatalf("NormalizeHistory(extension) error = %v", err)
	}
	if len(result.Messages[0].Parts) != 1 || strings.Contains(result.Messages[0].Text(), "secret") {
		t.Fatalf("provider extension survived normalization: %#v", result.Messages)
	}
}

func TestPlannerDeterministicBudgetImmutableAndExact(t *testing.T) {
	store := agentcontext.NewMemoryStore()
	planner, err := agentcontext.NewPlanner(textCounter{}, store)
	if err != nil {
		t.Fatalf("NewPlanner() error = %v", err)
	}
	request := prepareRequest("tenant/a")
	first, err := planner.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	second, err := planner.Prepare(context.Background(), request)
	if err != nil || first.Ref() != second.Ref() {
		t.Fatalf("Prepare() is not deterministic: refs=%#v %#v error=%v", first.Ref(), second.Ref(), err)
	}
	if first.Estimate().TotalInputTokens != first.Estimate().MessageTokens+7+3 {
		t.Fatalf("budget components missing: %#v", first.Estimate())
	}
	messages := first.Messages()
	messages[0].Parts[0].Text = "tampered"
	loaded, err := store.Get(context.Background(), first.Ref())
	if err != nil || loaded.Messages()[0].Parts[0].Text == "tampered" {
		t.Fatalf("stored plan was mutated: error=%v", err)
	}

	wrongTenant := first.Ref()
	wrongTenant.TenantKey = "tenant/b"
	if _, err := store.Get(context.Background(), wrongTenant); !errors.Is(err, agentcontext.ErrPlanNotFound) {
		t.Fatalf("cross-tenant Get() error = %v", err)
	}
	tampered := first.Ref()
	tampered.Budget.ContextTokens++
	if _, err := store.Get(context.Background(), tampered); !errors.Is(err, agentcontext.ErrPlanDrift) {
		t.Fatalf("tampered exact ref error = %v", err)
	}

	data, err := first.MarshalWire()
	if err != nil {
		t.Fatalf("MarshalWire() error = %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	wire["ref"].(map[string]any)["plan_digest"] = "tampered"
	data, err = json.Marshal(wire)
	if err != nil {
		t.Fatalf("Marshal(tampered) error = %v", err)
	}
	if _, err := agentcontext.UnmarshalPlan(data); !errors.Is(err, agentcontext.ErrPlanDrift) {
		t.Fatalf("UnmarshalPlan(tampered) error = %v", err)
	}

	request.ProtectedFacts = nil
	request.MainlineMessages = []agent.Message{agent.NewAssistantMessage(strings.Repeat("old", 20))}
	request.Budget.ContextTokens = 120
	if _, err := planner.Prepare(context.Background(), request); !errors.Is(err, agentcontext.ErrCompactionRequired) {
		t.Fatalf("over-budget Prepare() error = %v", err)
	}
}

func TestPlanImageWireRoundTripAndAccessorCopies(t *testing.T) {
	store := agentcontext.NewMemoryStore()
	planner, err := agentcontext.NewPlanner(textCounter{}, store)
	if err != nil {
		t.Fatal(err)
	}
	request := prepareRequest("tenant/image")
	data := []byte{1, 2, 3}
	request.InvocationMessages = []agent.Message{{Role: agent.RoleUser, Parts: []agent.ContentPart{{Type: agent.PartImage, Image: &agent.ImageContent{MediaType: "image/png", Data: data}}}}}
	plan, err := planner.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 9
	messages := plan.Messages()
	messages[len(messages)-1].Parts[0].Image.Data[1] = 9
	wire, err := plan.MarshalWire()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := agentcontext.UnmarshalPlan(wire)
	if err != nil {
		t.Fatal(err)
	}
	decodedMessages := decoded.Messages()
	got := decodedMessages[len(decodedMessages)-1].Parts[0].Image.Data
	if !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("plan image aliased or changed: %v", got)
	}
}

func TestMemoryStoreConcurrentCreateOrVerify(t *testing.T) {
	store := agentcontext.NewMemoryStore()
	planner, err := agentcontext.NewPlanner(textCounter{}, store)
	if err != nil {
		t.Fatalf("NewPlanner() error = %v", err)
	}
	request := prepareRequest("tenant/race")
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			plan, err := planner.Prepare(context.Background(), request)
			if err != nil {
				t.Errorf("Prepare() error = %v", err)
				return
			}
			if _, err := store.Get(context.Background(), plan.Ref()); err != nil {
				t.Errorf("Get() error = %v", err)
			}
		}()
	}
	wait.Wait()
}

type fixedSummarizer struct{}

func (fixedSummarizer) Summarize(_ context.Context, request agentcontext.SummaryRequest) (agentcontext.SummaryResult, error) {
	if len(request.ProtectedFacts) != 1 || request.ProtectedFacts[0].Key != "boundary" {
		return agentcontext.SummaryResult{}, errors.New("protected facts missing")
	}
	return agentcontext.SummaryResult{Messages: []agent.Message{agent.NewAssistantMessage("sum")}, UnresolvedWork: []string{"follow up"}, Generation: "summary-gen-1"}, nil
}

func TestCompactionArtifactPivotAndProtectedContent(t *testing.T) {
	store := agentcontext.NewMemoryArtifactStore()
	compactor, err := agentcontext.NewCompactor(agentcontext.CompactionOptions{
		Summarizer: fixedSummarizer{}, Artifacts: store, Counter: textCounter{},
		Clock: func() time.Time { return time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC) }, MinSavings: 10,
	})
	if err != nil {
		t.Fatalf("NewCompactor() error = %v", err)
	}
	request := agentcontext.CompactRequest{
		Source: agentcontext.SourceRef{TenantKey: "tenant/a", SessionKey: "session", SessionRevision: 9},
		Messages: []agent.Message{
			agent.NewUserMessage("old user intent"),
			agent.NewAssistantMessage(strings.Repeat("old", 20)),
			agent.NewUserMessage("latest user intent"),
		},
		ProtectedFacts: []agentcontext.ProtectedFact{{Key: "boundary", Kind: "boundary", Value: "never disclose", SourceKey: "m1", SourceRevision: 2, PolicyVersion: "v1"}},
		RuntimeDigest:  "runtime-1", TargetTokens: 40, CoveredThrough: 8,
	}
	first, err := compactor.Compact(context.Background(), request)
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if first.Pivot.Artifact != first.Artifact.Ref() || first.Pivot.FactSetDigest != first.Artifact.ProtectedFactSet().Digest {
		t.Fatalf("pivot is not bound to exact artifact: %#v", first.Pivot)
	}
	if len(first.Kept) != 1 || first.Kept[0].Text() != "latest user intent" {
		t.Fatalf("latest user intent was compacted: %#v", first.Kept)
	}
	if facts := first.Artifact.ProtectedFactSet().Facts; len(facts) != 1 || facts[0].Value != "never disclose" {
		t.Fatalf("protected fact was lost: %#v", facts)
	}
	loaded, err := store.Get(context.Background(), "tenant/a", first.Artifact.Ref())
	if err != nil || loaded.Ref() != first.Artifact.Ref() {
		t.Fatalf("Get(artifact) = %#v, %v", loaded.Ref(), err)
	}
	second, err := compactor.Compact(context.Background(), request)
	if err != nil || second.Artifact.Ref() != first.Artifact.Ref() {
		t.Fatalf("compaction is not idempotent: %#v %v", second.Artifact.Ref(), err)
	}
	if _, err := store.Get(context.Background(), "tenant/b", first.Artifact.Ref()); !errors.Is(err, agentcontext.ErrArtifactNotFound) {
		t.Fatalf("cross-tenant artifact Get() error = %v", err)
	}
}

func prepareRequest(tenant agent.TenantKey) agentcontext.PrepareRequest {
	return agentcontext.PrepareRequest{
		Source: agentcontext.SourceRef{TenantKey: tenant, SessionKey: "session", SessionRevision: 4, BranchKey: "branch", BranchVersion: 2},
		Runtime: agentcontext.RuntimeArtifacts{
			DefinitionDigest: "runtime-digest", ProjectionVersion: "projection/v1", TokenizerID: "text/v1",
			SystemMessages: []agent.Message{agent.NewSystemMessage("system")},
		},
		ProtectedFacts:     []agentcontext.ProtectedFact{{Key: "identity", Kind: "identity", Value: "Ada", SourceKey: "m1", SourceRevision: 1, PolicyVersion: "v1"}},
		InvocationMessages: []agent.Message{agent.NewUserMessage("help")},
		Budget:             agentcontext.Budget{ContextTokens: 1000, ReservedOutputTokens: 100, SafetyMarginTokens: 10, ToolSchemaTokens: 7, MediaTokens: 3},
	}
}
