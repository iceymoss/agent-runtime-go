# 子包指南

根包始终是执行内核。子包提供独立机制，不会自动连接成一个全局框架。

## 快速选择

| 包 | 解决的问题 | 暂时不需要它的情况 |
|---|---|---|
| root `agent` | 单次 model/tool loop | 始终需要 |
| `provider` | 多模型目录、价格和 factory | 只有一个已构造 Model |
| `prompt` | 确定性模板与版本 | Prompt 是一个固定字符串 |
| `context` | 历史修复、预算与 compaction | 历史很短且无需预算 |
| `message` | 权威消息聚合与 revision | 应用自己原子保存简单 turn |
| `session` | Session/branch 与 worker host | 无状态 API 或单进程简单会话 |
| `durable` | Checkpoint、lease、fence、恢复 | Run 不需要跨进程恢复 |
| `permission` | allow/deny/ask 与 grant | 工具无敏感 effect |
| `tool` | 高级 effect 生命周期 | 根 `agent.Tool` 足够 |
| `event` | 可靠事件、outbox、replay | 只需要实时 UI progress |
| `skills` | 不可信 instruction catalog | 没有动态 skills |
| `mcp` | MCP discovery、generation 和调用 | 只有本地工具 |
| `coordinator` | capability 到 definition 的原子发布 | Definition 在启动时固定 |
| `subagent` | Durable child run 与预算 | 不委派独立任务 |
| `app` | readiness、admission、shutdown | 调用方已有 lifecycle host |
| `agenttest` | adapter conformance | 仅在测试代码使用 |

## 状态相关包

`message` 拥有消息事实，`session` 拥有会话和 branch 状态，`durable` 拥有执行 checkpoint。三者不能用同一张“万能 Agent 表”替代：

```text
message: what was committed
session: which conversation/branch is authoritative
durable: where an execution can safely resume
```

简单应用可以在自己的 transaction 中保存 user + result messages，不必立即实现三个 adapter。

## 能力相关包

`provider`、`prompt`、`skills` 和 `mcp` 产生带 generation/version 的能力。`coordinator` 将这些能力解析成不可变 `agent.RuntimeDefinition`：

```text
provider snapshot
prompt version
skills generation
MCP generation
tool set
      |
      v
RuntimeDefinition
```

正在运行的请求固定使用一个 definition，不应在中途看到部分刷新。

## 安全相关包

- 根 `agent.Tool` 定义模型可调用的能力。
- `permission` 决定某个 subject 能否执行具体 action。
- `tool` 管理 interceptor、ledger、fence 和模糊 effect 恢复。
- OS sandbox、网络 allowlist 和 credential 管理仍属于应用基础设施。

Prompt、Skills、MCP 描述和 Tool description 都不是权限边界。

## 生命周期相关包

`app` 只编排组件 startup、readiness、admission 和 shutdown，不拥有 provider、session 或 durable 的领域状态。`subagent` 创建独立 child run，而不是递归共享 parent 的可变内存。

## 推荐组合

```text
Simple API:
  agent

Tool-using business Agent:
  agent + prompt + application tools

Stateful application:
  agent + context + application turn store

Distributed resumable host:
  agent + provider + coordinator + message + session
        + durable + permission + event + app
```

最后一组不是默认模板。只有 detached worker、并发 claim、审批恢复等需求成立时才值得引入。
