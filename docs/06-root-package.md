# 根包 `agent` API 详解

模块路径是 `github.com/iceymoss/agent-runtime-go`，但根包名是 `agent`：

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

根包提供 provider-neutral 的消息、模型、工具、多步执行循环和可选的 checkpoint 恢复协议。`Agent` 自身无会话状态，不读取配置文件，不选择 provider，也不会自动保存输入、输出或业务记录。

## Config、Agent、RunRequest、RunResult

### `Config`

`Config` 是一次装配的执行配置：

| 字段 | 含义 |
|---|---|
| `Key` | Agent 业务标识；运行时只保存和返回，不赋予格式语义 |
| `ModelName` | 传入每次 `GenerateRequest.Model` 的模型名 |
| `MaxSteps` | 单轮最多执行的模型步骤，必须大于 0 |
| `AllowedTools` | 工具白名单；`nil` 表示 registry 中全部工具，空切片表示不开放工具 |
| `LoopDetectWindow`、`LoopDetectThreshold` | 重复工具调用检测；非正数采用默认值，窗口不能大于 `MaxSteps` |
| `ContextWindow` | 上下文容量；0 表示未知，不做预算中断 |
| `Temperature`、`MaxTokens`、`TopP` | 可选生成参数；指针用于区分“未设置”和显式零值 |
| `StopConditions` | 附加停止条件，和内置 `StepCountIs(MaxSteps)` 做 OR 组合 |
| `ToolChoice` | 默认工具选择：auto、none、required 或 named |
| `ToolRepairLimit` | 非法工具参数允许回灌模型修正的次数；0 取默认 1，最大 2 |

`New` 会立即检查 model、工具白名单、schema、capability、工具选择和循环检测参数。配置错误应在启动期暴露，而不是等到请求中途。

### `Agent`

`Agent` 保存 model、配置和不可变 `ToolSet` 快照。它不保存某次 `Run` 的 history，因此装配后可供多个 goroutine 和会话复用。`Key()` 和 `ContextWindow()` 可读取两个常用装配值。

### `RunRequest`

`RunRequest.Messages` 是本轮完整输入历史，至少一条，运行时会深复制而不修改调用方数据。其余字段均为可选：

- `ToolChoice` 覆盖 `Config.ToolChoice`。
- `StepPolicy` 只能选择下一步的工具子集、tool choice、模型名和标量生成参数，不能替换 `Model` 或改写 history。
- `ObservationEmitter` 接收可丢弃、非权威的进度快照。
- `DurableRun` 启用根包 checkpoint 执行；为 `nil` 时是普通内存执行。

### `RunResult`

`Messages` 只含本轮新产生的 assistant/tool 消息，调用方需将输入 user message 和这些新消息按自己的事务规则保存。`Steps` 是每次模型调用及其工具结果，`Usage` 是累计用量，`Text` 是最后一步文本，`ModelName` 是实际模型名。

必须联合判断 error、`Outcome` 和 `StopReason`：

- `OutcomeCompleted`：正常文本完成，或工具以 `StopTurn` 主动结束。
- `OutcomeSuspended`：输出长度、最大步数、上下文预算或自定义停止条件中断，通常可由上层决定续跑。
- `OutcomeFailed`：`Run` 返回非 nil error 时设置；循环检测也以 error 结束。
- `StopReasonComplete`、`StopReasonOutputLimit`、`StopReasonMaxSteps`、`StopReasonContextBudget` 等解释具体原因。

耐久执行还可能返回 `DurableCompletion`、`DurableFailure` 或 `DurableSuspension`，让领域 owner 在自己的事务里完成最终状态投影；普通运行这些字段为空。

### 实用例子：装配并执行一轮

