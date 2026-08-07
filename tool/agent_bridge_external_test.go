package tool_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	lifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

type bridgeTool struct {
	name       string
	replay     agent.ReplayPolicy
	result     agent.ToolResult
	err        error
	calls      atomic.Int32
	invocation agent.ToolInvocation
}

func (t *bridgeTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        t.name,
		Description: "bridge test tool",
		Strict:      true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"count": map[string]any{"type": "number"},
				"flag":  map[string]any{"type": "boolean"},
			},
			"required": []any{"count", "flag"},
		},
	}
}

func (t *bridgeTool) ReplayPolicy() agent.ReplayPolicy { return t.replay }

func (t *bridgeTool) Execute(_ context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	t.calls.Add(1)
	t.invocation = invocation
	return t.result, t.err
}

type bridgeInterceptor struct {
	version string
	before  func(lifecycle.InvocationIdentity) lifecycle.PreflightResult
}

func (bridgeInterceptor) Name() string { return "rewrite" }
func (i bridgeInterceptor) Version() string {
	return i.version
}
func (i bridgeInterceptor) Before(_ context.Context, invocation lifecycle.InvocationIdentity) (lifecycle.PreflightResult, error) {
	if i.before != nil {
		return i.before(invocation), nil
	}
	return lifecycle.PreflightResult{Decision: lifecycle.PreflightContinue}, nil
}
func (bridgeInterceptor) After(_ context.Context, _ lifecycle.PreparedExecution, result agent.ToolResult) (agent.ToolResult, error) {
	return result, nil
}
func (bridgeInterceptor) OnError(context.Context, lifecycle.PreparedExecution, error) error {
	return nil
}

type bridgeModel struct {
	step int
}

func (*bridgeModel) Name() string { return "bridge-model" }
func (*bridgeModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}
func (m *bridgeModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	m.step++
	chunks := make(chan agent.StreamChunk, 2)
	if m.step == 1 {
		call := agent.ToolCall{ID: "call", Name: "write", Input: `{"count":"invalid","flag":false}`}
		message := agent.Message{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}, FinishReason: agent.FinishToolCalls}
		chunks <- agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call}
		chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{Message: message, FinishReason: agent.FinishToolCalls}}
	} else {
		message := agent.NewAssistantMessage("repaired")
		chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: "repaired"}
		chunks <- agent.StreamChunk{Type: agent.ChunkFinish, Response: &agent.Response{Message: message, FinishReason: agent.FinishStop}}
	}
	close(chunks)
	return chunks, nil
}

func TestNewAgentRegistryRejectsInvalidConfiguration(t *testing.T) {
	implementation := newBridgeTool()
	generation, executor := newBridgeExecutor(t, implementation, nil, nil)
	resolver := bridgeResolver(nil)

	tests := []struct {
		name       string
		generation *lifecycle.Generation
		executor   *lifecycle.Executor
		resolver   lifecycle.AgentInvocationResolver
	}{
		{name: "nil generation", executor: executor, resolver: resolver},
		{name: "nil executor", generation: generation, resolver: resolver},
		{name: "nil resolver", generation: generation, executor: executor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry, err := lifecycle.NewAgentRegistry(tt.generation, tt.executor, lifecycle.AgentBridgeOptions{ResolveInvocation: tt.resolver})
			if registry != nil || !errors.Is(err, lifecycle.ErrInvalidConfiguration) {
				t.Fatalf("NewAgentRegistry() = %#v, %v, want nil ErrInvalidConfiguration", registry, err)
			}
		})
	}
}

