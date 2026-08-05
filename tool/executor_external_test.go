package tool_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	lifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

type testTool struct {
	calls   atomic.Int32
	result  agent.ToolResult
	err     error
	start   chan struct{}
	release chan struct{}
}

func (t *testTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "write", Strict: true, Parameters: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"count": map[string]any{"type": "number"},
			"flag":  map[string]any{"type": "boolean"},
		},
		"required": []any{"count", "flag"},
	}}
}
func (t *testTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t *testTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	t.calls.Add(1)
	if t.start != nil {
		close(t.start)
		<-t.release
	}
	return t.result, t.err
}

type memoryLedger struct {
	mu       sync.Mutex
	records  map[string]lifecycle.ExecutionRecord
	beginErr error
}

func newMemoryLedger() *memoryLedger {
	return &memoryLedger{records: make(map[string]lifecycle.ExecutionRecord)}
}
func (m *memoryLedger) Prepare(_ context.Context, prepared lifecycle.PreparedExecution) (lifecycle.ExecutionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.records[prepared.ExecutionKey]; ok {
		if !reflect.DeepEqual(existing.Prepared, prepared) {
			return lifecycle.ExecutionRecord{}, false, lifecycle.ErrExecutionConflict
		}
		return cloneTestRecord(existing), false, nil
	}
	record := lifecycle.ExecutionRecord{Prepared: prepared, Status: lifecycle.StatusPrepared, Revision: 1}
	m.records[prepared.ExecutionKey] = record
	return cloneTestRecord(record), true, nil
}
func (m *memoryLedger) Begin(_ context.Context, key string, fence uint64) (lifecycle.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.beginErr != nil {
		return lifecycle.ExecutionRecord{}, m.beginErr
	}
	record := m.records[key]
	if record.Status != lifecycle.StatusPrepared {
		return lifecycle.ExecutionRecord{}, lifecycle.ErrExecutionConflict
	}
	record.Status, record.FenceToken, record.Revision = lifecycle.StatusRunning, fence, record.Revision+1
	m.records[key] = record
	return cloneTestRecord(record), nil
}
func (m *memoryLedger) Reject(_ context.Context, key string, fence uint64, failure lifecycle.Failure) (lifecycle.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[key]
	if record.Status != lifecycle.StatusPrepared || record.Prepared.FenceToken != fence {
		return lifecycle.ExecutionRecord{}, lifecycle.ErrStaleFence
	}
	record.Status, record.Failure, record.FenceToken = lifecycle.StatusFailed, &failure, fence
	m.records[key] = cloneTestRecord(record)
	return cloneTestRecord(record), nil
}
func (m *memoryLedger) Complete(_ context.Context, command lifecycle.CompleteExecution) (lifecycle.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[command.ExecutionKey]
	if record.Status != lifecycle.StatusRunning || record.FenceToken != command.FenceToken {
		return lifecycle.ExecutionRecord{}, lifecycle.ErrStaleFence
	}
	record.Result, record.Failure, record.Revision = command.Result, command.Failure, record.Revision+1
	if command.Result != nil {
		record.Status = lifecycle.StatusSucceeded
	} else {
		record.Status = lifecycle.StatusFailed
	}
	m.records[command.ExecutionKey] = cloneTestRecord(record)
	return cloneTestRecord(record), nil
}
func (m *memoryLedger) MarkUnknown(_ context.Context, key string, fence uint64, failure lifecycle.Failure) (lifecycle.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[key]
	if record.Status != lifecycle.StatusRunning || record.FenceToken != fence {
		return lifecycle.ExecutionRecord{}, lifecycle.ErrStaleFence
	}
	record.Status, record.Failure = lifecycle.StatusUnknown, &failure
	m.records[key] = cloneTestRecord(record)
	return cloneTestRecord(record), nil
}
func (m *memoryLedger) Load(_ context.Context, key string) (lifecycle.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record, ok := m.records[key]
	if !ok {
		return lifecycle.ExecutionRecord{}, lifecycle.ErrToolNotFound
	}
	return cloneTestRecord(record), nil
}
func cloneTestRecord(record lifecycle.ExecutionRecord) lifecycle.ExecutionRecord {
	if record.Result != nil {
		result := *record.Result
		record.Result = &result
	}
	if record.Failure != nil {
		failure := *record.Failure
		record.Failure = &failure
	}
	return record
}