```go
package main

import (
	"context"
	"fmt"
	"log"

	agent "github.com/iceymoss/agent-runtime-go"
)

// model 由下文的 provider adapter 实现。
func run(ctx context.Context, model agent.Model) error {
	registry := agent.NewRegistry() // 没有工具也必须提供 registry
	maxTokens := 800
	runner, err := agent.New(agent.Config{
		Key:           "assistant.support",
		ModelName:     "production-model",
		MaxSteps:      4,
		AllowedTools:  []string{}, // 明确禁止工具
		ContextWindow: 32_000,
		MaxTokens:     &maxTokens,
	}, model, registry)
	if err != nil {
		return err
	}

	result, err := runner.Run(ctx, agent.RunRequest{Messages: []agent.Message{
		agent.NewSystemMessage("回答要简洁。"),
		agent.NewUserMessage("解释幂等键的用途。"),
	}})
	if err != nil {
		return err
	}
	if result.Outcome != agent.OutcomeCompleted {
		return fmt.Errorf("run 未完成: outcome=%s stop=%s", result.Outcome, result.StopReason)
	}
	log.Printf("model=%s tokens=%d text=%s", result.ModelName, result.Usage.TotalTokens, result.Text)
	// 在此由应用保存 user message 和 result.Messages。
	return nil
}
```

## Message、Model、Stream

### 消息值模型

`Message` 由 `Role`、有序 `Parts` 和可选 `FinishReason` 组成。支持四种 role：system、user、assistant、tool；支持 text、tool call、tool result、image 四种 part。

重要约束由 `ValidateMessage` 检查：

- 消息必须至少有一个 part；tool role 只能携带 tool result。
- tool call 只能在 assistant 消息中，且 `ID`、`Name` 非空。
- tool result 只能在 tool 消息中，且通过 `ToolCallID` 与调用配对。
- image 只能出现在 user 消息；MIME 仅支持 JPEG、PNG、WebP。
- image 必须二选一：内联 `Data`，或带 `sha256:` digest 和正数 `SizeBytes` 的不透明 `Ref`。

辅助函数 `NewSystemMessage`、`NewUserMessage`、`NewAssistantMessage`、`NewToolMessage` 构造常见消息；`Text()`、`ToolCalls()`、`ToolResults()` 提取内容但不代表完整原始结构。

### `Model` 与请求/响应

每个 adapter 必须实现：

```go
type Model interface {
	Name() string
	Capabilities() Capabilities
	Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}
```

`GenerateRequest` 包含完整 history、已过白名单的工具定义、tool choice 和生成参数。adapter 在发上游前应调用 `ValidateGenerateRequestCapabilities`，并对不支持的能力直接拒绝，不能静默降级。

`Response` 是一步的完整终态。`Usage.TotalTokens` 必须等于 prompt、completion、cache creation、cache read 的归一化分量总和；`ReasoningTokens` 是 completion 的子集。`FinishReason` 与 `Response.Message.FinishReason` 必须一致。

### 流协议

一次 `Stream` 可以发送多个：

- `ChunkText`：文本增量。
- `ChunkToolCall`：一个已经拼装完整的工具调用，不能是半截 JSON delta。
- `ChunkFinish`：唯一终态，携带完整 `Response`。
- `ChunkError`：携带原错误，然后关闭 channel。

`ChunkFinish` 前的文本和工具调用必须与终态 response 完全一致。缺少终态、终态后继续发 chunk、usage 非法或 channel 不关闭都会被视为协议错误。调用取消后 adapter 必须停止上游工作并关闭 channel。

### 实用例子：一个最小流式 model

```go
type fixedModel struct{}

func (fixedModel) Name() string { return "fixed" }
func (fixedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}
func (fixedModel) Stream(ctx context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	if err := agent.ValidateGenerateRequestCapabilities(req, (fixedModel{}).Capabilities()); err != nil {
		return nil, err
	}
	out := make(chan agent.StreamChunk, 2)
	go func() {
		defer close(out)
		message := agent.NewAssistantMessage("你好")
		response := &agent.Response{
			Message: message, Usage: agent.Usage{TotalTokens: 3},
			FinishReason: agent.FinishStop, ModelName: "fixed-v1",
		}
		select {
		case out <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: "你好"}:
		case <-ctx.Done():
			return
		}
		select {
		case out <- agent.StreamChunk{Type: agent.ChunkFinish, Response: response}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}
```

真实 adapter 应用 `NewModelError` 归一化拒绝、认证、限流、传输和协议错误，并用 `agenttest.TestModel` 做一致性测试。

## Tool、Registry、ToolSet

