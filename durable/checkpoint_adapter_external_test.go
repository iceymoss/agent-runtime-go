package durable_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/durable"
)

// scriptedStep is one canned model turn: either tool calls or a final answer.
type scriptedStep struct {
	text  string
	calls []agent.ToolCall
}

// scriptedModel replays scriptedStep values in order so a durable run is fully
// deterministic. It records how many times the provider was actually reached,
// which is how the recovery tests prove work was not repeated.
type scriptedModel struct {
	steps []scriptedStep
	calls int
}

func (*scriptedModel) Name() string { return "scripted" }

func (*scriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true, ToolChoiceNone: true, ToolChoiceRequired: true, ToolChoiceNamed: true, UsageDetails: true}
}

func (m *scriptedModel) Stream(ctx context.Context, _ *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	if m.calls >= len(m.steps) {
		return nil, errors.New("scripted model exhausted")
	}
	step := m.steps[m.calls]
	m.calls++
	chunks := make(chan agent.StreamChunk, len(step.calls)+2)
	message := agent.Message{Role: agent.RoleAssistant}
	finish := agent.FinishStop
	if len(step.calls) > 0 {
		finish = agent.FinishToolCalls
		for i := range step.calls {
			call := step.calls[i]
			message.Parts = append(message.Parts, agent.ContentPart{Type: agent.PartToolCall, ToolCall: &call})
			chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
		}
	} else {
		message.Parts = append(message.Parts, agent.ContentPart{Type: agent.PartText, Text: step.text})
		chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: step.text}
	}
	message.FinishReason = finish
	chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{
		Message: message, FinishReason: finish, ModelName: "scripted",
		Usage: agent.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5},
	}}
	close(chunks)
	return chunks, nil
}

// countingTool records each execution so replay behavior is directly testable.
type countingTool struct {
	name       string
	replay     agent.ReplayPolicy
	executions int
	fail       error
}

func (t *countingTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: t.name, Description: "test tool", Parameters: map[string]any{"type": "object"}}
}

func (t *countingTool) ReplayPolicy() agent.ReplayPolicy { return t.replay }

func (t *countingTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	t.executions++
	if t.fail != nil {
		return agent.ToolResult{}, t.fail
	}
	return agent.ToolResult{Content: "ok"}, nil
}

func newAdapter(t *testing.T, store *durable.MemoryStore) *durable.CheckpointAdapter {
	t.Helper()
	adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{Store: store, Ledger: store, AttemptKey: "attempt-1"})
	if err != nil {
		t.Fatalf("NewCheckpointAdapter() error = %v", err)
	}
	return adapter
}

func newRunner(t *testing.T, model agent.Model, tools ...agent.Tool) *agent.Agent {
	t.Helper()
	registry := agent.NewRegistry()
	for _, tool := range tools {
		if err := registry.Register(tool); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}
	runner, err := agent.New(agent.Config{Key: "adapter-test", ModelName: "scripted", MaxSteps: 8}, model, registry)
	if err != nil {
		t.Fatalf("agent.New() error = %v", err)
	}
	return runner
}

func durableRequest(store agent.CheckpointStore, runKey string) agent.RunRequest {
	return agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("input")},
		DurableRun: &agent.DurableRunConfig{
			Identity:        agent.RunIdentity{RunKey: runKey, AgentKey: "adapter-test", SessionID: "session", RequestID: "request-" + runKey},
			CheckpointStore: store, LeaseOwner: "worker-1", LeaseDuration: time.Minute,
		},
	}
}

func TestNewCheckpointAdapterRejectsIncompletePorts(t *testing.T) {
	tests := []struct {
		name    string
		options durable.CheckpointAdapterOptions
	}{
		{name: "missing store and ledger", options: durable.CheckpointAdapterOptions{}},
		{name: "missing ledger", options: durable.CheckpointAdapterOptions{Store: durable.NewMemoryStore()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := durable.NewCheckpointAdapter(test.options); err == nil {
				t.Fatal("NewCheckpointAdapter() accepted an incomplete composition")
			}
		})
	}
}

func TestCheckpointAdapterRunsToolLoopAndRecordsEffects(t *testing.T) {
	store := durable.NewMemoryStore()
	tool := &countingTool{name: "probe", replay: agent.ReplayPolicyNever}
	model := &scriptedModel{steps: []scriptedStep{
		{calls: []agent.ToolCall{{ID: "call-1", Name: "probe", Input: "{}"}}},
		{text: "done"},
	}}
	runner := newRunner(t, model, tool)
	request := durableRequest(newAdapter(t, store), "run-tool-loop")

	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome != agent.OutcomeCompleted || result.StopReason != agent.StopReasonComplete || result.Text != "done" {
		t.Fatalf("result = %+v", result)
	}
	if tool.executions != 1 {
		t.Fatalf("tool executions = %d, want 1", tool.executions)
	}
	if result.DurableCompletion == nil {
		t.Fatal("Run() did not return a completion guard for the domain owner")
	}
	snapshot, err := store.Load(context.Background(), "run-tool-loop")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snapshot.Status != durable.StatusRunning || snapshot.Phase != durable.PhaseFinalizing {
		t.Fatalf("snapshot before finalization = %s/%s", snapshot.Status, snapshot.Phase)
	}
	effects, err := store.ListEffects(context.Background(), "run-tool-loop")
	if err != nil {
		t.Fatalf("ListEffects() error = %v", err)
	}
	if len(effects) != 1 || effects[0].Status != durable.EffectSucceeded || effects[0].Result == nil || effects[0].Result.Content != "ok" {
		t.Fatalf("effects = %+v", effects)
	}
	if _, err := store.Save(context.Background(), durable.SaveRequest{
		Guard:  durable.Guard{RunKey: snapshot.IdentityKey(), LeaseOwner: snapshot.LeaseOwner, Revision: snapshot.Revision, FenceToken: snapshot.FenceToken},
		Status: durable.StatusCompleted, Phase: durable.PhaseTerminal, Checkpoint: result.DurableCompletion.Checkpoint,
	}); err != nil {
		t.Fatalf("finalize error = %v", err)
	}
}

