# tool

`tool` 包为本地工具提供生产级执行生命周期：冻结工具代次（Generation）、拦截器 preflight、输入规范化、权限授权、执行账本（ledger）与错误分类，保证一次工具副作用恰好被执行并记录一次。

## 是什么

这是仓库中工具系统的第二层。第一层是根包的 `agent.Tool` / `agent.Registry` / `agent.ToolSet`：负责模型可见的工具定义、JSON Schema 校验和白名单，由根包 `Agent` 循环自动调用。本包复用同一个 `agent.Tool` 接口作为工具实现，但在其外面包一层生产编排：`tool.Registry.Register(agent.Tool, Metadata)` 附加副作用元数据后 `Freeze()` 出不可变的 `Generation`，再由 `Executor` 驱动完整生命周期（prepare → preflight → authorize → execute → record → complete）。`NewAgentRegistry` 可以把冻结代次和 Executor 暴露给根 Agent。

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

当前 bridge 要求调用方通过 `ResolveInvocation` 提供稳定的 tenant/run/attempt/fence/resource/policy 身份，适用于 non-durable 或不需要审批 suspension 的执行。若 Executor 返回 approval blocker，bridge 会返回 `ErrAgentBridgeSuspensionUnsupported`；在 root durable suspension/resume contract 完成前，它不会把 blocker 伪装成普通工具结果。

核心数据流：调用方提供 `InvocationIdentity`（租户、run、attempt、call、原始输入等身份信息），`Executor` 将输入规范化为 `CanonicalInput` 并计算摘要，冻结为不可变的 `PreparedExecution`，其中 `ExecutionKey` 由身份 + 输入摘要 + 代次摘要确定性推导——同一次逻辑调用无论重试多少次，`ExecutionKey` 都相同，这是幂等的基础。

持久化通过 `ExecutionLedger` 接口外置：它是生命周期的消费端口（`Prepare` / `Begin` / `Complete` / `Reject` / `MarkUnknown` / `Load`），由应用适配到自己的持久化层。本包不提供 ledger 实现。授权通过可选的 `permission.Service` 注入，`Executor` 会用 `PreparedExecution` 自动构造 `permission.CheckRequest`。

## 为什么需要它

没有这个包，你要自己解决：重试导致副作用重复执行（需要确定性执行键 + 账本状态机）、审批必须发生在输入改写之后（否则批准的不是实际 effect）、工具 panic 或超时后结果不明时禁止自动重放、拦截器改写输入后如何防止死循环、失败如何区分"确定失败可重试"与"结果未知"。这些语义靠在 `agent.Tool.Execute` 外面随手包一层是做不对的。

什么时候不需要它：工具全部只读、无副作用，或者是不要求 exactly-once 语义的单机原型——直接用根包 `agent.Registry` + `Agent` 循环即可，不必引入 ledger 和代次管理。

## 怎么用

`ExecutionLedger` 需要自己实现。下面用一个简化的内存 ledger 演示最小流程（省略了 fence/revision 冲突检查，生产实现必须补齐并持久化）：

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

运行输出：

```text
succeeded saved
```

关键行为：

- `InvocationIdentity` 中 `TenantKey`、`RunKey`、`AttemptKey`、`CallID`、`ToolName` 必填，否则返回 `ErrToolInputInvalid`。
- `RawInput` 会先做 JSON Schema 校验再规范化，显式的 JSON 零值（如 `{"count":0}`）会被保留进 `CanonicalInput`，摘要基于规范化后的值计算。
- `Metadata` 有默认值：`Concurrency` 默认 `ConcurrencySequential`，`EffectClass` 默认 `EffectExternal`，`ReplayPolicy` 默认取工具自身的 `ReplayPolicy()`。
- 示例没有注入 `ExecutorOptions.Permission`，授权阶段被跳过；接入 `permission.Service` 后，deny 会记账为失败，ask 会返回 `ErrApprovalPending` 和 `ExecuteResult.Blocker`。
- 用相同的 `ExecuteRequest` 再调用一次，`Executor` 会从 ledger 读到已完成记录直接返回，不会再次执行工具。

## 常见问题

**Q: 这个包和根包的 `agent.Registry` 是什么关系，工具要注册两次吗？**
A: 是两层系统。根包 registry 服务于模型可见定义和根包 Agent 循环；`tool.Registry` 服务于生产生命周期，`Register` 时额外附加 `Metadata`（版本、副作用类别、幂等声明等）。工具实现（`agent.Tool`）是同一份。根包 `Agent` 不会自动走 `Executor`，需要的话由应用提供桥接（例如实现一个内部调用 `Executor` 的 `agent.Tool`）。

**Q: `Register` 返回 `ErrInvalidConfiguration`，元数据哪里不对？**
A: 常见三种：`Version` 或 `SchemaVersion` 为空；`EffectClass` 不是 `EffectNone` 但没有声明 `Action`；`ReplayPolicy` 为 `agent.ReplayPolicyIdempotent` 但 `Idempotency` 不是 `IdempotencyExecutionKey`。另外重复的工具名也会被拒绝。

**Q: 同一个调用并发执行两次会怎样？**
A: 两次调用推导出相同的 `ExecutionKey`。先到者在 ledger 中占据记录，后到者在 `Prepare` 时拿到已存在的记录：如果仍在执行中，收到 `ErrExecutionInProgress`；如果已完成，直接返回已记录的结果。工具本体只会被执行一次。

**Q: 什么情况下会得到 `StatusUnknown`？为什么不能自动重试？**
A: 工具 panic、`context.Canceled` / `context.DeadlineExceeded`，或 `After` 拦截器在副作用已发生后失败，都会被归类为"结果不明"并记为 `StatusUnknown`。此时副作用可能已经发生，自动重放可能造成重复副作用，所以后续对同一执行键的调用固定返回 `ErrExecutionUnknown`，需要人工或对账流程裁决。工具可以实现 `ClassifiedError` 接口显式声明错误是 `DispositionFailed`（确定无效果）、`DispositionRetryable` 还是 `DispositionUnknown`。

**Q: 拦截器 `Before` 返回 `PreflightRewrite` 可以无限改写输入吗？**
A: 不行。改写上限为 2 次，超出返回 `ErrRewriteLoop`；改写回到出现过的"工具名 + 输入摘要"组合同样判定为循环。改写后会重新跑整条拦截器链，权限检查发生在所有改写完成、输入规范化之后——批准的永远是最终实际执行的 effect。

**Q: `After` 拦截器能把 `IsError` 或 `StopTurn` 从 true 改成 false 吗？**
A: 不能。`Executor` 会把这两个标志与改写前的值做或运算，只能置起不能清除；同时 `ToolResult.ToolCallID` 和 `Name` 不允许改变，违反会返回 `ErrResultInvariant`。
