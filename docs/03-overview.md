# 总览：核心概念与包地图

## 核心概念

### Agent 与 RuntimeDefinition

`Agent` 是无会话状态的执行器，持有 `Config`、`Model`、不可变 `ToolSet` 快照、循环检测器和停止条件。最直接的构造方式是 `agent.New`。

`RuntimeDefinition` 是更严格的不可变组合产物：它把 model 元数据、执行设置、prompt/policy 版本和工具快照一起校验并计算 artifact digest，再通过 `NewAgent` 创建执行器。需要精确标识、重建或 durable artifact 时优先使用它；简单嵌入可以直接使用 `agent.New`。

### Model

`Model` 是根包唯一需要的模型能力：

```go
	Name() string
	Capabilities() Capabilities
	Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}
```

可选的 `Generator` 表示非流式生成能力，但运行时不会探测或依赖它。真实 provider 适配器负责消息映射、参数分片聚合、错误分类和 usage 归一化。

### Message 与 Step

一条 `Message` 包含 role 和内容块。一次模型响应及紧随其后的工具执行组成一个 `StepResult`。`RunResult.Messages` 只包含本轮新产生的 assistant/tool 消息，而 `RunResult.Steps` 保留每一步的消息、工具调用、结果、usage 和 finish reason。

### Registry、ToolSet 与 Tool

`Registry` 是并发安全的可变注册表；`Agent` 构造时从中创建不可变 `ToolSet` 快照。因此，在构造 Agent 后修改 Registry，不会改变该 Agent 的工具集合。

`AllowedTools` 的含义需要区分：

| 值 | 含义 |
|---|---|
| `nil` | 使用注册表中的全部工具 |
| `[]string{}` | 不允许任何工具 |
| `[]string{"x"}` | 只允许列出的工具，缺失项在装配期报错 |

`Tool.Definition` 提供 JSON Schema；`ReplayPolicy` 描述 durable 恢复中的重放策略；`Execute` 接收原始 JSON 和可选稳定 execution key。

### 普通运行与 durable 运行

普通运行仅在内存中建立 history，适合请求级执行或由应用自己管理事务。durable 运行设置 `RunRequest.DurableRun`，把阶段、检查点和工具执行记录写入 `CheckpointStore`，用于租约接管和恢复。

二者共享消息、流协议、工具校验、停止条件和结果类型，但 durable 路径还要求 identity、lease owner、lease duration 和 store，并返回可供领域事务最终提交的 guard/checkpoint。

### Observation 与可靠事件

根包 `ObservationEmitter` 提供文本增量、工具开始、工具结果和步骤完成信号。它是非阻塞、有界、best-effort 的，不表示终态。

`event` 子包提供持久事件、outbox、投递和 replay 契约。需要可靠终态通知时，应在保存权威业务状态的事务内写 event/outbox，而不是依赖 Observation。

## 包地图

| 包 | 主要职责 | 何时引入 |
|---|---|---|
| 根包 `agent` | 消息、模型/工具端口、普通与 checkpointed loop、运行定义 | 所有执行场景 |
| `agenttest` | 可编排的测试 model/tool | 单元测试与协议测试 |
| `provider` | provider/model catalog、选择与 factory 契约 | 多 provider、多模型角色或定价元数据 |
| `tool` | 独立的 capability registry/executor 契约 | 需要租户作用域、权限和观察链的工具层 |
| `permission` | 权限决策、请求和内存存储 | 工具调用需用户/策略授权 |
| `message` | 带 revision、幂等 mutation 的消息存储契约 | 持久会话历史 |
| `session` | 会话 aggregate、分支、merge 和 usage 引用 | 多轮会话、分支执行 |
| `context` | 消息规范化、预算、不可变 context plan/artifact | 长上下文、总结和可复现上下文投影 |
| `prompt` | prompt artifact 与渲染相关类型 | prompt 需要版本化与组合 |
| `skills` | skill catalog、文件系统导入和快照 | 运行时加载技能资产 |
| `mcp` | MCP 配置、连接、tool 映射与 live generation | 接入 stdio 或 streamable HTTP MCP server |
| `coordinator` | selector 到不可变 `RuntimeDefinition` 的解析与 manifest | 多租户、版本化运行定义、精确重建 |
| `durable` | durable store、lease/fence、effect/usage ledger、reconcile | 多 worker 接管、恢复、审计副作用 |
| `event` | 持久事件、outbox、dispatcher 和 replay | 可靠发布与断点重放 |
| `subagent` | 独立 durable child run、预算、唤醒与取消 | 父子 Agent 编排 |
| `app` | 组件 readiness 与分阶段 shutdown | 生产进程生命周期管理 |

根包有一条源码测试保护的依赖规则：根包不得导入任何子包，生产包也不得依赖 `agenttest`。子包可以依赖根包的稳定值和端口。

## 选择指南

| 需求 | 最小选择 |
|---|---|
| 一次请求，模型直接回答 | 根包 + `Model` |
| 模型调用本地业务函数 | 根包 + `Model` + 根包 `Tool`/`Registry` |
| 保存简单会话 | 根包 + 应用存储，或 `message`/`session` |
| 接入 OpenAI/Anthropic 等 | 实现根包 `Model`；模型目录需求再加 `provider` |
| 动态控制每一步模型、采样和活跃工具 | `RunRequest.StepPolicy` |
| 工具需授权 | 在工具适配器外组合 `permission`，不要把白名单当授权 |
| 只显示实时 token | `ObservationEmitter` |
| 可靠投递终态 | 权威事务 + `event`/outbox |
| 崩溃后恢复同一 run | `DurableRunConfig` + `CheckpointStore` |
| 多 worker 与副作用审计 | `durable` store/ledger + 工具侧幂等或解析能力 |
| 可复现地解析模型、prompt、工具版本 | `RuntimeDefinition` + `coordinator` |
| 长会话上下文规划 | `message` + `session` + `context` |
| MCP 或子 Agent | 分别使用 `mcp` 或 `subagent`，再适配为应用能力 |

## 常见边界误判

- `MaxSteps` 是工具循环步数上限，不是 provider 内部重试次数。
- `FinishLength` 返回 `OutcomeSuspended` 和 `StopReasonOutputLimit`，不是完整答案。
- 工具返回 `ToolResult{IsError: true}` 是给模型看的可处理结果；`Execute` 返回非 nil Go error 会中断本次运行。
- 工具不存在或不在当前 ToolSet 时会形成 model-visible error result；参数 schema 失败还会消耗 repair budget。
- `ContextBudgetExceeded` 只判断最新模型请求的输入占用，不会自动总结或裁剪历史。
- `RunResult.Text` 是最后一步 assistant 文本，不是所有消息的拼接。

继续阅读：[架构：端口、适配器与组合](04-architecture.md)。