### `Tool`

工具实现提供三个方法：

- `Definition()`：模型可见的名称、描述和 JSON Schema。
- `ReplayPolicy()`：模糊恢复时的策略，取 `never`、`idempotent` 或 `resolve`。
- `Execute()`：接收原始 JSON 和可选 `ExecutionKey`，返回 `ToolResult`。

参数不合法、工具不在白名单等“模型可修正”问题会作为 `IsError` 结果回灌模型；工具返回非 nil Go error 则中止当前 attempt。`StopTurn` 可要求本轮立即结束。

用于 durable artifact 的工具还应实现 `ExecutableVersioner`。`ExecutableVersion()` 标识 schema 之外的实际实现版本；`ToolSet.ValidateExecutableVersions` 会拒绝缺失版本的耐久快照。

### `NewTool`：从函数生成工具

大多数工具不需要手写 JSON Schema。`NewTool` / `MustNewTool` 从类型化 Go 函数生成完整的 `Tool`，schema 由结构体反射得出：

```go
type WeatherInput struct {
	City string `json:"city" description:"City name"`
	Days int    `json:"days,omitempty" description:"Forecast days"`
}

tool := agent.MustNewTool("get_weather", "Get the current weather for a city.",
	func(ctx context.Context, input WeatherInput) (agent.ToolResult, error) {
		return agent.ToolResult{Content: lookup(input.City)}, nil
	},
	agent.WithToolReplayPolicy(agent.ReplayPolicyIdempotent))
```

规则：属性名取 `json` tag；非指针且未标 `omitempty`/`omitzero` 的字段进入 `required`；`description` tag 成为属性描述；支持嵌套结构体、切片和 string-key map。schema 默认 strict，可用 `agent.WithoutStrictSchema()` 关闭；`agent.WithToolExecutableVersion` 声明 durable 所需的实现版本。运行时会先按 schema 校验原始输入再调用函数，函数拿到的是解码好的结构体。

### `Registry` 与 `ToolSet`

`Registry` 是并发安全的可变装配表。`Register` 拒绝重名，`Replace` 只替换已有名称，注册时即规范化和编译 Draft 2020-12 schema。`Strict` 会在对象 schema 未声明时补 `additionalProperties: false`，外部 `$ref` 不受支持。

`NewToolSet(registry, allowed)` 创建不可变快照：

- `allowed == nil`：复制全部注册工具。
- `allowed` 是空切片：空工具集。
- 白名单包含未注册名称：装配失败。

`Definitions()` 按名称排序并深复制；`Version()` 是定义、replay policy 和已声明 executable version 的确定性摘要；`Subset()` 只能继续收窄，不能引入新工具。`Agent.New` 内部已创建 ToolSet，之后改 registry 不影响现有 Agent。

### 实用例子：严格 schema 和幂等工具

```go
type lookupOrder struct{}

func (lookupOrder) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name: "lookup_order", Description: "按订单号查询订单",
		Strict: true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"order_id": map[string]any{"type": "string", "minLength": 1},
			},
			"required": []string{"order_id"},
		},
	}
}
func (lookupOrder) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (lookupOrder) ExecutableVersion() string        { return "lookup-order/v3" }
func (lookupOrder) Execute(ctx context.Context, in agent.ToolInvocation) (agent.ToolResult, error) {
	// 生产实现应解析 in.RawInput，并以 in.ExecutionKey 对下游写操作去重。
	return agent.ToolResult{Name: in.Name, Content: `{"status":"paid"}`}, nil
}

func buildTools() (*agent.Registry, *agent.ToolSet, error) {
	registry := agent.NewRegistry()
	if err := registry.Register(lookupOrder{}); err != nil {
		return nil, nil, err
	}
	set, err := agent.NewToolSet(registry, []string{"lookup_order"})
	if err != nil {
		return nil, nil, err
	}
	if err := set.ValidateExecutableVersions(); err != nil {
		return nil, nil, err
	}
	return registry, set, nil
}
```

## RuntimeDefinition

`RuntimeDefinition` 是一次完整、不可变、可校验的运行组合，不等于可序列化配置。它持有真实 `Model` 和 `ToolSet`，因此只应存在内存中。