func TestCheckpointAdapterReplaysCompletedRunWithoutReachingProvider(t *testing.T) {
	store := durable.NewMemoryStore()
	tool := &countingTool{name: "probe", replay: agent.ReplayPolicyNever}
	model := &scriptedModel{steps: []scriptedStep{
		{calls: []agent.ToolCall{{ID: "call-1", Name: "probe", Input: "{}"}}},
		{text: "done"},
	}}
	runner := newRunner(t, model, tool)
	request := durableRequest(newAdapter(t, store), "run-replay")

	first, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if _, err := store.Save(context.Background(), durable.SaveRequest{
		Guard:  durable.Guard{RunKey: "run-replay", LeaseOwner: "worker-1", Revision: revisionOf(t, store, "run-replay"), FenceToken: fenceOf(t, store, "run-replay")},
		Status: durable.StatusCompleted, Phase: durable.PhaseTerminal, Checkpoint: first.DurableCompletion.Checkpoint,
	}); err != nil {
		t.Fatalf("finalize error = %v", err)
	}

	modelCallsBefore, toolCallsBefore := model.calls, tool.executions
	second, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if second.Text != "done" || second.Outcome != agent.OutcomeCompleted {
		t.Fatalf("second result = %+v", second)
	}
	if model.calls != modelCallsBefore || tool.executions != toolCallsBefore {
		t.Fatalf("completed run repeated work: model=%d tool=%d", model.calls, tool.executions)
	}
}

