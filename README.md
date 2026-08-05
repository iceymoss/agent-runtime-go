# Agent Runtime for Go

`agent-runtime-go` 是一个与具体模型供应商和业务领域解耦的 Go Agent runtime。根包提供无状态的 model/tool 运行时；会话、持久化事件、权限、MCP、Skills、Sub-Agent 和应用生命周期等能力位于可选子包中。

完整的分包使用指南见 [`docs/`](docs/README.md)，可运行 Code Agent reference application 见 [`demo/icoder/`](demo/icoder/README.md)。

```text
应用 / 适配器
      |
      v
可选子包（session、durable、permission、mcp、skills ...）
      |
      v
根包 agent（消息、模型、工具、无状态运行时）
```

根包不会读取配置、连接数据库或选择模型供应商，也不会保存会话。调用方负责实现模型适配器、提供工具，并持久化 `RunResult`。

## 功能

- 供应商无关的消息、图片、模型请求、流式响应和 usage 契约
- 多步 model/tool 调用循环与工具白名单
- JSON Schema Draft 2020-12 工具参数校验与有限自动纠错
- 模型能力、`ToolChoice` 和流协议校验
- 最大步数、自定义停止条件、上下文预算和工具死循环检测
- 不阻塞运行时的 best-effort 流式 observation
- 不可变 `RuntimeDefinition` 与可复现 artifact digest
- 可选的 checkpoint、lease、fence 和工具恢复机制
- Provider、Session、Permission、Event、MCP、Skills、Sub-Agent 等可组合子包

## 安装

安装模块：

```go
import "github.com/iceymoss/agent-runtime-go"
```

普通运行只需要根包。按需单独导入子包，不需要引入完整 host 或基础设施。

## 快速开始

SDK 不内置具体模型客户端。首先实现 `agent.Model`：

```go
type Model interface {
	Name() string
	Capabilities() Capabilities
	Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}
```

以下示例使用一个本地模型实现完成最小文本运行：

```go
package main

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
)

type localModel struct{}

func (localModel) Name() string { return "local" }

func (localModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{}
}

func (localModel) Stream(ctx context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	if err := agent.ValidateGenerateRequestCapabilities(req, (localModel{}).Capabilities()); err != nil {
		return nil, err
	}

	chunks := make(chan agent.StreamChunk, 2)
	go func() {
		defer close(chunks)

		message := agent.NewAssistantMessage("Hello from the Agent SDK.")
		select {
		case chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: message.Text()}:
		case <-ctx.Done():
			return
		}
		select {
		case chunks <- agent.StreamChunk{
			Type: agent.ChunkFinish,
			Response: &agent.Response{
				Message:      message,
				FinishReason: agent.FinishStop,
				ModelName:    "local-v1",
				Usage: agent.Usage{
					PromptTokens:     4,
					CompletionTokens: 6,
					TotalTokens:      10,
				},
			},
		}:
		case <-ctx.Done():
		}
	}()
	return chunks, nil
}

func main() {
	runner, err := agent.New(agent.Config{
		Key:          "example.assistant",
		ModelName:    "local-v1",
		MaxSteps:     4,
		AllowedTools: []string{},
	}, localModel{}, agent.NewRegistry())
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("Answer concisely."),
			agent.NewUserMessage("Say hello."),
		},
	})
	if err != nil {
		panic(err)
	}

	fmt.Println(result.Text)
	fmt.Println(result.Outcome, result.StopReason)
}
```

输出：

```text
Hello from the Agent SDK.
completed complete
```

`Agent` 不持有会话状态，可以被多个请求复用。`RunRequest.Messages` 会被复制而不会被修改，`RunResult.Messages` 只包含本次运行新增的 assistant 和 tool 消息。

## 添加工具

工具实现 `agent.Tool`，其参数声明使用 JSON Schema：

```go
type weatherTool struct{}

func (weatherTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "get_weather",
		Description: "Return the weather for a city.",
		Strict:      true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required": []any{"city"},
		},
	}
}

func (weatherTool) ReplayPolicy() agent.ReplayPolicy {
	return agent.ReplayPolicyIdempotent
}

func (weatherTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: `{"condition":"sunny"}`}, nil
}
```

