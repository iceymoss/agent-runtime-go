# 介绍

Agent Runtime for Go 是一个可组合、与模型供应商无关的 Go Agent runtime。

它只负责一件事：**可靠地执行 model/tool loop**——多步循环、JSON Schema 校验、工具白名单、停止条件、错误分类、崩溃恢复。模型适配、Prompt、工具实现、权限策略、存储和业务 API 属于你的应用。

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

> 当前版本处于 `v0.x` 阶段，API 仍可能调整，建议固定版本使用。

## 为什么需要它

直接裸调模型 API 写 Agent，很快会遇到一批和业务无关、却必须做对的问题：

- 模型要求调工具时，参数校验、执行、把结果回灌给模型的循环怎么写对？
- 模型输出不合法、工具执行失败、上下文超预算时，哪些该重试、哪些该终止？
- 进程崩溃后，跑到一半的运行怎么恢复，副作用如何不重复执行？
- 换一家模型供应商，这些代码要重写多少？

这些问题的答案彼此耦合，散落在业务代码里就会各处实现不一致。Agent Runtime 把它们收敛为一个经过测试约束的 runtime，你的代码只面对稳定的 `Model` / `Tool` 契约。

## 设计哲学

**渐进式采用。** 根包 `agent` 自身就是完整可用的：一个 `Model`、一个工具注册表、一次 `Run`。Session 持久化、崩溃恢复、权限审批、MCP、Sub-Agent 都是独立子包，需要哪个再引入哪个，不用为一次简单调用背上整套架构。

**不替应用做决定。** 根包不读环境变量、不选择 credential、不连接数据库、不隐式注册全局工具。所有策略在你的 composition root 中显式组装，因此每一层都可以单独测试和替换。

**供应商无关。** 唯一的模型端口是三个方法的 `Model` 接口。OpenAI 兼容的 API（OpenAI / DeepSeek / Qwen / Kimi / vLLM / Ollama 等）有官方适配器 `providers/openaicompat` 直接可用；其他协议自行适配同一接口。

## 能做什么

- 统一的消息、图片、模型、流式响应和 usage 契约
- 多步 model/tool loop、JSON Schema 校验和工具白名单
- 最大步数、停止条件、上下文预算和工具循环检测
- 可选的 Session、Durable、Permission、Event、MCP、Skills 与 Sub-Agent
- 不可变 runtime definition 和可复现 artifact digest
- 可测试的 provider/tool ports，不绑定具体供应商或数据库

## 安装

```bash
go get github.com/iceymoss/agent-runtime-go
```

要求 Go `1.25.0` 及以上。仓库自带一个无需 API Key 的确定性示例，可以立即验证安装：

```bash
go run ./examples/hello
```

运行输出：

```text
Hello from Agent Runtime for Go.
```

## 如何阅读这份文档

按你所处的阶段选择路径：

- **第一次接触**：[快速开始](quickstart.md)（10 分钟跑通一个会调工具的真实模型 Agent）→ [核心概念](concepts.md)（建立 Message / Model / Tool / 停止语义的心智模型）。
- **开始写自己的 Agent**：先读 [agent](packages/agent.md) 和 [providers/openaicompat](packages/openaicompat.md)，这两篇覆盖了大多数应用的全部需求；其余子包在需要时按侧边栏分组查阅，每篇结构统一：是什么 → 为什么需要它 → 怎么用 → 常见问题。
- **准备上生产**：[生产组合模式](production.md) 讲 provider、权限、状态、事件如何组合成服务；[运行循环内部机制](internals.md) 用于排查 stream、工具执行、停止与恢复问题。
- **看完整参考实现**：[iCoder 端到端教程](icoder.md) 展示一个真实 Code Agent 如何组合所有子包；[速查表](reference.md) 汇总枚举、错误分类与验证命令。

## 阅读约定

- "根包"指模块根目录的 `package agent`；"普通运行"指 `RunRequest.DurableRun == nil` 的内存内执行。
- `Observation` 是可丢弃的进度信号；`RunResult` 和 durable store 才是终态依据。
- 文档以源码公开接口和测试约束为准。