func TestCheckpointAdapterRejectsStaleGuard(t *testing.T) {
	store := durable.NewMemoryStore()
	adapter := newAdapter(t, store)
	identity := agent.RunIdentity{RunKey: "run-stale", AgentKey: "adapter-test", SessionID: "session", RequestID: "request"}
	if _, err := adapter.Begin(context.Background(), identity, "sha256:input", "sha256:config", agent.Checkpoint{}); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	snapshot, err := adapter.Acquire(context.Background(), "run-stale", "worker-1", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	stale := snapshot.Guard()
	if _, err := adapter.ModelInflight(context.Background(), stale, agent.Checkpoint{}); err != nil {
		t.Fatalf("ModelInflight() error = %v", err)
	}
	_, err = adapter.ModelInflight(context.Background(), stale, agent.Checkpoint{})
	if !errors.Is(err, agent.ErrCheckpointConflict) || !errors.Is(err, durable.ErrLeaseLost) {
		t.Fatalf("stale guard error = %v, want both agent.ErrCheckpointConflict and durable.ErrLeaseLost", err)
	}
}

func TestCheckpointAdapterBeginToolRefusesUnknownEffectWithoutSafeReplay(t *testing.T) {
	tests := []struct {
		name       string
		ledger     durable.ExecutionLedger
		safeReplay bool
		wantErr    bool
	}{
		{name: "unsafe replay is refused", safeReplay: false, wantErr: true},
		{name: "safe replay is re-armed", safeReplay: true, wantErr: false},
		{name: "ledger without replayer refuses safe replay", ledger: nil, safeReplay: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := durable.NewMemoryStore()
			var ledger durable.ExecutionLedger = store
			if test.name == "ledger without replayer refuses safe replay" {
				ledger = plainLedger{store}
			}
			adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{Store: store, Ledger: ledger, AttemptKey: "attempt-1"})
			if err != nil {
				t.Fatalf("NewCheckpointAdapter() error = %v", err)
			}
			guard, execution := prepareUnknownEffect(t, store, adapter)
			_, _, err = adapter.BeginTool(context.Background(), guard, execution.IdempotencyKey, test.safeReplay)
			if test.wantErr {
				if !errors.Is(err, agent.ErrToolExecutionUnknown) {
					t.Fatalf("BeginTool() error = %v, want agent.ErrToolExecutionUnknown", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("BeginTool() error = %v", err)
			}
		})
	}
}

// plainLedger hides MemoryStore's EffectReplayer so the conservative default is
// observable: a ledger that cannot prove replay safety never re-arms an effect.
type plainLedger struct{ inner durable.ExecutionLedger }

func (l plainLedger) PrepareEffect(ctx context.Context, request durable.PrepareEffectRequest) (durable.EffectRecord, bool, error) {
	return l.inner.PrepareEffect(ctx, request)
}

func (l plainLedger) BeginEffect(ctx context.Context, guard durable.Guard, key durable.ExecutionKey, at time.Time) (durable.EffectRecord, error) {
	return l.inner.BeginEffect(ctx, guard, key, at)
}

func (l plainLedger) CompleteEffect(ctx context.Context, request durable.CompleteEffectRequest) (durable.EffectRecord, error) {
	return l.inner.CompleteEffect(ctx, request)
}

func (l plainLedger) MarkEffectUnknown(ctx context.Context, guard durable.Guard, key durable.ExecutionKey, at time.Time) (durable.EffectRecord, error) {
	return l.inner.MarkEffectUnknown(ctx, guard, key, at)
}

func (l plainLedger) LoadEffect(ctx context.Context, key durable.ExecutionKey) (durable.EffectRecord, error) {
	return l.inner.LoadEffect(ctx, key)
}

func (l plainLedger) ListEffects(ctx context.Context, key durable.RunKey) ([]durable.EffectRecord, error) {
	return l.inner.ListEffects(ctx, key)
}

// prepareUnknownEffect drives one effect to the ambiguous state a crashed tool
// leaves behind: prepared, started, and then abandoned by a revoked lease.
func prepareUnknownEffect(t *testing.T, store *durable.MemoryStore, adapter *durable.CheckpointAdapter) (agent.MutationGuard, agent.ToolExecution) {
	t.Helper()
	ctx := context.Background()
	identity := agent.RunIdentity{RunKey: "run-unknown", AgentKey: "adapter-test", SessionID: "session", RequestID: "request"}
	if _, err := adapter.Begin(ctx, identity, "sha256:input", "sha256:config", agent.Checkpoint{}); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	snapshot, err := adapter.Acquire(ctx, "run-unknown", "worker-1", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	call := agent.ToolCall{ID: "call-1", Name: "probe", Input: "{}"}
	execution, err := agent.NewToolExecution(identity, 0, 0, call)
	if err != nil {
		t.Fatalf("NewToolExecution() error = %v", err)
	}
	checkpoint := agent.Checkpoint{PendingToolCalls: []agent.ToolCall{call}}
	// The tools phase is only reachable through a committed model response, the
	// same edge the runtime takes.
	snapshot, err = adapter.ModelInflight(ctx, snapshot.Guard(), agent.Checkpoint{})
	if err != nil {
		t.Fatalf("ModelInflight() error = %v", err)
	}
	snapshot, err = adapter.CommitModelResponse(ctx, snapshot.Guard(), agent.Response{}, checkpoint)
	if err != nil {
		t.Fatalf("CommitModelResponse() error = %v", err)
	}
	snapshot, err = adapter.PrepareTools(ctx, snapshot.Guard(), []agent.ToolExecution{execution}, checkpoint)
	if err != nil {
		t.Fatalf("PrepareTools() error = %v", err)
	}
	if _, _, err := adapter.BeginTool(ctx, snapshot.Guard(), execution.IdempotencyKey, false); err != nil {
		t.Fatalf("BeginTool() error = %v", err)
	}
	// A revoked lease is exactly what a crashed worker leaves behind: running
	// effects become unknown and the run returns to a safe suspended phase.
	if _, err := store.RevokeLease(ctx, durable.RevokeLeaseRequest{
		RunKey: "run-unknown", ExpectedRevision: snapshot.Revision, ExpectedFence: snapshot.FenceToken, Now: time.Now(),
	}); err != nil {
		t.Fatalf("RevokeLease() error = %v", err)
	}
	resumed, err := adapter.Acquire(ctx, "run-unknown", "worker-2", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("re-Acquire() error = %v", err)
	}
	return resumed.Guard(), execution
}

func revisionOf(t *testing.T, store *durable.MemoryStore, key durable.RunKey) uint64 {
	t.Helper()
	snapshot, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return snapshot.Revision
}

func fenceOf(t *testing.T, store *durable.MemoryStore, key durable.RunKey) uint64 {
	t.Helper()
	snapshot, err := store.Load(context.Background(), key)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	return snapshot.FenceToken
}

// TestCheckpointAdapterConformance holds the adapter to the runtime's shared
// definition of a correct CheckpointStore, rather than to this package's own
// idea of one.
func TestCheckpointAdapterConformance(t *testing.T) {
	agenttest.TestCheckpointStore(t, func(t *testing.T) agent.CheckpointStore {
		store := durable.NewMemoryStore()
		adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
			Store: store, Ledger: store, AttemptKey: "conformance-attempt",
		})
		if err != nil {
			t.Fatalf("NewCheckpointAdapter() error = %v", err)
		}
		return adapter
	})
}

// TestMemoryStoreConformance holds the package's own reference implementation to
// the shared suite, so the suite and the reference cannot drift apart.
func TestMemoryStoreConformance(t *testing.T) {
	agenttest.TestDurableStore(t, func(*testing.T) agenttest.DurableStore { return durable.NewMemoryStore() })
}