注册工具并通过白名单暴露给模型：

```go
registry := agent.NewRegistry()
if err := registry.Register(weatherTool{}); err != nil {
	return err
}

runner, err := agent.New(agent.Config{
	Key:          "example.weather",
	ModelName:    "model-with-tools",
	MaxSteps:     4,
	AllowedTools: []string{"get_weather"},
}, model, registry)
```

模型以 `FinishToolCalls` 返回完整 `ToolCall` 后，运行时会按顺序执行工具、追加 tool 消息，再请求下一步模型输出。

工具错误分两类：

- 返回 `ToolResult{IsError: true}`：错误对模型可见，模型可以修正参数或换用其他工具。
- 返回非 `nil` Go error：当前运行失败并保留原始 cause。

`AllowedTools` 的语义：

| 值 | 含义 |
|---|---|
| `nil` | 使用构造 Agent 时 registry 中的全部工具 |
| `[]string{}` | 不向模型暴露任何工具 |
| 非空列表 | 只暴露列出的工具；工具未注册时构造失败 |

Agent 持有不可变的 `ToolSet` 快照。Agent 创建后再修改 registry，不会改变已有 Agent。

## 示例：封装一个 Code Agent

实际项目通常不会让 CLI、HTTP 或 IDE 直接组装 `agent.Agent`，而是在 SDK 上封装一个面向具体用途的 facade。以 Code Agent 为例，推荐的边界是：

```text
CLI / HTTP / IDE
       |
       v
codeagent facade（Prompt、工具组合、运行策略）
       |
       +-- Workspace（本地目录、容器或远端 sandbox）
       +-- read_file / search_code / apply_patch / run_command
       +-- Permission Policy
       |
       v
agent-runtime-go（model/tool loop）
```

Code Agent 的文件系统、Shell、Git、Prompt 和权限规则属于消费方应用，不应添加到通用 SDK。下面从 port、tool、facade、model 和执行轨迹五个部分实现一个可观察的 Code Agent。示例只提供 `read_file`，以便把重点放在 SDK 的 tool loop 上。

### 1. 定义 Workspace port 和 Tool

`Workspace` 是 Code Agent 自己拥有的边界。SDK 不知道文件来自本地目录、容器还是远端 sandbox。

```go
package codeagent

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
	"encoding/json"
)

// Workspace 隔离具体执行环境。它可以由本地目录、容器或远端 sandbox 实现。
type Workspace interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

type Config struct {
	Model     agent.Model
	ModelName string
	Workspace Workspace
	MaxSteps  int
}

type readFileTool struct {
	workspace Workspace
}

func (t *readFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{
		Name:        "read_file",
		Description: "Read a UTF-8 text file from the current workspace.",
		Strict:      true,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []any{"path"},
		},
	}
}

func (t *readFileTool) ReplayPolicy() agent.ReplayPolicy {
	return agent.ReplayPolicyIdempotent
}

func (t *readFileTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode read_file input: %w", err)
	}

	content, err := t.workspace.ReadFile(ctx, input.Path)
	if err != nil {
		// 文件不存在等可修正错误回灌给模型，不中断整个 run。
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: string(content)}, nil
}
```

工具的 `Definition` 会进入下一次 `GenerateRequest.Tools`。模型返回工具参数后，SDK 会先执行 JSON Schema 校验，再调用 `Execute`。因此工具实现收到的是 schema 已接受、但尚未解析的原始 JSON。

### 2. 组装 Code Agent facade

facade 决定 Prompt、工具组合和运行参数，根 SDK 负责执行循环：

