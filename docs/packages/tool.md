# tool

`tool` 包为本地工具提供生产级执行生命周期：冻结工具代次（Generation）、拦截器 preflight、输入规范化、权限授权、执行账本（ledger）与错误分类，保证一次工具副作用恰好被执行并记录一次。

## 是什么

这是仓库中工具系统的第二层。第一层是根包的 `agent.Tool` / `agent.Registry` / `agent.ToolSet`：负责模型可见的工具定义、JSON Schema 校验和白名单，由根包 `Agent` 循环自动调用。本包复用同一个 `agent.Tool` 接口作为工具实现，但在其外面包一层生产编排：`tool.Registry.Register(agent.Tool, Metadata)` 附加副作用元数据后 `Freeze()` 出不可变的 `Generation`，再由 `Executor` 驱动完整生命周期（prepare → preflight → authorize → execute → record → complete）。`NewAgentRegistry` 可以把冻结代次和 Executor 暴露给根 Agent。

```go
func NewExecutor(options ExecutorOptions) (*Executor, error)

func (e *Executor) Execute(ctx context.Context, request ExecuteRequest) (ExecuteResult, error)

func (e *Executor) ResumeApproval(ctx context.Context, request ResumeApprovalRequest) (ExecuteResult, error)

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

bridge 要求调用方通过 `ResolveInvocation` 提供稳定的 tenant/run/attempt/fence/resource/policy 身份。root Agent 会把 approval blocker 返回为 `OutcomeSuspended`、`StopReasonToolSuspended` 和不含原始输入的 `RunResult.Suspension`。durable 运行还会将 blocker、step 和 ordinal 写入 checkpoint；审批解决后，调用方把原样保存的 blocker 放入 `DurableRunConfig.ToolResume` 再次运行。runtime 会拒绝与 checkpoint 不完全匹配的 receipt，并由 bridge 调用 `ResumeApproval`。

bridge 工具实现了 `agent.ToolExecutionLifecycleOwner`。durable root 因此只用自己的 tool record 保存模型循环的 prepared/completed cursor，不会在高级 Executor 外再调用 `BeginTool`：running、unknown、fence 和 effect boundary 只由 `ExecutionLedger` 的后端拥有。普通 `agent.Tool` 不受影响，仍由 root durable ledger 管理完整生命周期。

审批通过后，调用方使用原始 `ExecuteRequest` 和 blocker 中的 request ref、resume token、revision 调用 `ResumeApproval`。Executor 会重新计算并验证 immutable prepared identity，调用 permission revalidation，只有精确 effect 仍被允许时才进入 `Begin` 和执行；重复 resume 会复用 ledger 中的成功结果，不会重复副作用。

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

## 挂起与恢复

工具不是只有「成功」和「失败」两种结局。有些工具会把活儿交出去——委派给 child agent、等一个 webhook、把任务丢进队列——它启动了工作，但现在给不出结果。把这种情况记成失败会丢掉恢复所需的句柄，记成 unknown 又会禁止本来完全安全的重放。所以它有自己的状态：`StatusSuspended`。

工具通过返回 `agent.ToolSuspensionError` 表达挂起：

```go
func (t *delegateTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	if invocation.Resume == nil {
		handle := t.startChild(ctx, invocation.RawInput)   // 启动外部工作
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{
			Kind: agent.ToolSuspensionExternal, RequestRef: handle, ResumeToken: token, Revision: 1,
		}}
	}
	// 恢复时工具拿回自己签发的句柄，认领这份工作而不是重新开始
	return t.collect(ctx, invocation.Resume.RequestRef)
}
```

接下来发生的事：

1. Executor 调 `ExecutionLedger.Suspend` 把执行停在 `StatusSuspended` 并存下句柄。拦截器的 `After` / `OnError` **不会**触发——什么都没失败，也还没有结果可处理，它们会在真正结束时才跑。
2. Bridge 把它投影成根包的 `ToolSuspensionError`，运行时据此 checkpoint 整个 run，返回 `suspended` + `tool_suspended`。
3. 后续 attempt 通过 `DurableRun.ToolResume` 交回同一个句柄，Executor `Resume` 用**原来那个 fence** 重新武装执行，再带着 `invocation.Resume` 调一次工具。

`Kind` 由 Executor 分派：`ToolSuspensionApproval` 走 permission 服务重新校验，其他一律交还给工具。运行时本身从不解释 `Kind`，所以应用可以定义自己的挂起类型，只要恢复方认识它。

恢复必须带上 Executor 当初签发的**那一个**句柄：token 或 revision 对不上会返回 `ErrExecutionConflict`，否则任何知道 execution key 的人都能重启别人的工作。

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