`RuntimeDefinitionSpec` 固定：

- `Key`：定义键。
- `ModelMetadata`：模型名、版本、上下文窗口和 capability。
- `ExecutionSettings`：步数、循环检测、工具修正、生成参数、tool choice、stop conditions。
- `PromptVersion`、`PolicyVersion`：函数或外部内容无法直接序列化时的版本身份。

`NewRuntimeDefinition` 要求 metadata capability 与 `model.Capabilities()` 完全一致，并生成 `ArtifactVersions`：definition、model、tools、prompt、policy。`NewAgent()` 每次返回共享不可变定义快照的无状态 runner。

注意 `StopConditions` 是 Go 函数，不能进入 durable wire manifest；生产恢复应保存其 policy version，并由应用按精确版本重建函数。

### 实用例子：构造可追踪定义

```go
func definition(model agent.Model, tools *agent.ToolSet) (*agent.RuntimeDefinition, error) {
	return agent.NewRuntimeDefinition(agent.RuntimeDefinitionSpec{
		Key: "assistant.support/v4",
		Model: agent.ModelMetadata{
			Name: "provider/model", Version: "model-2026-07-01",
			ContextWindow: 128_000, Capabilities: model.Capabilities(),
		},
		Execution: agent.ExecutionSettings{
			MaxSteps: 8, ToolRepairLimit: 1,
		},
		PromptVersion: "sha256:prompt-digest",
		PolicyVersion: "support-policy/v4",
	}, model, tools)
}

func executeDefinition(ctx context.Context, def *agent.RuntimeDefinition) error {
	runner, err := def.NewAgent()
	if err != nil {
		return err
	}
	versions := def.ArtifactVersions()
	fmt.Printf("definition=%s tools=%s\n", versions.Definition, versions.Tools)
	_, err = runner.Run(ctx, agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("查询订单 A-1")},
	})
	return err
}
```

## Observation

`Observation` 只有文本增量、工具开始、工具结果和步骤结束四类。它刻意没有“运行已完成”事件：最终状态必须来自 `RunResult` 或 durable store。

`ObservationEmitter` 使用有界 channel 和单个消费 worker。runtime 只做非阻塞入队，队列满、锁竞争或 emitter 已关闭时 observation 会丢失；消费回调慢不会阻塞模型、工具或 checkpoint。传给回调的是深复制快照。

普通运行由调用方在不再使用后 `Close()`；durable 运行会在 attempt 退出时关闭 admission，调用方仍可调用 `Close()` 等待已入队项排空。不要在 consumer 中修改权威状态或依赖每个 text delta 都到达。

### 实用例子：把进度写入日志

```go
emitter := agent.NewObservationEmitter(64, func(o agent.Observation) {
	switch o.Type {
	case agent.ObservationTextDelta:
		log.Printf("delta=%q", o.Text)
	case agent.ObservationToolCall:
		log.Printf("tool start=%s", o.ToolCall.Name)
	case agent.ObservationStepFinished:
		log.Printf("step=%d finished", o.Step.StepNumber)
	}
})
defer emitter.Close()

result, err := runner.Run(ctx, agent.RunRequest{
	Messages:           []agent.Message{agent.NewUserMessage("你好")},
	ObservationEmitter: emitter,
})
// 权威判断仍然只看 err 和 result。
_ = result
_ = err
```

## CheckpointStore

根包 `CheckpointStore` 是 `Agent.Run` 的直接恢复边界。它围绕 `RunSnapshot` 保存完整 `Checkpoint`，并把模型调用和每个工具副作用拆成原子状态迁移。

核心值：

- `RunIdentity`：`RunKey`、`AgentKey`、`SessionID`、`RequestID`，创建后不可变。
- `RunSnapshot`：schema version、输入/配置 digest、status、phase、revision、fence、lease 和 checkpoint。
- `MutationGuard`：`RunKey + LeaseOwner + Revision + FenceToken`，每次写必须在同一事务内完整 CAS。
- `Checkpoint`：完整 history、本轮新消息、已完成 steps、usage、修正次数、下一步和待执行工具。
- `ToolExecution`：工具副作用账本，含稳定 `IdempotencyKey` 和 prepared/executing/completed/unknown 状态。