type testPermission struct {
	decision permission.CheckDecision
	checks   atomic.Int32
}

func (p *testPermission) Check(_ context.Context, request permission.CheckRequest) (permission.CheckResult, error) {
	p.checks.Add(1)
	result := permission.CheckResult{Decision: p.decision, InputDigest: request.InputDigest, PolicyVersion: request.PolicyVersion}
	if p.decision == permission.DecisionAsk {
		result.Blocker = &permission.SuspensionBlocker{RequestRef: request.RequestKey, ResumeToken: "opaque", Revision: 1}
	}
	return result, nil
}
func (*testPermission) Resolve(context.Context, permission.ResolveCommand) (permission.Snapshot, bool, error) {
	return permission.Snapshot{}, false, nil
}
func (*testPermission) Cancel(context.Context, permission.CancelCommand) (permission.Snapshot, bool, error) {
	return permission.Snapshot{}, false, nil
}
func (*testPermission) Revalidate(context.Context, permission.RevalidateCommand) (permission.CheckResult, error) {
	return permission.CheckResult{}, nil
}
func (*testPermission) GetRequest(context.Context, permission.GetRequestQuery) (permission.Snapshot, error) {
	return permission.Snapshot{}, nil
}
func (*testPermission) ListPending(context.Context, permission.ListPendingQuery) ([]permission.Snapshot, error) {
	return nil, nil
}
func (*testPermission) ConsumeGrant(context.Context, permission.ConsumeGrantCommand) (permission.Grant, error) {
	return permission.Grant{}, nil
}
func (*testPermission) RevokeGrant(context.Context, permission.RevokeGrantCommand) (permission.Grant, error) {
	return permission.Grant{}, nil
}
func (*testPermission) ExpireDue(context.Context, permission.ExpireCommand) (permission.ExpireResult, error) {
	return permission.ExpireResult{}, nil
}

type testInterceptor struct {
	name  string
	order *[]string
	after func(agent.ToolResult) (agent.ToolResult, error)
}

type testObserver struct {
	mu     sync.Mutex
	phases []lifecycle.Phase
}

func (o *testObserver) TryObserve(observation lifecycle.Observation) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.phases = append(o.phases, observation.Phase)
	return true
}

func (i testInterceptor) Name() string    { return i.name }
func (i testInterceptor) Version() string { return "v1" }
func (i testInterceptor) Before(context.Context, lifecycle.InvocationIdentity) (lifecycle.PreflightResult, error) {
	*i.order = append(*i.order, "before:"+i.name)
	return lifecycle.PreflightResult{Decision: lifecycle.PreflightContinue}, nil
}
func (i testInterceptor) After(_ context.Context, _ lifecycle.PreparedExecution, result agent.ToolResult) (agent.ToolResult, error) {
	*i.order = append(*i.order, "after:"+i.name)
	if i.after != nil {
		return i.after(result)
	}
	return result, nil
}
func (i testInterceptor) OnError(context.Context, lifecycle.PreparedExecution, error) error {
	*i.order = append(*i.order, "error:"+i.name)
	return nil
}