```go
package codeagent

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
)

type CodeAgent struct {
	runner *agent.Agent
}

func New(cfg Config) (*CodeAgent, error) {
	if cfg.Model == nil || cfg.ModelName == "" || cfg.Workspace == nil {
		return nil, fmt.Errorf("model, model name, and workspace are required")
	}
	if !cfg.Model.Capabilities().Tools {
		return nil, fmt.Errorf("model does not support tools")
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 12
	}

	registry := agent.NewRegistry()
	if err := registry.Register(&readFileTool{workspace: cfg.Workspace}); err != nil {
		return nil, err
	}

	runner, err := agent.New(agent.Config{
		Key:          "code.assistant",
		ModelName:    cfg.ModelName,
		MaxSteps:     cfg.MaxSteps,
		AllowedTools: []string{"read_file"},
	}, cfg.Model, registry)
	if err != nil {
		return nil, err
	}
	return &CodeAgent{runner: runner}, nil
}

func (a *CodeAgent) Run(
	ctx context.Context,
	instruction string,
	history []agent.Message,
	emitter *agent.ObservationEmitter,
) (*agent.RunResult, error) {
	messages := make([]agent.Message, 0, len(history)+2)
	messages = append(messages, agent.NewSystemMessage(
		"You are a code agent. Inspect relevant files before answering and do not invent repository contents.",
	))
	messages = append(messages, history...)
	messages = append(messages, agent.NewUserMessage(instruction))

	return a.runner.Run(ctx, agent.RunRequest{
		Messages:           messages,
		ObservationEmitter: emitter,
	})
}
```

### 3. 模型适配器实际收到什么

生产适配器会把 `GenerateRequest` 转成 OpenAI、Anthropic、Gemini 或本地模型协议。为了完整展示 SDK 行为，下面的确定性模型在第一次请求时调用 `read_file`，拿到 tool message 后再返回最终文本：

```go
package main

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
)

type walkthroughModel struct{}

func (walkthroughModel) Name() string { return "walkthrough" }

func (walkthroughModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}

func (m walkthroughModel) Stream(
	ctx context.Context,
	req *agent.GenerateRequest,
) (<-chan agent.StreamChunk, error) {
	if err := agent.ValidateGenerateRequestCapabilities(req, m.Capabilities()); err != nil {
		return nil, err
	}

	chunks := make(chan agent.StreamChunk, 2)
	go func() {
		defer close(chunks)

		if req.Messages[len(req.Messages)-1].Role != agent.RoleTool {
			call := agent.ToolCall{
				ID:    "call_read_1",
				Name:  "read_file",
				Input: `{"path":"go.mod"}`,
			}
			message := agent.Message{
				Role: agent.RoleAssistant,
				Parts: []agent.ContentPart{{
					Type:     agent.PartToolCall,
					ToolCall: &call,
				}},
				FinishReason: agent.FinishToolCalls,
			}
			send(ctx, chunks, agent.StreamChunk{Type: agent.ChunkToolCall, ToolCall: &call})
			send(ctx, chunks, terminal(message, agent.FinishToolCalls, 12, 3))
			return
		}

		results := req.Messages[len(req.Messages)-1].ToolResults()
		text := fmt.Sprintf("已读取 go.mod：\n%s", results[0].Content)
		message := agent.NewAssistantMessage(text)
		send(ctx, chunks, agent.StreamChunk{Type: agent.ChunkText, TextDelta: text})
		send(ctx, chunks, terminal(message, agent.FinishStop, 24, 8))
	}()
	return chunks, nil
}

func terminal(
	message agent.Message,
	reason agent.FinishReason,
	promptTokens int,
	completionTokens int,
) agent.StreamChunk {
	return agent.StreamChunk{
		Type: agent.ChunkFinish,
		Response: &agent.Response{
			Message:      message,
			FinishReason: reason,
			ModelName:    "walkthrough-v1",
			Usage: agent.Usage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			},
		},
	}
}

func send(ctx context.Context, out chan<- agent.StreamChunk, chunk agent.StreamChunk) {
	select {
	case out <- chunk:
	case <-ctx.Done():
	}
}
```

这个模型是协议演示，不是生产模型客户端。重点是它展现了适配器职责：发送完整 tool call、发送唯一终态 response，并保证流式增量与终态消息完全一致。

### 4. 从入口运行并观察事件

上层程序只依赖 Code Agent facade，同时可以消费非阻塞 observations：

