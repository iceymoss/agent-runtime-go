# 文档导航

这些文档从一次无状态运行开始，逐步扩展到可恢复的 Agent host。第一次使用建议按顺序阅读前三篇，其余按需求选择。

## 入门

1. [Core 运行原理](01-core-runtime.md)：一次 Run 内部发生什么。
2. [封装自己的 Agent](02-build-your-agent.md)：怎样划分 Model、Tool、facade 和应用边界。
3. [子包指南](03-packages.md)：每个子包解决什么问题，什么时候引入。

## 专题

4. [Provider adapter](04-provider-adapter.md)
5. [Tools 与 Permission](05-tools-permission.md)
6. [Message、Session 与 Durable](06-state-and-durable.md)
7. [Event 与 Observations](07-events-observations.md)
8. [Prompt、Context 与 Skills](08-prompt-context-skills.md)
9. [MCP 与 Sub-Agent](09-mcp-subagent.md)
10. [生产集成检查表](10-production.md)

## 学习路径

| 目标 | 阅读与示例 |
|---|---|
| 跑通最小 Agent | README + `examples/hello` |
| 理解工具循环 | `examples/tool-agent` + 01 + 05 |
| 封装业务 Agent | 02 + 03 |
| 接入真实模型 | 04 |
| 保存多轮会话 | 06 |
| 支持中断和恢复 | 06 + 07 + 10 |
| 接入 MCP 或 child Agent | 09 |
| 阅读完整应用 | `demo/icoder` |

原则是从最小组合开始。普通请求不需要 Session host，单进程应用不需要伪装成分布式 worker，可靠事件也不能用 Observation 代替。
