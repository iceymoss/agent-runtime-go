# 构建你的 Agent

这份文档带你从一次最小调用走到一个能上生产的 agent。按顺序读，每章结束时手上的程序都能跑。

读完你会知道：怎么让模型调用你的 Go 函数、怎么接任意 OpenAI 兼容的模型、多轮对话的历史怎么接、流式输出怎么给用户看、危险操作怎么要求人工审批、进程崩溃后运行怎么恢复。

需要 Go `1.25.0`。第 1、2 章不需要 API key。

## 最短路径

赶时间就只读前三章。读完你有一个接真实模型、会调工具的 agent，这已经覆盖大多数应用。

## 章节

| # | 章节 | 解决的问题 |
|---|---|---|
| 1 | [从一次调用开始](./01-first-run.md) | 模型和工具之间的循环是什么，为什么不该自己写 |
| 2 | [让模型调用你的代码](./02-tools.md) | 工具怎么定义，两种失败怎么分 |
| 3 | [接上真实模型](./03-real-model.md) | 换供应商、重试、多模型、Prompt 版本 |
| 4 | [多轮对话与上下文](./04-conversation.md) | 历史怎么接，上下文装不下了怎么办 |
| 5 | [把过程给用户看](./05-streaming.md) | 流式输出，以及默认为什么会丢字 |
| 6 | [让输出能被代码消费](./06-structured-output.md) | 结构化输出、推理模型 |
| 7 | [危险操作要问人](./07-permission.md) | 权限、审批、副作用台账 |
| 8 | [扩展工具来源](./08-tool-sources.md) | 远端工具、非可信指令 |
| 9 | [崩溃了还能接着跑](./09-durability.md) | checkpoint、消息与会话持久化 |
| 10 | [多 Agent 与可复现](./10-orchestration.md) | 子 Agent 委派、不可变组合 |
| 11 | [上生产](./11-production.md) | 事件投递、就绪与优雅关闭 |
| 12 | [测试你写的适配器](./12-testing.md) | 一致性套件 |

## 找某个子包

每个子包都有自己的参考文档（是什么 → 为什么 → 怎么用 → FAQ）。这张表告诉你它在教程里出现在哪一章：

| 子包 | 章节 | 参考文档 |
|---|---|---|
| 根包 `agent` | 1、2、5、6 | [agent](../packages/agent.md) |
| `providers/openaicompat` | 3 | [openaicompat](../packages/openaicompat.md) |
| `providers/retry` | 3 | [retry](../packages/retry.md) |
| `provider` | 3 | [provider](../packages/provider.md) |
| `prompt` | 3 | [prompt](../packages/prompt.md) |
| `context` | 4 | [context](../packages/context.md) |
| `permission` | 7 | [permission](../packages/permission.md) |
| `tool` | 7 | [tool](../packages/tool.md) |
| `mcp` | 8 | [mcp](../packages/mcp.md) |
| `skills` | 8 | [skills](../packages/skills.md) |
| `durable` | 9 | [durable](../packages/durable.md) |
| `message` | 9 | [message](../packages/message.md) |
| `session` | 9 | [session](../packages/session.md) |
| `subagent` | 10 | [subagent](../packages/subagent.md) |
| `coordinator` | 10 | [coordinator](../packages/coordinator.md) |
| `event` | 11 | [event](../packages/event.md) |
| `app` | 11 | [app](../packages/app.md) |
| `agenttest` | 12 | [agenttest](../packages/agenttest.md) |

想先建立整体心智模型，看[核心概念](../concepts.md)。想看一个真实应用怎么把这些组合起来，看 [iCoder 教程](../icoder.md)。
