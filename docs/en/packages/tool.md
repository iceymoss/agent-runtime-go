# tool

The `tool` package provides a production-grade execution lifecycle for local tools: frozen tool generations, interceptor preflight, input canonicalization, permission authorization, an execution ledger, and error classification, ensuring each tool side effect is executed and recorded exactly once.

## What it is

This is the repository's second tool-system layer. The first layer is the root package's `agent.Tool` / `agent.Registry` / `agent.ToolSet`, which provides model-visible tool definitions, JSON Schema validation, and allowlists and is invoked automatically by the root `Agent` loop. This package reuses the same `agent.Tool` interface for implementations but wraps it in production orchestration: `tool.Registry.Register(agent.Tool, Metadata)` attaches side-effect metadata, `Freeze()` produces an immutable `Generation`, and `Executor` drives the complete lifecycle (prepare -> preflight -> authorize -> execute -> record -> complete). `NewAgentRegistry` exposes a frozen generation and Executor to the root Agent.

```go
func NewExecutor(options ExecutorOptions) (*Executor, error)

func (e *Executor) Execute(ctx context.Context, request ExecuteRequest) (ExecuteResult, error)

func NewAgentRegistry(generation *Generation, executor *Executor, options AgentBridgeOptions) (*agent.Registry, error)

// 拦截器在 Freeze 时按顺序固定：Before 正序执行，After/OnError 逆序执行
type Interceptor interface {
    Name() string
    Version() string
    Before(context.Context, InvocationIdentity) (PreflightResult, error)
    After(context.Context, PreparedExecution, agent.ToolResult) (agent.ToolResult, error)
    OnError(context.Context, PreparedExecution, error) error
}
```

The current bridge requires `ResolveInvocation` to provide stable tenant/run/attempt/fence/resource/policy identity. It is intended for non-durable execution or execution that does not require approval suspension. If the Executor returns an approval blocker, the bridge returns `ErrAgentBridgeSuspensionUnsupported`; it does not disguise the blocker as a normal tool result before the root durable suspension/resume contract exists.

Core data flow: the caller supplies `InvocationIdentity` (tenant, run, attempt, call, raw input, and other identity data). `Executor` canonicalizes the input as `CanonicalInput`, computes its digest, and freezes it as an immutable `PreparedExecution`. Its `ExecutionKey` is derived deterministically from identity + input digest + generation digest. The same logical call therefore has the same `ExecutionKey` across every retry, which is the basis of idempotency.

Persistence is externalized through the `ExecutionLedger` interface, the consumer port for the lifecycle (`Prepare` / `Begin` / `Complete` / `Reject` / `MarkUnknown` / `Load`) that applications adapt to their own persistence layer. This package does not provide a ledger implementation. Authorization is injected through an optional `permission.Service`; `Executor` automatically constructs a `permission.CheckRequest` from `PreparedExecution`.

## Why you need it

Without this package, you must solve duplicate side effects caused by retries (requiring deterministic execution keys plus a ledger state machine), ensure approval occurs after input rewriting (otherwise the approved effect differs from the actual one), prohibit automatic replay when a tool panic or timeout leaves the result unknown, prevent loops when interceptors rewrite input, and classify failures as definitely failed and retryable versus outcome unknown. Ad hoc wrappers around `agent.Tool.Execute` cannot implement these semantics correctly.

When you do not need it: if all tools are read-only and side-effect free, or you have a single-machine prototype that does not require exactly-once semantics, use the root `agent.Registry` plus the `Agent` loop directly. You do not need ledger and generation management.

## How to use it

You must implement `ExecutionLedger`. The following simplified in-memory ledger demonstrates the minimal flow (it omits fence/revision conflict checks, which a production implementation must add and persist):