```go
workspace := memoryWorkspace{
	"go.mod": []byte("module example.com/code-agent-demo\n\ngo 1.22\n"),
}
coder, err := codeagent.New(codeagent.Config{
	Model:     walkthroughModel{},
	ModelName: "walkthrough-v1",
	Workspace: workspace,
	MaxSteps:  20,
})
if err != nil {
	return err
}

emitter := agent.NewObservationEmitter(16, func(observation agent.Observation) {
	fmt.Println("observation:", observation.Type)
})
defer emitter.Close()

instruction := "先查看 go.mod，再告诉我这是哪个 Go module"
result, err := coder.Run(ctx, instruction, history, emitter)
if err != nil {
	return err
}
history = append(history, agent.NewUserMessage(instruction))
history = append(history, result.Messages...)

fmt.Println("answer:", result.Text)
fmt.Println("outcome:", result.Outcome)
fmt.Println("stop:", result.StopReason)
fmt.Println("steps:", len(result.Steps))
```

其中 walkthrough 使用的 workspace 只是一个内存实现：

```go
type memoryWorkspace map[string][]byte

func (w memoryWorkspace) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, ok := w[path]
	if !ok {
		return nil, fmt.Errorf("file %q does not exist", path)
	}
	return append([]byte(nil), content...), nil
}
```

生产实现必须将路径限定在 workspace 根目录内，并处理符号链接、路径穿越、文件大小和文本编码；这些是 sandbox 的职责，不是 `agent-runtime-go` 的职责。

### 5. SDK 内部发生了什么

这一轮运行的完整状态变化如下：

```text
RunRequest.Messages
  system: You are a code agent ...
  user:   先查看 go.mod，再告诉我这是哪个 Go module
             |
             v
Step 0: Agent 构造 GenerateRequest
  model: walkthrough-v1
  tools: [read_file JSON Schema]
             |
             v
Model.Stream
  ChunkToolCall(read_file, {"path":"go.mod"})
  ChunkFinish(FinishToolCalls, complete assistant message)
             |
             v
Agent 校验 stream、finish reason、tool choice 和 schema
             |
             v
readFileTool.Execute
  ToolResult(call_read_1, content=<go.mod bytes>)
             |
             v
Agent 追加两条新消息
  assistant: tool_call(read_file)
  tool:      tool_result(read_file)
             |
             v
Step 1: Agent 用扩展后的 history 再次调用 Model.Stream
  ChunkText("已读取 go.mod ...")
  ChunkFinish(FinishStop, complete assistant message)
             |
             v
RunResult
  Outcome:    completed
  StopReason: complete
  Messages:   [assistant tool_call, tool result, assistant text]
  Steps:      [step 0, step 1]
  Usage:      两次模型调用的累计 usage
```

在这个循环中，消费方负责实现的部分是 `Model`、`Tool`、`Workspace`、Prompt 和历史持久化；SDK 负责：

- 复制并维护本轮 history，不修改调用方传入切片。
- 将白名单内的 tool definitions 放入每一步模型请求。
- 校验 stream 的唯一终态、增量一致性、finish reason 和 usage。
- 校验工具名和 JSON Schema，在有限预算内把非法参数回灌给模型修正。
- 顺序执行工具并自动构造 assistant/tool 配对消息。
- 累计 `Steps`、`Messages` 和 `Usage`。
- 执行最大步数、停止条件、上下文预算和死循环检测。
- 发出 best-effort observations，并通过 `RunResult` 返回权威终态。

继续扩展时，保持相同模式注册 `list_files`、`search_code`、`apply_patch`、`run_command` 和 Git 工具即可。建议让所有工具依赖一个受限的 `Workspace` port，而不是直接接受任意文件路径或 Shell 字符串：

- 读取和搜索通常可以自动允许。
- 写文件应限制在 workspace 内，并防止路径穿越。
- 命令使用 `Program + Args + WorkingDirectory + Timeout` 等结构化参数，不直接执行任意 Shell 文本。
- 网络访问、提交、推送、部署和删除等外部副作用应经过显式权限策略。
- Prompt 只定义工作方式，真正的安全边界必须由 workspace、sandbox 和 permission 层执行。

MVP 只需要根包。需要长期会话时增加 `agent/session` 和 `agent/message`；需要崩溃恢复时增加 durable store；需要命令审批和副作用审计时组合 `agent/tool` 与 `agent/permission`。

## 流协议

`Model.Stream` 是供应商适配器必须遵守的核心契约：

