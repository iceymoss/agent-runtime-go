package icoder

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/subagent"
)

type reviewModel struct{}

func (reviewModel) Name() string                     { return "fixture" }
func (reviewModel) Capabilities() agent.Capabilities { return agent.Capabilities{Tools: true} }
func (reviewModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	stream := make(chan agent.StreamChunk, 2)
	stream <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: "finding: main.go:10 is incorrect"}
	stream <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{Message: agent.NewAssistantMessage("finding: main.go:10 is incorrect"), FinishReason: agent.FinishStop, Usage: agent.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}}
	close(stream)
	return stream, nil
}

func newDelegationStore(t *testing.T) *Store {
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
	return store
}

func TestDelegationParksTheParentAndResumesWithTheChildResult(t *testing.T) {
	store := newDelegationStore(t)
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entries, service, err := subagentToolEntries(reviewModel{}, "fixture", workspace, store, func() string { return "session" })
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("delegate tools = %d, want reviewer and explorer", len(entries))
	}
	names := map[string]bool{}
	for _, entry := range entries {
		names[entry.tool.Definition().Name] = true
		// A delegate must never be able to change the workspace, so its action
		// stays a spawn rather than a write.
		if entry.metadata.Action != "subagent.spawn" {
			t.Fatalf("delegate action = %q", entry.metadata.Action)
		}
	}
	if !names["delegate_review"] || !names["delegate_explore"] {
		t.Fatalf("delegate tools = %v", names)
	}

	catalog := newTestCatalog(t, newTestGate(t, false), entries)
	ctx := testRunContext(context.Background(), "run-delegate", "session", nil)
	call := agent.ToolInvocation{CallID: "call-explore", Name: "delegate_explore", RawInput: `{"task":"where is the entry point"}`}

	// The parent parks instead of blocking, so a crash while the child works
	// leaves a resumable run rather than losing the delegation.
	_, err = executeCatalogTool(t, ctx, catalog, call)
	suspension, ok := agent.AsToolSuspension(err)
	if !ok {
		t.Fatalf("delegate_explore error = %v, want a tool suspension", err)
	}
	if suspension.Kind != agent.ToolSuspensionExternal || suspension.RequestRef == "" || suspension.ResumeToken == "" {
		t.Fatalf("suspension = %#v", suspension)
	}

	// Nothing has run the child yet, so waking early must park again instead of
	// inventing a result or burning the delegation.
	_, err = executeCatalogTool(t, ctx, catalog, agent.ToolInvocation{
		CallID: call.CallID, Name: call.Name, RawInput: call.RawInput, Resume: &suspension,
	})
	if again, ok := agent.AsToolSuspension(err); !ok || again.RequestRef != suspension.RequestRef {
		t.Fatalf("early wake = %v, want the same suspension", err)
	}

	if _, claimed, err := service.RunNext(context.Background(), tenantKey); err != nil || !claimed {
		t.Fatalf("RunNext() claimed = %v, error = %v", claimed, err)
	}
	if _, err := service.Reconcile(context.Background(), subagent.ReconcileRequest{TenantKey: tenantKey, Limit: 10}); err != nil {
		t.Fatal(err)
	}

	result, err := executeCatalogTool(t, ctx, catalog, agent.ToolInvocation{
		CallID: call.CallID, Name: call.Name, RawInput: call.RawInput, Resume: &suspension,
	})
	if err != nil || result.IsError {
		t.Fatalf("resumed delegate_explore = %#v, error %v", result, err)
	}
	if !strings.Contains(result.Content, "finding: main.go:10 is incorrect") || !strings.Contains(result.Content, "icoder.explorer") {
		t.Fatalf("delegate result = %q", result.Content)
	}

	// The child's answer must be readable after the call, which is what makes it
	// auditable rather than only visible to the model that asked.
	delegations, err := store.ListDelegations(context.Background(), 10)
	if err != nil || len(delegations) != 1 || delegations[0].AgentKey != "icoder.explorer" {
		t.Fatalf("ListDelegations() = %+v, error %v", delegations, err)
	}
	events, err := store.ReplayEvents(context.Background(), "session", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var completed bool
	for _, envelope := range events {
		completed = completed || envelope.Type == "agent.subagent.completed"
	}
	if !completed {
		t.Fatalf("no child completion event was recorded: %+v", events)
	}
}

func TestDelegationRunnerRejectsUnknownAgentKey(t *testing.T) {
	runner := &delegationRunner{agents: map[string]childRuntime{}, store: newDelegationStore(t)}
	if _, err := runner.Run(context.Background(), subagent.RunRequest{AgentKey: "icoder.missing", Input: []byte("x")}); err == nil {
		t.Fatal("Run() accepted an unregistered child agent")
	}
}
