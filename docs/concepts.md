# 核心概念

一页讲清根包的核心类型和执行语义。读完它，其他文档都是查阅性质的。

## 全景

```text
RunRequest.Messages（完整输入历史）
        │
        ▼
Agent.Run ──构造──▶ GenerateRequest ──▶ Model.Stream（你的模型适配器）
        ▲                                    │
        │                              StreamChunk 流
        │                                    ▼
        │                          聚合并校验为一步 Response
        │                                    │
        │          ┌── 最终文本 ────────────▶ RunResult
        │          │
        │          └── tool calls
        │                 │ 白名单 + JSON Schema 校验
        │                 ▼
        └───────── Tool.Execute（你的业务函数）
                   结果包装为 tool 消息，追加历史，进入下一步
```

一次 `Run` 内部循环执行"模型 → 工具 → 模型"，直到模型给出最终文本、命中停止条件或出错。每次模型调用及其后的工具执行称为一个 **step**。

## Message：对话的通用表示

`Message` = `Role` + 有序的 `Parts` + 可选 `FinishReason`。

- 四种 role：`system`、`user`、`assistant`、`tool`；
- 四种 part：文本、tool call（仅 assistant）、tool result（仅 tool）、图片（仅 user）；
- 构造用辅助函数：`NewSystemMessage` / `NewUserMessage` / `NewAssistantMessage` / `NewToolMessage`；
- `ValidateMessage` 强制结构约束（tool call 必须有 ID 和 Name、tool result 必须配对 `ToolCallID` 等），非法消息在进入循环前就被拒绝。

## Model：唯一的模型端口

```go
type Model interface {
	Name() string
	Capabilities() Capabilities
	Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}
```

`Stream` 返回的 channel 上流动四种 chunk：

| Chunk | 含义 |
|---|---|
| `ChunkText` | 文本增量 |
| `ChunkToolCall` | 一个**已拼装完整**的工具调用（不允许半截 JSON） |
| `ChunkFinish` | 唯一终态，携带完整 `Response`；之前所有增量必须与它一致 |
| `ChunkError` | 携带错误后关闭 channel |

运行时会校验协议一致性：缺终态、终态后继续发 chunk、文本拼接与终态不符，都是协议错误。`Capabilities` 声明模型支持什么（工具、tool choice 模式、图片），装配期与配置比对，不符直接拒绝——绝不静默降级。

自己写适配器前先看 [providers/openaicompat](packages/openaicompat.md) 是否已覆盖；协议细节见[运行循环内部机制](internals.md)。

## Tool：模型可调用的业务函数

```go
type Tool interface {
	Definition() ToolDefinition   // 名称、描述、JSON Schema
	ReplayPolicy() ReplayPolicy   // 恢复时能否重放：never / idempotent / resolve
	Execute(context.Context, ToolInvocation) (ToolResult, error)
}
```

多数场景不用手写 Schema，`agent.NewTool` 从函数签名的输入结构体反射生成（见[根包文档](packages/agent.md)）。

**工具错误的两种语义**（最容易混淆的设计点）：

| 返回 | 语义 | 循环行为 |
|---|---|---|
| `ToolResult{IsError: true}` | 模型可见、可修正（"订单不存在"） | 继续，模型看到错误自行调整 |
| 非 nil Go error | 基础设施故障（数据库挂了） | 立即中止本次运行 |

`Registry` 是可变注册表；`Agent.New` 时会做一份不可变 `ToolSet` 快照，之后改 Registry 不影响已装配的 Agent。`Config.AllowedTools` 的三种取值：`nil` = 全部注册工具；`[]string{}` = 禁用工具；列名单 = 只允许这些。

## 停止语义：Outcome 与 StopReason

`Run` 结束后联合判断三个值：

| `err` | `Outcome` | 含义 |
|---|---|---|
| nil | `OutcomeCompleted` | 正常完成（模型给出最终文本，或工具主动 `StopTurn`） |
| nil | `OutcomeSuspended` | 被中断但可续跑：`StopReason` 说明原因（`max_steps` / `output_limit` / `context_budget` / 自定义条件） |
| 非 nil | `OutcomeFailed` | 失败：模型错误、工具基础设施错误、循环检测（`ErrLoopDetected`）等 |

`OutcomeSuspended` 是刻意设计的中间态：上层拿着已产生的消息决定"继续跑一轮"还是"就此打住"，运行时不替你做这个决策。

## 错误分类

- **模型侧**：适配器统一包装为 `ModelError`，带 `Kind`（auth / rate_limit / transport / rejected / protocol）和 `Retryable` 标记；
- **工具参数非法**：回灌模型修正，最多 `ToolRepairLimit` 次（默认 1）；
- **配置错误**：`agent.New` 装配期返回 `ErrAgentConfigInvalid`，不会进入运行期；
- 哨兵错误（`ErrLoopDetected`、`ErrToolNotAllowed` 等）都可用 `errors.Is` 判断。

## 两种运行模式

- **普通运行**（默认）：全程在内存，进程挂了这轮就没了。适合请求级调用。
- **durable 运行**：设置 `RunRequest.DurableRun` 后，每一步写入 `CheckpointStore`，崩溃后可被其他 worker 按租约接管续跑。需要它时读 [durable](packages/durable.md) 和[运行循环内部机制](internals.md)。

## Observation ≠ 事件

`ObservationEmitter` 是给 UI 看的实时进度（文本增量、工具开始/结束），**有界、非阻塞、可丢失**。它刻意没有"运行完成"事件——权威终态只来自 `RunResult` 和 durable store。需要可靠投递（计费、审计、通知下游）用 [event 子包](packages/event.md)的 outbox 模式。

## 各子包是什么关系

根包不导入任何子包（有测试强制保证）；子包都依赖根包的类型和端口；你的应用在组装层把它们拼起来。所以：**从根包开始，感到疼了再引入对应子包**。每个子包一篇文档，见[文档首页](/)的子包索引。