1. 成功流包含零个或多个 `ChunkText` / `ChunkToolCall`、恰好一个 `ChunkFinish`，随后关闭 channel。
2. `ChunkToolCall` 必须包含已完整拼装的调用参数；供应商的参数增量应由适配器先缓冲。
3. 所有文本增量拼接后必须等于终态 `Response.Message.Text()`。
4. 所有工具调用必须与终态 `Response.ToolCalls()` 在内容和顺序上完全一致。
5. `Response.Message.FinishReason` 必须与 `Response.FinishReason` 一致。
6. `ChunkFinish` 后不能继续发送分片；channel 也不能在终态分片前关闭。
7. 流中错误通过携带原始错误的 `ChunkError` 返回，然后关闭 channel。
8. `context.Canceled` 和 `context.DeadlineExceeded` 必须保持标准 context 错误语义。

违反协议会得到 `ModelErrorKindProtocol`。供应商错误应使用 `agent.NewModelError` 分类为 `transport`、`rate_limit`、`auth`、`rejected`、`protocol` 或 `unsupported`。`SafeDetail` 可以暴露给调用方，`Cause` 不会出现在 `Error()` 文本中，以避免泄漏供应商响应中的敏感信息。

适配器可以使用 test-only 包执行一致性测试：

```go
func TestModelConformance(t *testing.T) {
	agenttest.TestModel(t, newFixtureModel)
}
```

生产代码不得导入 `agenttest`。

## 运行结果

调用方应同时检查 error、`Outcome` 和 `StopReason`：

```go
result, err := runner.Run(ctx, request)
if err != nil {
	return err
}

switch result.Outcome {
case agent.OutcomeCompleted:
	// result.Text 是最终文本。
case agent.OutcomeSuspended:
	// 根据 StopReason 决定续跑、调整预算或提示调用方。
}
```

| Outcome | 含义 |
|---|---|
| `completed` | 得到完整答复，或工具要求结束当前 turn |
| `suspended` | 当前尝试可恢复或需要外部决策，例如输出达到上限或上下文预算耗尽 |
| `failed` | 运行返回错误 |

常见 `StopReason` 包括 `complete`、`max_steps`、`stop_condition`、`tool_stop_turn`、`loop_detected`、`context_budget` 和 `output_limit`。部分中止情况会返回 `OutcomeSuspended` 和 `nil` error，因此不能只检查 error。

## Observations

文本增量和工具进度可以通过 `ObservationEmitter` 消费：

```go
emitter := agent.NewObservationEmitter(32, func(observation agent.Observation) {
	// 更新 UI、metrics 或临时日志。
})
defer emitter.Close()

result, err := runner.Run(ctx, agent.RunRequest{
	Messages:           messages,
	ObservationEmitter: emitter,
})
```

Observation 是有界、非阻塞、允许丢失的进度快照，不代表权威终态。最终状态始终以 `RunResult`、返回的 error 和 durable store 为准。需要持久化、重放和确认机制时使用 `event`。

## 不可变运行定义

需要记录精确运行组成时，使用 `RuntimeDefinition` 代替直接装配 `Agent`：

```go
toolSet, err := agent.NewToolSet(registry, []string{"get_weather"})
if err != nil {
	return err
}

definition, err := agent.NewRuntimeDefinition(agent.RuntimeDefinitionSpec{
	Key: "example.weather",
	Model: agent.ModelMetadata{
		Name:          "model-with-tools",
		Version:       "model-release-2026-08",
		ContextWindow: 128000,
		Capabilities:  model.Capabilities(),
	},
	Execution: agent.ExecutionSettings{MaxSteps: 4},
	PromptVersion: "prompt-v1",
	PolicyVersion: "policy-v1",
}, model, toolSet)
if err != nil {
	return err
}

runner, err := definition.NewAgent()
```

`ArtifactVersions()` 包含 definition、model、tools、prompt 和 policy 的版本标识。模型声明的 capabilities 必须与 `model.Capabilities()` 完全一致。

## 可选子包

