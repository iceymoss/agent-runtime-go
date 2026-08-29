package tool_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
	"github.com/iceymoss/agent-runtime-go/permission"
	lifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

// scriptedStep is one canned model turn: either tool calls or a final answer.
type scriptedStep struct {
	text  string
	calls []agent.ToolCall
}

// scriptedModel replays scriptedStep values in order so a durable run is fully
// deterministic, and records how many times the provider was actually reached.
type scriptedModel struct {
	steps []scriptedStep
	calls int
}

func (*scriptedModel) Name() string { return "scripted" }

func (*scriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true, ToolChoiceNone: true, ToolChoiceRequired: true, ToolChoiceNamed: true, UsageDetails: true}
}

func (m *scriptedModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
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

// delegatingTool models a tool that hands work to something outside this
// runtime: the first call starts the work and parks, and a later call recognizes
// its own handle and reports the answer.
//
// This is the shape of a sub-agent delegation, a webhook, or a queued job. None
// of them are approvals, which is why they need a suspension kind of their own.
type delegatingTool struct {
	started  int
	finished int
	answers  map[string]string
}

func newDelegatingTool() *delegatingTool {
	return &delegatingTool{answers: make(map[string]string)}
}

func (t *delegatingTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "delegate", Description: "delegate work", Parameters: map[string]any{"type": "object"}}
}

func (*delegatingTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }

func (t *delegatingTool) Execute(_ context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	if invocation.Resume == nil {
		t.started++
		handle := "child-" + invocation.CallID
		t.answers[handle] = "the child answered"
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{
			Kind: agent.ToolSuspensionExternal, RequestRef: handle, ResumeToken: "token-1", Revision: 1,
		}}
	}
	answer, ok := t.answers[invocation.Resume.RequestRef]
	if !ok {
		return agent.ToolResult{}, errors.New("resumed with a handle this tool never issued")
	}
	t.finished++
	// The advanced executor validates pairing metadata before its own hooks run,
	// so a tool registered with it fills these in itself.
	return agent.ToolResult{ToolCallID: invocation.CallID, Name: invocation.Name, Content: answer}, nil
}

func newSuspendingExecutor(t *testing.T, implementation agent.Tool) (*lifecycle.Generation, *lifecycle.Executor, *lifecycle.MemoryLedger) {
	t.Helper()
	registry := lifecycle.NewRegistry()
	metadata := lifecycle.Metadata{
		Version: "tool-v1", SchemaVersion: "schema-v1", Action: "delegate",
		EffectClass: lifecycle.EffectExternal, ReplayPolicy: agent.ReplayPolicyNever,
	}
	if err := registry.Register(implementation, metadata); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	generation, err := registry.Freeze()
	if err != nil {
		t.Fatalf("Freeze() error = %v", err)
	}
	ledger := lifecycle.NewMemoryLedger()
	executor, err := lifecycle.NewExecutor(lifecycle.ExecutorOptions{Generation: generation, Ledger: ledger})
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	return generation, executor, ledger
}

func suspensionResolver(runKey string) lifecycle.AgentInvocationResolver {
	return func(_ context.Context, invocation agent.ToolInvocation) (lifecycle.ExecuteRequest, error) {
		return lifecycle.ExecuteRequest{Invocation: lifecycle.InvocationIdentity{
			TenantKey: "tenant-1", RunKey: runKey, AttemptKey: "attempt-1", FenceToken: 1,
			CallID: invocation.CallID, ToolName: invocation.Name, RawInput: invocation.RawInput,
			PrincipalKey: "principal-1", SessionRef: "session-1",
			Resource: permission.Resource{Kind: "delegation", Key: "child"},
		}}, nil
	}
}

func TestExecutorParksAndResumesANonApprovalSuspension(t *testing.T) {
	implementation := newDelegatingTool()
	generation, executor, ledger := newSuspendingExecutor(t, implementation)
	registry, err := lifecycle.NewAgentRegistry(generation, executor, lifecycle.AgentBridgeOptions{
		ResolveInvocation: suspensionResolver("run-1"),
	})
	if err != nil {
		t.Fatalf("NewAgentRegistry() error = %v", err)
	}
	bridged, ok := registry.Get("delegate")
	if !ok {
		t.Fatal("delegate was not bridged into the root registry")
	}
	ctx := context.Background()

	_, err = bridged.Execute(ctx, agent.ToolInvocation{CallID: "call-1", Name: "delegate", RawInput: `{}`})
	suspension, ok := agent.AsToolSuspension(err)
	if !ok {
		t.Fatalf("Execute() error = %v, want a tool suspension", err)
	}
	if suspension.Kind != agent.ToolSuspensionExternal || suspension.RequestRef != "child-call-1" || suspension.ExecutionKey == "" {
		t.Fatalf("suspension = %+v", suspension)
	}
	if implementation.started != 1 || implementation.finished != 0 {
		t.Fatalf("tool started %d finished %d", implementation.started, implementation.finished)
	}
	// The parked execution keeps its handle rather than being recorded as a
	// failure, which is what makes it resumable at all.
	record, err := ledger.Load(ctx, suspension.ExecutionKey)
	if err != nil || record.Status != lifecycle.StatusSuspended || record.Suspension == nil {
		t.Fatalf("ledger record = %+v, error %v", record, err)
	}

	result, err := bridged.Execute(ctx, agent.ToolInvocation{
		CallID: "call-1", Name: "delegate", RawInput: `{}`, Resume: &suspension,
	})
	if err != nil || result.Content != "the child answered" {
		t.Fatalf("resumed Execute() = %+v, error %v", result, err)
	}
	if implementation.started != 1 || implementation.finished != 1 {
		t.Fatalf("resume restarted the delegated work: started %d finished %d", implementation.started, implementation.finished)
	}
	settled, err := ledger.Load(ctx, suspension.ExecutionKey)
	if err != nil || settled.Status != lifecycle.StatusSucceeded || settled.Suspension != nil {
		t.Fatalf("ledger record after resume = %+v, error %v", settled, err)
	}
}

