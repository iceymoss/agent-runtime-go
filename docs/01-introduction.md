# 简介：定位、能力与边界

## 定位

Agent Runtime for Go 是一个 provider-neutral、存储无关、可嵌入的 Agent 运行时。根包接受应用提供的 `Model` 和 `Tool` 实现，负责在一轮 `Run` 内维护消息历史，驱动“模型生成 → 工具执行 → 结果回灌 → 再次生成”的多步循环。

`Agent` 自身不保存会话状态。一个装配完成的 `Agent` 可以复用于不同请求和会话，因为每次运行都从 `RunRequest.Messages` 建立独立历史，且会复制输入而不修改调用方切片。需要会话、事件、上下文规划或持久恢复时，仓库提供独立子包和端口，但应用仍是最终组合者。

## 核心能力

- **统一消息模型**：`Message` 由有类型的 `ContentPart` 构成，支持文本、tool call、tool result 和用户图片输入。
- **流式模型端口**：`Model.Stream` 以 `StreamChunk` 输出文本增量、完整工具调用、错误和唯一终止响应。
- **多步工具循环**：运行时按顺序执行一个模型响应中的工具调用，把一批 `ToolResult` 组成 tool 消息后回灌模型。
- **装配期约束**：模型能力、工具白名单、工具 schema、`ToolChoice`、最大步数和循环检测配置尽早校验。
- **运行期防护**：校验模型终止响应和流聚合一致性，限制工具参数修复次数，检测重复工具调用，并检查上下文预算。
- **明确结果语义**：`Outcome` 区分 `completed`、`suspended` 和 `failed`；`StopReason` 说明正常完成、输出截断、最大步数、上下文预算等原因。
- **可选 durable 执行**：通过根包的 `CheckpointStore` 端口保存模型与工具边界，使用 lease、revision 和 fence token 拒绝陈旧写入。
- **可选生态组件**：仓库另有 session、message、context、event、provider、coordinator、permission、skills、MCP、subagent 和应用生命周期等包。

## 非目标

以下职责没有被根运行时隐式接管：

- **不是模型 SDK**：仓库定义 `Model` 协议，不内置某个云模型的 HTTP 客户端。provider adapter 需要由应用或外部包实现。
- **不是自治 prompt 框架**：根包不会创建 system prompt、检索知识、总结历史或自动压缩上下文；调用方传入最终消息。
- **不是会话数据库**：普通 `Run` 不读取或落库历史。应用可以使用仓库的存储契约，也可以使用自己的数据库模型。
- **不是权限策略引擎的替代品**：工具白名单只控制本次模型可见和可执行的工具，不等同于用户授权、租户隔离或业务审计。
- **不承诺工具 exactly-once**：在外部副作用成功而结果尚未提交时崩溃，运行时无法凭空判断副作用是否发生。有效一次语义需要工具侧按稳定 execution key 去重或查询。
- **不把 Observation 当可靠事件**：观察发射器使用有界、非阻塞队列；队列满时允许丢弃，且不包含权威终态。
- **不隐藏 provider 差异**：`Capabilities` 要求适配器显式声明能力；不支持的图片或 tool choice 应失败，而不是静默降级。
- **不负责进程级编排**：根包不启动 worker、不管理网络服务，也不决定数据库事务边界。

## 适用场景

适合：需要在 Go 服务中嵌入可测试的 model/tool loop，希望 provider 和存储可替换，或需要逐步演进到检查点恢复的应用。

不应只靠根包解决：完整聊天产品、跨服务任务队列、计费权威账本、可靠事件投递、凭据管理和生产级 provider 重试。仓库中的可选包提供部分契约与内存实现，但生产适配器和最终一致性仍属于应用。

## 最小心智模型

一次普通运行只有四个必要输入：

1. 一个实现 `agent.Model` 的模型适配器。
2. 一个 `agent.Registry`，可以为空。
3. 一个有效的 `agent.Config`，其中 `MaxSteps > 0`。
4. 至少一条合法的 `RunRequest.Messages`。

`Run` 返回本轮新增的 assistant/tool 消息、每步详情、累计 usage、最终文本、停止原因和 outcome。调用方决定如何展示、持久化或继续处理这些值。

下一章：[快速开始：最小可运行 Agent](02-quick-start.md)。