协议顺序是 `Begin -> Acquire -> ModelInflight -> CommitModelResponse -> PrepareTools -> BeginTool -> CommitTool`，最终使用 `Complete`、`Suspend` 或 `Fail`。每次成功变更 revision 加一；每次成功 acquire 增加 fence。store 不能在调用 model 或 tool 时持有数据库事务。

恢复保证有明确上限：

- model 在 `model_inflight` 崩溃后只能重试，因此是 at-least-once。
- 工具在外部效果成功但 `CommitTool` 前崩溃时结果未知。
- 只有声明 `ReplayPolicyIdempotent` 且下游按稳定 execution key 去重，才可安全重放并达到 effectively-once。
- 非幂等模糊效果必须成为 `ErrToolExecutionUnknown`，交给查询/人工处理，不能盲重试。

### 实用例子：启用根包 durable run

```go
func durableRun(ctx context.Context, runner *agent.Agent, store agent.CheckpointStore) (*agent.RunResult, error) {
	return runner.Run(ctx, agent.RunRequest{
		Messages: []agent.Message{agent.NewUserMessage("执行已批准的订单操作")},
		DurableRun: &agent.DurableRunConfig{
			Identity: agent.RunIdentity{
				RunKey: "run_01", AgentKey: runner.Key(),
				SessionID: "session_42", RequestID: "request_99",
			},
			CheckpointStore: store,
			LeaseOwner:      "worker-a",
			LeaseDuration:   30 * time.Second,
			PromptVersion:   "prompt/v7",
			PolicyVersion:   "policy/v3",
			AutoComplete:    false,
		},
	})
}

result, err := durableRun(ctx, runner, checkpointStore)
if err == nil && result.DurableCompletion != nil {
	// 生产 adapter 应在同一个 DB 事务中：
	// 1. 用 Guard 完成 checkpoint；2. 写业务终态；3. 追加 outbox event。
	err = completeDomainTransaction(ctx, result.DurableCompletion)
}
```

如果不需要领域记录与 checkpoint 原子完成，可设置 `AutoComplete: true`。持久化前可用 `MarshalRunSnapshot`，恢复时用严格的 `UnmarshalRunSnapshot`；未知 schema version 会被拒绝而不是猜测兼容。

## MemoryStore

根包 `MemoryStore` 实现的是简单 `Store`，即 `SessionStore + MessageStore`，不是 `CheckpointStore`。它保存 `Session`、累计 `Usage` 和有序 `Message`，适合测试、示例和单进程嵌入；进程退出即丢失，不提供数据库事务、分布式 fence 或 durable run 恢复。

`CreateSession` 拒绝重复 ID；`GetSession`、状态更新、usage 累加、消息追加和列表均并发安全。消息跨边界深复制，包括 image bytes。调用方仍需自行分配唯一的 uint session ID。

### 实用例子：保存一轮普通运行

```go
store := agent.NewMemoryStore()
session := agent.Session{
	ID: 42, UserID: 7, AgentKey: runner.Key(),
	Status: agent.SessionActive,
}
if err := store.CreateSession(ctx, session); err != nil {
	return err
}

input := agent.NewUserMessage("你好")
if err := store.AppendMessages(ctx, session.ID, input); err != nil {
	return err
}
result, err := runner.Run(ctx, agent.RunRequest{Messages: []agent.Message{input}})
if err != nil {
	_ = store.UpdateSessionStatus(ctx, session.ID, agent.SessionFailed)
	return err
}
if err := store.AppendMessages(ctx, session.ID, result.Messages...); err != nil {
	return err
}
if err := store.AddUsage(ctx, session.ID, result.Usage); err != nil {
	return err
}
if result.Outcome == agent.OutcomeCompleted {
	return store.UpdateSessionStatus(ctx, session.ID, agent.SessionFinished)
}
return nil
```

生产会话通常应使用 `session`、`message` 子包定义的 revision/fence 聚合端口，并由应用数据库 adapter 实现，而不是把根包 `MemoryStore` 换成一个没有事务设计的全局 map。