func TestAgentRegistryRegistersGenerationDefinitionsAndMetadata(t *testing.T) {
	implementation := newBridgeTool()
	implementation.replay = agent.ReplayPolicyResolve
	generation, executor := newBridgeExecutor(t, implementation, nil, nil)
	registry := newBridgeRegistry(t, generation, executor, bridgeResolver(nil))

	if got, want := registry.Names(), []string{"write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	bridged, ok := registry.Get("write")
	if !ok {
		t.Fatal("generation definition was not registered in root registry")
	}
	if !reflect.DeepEqual(bridged.Definition(), generation.Definitions()[0]) {
		t.Fatalf("Definition() = %#v, want %#v", bridged.Definition(), generation.Definitions()[0])
	}
	if bridged.ReplayPolicy() != agent.ReplayPolicyResolve {
		t.Fatalf("ReplayPolicy() = %q, want %q", bridged.ReplayPolicy(), agent.ReplayPolicyResolve)
	}
	versioned, ok := bridged.(agent.ExecutableVersioner)
	if !ok || versioned.ExecutableVersion() != generation.Digest()+":tool-v1" {
		t.Fatalf("ExecutableVersion() = %q, want generation digest and tool version", versioned.ExecutableVersion())
	}
	toolSet, err := agent.NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := toolSet.ValidateExecutableVersions(); err != nil {
		t.Fatalf("ValidateExecutableVersions() error = %v", err)
	}
}

func TestAgentRegistryRootSchemaValidationPrecedesResolverAndExecutor(t *testing.T) {
	implementation := newBridgeTool()
	generation, executor := newBridgeExecutor(t, implementation, nil, nil)
	var resolverCalls atomic.Int32
	registry := newBridgeRegistry(t, generation, executor, bridgeResolver(&resolverCalls))
	runner, err := agent.New(agent.Config{Key: "bridge", MaxSteps: 4}, &bridgeModel{}, registry)
	if err != nil {
		t.Fatal(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{Messages: []agent.Message{agent.NewUserMessage("run")}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if resolverCalls.Load() != 0 || implementation.calls.Load() != 0 {
		t.Fatalf("invalid root input reached resolver/tool: resolver=%d tool=%d", resolverCalls.Load(), implementation.calls.Load())
	}
	if len(result.Steps) == 0 || len(result.Steps[0].ToolResults) != 1 || !result.Steps[0].ToolResults[0].IsError {
		t.Fatalf("invalid input result = %#v", result.Steps)
	}
}

func TestAgentRegistryResolverMapsFieldsWithoutChangingInvocationIdentity(t *testing.T) {
	implementation := newBridgeTool()
	generation, executor := newBridgeExecutor(t, implementation, nil, nil)
	var resolved agent.ToolInvocation
	resolver := func(_ context.Context, invocation agent.ToolInvocation) (lifecycle.ExecuteRequest, error) {
		resolved = invocation
		request := bridgeRequest()
		request.Invocation.CallID = ""
		request.Invocation.ToolName = ""
		request.Invocation.RawInput = ""
		return request, nil
	}
	bridged := getBridgeTool(t, newBridgeRegistry(t, generation, executor, resolver))
	invocation := agent.ToolInvocation{CallID: "call", Name: "write", RawInput: `{"count":1,"flag":false}`, ExecutionKey: "root-key"}
	result, err := bridged.Execute(context.Background(), invocation)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if resolved != invocation {
		t.Fatalf("resolver invocation = %#v, want %#v", resolved, invocation)
	}
	if result.ToolCallID != invocation.CallID || result.Name != invocation.Name || implementation.invocation.CallID != invocation.CallID || implementation.invocation.Name != invocation.Name {
		t.Fatalf("identity mapping result=%#v underlying=%#v", result, implementation.invocation)
	}

	mutations := []struct {
		name   string
		mutate func(*lifecycle.InvocationIdentity)
	}{
		{name: "call ID", mutate: func(identity *lifecycle.InvocationIdentity) { identity.CallID = "changed" }},
		{name: "tool name", mutate: func(identity *lifecycle.InvocationIdentity) { identity.ToolName = "changed" }},
		{name: "raw input", mutate: func(identity *lifecycle.InvocationIdentity) { identity.RawInput = `{}` }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			candidate := newBridgeTool()
			candidateGeneration, candidateExecutor := newBridgeExecutor(t, candidate, nil, nil)
			candidateResolver := func(_ context.Context, invocation agent.ToolInvocation) (lifecycle.ExecuteRequest, error) {
				request := bridgeRequest()
				request.Invocation.CallID = invocation.CallID
				request.Invocation.ToolName = invocation.Name
				request.Invocation.RawInput = invocation.RawInput
				tt.mutate(&request.Invocation)
				return request, nil
			}
			candidateBridge := getBridgeTool(t, newBridgeRegistry(t, candidateGeneration, candidateExecutor, candidateResolver))
			_, err := candidateBridge.Execute(context.Background(), invocation)
			if !errors.Is(err, lifecycle.ErrInvalidConfiguration) || candidate.calls.Load() != 0 {
				t.Fatalf("Execute() error=%v calls=%d, want invalid configuration before tool", err, candidate.calls.Load())
			}
		})
	}
}

func TestAgentRegistryInterceptorRewriteCanonicalInputReachesTool(t *testing.T) {
	implementation := newBridgeTool()
	interceptor := bridgeInterceptor{version: "rewrite-v1", before: func(invocation lifecycle.InvocationIdentity) lifecycle.PreflightResult {
		if invocation.RawInput == `{"count":1,"flag":false}` {
			return lifecycle.PreflightResult{Decision: lifecycle.PreflightRewrite, Input: `{ "flag": true, "count": 2 }`}
		}
		return lifecycle.PreflightResult{Decision: lifecycle.PreflightContinue}
	}}
	generation, executor := newBridgeExecutor(t, implementation, nil, []lifecycle.Interceptor{interceptor})
	bridged := getBridgeTool(t, newBridgeRegistry(t, generation, executor, bridgeResolver(nil)))

	_, err := bridged.Execute(context.Background(), agent.ToolInvocation{CallID: "call", Name: "write", RawInput: `{"count":1,"flag":false}`})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if implementation.invocation.RawInput != `{"count":2,"flag":true}` {
		t.Fatalf("underlying RawInput = %q, want rewritten canonical input", implementation.invocation.RawInput)
	}
	if implementation.invocation.ExecutionKey == "" {
		t.Fatal("underlying invocation omitted lifecycle execution key")
	}
}

func TestAgentRegistryPreservesToolResultAndFatalCause(t *testing.T) {
	t.Run("IsError result", func(t *testing.T) {
		implementation := newBridgeTool()
		implementation.result.IsError = true
		generation, executor := newBridgeExecutor(t, implementation, nil, nil)
		result, err := getBridgeTool(t, newBridgeRegistry(t, generation, executor, bridgeResolver(nil))).Execute(context.Background(), bridgeInvocation())
		if err != nil || !result.IsError {
			t.Fatalf("Execute() = %#v, %v, want original IsError result", result, err)
		}
	})

	t.Run("fatal cause", func(t *testing.T) {
		cause := errors.New("bridge tool failure")
		implementation := newBridgeTool()
		implementation.err = cause
		generation, executor := newBridgeExecutor(t, implementation, nil, nil)
		_, err := getBridgeTool(t, newBridgeRegistry(t, generation, executor, bridgeResolver(nil))).Execute(context.Background(), bridgeInvocation())
		if !errors.Is(err, cause) || !errors.Is(err, lifecycle.ErrToolFatal) {
			t.Fatalf("Execute() error = %v, want fatal error retaining cause", err)
		}
	})
}

func TestAgentRegistryProjectsApprovalSuspension(t *testing.T) {
	implementation := newBridgeTool()
	authorizer := &testPermission{decision: permission.DecisionAsk}
	generation, executor := newBridgeExecutor(t, implementation, authorizer, nil)
	_, err := getBridgeTool(t, newBridgeRegistry(t, generation, executor, bridgeResolver(nil))).Execute(context.Background(), bridgeInvocation())
	suspension, ok := agent.AsToolSuspension(err)
	if !ok || !errors.Is(err, lifecycle.ErrApprovalPending) || suspension.Kind != agent.ToolSuspensionApproval || suspension.RequestRef == "" || suspension.ResumeToken == "" || suspension.Revision != 1 {
		t.Fatalf("Execute() error = %v, suspension = %#v, want approval suspension", err, suspension)
	}
	if implementation.calls.Load() != 0 {
		t.Fatalf("approval-blocked tool calls = %d, want 0", implementation.calls.Load())
	}
}

func TestAgentRegistryResumesProjectedApproval(t *testing.T) {
	implementation := newBridgeTool()
	authorizer := &testPermission{decision: permission.DecisionAsk, revalidate: permission.DecisionAllow}
	generation, executor := newBridgeExecutor(t, implementation, authorizer, nil)
	bridged := getBridgeTool(t, newBridgeRegistry(t, generation, executor, bridgeResolver(nil)))
	invocation := bridgeInvocation()

	_, err := bridged.Execute(context.Background(), invocation)
	suspension, ok := agent.AsToolSuspension(err)
	if !ok {
		t.Fatalf("Execute() error = %v, want approval suspension", err)
	}
	invocation.Resume = &suspension
	result, err := bridged.Execute(context.Background(), invocation)
	if err != nil || result.ToolCallID != invocation.CallID || implementation.calls.Load() != 1 || authorizer.revalidations.Load() != 1 {
		t.Fatalf("resumed Execute() = %#v, %v, calls=%d revalidations=%d", result, err, implementation.calls.Load(), authorizer.revalidations.Load())
	}
	repeated, err := bridged.Execute(context.Background(), invocation)
	if err != nil || repeated != result || implementation.calls.Load() != 1 {
		t.Fatalf("repeated Execute() = %#v, %v, calls=%d", repeated, err, implementation.calls.Load())
	}
}

func TestAgentRegistryRejectsNilExecutorResult(t *testing.T) {
	implementation := newBridgeTool()
	registry := lifecycle.NewRegistry()
	if err := registry.Register(implementation, bridgeMetadata("tool-v1")); err != nil {
		t.Fatal(err)
	}
	generation, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	executor, err := lifecycle.NewExecutor(lifecycle.ExecutorOptions{Generation: generation, Ledger: nilResultLedger{}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = getBridgeTool(t, newBridgeRegistry(t, generation, executor, bridgeResolver(nil))).Execute(context.Background(), bridgeInvocation())
	if !errors.Is(err, lifecycle.ErrResultInvariant) {
		t.Fatalf("Execute() error = %v, want ErrResultInvariant", err)
	}
	if implementation.calls.Load() != 0 {
		t.Fatalf("tool calls = %d, want 0 for preexisting nil result", implementation.calls.Load())
	}
}

func TestAgentRegistryVersionsAffectRootToolSetDigest(t *testing.T) {
	tests := []struct {
		name         string
		firstTool    string
		secondTool   string
		firstFilter  string
		secondFilter string
	}{
		{name: "tool generation version", firstTool: "tool-v1", secondTool: "tool-v2"},
		{name: "interceptor version", firstTool: "tool-v1", secondTool: "tool-v1", firstFilter: "filter-v1", secondFilter: "filter-v2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := bridgeToolSetVersion(t, tt.firstTool, tt.firstFilter)
			second := bridgeToolSetVersion(t, tt.secondTool, tt.secondFilter)
			if first == second {
				t.Fatalf("ToolSet.Version() did not change: %q", first)
			}
		})
	}
}

type nilResultLedger struct{}

func (nilResultLedger) Prepare(_ context.Context, prepared lifecycle.PreparedExecution) (lifecycle.ExecutionRecord, bool, error) {
	return lifecycle.ExecutionRecord{Prepared: prepared, Status: lifecycle.StatusSucceeded}, false, nil
}
func (nilResultLedger) Reject(context.Context, string, uint64, lifecycle.Failure) (lifecycle.ExecutionRecord, error) {
	panic("unexpected Reject")
}
func (nilResultLedger) Begin(context.Context, string, uint64) (lifecycle.ExecutionRecord, error) {
	panic("unexpected Begin")
}
func (nilResultLedger) Complete(context.Context, lifecycle.CompleteExecution) (lifecycle.ExecutionRecord, error) {
	panic("unexpected Complete")
}
func (nilResultLedger) MarkUnknown(context.Context, string, uint64, lifecycle.Failure) (lifecycle.ExecutionRecord, error) {
	panic("unexpected MarkUnknown")
}
func (nilResultLedger) Load(context.Context, string) (lifecycle.ExecutionRecord, error) {
	panic("unexpected Load")
}

func newBridgeTool() *bridgeTool {
	return &bridgeTool{
		name:   "write",
		replay: agent.ReplayPolicyNever,
		result: agent.ToolResult{ToolCallID: "call", Name: "write", Content: "result"},
	}
}

func bridgeMetadata(version string) lifecycle.Metadata {
	return lifecycle.Metadata{
		Version:       version,
		SchemaVersion: "schema-v1",
		Action:        "write",
		EffectClass:   lifecycle.EffectWrite,
		ReplayPolicy:  agent.ReplayPolicyNever,
	}
}

func newBridgeExecutor(t *testing.T, implementation *bridgeTool, authorizer permission.Service, interceptors []lifecycle.Interceptor) (*lifecycle.Generation, *lifecycle.Executor) {
	t.Helper()
	registry := lifecycle.NewRegistry()
	metadata := bridgeMetadata("tool-v1")
	metadata.ReplayPolicy = implementation.replay
	if err := registry.Register(implementation, metadata); err != nil {
		t.Fatal(err)
	}
	generation, err := registry.Freeze(interceptors...)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := lifecycle.NewExecutor(lifecycle.ExecutorOptions{Generation: generation, Ledger: newMemoryLedger(), Permission: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	return generation, executor
}

func newBridgeRegistry(t *testing.T, generation *lifecycle.Generation, executor *lifecycle.Executor, resolver lifecycle.AgentInvocationResolver) *agent.Registry {
	t.Helper()
	registry, err := lifecycle.NewAgentRegistry(generation, executor, lifecycle.AgentBridgeOptions{ResolveInvocation: resolver})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func getBridgeTool(t *testing.T, registry *agent.Registry) agent.Tool {
	t.Helper()
	bridged, ok := registry.Get("write")
	if !ok {
		t.Fatal("bridged tool not registered")
	}
	return bridged
}

func bridgeResolver(calls *atomic.Int32) lifecycle.AgentInvocationResolver {
	return func(_ context.Context, invocation agent.ToolInvocation) (lifecycle.ExecuteRequest, error) {
		if calls != nil {
			calls.Add(1)
		}
		request := bridgeRequest()
		request.Invocation.CallID = invocation.CallID
		request.Invocation.ToolName = invocation.Name
		request.Invocation.RawInput = invocation.RawInput
		return request, nil
	}
}

func bridgeRequest() lifecycle.ExecuteRequest {
	return lifecycle.ExecuteRequest{Invocation: lifecycle.InvocationIdentity{
		TenantKey:    "tenant",
		PrincipalKey: "principal",
		RunKey:       "run",
		AttemptKey:   "attempt",
		FenceToken:   7,
		StepNumber:   1,
	}, PolicyVersion: "policy-v1"}
}

func bridgeInvocation() agent.ToolInvocation {
	return agent.ToolInvocation{CallID: "call", Name: "write", RawInput: `{"count":1,"flag":false}`}
}

func bridgeToolSetVersion(t *testing.T, toolVersion, interceptorVersion string) string {
	t.Helper()
	implementation := newBridgeTool()
	registry := lifecycle.NewRegistry()
	if err := registry.Register(implementation, bridgeMetadata(toolVersion)); err != nil {
		t.Fatal(err)
	}
	var interceptors []lifecycle.Interceptor
	if interceptorVersion != "" {
		interceptors = []lifecycle.Interceptor{bridgeInterceptor{version: interceptorVersion}}
	}
	generation, err := registry.Freeze(interceptors...)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := lifecycle.NewExecutor(lifecycle.ExecutorOptions{Generation: generation, Ledger: newMemoryLedger()})
	if err != nil {
		t.Fatal(err)
	}
	root := newBridgeRegistry(t, generation, executor, bridgeResolver(nil))
	toolSet, err := agent.NewToolSet(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	return toolSet.Version()
}