```go
package main

import (
	"context"
	"fmt"
	"sync"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/tool"
)

type noteTool struct{}

func (noteTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "write_note", Strict: true, Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"text": map[string]any{"type": "string"}},
		"required":   []any{"text"},
	}}
}
func (noteTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (noteTool) Execute(_ context.Context, inv agent.ToolInvocation) (agent.ToolResult, error) {
	return agent.ToolResult{ToolCallID: inv.CallID, Name: inv.Name, Content: "saved"}, nil
}

type memoryLedger struct {
	mu      sync.Mutex
	records map[string]tool.ExecutionRecord
}

func (m *memoryLedger) Prepare(_ context.Context, p tool.PreparedExecution) (tool.ExecutionRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.records[p.ExecutionKey]; ok {
		return existing, false, nil
	}
	record := tool.ExecutionRecord{Prepared: p, Status: tool.StatusPrepared, Revision: 1}
	m.records[p.ExecutionKey] = record
	return record, true, nil
}
func (m *memoryLedger) Begin(_ context.Context, key string, fence uint64) (tool.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[key]
	record.Status, record.FenceToken = tool.StatusRunning, fence
	m.records[key] = record
	return record, nil
}
func (m *memoryLedger) Reject(_ context.Context, key string, _ uint64, failure tool.Failure) (tool.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[key]
	record.Status, record.Failure = tool.StatusFailed, &failure
	m.records[key] = record
	return record, nil
}
func (m *memoryLedger) Complete(_ context.Context, c tool.CompleteExecution) (tool.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[c.ExecutionKey]
	record.Result, record.Failure = c.Result, c.Failure
	if c.Result != nil {
		record.Status = tool.StatusSucceeded
	} else {
		record.Status = tool.StatusFailed
	}
	m.records[c.ExecutionKey] = record
	return record, nil
}
func (m *memoryLedger) MarkUnknown(_ context.Context, key string, _ uint64, failure tool.Failure) (tool.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.records[key]
	record.Status, record.Failure = tool.StatusUnknown, &failure
	m.records[key] = record
	return record, nil
}
func (m *memoryLedger) Load(_ context.Context, key string) (tool.ExecutionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.records[key], nil
}

func main() {
	registry := tool.NewRegistry()
	err := registry.Register(noteTool{}, tool.Metadata{
		Version: "v1", SchemaVersion: "s1",
		Action: "create", EffectGroup: "notes", EffectClass: tool.EffectWrite,
	})
	if err != nil {
		panic(err)
	}
	generation, err := registry.Freeze()
	if err != nil {
		panic(err)
	}
	executor, err := tool.NewExecutor(tool.ExecutorOptions{
		Generation: generation,
		Ledger:     &memoryLedger{records: make(map[string]tool.ExecutionRecord)},
	})
	if err != nil {
		panic(err)
	}
	result, err := executor.Execute(context.Background(), tool.ExecuteRequest{
		Invocation: tool.InvocationIdentity{
			TenantKey: "tenant-1", RunKey: "run-1", AttemptKey: "attempt-1",
			CallID: "call-1", ToolName: "write_note", RawInput: `{"text":"hello"}`,
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Status, result.Result.Content) // succeeded saved
}
```

Output:

```text
succeeded saved
```

Key behavior:

- `TenantKey`, `RunKey`, `AttemptKey`, `CallID`, and `ToolName` are required in `InvocationIdentity`; otherwise it returns `ErrToolInputInvalid`.
- `RawInput` undergoes JSON Schema validation before canonicalization. Explicit JSON zero values such as `{"count":0}` are retained in `CanonicalInput`, and the digest is computed from the canonicalized value.
- `Metadata` has defaults: `Concurrency` defaults to `ConcurrencySequential`, `EffectClass` defaults to `EffectExternal`, and `ReplayPolicy` defaults to the tool's own `ReplayPolicy()`.
- The example does not inject `ExecutorOptions.Permission`, so authorization is skipped. With a `permission.Service`, deny is recorded as a failure, while ask returns `ErrApprovalPending` and `ExecuteResult.Blocker`.
- Calling again with the same `ExecuteRequest` causes `Executor` to read the completed ledger record and return it directly without executing the tool again.

## FAQ

**Q: How does this package relate to the root package's `agent.Registry`? Must tools be registered twice?**
A: They are two system layers. The root registry serves model-visible definitions and the root Agent loop. `tool.Registry` serves the production lifecycle and attaches additional `Metadata` during `Register` (version, side-effect class, idempotency declaration, and so on). The tool implementation (`agent.Tool`) is shared. The root `Agent` does not automatically use `Executor`; applications must provide a bridge if needed, for example an `agent.Tool` that internally calls `Executor`.

**Q: `Register` returns `ErrInvalidConfiguration`. What is wrong with the metadata?**
A: There are three common causes: `Version` or `SchemaVersion` is empty; `EffectClass` is not `EffectNone` but no `Action` is declared; or `ReplayPolicy` is `agent.ReplayPolicyIdempotent` while `Idempotency` is not `IdempotencyExecutionKey`. Duplicate tool names are also rejected.

**Q: What happens if the same call executes twice concurrently?**
A: Both calls derive the same `ExecutionKey`. The first occupies the ledger record, and the second receives that existing record from `Prepare`: if execution is still in progress, it receives `ErrExecutionInProgress`; if execution is complete, it directly returns the recorded result. The tool itself executes only once.

**Q: When is `StatusUnknown` produced, and why can it not retry automatically?**
A: A tool panic, `context.Canceled` / `context.DeadlineExceeded`, or an `After` interceptor failure after the side effect occurred is classified as an unknown outcome and recorded as `StatusUnknown`. Because the side effect might have happened, automatic replay could duplicate it. Subsequent calls with the same execution key therefore always return `ErrExecutionUnknown` and require human adjudication or reconciliation. A tool can implement `ClassifiedError` to explicitly identify an error as `DispositionFailed` (definitely no effect), `DispositionRetryable`, or `DispositionUnknown`.

**Q: Can an interceptor's `Before` return `PreflightRewrite` indefinitely?**
A: No. Rewrites are limited to 2; exceeding the limit returns `ErrRewriteLoop`. Rewriting to a previously seen "tool name + input digest" combination is also a loop. The entire interceptor chain runs again after a rewrite, and permission checking occurs after all rewrites and input canonicalization, so approval always applies to the final effect actually executed.

**Q: Can an `After` interceptor change `IsError` or `StopTurn` from true to false?**
A: No. `Executor` ORs both flags with their pre-rewrite values, so they can be set but not cleared. `ToolResult.ToolCallID` and `Name` also cannot change; violating this returns `ErrResultInvariant`.
