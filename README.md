# Agent Runtime for Go

一个可组合、与模型供应商无关的 Go Agent runtime。

它负责可靠地执行 model/tool loop；你的应用负责模型适配、Prompt、工具、权限、存储和业务 API。可以只使用根包完成一次运行，也可以按需增加 Session、Durable、MCP、Skills 和 Sub-Agent。

> 当前版本处于 `v0.x` 阶段，API 仍可能调整。建议固定版本使用。

## 能做什么

- 统一的消息、图片、模型、流式响应和 usage 契约
- 多步 model/tool loop、JSON Schema 校验和工具白名单
- 最大步数、停止条件、上下文预算和工具循环检测
- 可选的 Session、Durable、Permission、Event、MCP、Skills 与 Sub-Agent
- 不可变 runtime definition 和可复现 artifact digest
- 可测试的 provider/tool ports，不绑定 OpenAI、Anthropic 或具体数据库

## 安装

```bash
go get github.com/iceymoss/agent-runtime-go
```

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

## 两分钟运行

仓库提供一个不需要 API Key 的确定性示例：

```bash
go run ./examples/hello
```

输出：

```text
Hello from Agent Runtime for Go.
```

最小调用只需要三步：

```go
runner, err := agent.New(agent.Config{
	Key:          "example.hello",
	ModelName:    "my-model",
	MaxSteps:     4,
	AllowedTools: []string{},
}, model, agent.NewRegistry())
if err != nil {
	return err
}

result, err := runner.Run(ctx, agent.RunRequest{Messages: []agent.Message{
	agent.NewSystemMessage("Answer clearly."),
	agent.NewUserMessage("Say hello."),
}})
if err != nil {
	return err
}
fmt.Println(result.Text)
```

其中 `model` 实现根包的唯一模型端口：

```go
type Model interface {
	Name() string
	Capabilities() agent.Capabilities
	Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error)
}
```

供应商 adapter 将 `GenerateRequest` 转为上游协议，再把上游响应转换成 canonical `StreamChunk`。完整最小实现见 [`examples/hello`](examples/hello/main.go)。

## 接入真实模型

任何 OpenAI 兼容的 API（OpenAI、DeepSeek、Qwen、Kimi、vLLM、Ollama 等）可以直接使用官方适配器 `providers/openaicompat`，无需自己实现 `Model`：

```go
import "github.com/iceymoss/agent-runtime-go/providers/openaicompat"

model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

runner, err := agent.New(agent.Config{
	Key:       "example.assistant",
	ModelName: "deepseek-chat",
	MaxSteps:  8,
}, model, agent.NewRegistry())
```

适配器默认走 SSE 流式，自动拼装工具调用分片、归一化 usage（含 cache tokens），并把失败分类为 `agent.ModelError`。可选项：

- `openaicompat.WithoutStreaming()`：SSE 不可用的供应商改走非流式请求
- `openaicompat.WithCapabilities(...)`：声明与默认值不同的能力
- `openaicompat.WithHeader(k, v)`：附加自定义请求头（如 OpenRouter 归因头）
- `openaicompat.WithHTTPClient(...)`：自定义超时、代理与传输层

其他协议（如 Anthropic Messages API）仍按 `Model` 接口自行适配。

## Model/Tool Loop

运行带工具的完整示例：

```bash
go run ./examples/tool-agent
```

执行过程：

```text
user message
  -> model requests get_weather
  -> runtime validates JSON Schema
  -> runtime executes get_weather
  -> tool result is appended to messages
  -> model returns the final answer
```

定义工具最简单的方式是泛型 helper `agent.NewTool`，JSON Schema 直接从结构体生成：

```go
type WeatherInput struct {
	City string `json:"city" description:"City name"`
	Days int    `json:"days,omitempty" description:"Forecast days"`
}

tool := agent.MustNewTool("get_weather", "Get the current weather for a city.",
	func(ctx context.Context, input WeatherInput) (agent.ToolResult, error) {
		return agent.ToolResult{Content: lookupWeather(input.City)}, nil
	})

registry := agent.NewRegistry()
_ = registry.Register(tool)
```

非指针且未标 `omitempty` 的字段自动进入 `required`；schema 默认 strict（拒绝未声明字段并回灌给模型修正）。需要完全控制 schema 时，实现 `Tool` 接口即可：

```go
type Tool interface {
	Definition() agent.ToolDefinition
	ReplayPolicy() agent.ReplayPolicy
	Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error)
}
```

完整代码见 [`examples/tool-agent`](examples/tool-agent/main.go)。示例使用确定性模型，便于直接观察两步循环；接入真实模型时只替换 `agent.Model` 实现。

## 封装自己的 Agent

不要让 HTTP、CLI 或业务 Service 到处组装 Prompt 和工具。推荐提供一个面向业务用途的 facade：