func TestExecutorLifecycleOrderAndDuplicateRetry(t *testing.T) {
	var order []string
	implementation := &testTool{result: agent.ToolResult{ToolCallID: "call", Name: "write", Content: "ok", StopTurn: true}}
	executor, ledger := newExecutor(t, implementation, nil,
		testInterceptor{name: "first", order: &order}, testInterceptor{name: "second", order: &order})
	request := validRequest(`{"count":0,"flag":false}`)
	first, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !first.Result.StopTurn || first.Status != lifecycle.StatusSucceeded {
		t.Fatalf("Execute() = %#v", first)
	}
	wantOrder := []string{"before:first", "before:second", "after:second", "after:first"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	second, err := executor.Execute(context.Background(), request)
	if err != nil || second.Result == nil || implementation.calls.Load() != 1 {
		t.Fatalf("duplicate Execute() result=%#v err=%v calls=%d", second, err, implementation.calls.Load())
	}
	ledger.mu.Lock()
	stored := ledger.records[first.Prepared.ExecutionKey]
	ledger.mu.Unlock()
	if stored.Prepared.CanonicalInput != `{"count":0,"flag":false}` || stored.Prepared.InputDigest == "" {
		t.Fatalf("zero values not preserved: %#v", stored.Prepared)
	}
}

func TestExecutorPermissionAskDoesNotExecute(t *testing.T) {
	implementation := &testTool{}
	authorizer := &testPermission{decision: permission.DecisionAsk}
	executor, _ := newExecutor(t, implementation, authorizer)
	result, err := executor.Execute(context.Background(), validRequest(`{"count":0,"flag":false}`))
	if !errors.Is(err, lifecycle.ErrApprovalPending) || result.Blocker == nil || implementation.calls.Load() != 0 {
		t.Fatalf("Execute() result=%#v err=%v calls=%d", result, err, implementation.calls.Load())
	}
}

func TestExecutorPermissionDenyIsTerminalWithoutExecution(t *testing.T) {
	implementation := &testTool{}
	authorizer := &testPermission{decision: permission.DecisionDeny}
	executor, _ := newExecutor(t, implementation, authorizer)
	result, err := executor.Execute(context.Background(), validRequest(`{"count":0,"flag":false}`))
	if !errors.Is(err, lifecycle.ErrPermissionDenied) || result.Status != lifecycle.StatusFailed || implementation.calls.Load() != 0 {
		t.Fatalf("Execute() result=%#v err=%v calls=%d", result, err, implementation.calls.Load())
	}
	result, err = executor.Execute(context.Background(), validRequest(`{"count":0,"flag":false}`))
	if !errors.Is(err, lifecycle.ErrToolFatal) || result.Status != lifecycle.StatusFailed || implementation.calls.Load() != 0 {
		t.Fatalf("retry Execute() result=%#v err=%v calls=%d", result, err, implementation.calls.Load())
	}
}

func TestExecutorConcurrentSameExecutionExecutesOnce(t *testing.T) {
	implementation := &testTool{result: agent.ToolResult{ToolCallID: "call", Name: "write"}, start: make(chan struct{}), release: make(chan struct{})}
	executor, _ := newExecutor(t, implementation, nil)
	request := validRequest(`{"count":0,"flag":false}`)
	firstDone := make(chan error, 1)
	go func() {
		_, err := executor.Execute(context.Background(), request)
		firstDone <- err
	}()
	<-implementation.start
	_, secondErr := executor.Execute(context.Background(), request)
	if !errors.Is(secondErr, lifecycle.ErrExecutionInProgress) {
		t.Fatalf("concurrent Execute() error = %v", secondErr)
	}
	close(implementation.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if implementation.calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1", implementation.calls.Load())
	}
}

func TestExecutorStaleFenceAndUnknownNeverReplay(t *testing.T) {
	implementation := &testTool{err: context.DeadlineExceeded}
	executor, ledger := newExecutor(t, implementation, nil)
	ledger.beginErr = lifecycle.ErrStaleFence
	_, err := executor.Execute(context.Background(), validRequest(`{"count":0,"flag":false}`))
	if !errors.Is(err, lifecycle.ErrStaleFence) || implementation.calls.Load() != 0 {
		t.Fatalf("stale Execute() err=%v calls=%d", err, implementation.calls.Load())
	}

	implementation = &testTool{err: context.DeadlineExceeded}
	executor, _ = newExecutor(t, implementation, nil)
	request := validRequest(`{"count":0,"flag":false}`)
	_, err = executor.Execute(context.Background(), request)
	if !errors.Is(err, lifecycle.ErrExecutionUnknown) {
		t.Fatalf("unknown Execute() error = %v", err)
	}
	_, err = executor.Execute(context.Background(), request)
	if !errors.Is(err, lifecycle.ErrExecutionUnknown) || implementation.calls.Load() != 1 {
		t.Fatalf("unknown replay err=%v calls=%d", err, implementation.calls.Load())
	}
}

func TestExecutorInterceptorFailurePreservesResultFlags(t *testing.T) {
	var order []string
	implementation := &testTool{result: agent.ToolResult{ToolCallID: "call", Name: "write", IsError: true, StopTurn: true}}
	interceptor := testInterceptor{name: "redact", order: &order, after: func(result agent.ToolResult) (agent.ToolResult, error) {
		result.IsError, result.StopTurn = false, false
		return result, nil
	}}
	executor, _ := newExecutor(t, implementation, nil, interceptor)
	result, err := executor.Execute(context.Background(), validRequest(`{"count":0,"flag":false}`))
	if err != nil || result.Result == nil || !result.Result.IsError || !result.Result.StopTurn {
		t.Fatalf("Execute() result=%#v err=%v", result, err)
	}
}

func TestExecutorAfterFailureMarksUnknownAndRunsErrorHook(t *testing.T) {
	var order []string
	implementation := &testTool{result: agent.ToolResult{ToolCallID: "call", Name: "write"}}
	interceptor := testInterceptor{name: "audit", order: &order, after: func(agent.ToolResult) (agent.ToolResult, error) {
		return agent.ToolResult{}, errors.New("upstream interceptor detail")
	}}
	executor, _ := newExecutor(t, implementation, nil, interceptor)
	result, err := executor.Execute(context.Background(), validRequest(`{"count":0,"flag":false}`))
	if !errors.Is(err, lifecycle.ErrInterceptorFailed) || result.Status != lifecycle.StatusUnknown {
		t.Fatalf("Execute() result=%#v err=%v", result, err)
	}
	want := []string{"before:audit", "after:audit", "error:audit"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

func TestExecutorReportsLifecyclePhasesInOrder(t *testing.T) {
	implementation := &testTool{result: agent.ToolResult{ToolCallID: "call", Name: "write"}}
	registry := lifecycle.NewRegistry()
	if err := registry.Register(implementation, lifecycle.Metadata{Version: "v1", SchemaVersion: "s1", Action: "write", EffectClass: lifecycle.EffectWrite, ReplayPolicy: agent.ReplayPolicyNever}); err != nil {
		t.Fatal(err)
	}
	generation, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	observer := &testObserver{}
	executor, err := lifecycle.NewExecutor(lifecycle.ExecutorOptions{Generation: generation, Ledger: newMemoryLedger(), Observer: observer})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), validRequest(`{"count":0,"flag":false}`)); err != nil {
		t.Fatal(err)
	}
	want := []lifecycle.Phase{lifecycle.PhasePrepare, lifecycle.PhasePreflight, lifecycle.PhaseAuthorize, lifecycle.PhaseExecute, lifecycle.PhaseRecord, lifecycle.PhaseComplete}
	observer.mu.Lock()
	got := append([]lifecycle.Phase(nil), observer.phases...)
	observer.mu.Unlock()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("phases = %v, want %v", got, want)
	}
}