func TestExecutorRefusesAForgedResumeHandle(t *testing.T) {
	implementation := newDelegatingTool()
	generation, executor, _ := newSuspendingExecutor(t, implementation)
	registry, err := lifecycle.NewAgentRegistry(generation, executor, lifecycle.AgentBridgeOptions{
		ResolveInvocation: suspensionResolver("run-1"),
	})
	if err != nil {
		t.Fatalf("NewAgentRegistry() error = %v", err)
	}
	bridged, _ := registry.Get("delegate")
	ctx := context.Background()
	_, err = bridged.Execute(ctx, agent.ToolInvocation{CallID: "call-1", Name: "delegate", RawInput: `{}`})
	suspension, ok := agent.AsToolSuspension(err)
	if !ok {
		t.Fatalf("Execute() error = %v, want a tool suspension", err)
	}

	// A resume is bound to the exact handle the executor issued. Accepting a
	// different one would let anything that knows an execution key restart work.
	forged := suspension
	forged.ResumeToken = "token-forged"
	if _, err := bridged.Execute(ctx, agent.ToolInvocation{
		CallID: "call-1", Name: "delegate", RawInput: `{}`, Resume: &forged,
	}); !errors.Is(err, lifecycle.ErrExecutionConflict) {
		t.Fatalf("resume with a forged handle error = %v, want lifecycle.ErrExecutionConflict", err)
	}
	if implementation.finished != 0 {
		t.Fatal("a forged handle reached the tool")
	}
}

// TestDurableRunSuspendsAndResumesADelegatedTool proves the whole path: the root
// runtime checkpoints a non-approval suspension, a later attempt hands the exact
// handle back, and the delegated work is finished rather than restarted.
func TestDurableRunSuspendsAndResumesADelegatedTool(t *testing.T) {
	implementation := newDelegatingTool()
	generation, executor, _ := newSuspendingExecutor(t, implementation)
	registry, err := lifecycle.NewAgentRegistry(generation, executor, lifecycle.AgentBridgeOptions{
		ResolveInvocation: suspensionResolver("run-delegated"),
	})
	if err != nil {
		t.Fatalf("NewAgentRegistry() error = %v", err)
	}
	model := &scriptedModel{steps: []scriptedStep{
		{calls: []agent.ToolCall{{ID: "call-1", Name: "delegate", Input: "{}"}}},
		{text: "reported"},
	}}
	runner, err := agent.New(agent.Config{Key: "suspension-test", ModelName: "scripted", MaxSteps: 8}, model, registry)
	if err != nil {
		t.Fatalf("agent.New() error = %v", err)
	}
	store := durable.NewMemoryStore()
	adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{Store: store, Ledger: store, AttemptKey: "attempt-1"})
	if err != nil {
		t.Fatalf("NewCheckpointAdapter() error = %v", err)
	}
	request := agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("delegate this")},
		DurableRun: &agent.DurableRunConfig{
			Identity:        agent.RunIdentity{RunKey: "run-delegated", AgentKey: "suspension-test", SessionID: "session-1", RequestID: "request-1"},
			CheckpointStore: adapter, LeaseOwner: "worker-1", LeaseDuration: time.Minute,
		},
	}
	ctx := context.Background()

	parked, err := runner.Run(ctx, request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if parked.Outcome != agent.OutcomeSuspended || parked.StopReason != agent.StopReasonToolSuspended {
		t.Fatalf("first Run() = %+v", parked)
	}
	if parked.Suspension == nil || parked.Suspension.Tool == nil || parked.Suspension.Tool.Kind != agent.ToolSuspensionExternal {
		t.Fatalf("first Run() suspension = %+v", parked.Suspension)
	}
	// The suspension has to survive in the checkpoint, or nothing could resume it.
	snapshot, err := store.Load(ctx, "run-delegated")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	stored := snapshot.Checkpoint.Outcome.Suspension
	if stored == nil || stored.Tool == nil || *stored.Tool != *parked.Suspension.Tool {
		t.Fatalf("checkpoint lost the suspension: %+v", stored)
	}

	resumeRequest := request
	resumeRequest.DurableRun.ToolResume = parked.Suspension.Tool
	resumed, err := runner.Run(ctx, resumeRequest)
	if err != nil {
		t.Fatalf("resumed Run() error = %v", err)
	}
	if resumed.Outcome != agent.OutcomeCompleted || resumed.Text != "reported" {
		t.Fatalf("resumed Run() = %+v", resumed)
	}
	if implementation.started != 1 || implementation.finished != 1 {
		t.Fatalf("the delegated work was restarted: started %d finished %d", implementation.started, implementation.finished)
	}
	if model.calls != 2 {
		t.Fatalf("model calls = %d, want the first step replayed from the checkpoint and one more", model.calls)
	}
}
