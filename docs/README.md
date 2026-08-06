# Agent Runtime for Go 文档

根包导入路径 `github.com/iceymoss/agent-runtime-go`，包名 `agent`。文档以源码公开接口和测试约束为准。

## 入门

| 文档 | 内容 |
|---|---|
| [快速开始](quickstart.md) | 10 分钟跑通一个会调工具的真实模型 Agent |
| [核心概念](concepts.md) | 一页讲清 Message / Model / Tool / 停止语义 / 错误分类 |

## 子包指南

每个包一篇独立文档，统一结构：是什么 → 为什么需要它 → 怎么用 → 常见问题。**从根包开始，需要时再引入子包。**

### 核心

| 包 | 一句话 |
|---|---|
| [agent（根包）](packages/agent.md) | model/tool 循环执行器，一切的起点 |
| [providers/openaicompat](packages/openaicompat.md) | 官方适配器：OpenAI / DeepSeek / Qwen / Ollama 等直接接入 |

### 模型与提示

| 包 | 一句话 |
|---|---|
| [provider](packages/provider.md) | 多模型目录、选择与工厂 |
| [prompt](packages/prompt.md) | Prompt 版本化与组合 |
| [context](packages/context.md) | 历史归一化、token 预算、可复现的上下文投影 |

### 会话与持久化

| 包 | 一句话 |
|---|---|
| [session](packages/session.md) | 会话聚合、分支、认领与续跑 |
| [message](packages/message.md) | 带 revision CAS 的消息存储契约 |
| [durable](packages/durable.md) | checkpoint、租约、fence：崩溃恢复与多 worker 接管 |

### 工具与安全

| 包 | 一句话 |
|---|---|
| [tool](packages/tool.md) | 高级工具生命周期与副作用账本 |
| [permission](packages/permission.md) | allow / deny / ask 权限决策 |
| [skills](packages/skills.md) | 不受信任的指令目录：加载与快照 |
| [mcp](packages/mcp.md) | MCP server 发现与工具调用 |

### 编排与运维

| 包 | 一句话 |
|---|---|
| [subagent](packages/subagent.md) | 独立预算的子 Agent 运行 |
| [coordinator](packages/coordinator.md) | 不可变运行定义的解析与精确重建 |
| [event](packages/event.md) | 可靠事件、outbox 与重放 |
| [app](packages/app.md) | 就绪探针与有界停机 |
| [agenttest](packages/agenttest.md) | 适配器一致性测试（test-only） |

## 进阶

| 文档 | 什么时候读 |
|---|---|
| [运行循环内部机制](internals.md) | 排查 stream 协议、工具执行、停止和恢复问题 |
| [生产组合模式](production.md) | 把 provider、权限、状态、事件组合成生产服务 |
| [iCoder 端到端教程](icoder.md) | 看一个完整 Code Agent 如何组合所有子包 |
| [速查表](reference.md) | 枚举、错误分类、验证命令 |

## 文档站点

```bash
npm install --prefix docs-site && npm --prefix docs-site run dev   # 终端 1
go run ./cmd/docs --dev-url http://127.0.0.1:5173                  # 终端 2
```

访问 `http://127.0.0.1:8080`。生产构建：`npm --prefix docs-site run build && go build -tags docsprod ./cmd/docs`。

## 阅读约定

- "根包"指模块根目录的 `package agent`；"普通运行"指 `RunRequest.DurableRun == nil` 的内存内执行。
- `Observation` 是可丢弃的进度信号；`RunResult` 和 durable store 才是终态依据。
- 仓库要求 Go `1.25.0`。