```go
type SupportAgent struct {
	runner *agent.Agent
}

type SupportAgentConfig struct {
	Model agent.Model
	CRM   CRM
}

func NewSupportAgent(cfg SupportAgentConfig) (*SupportAgent, error) {
	registry := agent.NewRegistry()
	if err := registry.Register(newFindOrderTool(cfg.CRM)); err != nil {
		return nil, err
	}

	runner, err := agent.New(agent.Config{
		Key:          "support.assistant",
		ModelName:    cfg.Model.Name(),
		MaxSteps:     8,
		AllowedTools: []string{"find_order"},
	}, cfg.Model, registry)
	if err != nil {
		return nil, err
	}
	return &SupportAgent{runner: runner}, nil
}

func (a *SupportAgent) Reply(ctx context.Context, input string, history []agent.Message) (*agent.RunResult, error) {
	messages := make([]agent.Message, 0, len(history)+2)
	messages = append(messages, agent.NewSystemMessage(
		"You are a support assistant. Confirm facts with tools before answering.",
	))
	messages = append(messages, history...)
	messages = append(messages, agent.NewUserMessage(input))
	return a.runner.Run(ctx, agent.RunRequest{Messages: messages})
}
```

推荐边界：

```text
HTTP / CLI / Worker
        |
        v
Your Agent facade
  - Prompt and policy
  - Tool selection
  - Domain ports
  - History mapping
        |
        v
agent-runtime-go
  - Model/tool loop
  - Validation
  - Stop and recovery semantics
```

应用负责保存当前 user message 和 `RunResult.Messages`。根 `Agent` 自身无会话状态，可以被多个请求并发复用。

完整入门步骤见[《快速开始》](docs/02-quick-start.md)。可运行的 Code Agent reference application 见 [`demo/icoder`](demo/icoder/README.md)。

## 选择子包

只引入当前需求需要的包。

| 需求 | 使用 |
|---|---|
| 一次 model/tool loop | 根包 `agent` |
| OpenAI 兼容模型接入 | `providers/openaicompat` |
| 多模型目录与 factory | `provider` |
| Prompt 模板与版本 | `prompt` |
| 历史归一化与 token 预算 | `context` |
| 消息聚合与 revision CAS | `message` |
| Session、branch、claim/resume | `session` |
| Checkpoint、lease、fence、恢复 | `durable` |
| allow/deny/ask 与授权 | `permission` |
| 高级工具生命周期与 effect ledger | `tool` |
| 可靠事件、outbox 与 replay | `event` |
| 非可信 instruction catalog | `skills` |
| MCP discovery 与调用 | `mcp` |
| 不可变 capability composition | `coordinator` |
| 独立 child run | `subagent` |
| readiness 与有界 shutdown | `app` |
| adapter conformance tests | `agenttest`，仅测试使用 |

详细职责、原理和最小组合见[《子包职责与接入指南》](docs/07-subpackages.md)。

## 运行原理

```text
RunRequest.Messages
        |
        v
build GenerateRequest -> agent.Model.Stream
        |                       |
        |                  canonical chunks
        v                       v
validate response <- aggregate one model step
        |
        +-- final text -------> RunResult
        |
        +-- tool calls
                |
                v
        schema + allowlist validation
                |
                v
           Tool.Execute
                |
                v
        append tool results and repeat
```

根包不读取环境变量、不选择 credential、不连接数据库，也不隐式注册全局工具。这些策略属于消费方应用。

更详细的执行语义见[《实现原理》](docs/05-runtime-internals.md)。

## 示例

| 示例 | 适合了解 |
|---|---|
| [`examples/hello`](examples/hello/main.go) | 最小 Model adapter 和一次运行 |
| [`examples/tool-agent`](examples/tool-agent/main.go) | 完整 model/tool loop 和 facade |
| [`demo/icoder`](demo/icoder/README.md) | Provider、工具、权限、Session、SQLite、Skills、MCP、Sub-Agent 的综合封装 |

## 文档

1. [简介：定位、能力与边界](docs/01-introduction.md)
2. [快速开始：最小可运行 Agent](docs/02-quick-start.md)
3. [总览：核心概念与包地图](docs/03-overview.md)
4. [架构：端口、适配器与组合](docs/04-architecture.md)
5. [实现原理：运行循环与 Durable 边界](docs/05-runtime-internals.md)
6. [根包核心内容](docs/06-root-package.md)
7. [子包职责与接入指南](docs/07-subpackages.md)
8. [生产组合模式](docs/08-production-patterns.md)
9. [iCoder 端到端教程](docs/09-icoder-tutorial.md)
10. [速查与术语](docs/10-reference.md)

## 关键语义

- `AllowedTools == nil` 使用 registry 中的全部工具；空 slice 明确禁用工具。
- `ToolResult{IsError: true}` 是模型可见、可修正的工具错误。
- 非 `nil` Go error 终止当前 attempt，并保留原始 cause。
- `RunResult.Messages` 只包含本轮新增的 assistant/tool messages。
- Observation 有界、非阻塞、允许丢失，不是权威事件日志。
- Durable execution 不天然保证外部副作用 exactly-once；下游需使用稳定 `ExecutionKey` 去重。
- Prompt、Skills 和模型输出都不是权限边界，真实权限必须在工具和应用 adapter 中执行。

## 验证

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

## 设计边界

根包 `agent` 是 portable Core，不依赖可选子包。子包按需依赖根包，应用在 composition root 中完成具体模型、数据库、权限和业务策略的组装。