func TestGenerationDeepCopiesDefinitions(t *testing.T) {
	implementation := &testTool{}
	registry := lifecycle.NewRegistry()
	metadata := lifecycle.Metadata{Version: "v1", SchemaVersion: "s1", Action: "write", EffectClass: lifecycle.EffectWrite, ReplayPolicy: agent.ReplayPolicyNever}
	if err := registry.Register(implementation, metadata); err != nil {
		t.Fatal(err)
	}
	generation, err := registry.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	definition := generation.Definitions()[0]
	definition.Parameters["type"] = "string"
	if generation.Definitions()[0].Parameters["type"] != "object" {
		t.Fatal("generation definition was mutated")
	}
	if err := registry.Register(implementation, metadata); !errors.Is(err, lifecycle.ErrInvalidConfiguration) {
		t.Fatalf("duplicate Register() error = %v", err)
	}
}

func newExecutor(t *testing.T, implementation *testTool, authorizer permission.Service, interceptors ...lifecycle.Interceptor) (*lifecycle.Executor, *memoryLedger) {
	t.Helper()
	registry := lifecycle.NewRegistry()
	if err := registry.Register(implementation, lifecycle.Metadata{Version: "v1", SchemaVersion: "s1", Action: "write", EffectClass: lifecycle.EffectWrite, ReplayPolicy: agent.ReplayPolicyNever}); err != nil {
		t.Fatal(err)
	}
	generation, err := registry.Freeze(interceptors...)
	if err != nil {
		t.Fatal(err)
	}
	ledger := newMemoryLedger()
	executor, err := lifecycle.NewExecutor(lifecycle.ExecutorOptions{Generation: generation, Ledger: ledger, Permission: authorizer})
	if err != nil {
		t.Fatal(err)
	}
	return executor, ledger
}

func validRequest(input string) lifecycle.ExecuteRequest {
	return lifecycle.ExecuteRequest{Invocation: lifecycle.InvocationIdentity{TenantKey: "tenant", PrincipalKey: "principal", RunKey: "run", AttemptKey: "attempt", FenceToken: 7, StepNumber: 1, CallID: "call", ToolName: "write", RawInput: input}, PolicyVersion: "p1"}
}
