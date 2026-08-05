# Agent SDK Guides

本目录解释如何把 `agent-runtime-go` 根包及其可选子包封装成应用。配套 reference application 位于 [`../demo/icoder`](../demo/icoder/README.md)。

## Reading Order

1. [Core 与 composition](01-core-and-composition.md)
2. [模型提供商](02-provider.md)
3. [Prompt、Skills 与 Context](03-prompt-skills-context.md)
4. [Tool 与 Permission](04-tools-permission.md)
5. [Message、Session 与 SQLite](05-message-session-sqlite.md)
6. [Event 与 observations](06-events.md)
7. [MCP](07-mcp.md)
8. [Sub-Agent](08-subagent.md)
9. [生产演进](09-production.md)

## Package Selection

| 需求 | 从哪里开始 |
|---|---|
| 单次 model/tool loop | 根包 `agent.Agent` |
| 模型目录和 factory | `agent/provider` |
| 历史归一化和预算 | `agent/context` |
| 不可变 Prompt | `agent/prompt` |
| 非可信 instruction catalog | `agent/skills` |
| 可信本地工具 | 根 `agent.Tool` |
| effect lifecycle 与审批 | `agent/tool` + `agent/permission` |
| 权威消息聚合 | `agent/message` |
| session/branch 聚合 | `agent/session` |
| checkpoint 和恢复 | root durable contract + `agent/durable` |
| 可靠事件/outbox | `agent/event` |
| MCP capability | `agent/mcp` |
| 独立 child run | `agent/subagent` |
| 长生命周期组件 host | `agent/app` |

最小正确组合通常优于一次引入所有子包。子包提供独立机制和 consumer ports，不是一个会自动连接数据库、权限和模型的 service locator。