| 包 | 职责 |
|---|---|
| `agent/provider` | 不可变模型目录、模型角色、价格元数据和 `agent.Model` factory |
| `agent/context` | 历史归一化、token 预算、context plan 和 compaction |
| `agent/message` | tenant-scoped 消息聚合、revision、CAS、branch 和 tombstone |
| `agent/permission` | allow/deny/ask、approval、grant、expiry 和 resume 绑定 |
| `agent/event` | 持久化事件、outbox、dispatcher、retry、replay 和 inbox 去重 |
| `agent/durable` | checkpoint store、lease/fence、effect ledger 和 reconciler |
| `agent/tool` | 不可变工具 generation、interceptor、权限和 durable effect execution |
| `agent/prompt` | 确定性模板编译、渲染和版本 digest |
| `agent/skills` | tenant-scoped、不可变、非可信的 instruction/artifact catalog |
| `agent/mcp` | tenant-scoped MCP 连接、transport、tool discovery 和 generation lease |
| `agent/coordinator` | 将 capability sources 解析为不可变 `RuntimeDefinition` generation |
| `agent/session` | session/branch 聚合及 admission、claim、cancel、resume host |
| `agent/subagent` | durable child run、层级/fanout 限制、预算和父子唤醒 |
| `agent/app` | 组件 readiness、admission、degraded health 和有界 shutdown |
| `agent/agenttest` | Model 与 Tool 的 test-only conformance suite |

边界选择建议：

- 普通无状态调用：只使用根包 `agent-runtime-go`。
- 需要会话运行和 claim/resume：增加 `session`。
- 需要可靠 checkpoint 和 effect 恢复：增加 root durable 配置和 `durable`。
- 需要审批、interceptor 或精确工具 generation：使用 `tool` 与 `permission`。
- 需要可靠事件：使用 `event`，不要把 observation 当作 event log。
- 需要托管多个长生命周期组件：使用 `app`。

## Durable 语义

在 `RunRequest.DurableRun` 中提供 `CheckpointStore`、稳定的 `RunIdentity`、lease owner 和 lease duration，即可启用 checkpointed execution。

Durable 模式不承诺模型调用或外部副作用天然 exactly-once：

- 模型在不确定恢复后可能被重新调用。
- `ReplayPolicyIdempotent` 只声明工具可以使用相同 `ToolInvocation.ExecutionKey` 重放；实际去重仍由工具或下游系统实现。
- 无法安全重放的模糊副作用会进入 unknown 状态，而不是盲目重试。
- lease、revision 和 fence 必须由 store 原子比较，过期 worker 不能提交结果。
- `DurableCompletion`、`DurableFailure` 和 `DurableSuspension` 用于让领域记录与运行时状态在调用方事务中共同完成。

简单内存运行不需要 durable 配置。

## 关键约束

- `MaxSteps` 必须大于零。默认死循环检测窗口是 `4`，因此使用默认值时 `MaxSteps` 至少为 `4`；否则需显式设置更小且合法的窗口和阈值。
- 即使不使用工具，也要传入 `agent.NewRegistry()`；使用空白名单明确禁用工具。
- 图片只允许出现在 user message，支持 JPEG、PNG 和 WebP；模型必须声明 `ImageInput` capability。
- 工具输入必须是 JSON object。`Strict` schema 默认拒绝未声明字段，外部 `$ref` 不受支持。
- `ToolRepairLimit` 为 `0` 时采用默认值 `1`，最大值为 `2`。
- 一个模型步骤接受的工具调用按顺序执行，不并行执行。
- usage 必须归一化：`TotalTokens` 等于 prompt、completion、cache creation 和 cache read token 之和；reasoning token 已包含在 completion 中。
- 根运行时不自动压缩历史、不保存消息，也不管理供应商 credential。

## 测试

```bash
go test ./agent-runtime-go/... -count=1
go test -race ./agent-runtime-go/... -count=1
go vet ./...
```

只测试根运行时：

```bash
go test ./agent-runtime-go -v -count=1
```

## 设计边界

`agent-runtime-go` 是 portable Core leaf：它不依赖任何子包、`internal/**`、产品 model、数据库框架或应用配置。子包可以依赖根包，应用适配器负责将具体供应商、数据库、认证和领域流程组装到 SDK ports。

通用机制属于 SDK；具体 workflow、Prompt 内容、模型选择策略、业务 DTO、持久化映射和 HTTP 接口属于应用层。
