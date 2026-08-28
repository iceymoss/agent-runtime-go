# agent

`agent` 是整个库的核心包：它可靠地执行"模型思考 → 调工具 → 把结果喂回模型"这个循环，其余一切（模型接入、工具实现、存储、权限）都通过接口交给你的应用。

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

## 是什么

根包只定义了两个你必须提供的端口，和一个你直接调用的执行器：

```go
// 模型端口：任何 LLM 适配成这个接口就能被运行时驱动
type Model interface {
	Name() string
	Capabilities() Capabilities
	Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}

// 工具端口：模型可以调用的业务函数
type Tool interface {
	Definition() ToolDefinition
	ReplayPolicy() ReplayPolicy
	Execute(context.Context, ToolInvocation) (ToolResult, error)
}
```

写 `Model` 适配器时有一条容易踩的规则：**流式分片累加后必须严格等于终态 response**——文本增量拼起来要逐字等于最终消息，工具调用也要一致。运行时不会掩盖"流出来一个答案、终态却报另一个"的适配器 bug。

问题在于最自然的最小实现（只发一个终态 chunk、不发任何增量）恰好违反它。所以已经拿到完整回答的适配器直接用 `agent.StreamResponse`：

```go
func (m myModel) Stream(ctx context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	response, err := m.callUpstream(ctx, req)
	if err != nil {
		return nil, err
	}
	return agent.StreamResponse(response), nil
}
```

非流式端点、缓存回复、测试 fake 都属于这一类。真正逐 token 流式的适配器自己发增量，最后用一个 `ChunkFinish` 收尾即可。

执行器是 `Agent`：用 `agent.New(config, model, registry)` 装配一次，之后并发安全地反复调用 `Run`。`Agent` 自身不保存会话历史、不读环境变量、不连数据库——这些都属于你的应用。

围绕这两个端口，根包内置了循环执行所需的全部机制：JSON Schema 校验、工具白名单、最大步数、停止条件、上下文预算、循环检测、非法参数回灌修正（tool repair）。

## 为什么需要它

没有它，你需要自己处理这些容易出错的细节：

- 解析模型返回的 tool call，校验参数是否合法，执行后把结果拼回消息历史，再次请求模型——每一步都有边界情况（半截 JSON、未注册的工具名、模型死循环重复调用同一工具）；
- 流式响应的协议一致性：文本增量拼起来必须等于最终消息，工具调用碎片要正确重组；
- 什么时候停：正常完成、达到步数上限、输出被截断、超出上下文预算，每种情况上层的处理方式不同。

**什么时候不需要引入其他子包**：只做"一次请求进来，模型（可能调几次工具）给出回答"的场景，根包就是全部。会话持久化、崩溃恢复、权限审批等需求出现时再看对应子包。

## 怎么用

一个完整的最小程序：接一个 OpenAI 兼容模型，注册一个工具，跑一轮。

```go
package main

import (
	"context"
	"fmt"
	"os"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
)

type WeatherInput struct {
	City string `json:"city" description:"城市名"`
}

func main() {
	// 1. 模型：openaicompat 是官方适配器，也可以自己实现 Model 接口
	model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

	// 2. 工具：NewTool 从函数生成 JSON Schema，无需手写
	weather := agent.MustNewTool("get_weather", "查询城市当前天气",
		func(ctx context.Context, in WeatherInput) (agent.ToolResult, error) {
			return agent.ToolResult{Content: `{"city":"` + in.City + `","temp":"26C"}`}, nil
		})
	registry := agent.NewRegistry()
	if err := registry.Register(weather); err != nil {
		panic(err)
	}

	// 3. 装配并运行
	runner, err := agent.New(agent.Config{
		Key:       "demo.weather",
		ModelName: "deepseek-chat",
		MaxSteps:  8,
	}, model, registry)
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("回答前先用工具确认事实。"),
			agent.NewUserMessage("北京现在多少度？"),
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
}
```

输出示意（实际内容因模型而异）：

```text
北京现在的气温是 26C。
```

关键点：

- `Config.AllowedTools` 控制白名单：`nil` = registry 里全部工具；`[]string{}` = 明确禁用工具；列出名称 = 只允许这些（缺失项在 `New` 时报错，不会等到运行中）。
- `agent.New` 在装配期就校验全部配置（model 非 nil、schema 合法、capability 覆盖需求），配置错误不会拖到请求中途才暴露。
- `RunResult.Messages` 只包含本轮新产生的 assistant/tool 消息；输入的 user message 由你自己按事务规则保存。
- 判断结果要联合看三个值：`err`、`result.Outcome`、`result.StopReason`。`OutcomeCompleted` 是正常完成；`OutcomeSuspended` 表示被步数/输出长度/上下文预算中断，上层可决定续跑；`OutcomeFailed` 伴随非 nil error。
- 工具返回 `ToolSuspensionError` 时，Agent 不会伪造 tool message，而是返回 `StopReasonToolSuspended` 和 typed `RunResult.Suspension`。durable 调用会 checkpoint 该 blocker；恢复时必须通过 `DurableRunConfig.ToolResume` 原样提交。

### 定义工具的两种方式

优先用 `NewTool` / `MustNewTool`（Schema 从结构体反射生成）：

```go
type Input struct {
	OrderID string `json:"order_id" description:"订单号"`
	Limit   int    `json:"limit,omitempty" description:"返回条数"`
}
// 非指针且没有 omitempty 的字段自动进入 required；
// schema 默认 strict：未声明的属性会被拒绝并回灌模型修正。
tool := agent.MustNewTool("find_order", "按订单号查询订单", handler)
```

需要完全控制 Schema（复杂约束、oneOf 等）时，直接实现 `Tool` 接口三个方法。

工具的错误分两种，语义完全不同：

- `ToolResult{IsError: true}`：模型可见、可修正的业务错误（如"订单不存在"），循环继续；
- `Execute` 返回非 nil Go error：基础设施故障，立即中止本次运行。

### 让模型按结构输出

答案要交给代码解析而不是给人读时——抽取、分类、路由、填表——只靠 prompt 约束不够：JSON 外面多一句话就把一次成功的运行变成解析错误，而运行时无法修复它。用 `ResponseFormat` 让供应商去强制：

```go
runner, err := agent.New(agent.Config{
	Key: "extract", ModelName: "gpt-4o-mini", MaxSteps: 4,
	ResponseFormat: &agent.ResponseFormat{
		Kind:   agent.ResponseFormatJSONSchema,
		Name:   "invoice",
		Schema: json.RawMessage(`{"type":"object","properties":{"total":{"type":"number"}}}`),
		Strict: true,
	},
}, model, registry)
```

`ResponseFormatJSON` 只要求合法 JSON，兼容面更广；`ResponseFormatJSONSchema` 要求符合给定 schema。`RunRequest.ResponseFormat` 可以按次覆盖，所以同一个 Agent 平时正常对话、只在需要机器可读的那一次约束输出。

模型没声明 `Capabilities.StructuredOutput` 时，`agent.New` 直接返回 `ErrAgentConfigInvalid`——装配期失败，而不是跑到一半才发现。**运行时不校验模型输出是否符合 schema**，强制是供应商的职责；真正要确定的应用仍然要自己解析和检查。

### 推理模型的思维链

推理模型（DeepSeek-R1、Qwen3-thinking、o 系列等）会把思考过程和答案分开返回。运行时用 `PartReasoning` 承载它：

```go
result, _ := runner.Run(ctx, agent.RunRequest{Messages: msgs})
answer := result.Text                     // 只有答案，不含思考
thinking := result.Messages[0].Reasoning() // 思考过程
```

流式时通过 `ObservationReasoningDelta` 单独下发，UI 可以折叠、隐藏或直接丢弃，不用去猜自己拿到的是哪半边。

两条规则值得记住：

- `Message.Text()` 不包含 reasoning，所以 `RunResult.Text` 永远是干净的答案；
- **适配器不会把 reasoning 回传给模型**。产生 reasoning 的供应商基本都会拒绝把它当 assistant 输入，而且把思维链当对话重放会改变模型在回答的问题。历史里保留它是为了展示和审计。

模型产出 reasoning 但没声明 `Capabilities.Reasoning` 会被判为协议错误——应用需要在第一个 token 到达之前就知道要不要渲染或脱敏。

### 观察运行进度

`ObservationEmitter` 提供文本增量、工具开始/结束、步骤完成四类信号，用于 UI 实时展示。

**默认是有界、非阻塞、可丢失的**：队列满时观察值被丢弃并计数，绝不阻塞模型和工具的推进。这对进度遥测是对的取舍，对"人正在读的文本"是错的——消费者只要比模型慢（SSE、WebSocket、慢客户端都会），增量就会被静默丢掉，读者看到一个被截断的回答，而 `RunResult.Text` 是完整的。

所以要区分两种用途：

```go
// 遥测：丢一点无所谓，但要能发现丢了
emitter := agent.NewObservationEmitter(64, recordProgress)
defer emitter.Close()
// ... 运行之后
if emitter.Dropped() > 0 { /* 观察值不完整 */ }
```

```go
// 交付给人看的文本：不能丢
emitter := agent.NewObservationEmitterWith(
	agent.ObservationOptions{QueueSize: 64, Lossless: true},
	func(o agent.Observation) {
		if o.Type == agent.ObservationTextDelta {
			fmt.Print(o.Text)
		}
	})
defer emitter.Close()
result, err := runner.Run(ctx, agent.RunRequest{Messages: msgs, ObservationEmitter: emitter})
```

`Lossless` 的代价是真实的：比模型慢的消费者会拖慢这次运行，所以 `consume` 要保持廉价（把观察值交给带缓冲的 writer，不要在里面直接做网络写）。等待受本次运行的 context 约束，运行被取消时不会卡在停止读取的消费者上。

无论哪种模式，权威结果只看 `RunResult`——`Observation` 不是事件日志。需要可靠的事件投递用 [event 子包](event.md)。

## 常见问题

**Q: `MaxSteps` 是重试次数吗？**
不是。它是一轮对话中模型被调用的最大次数（每次工具调用后模型会被再次调用，算一步）。达到上限返回 `OutcomeSuspended` + `StopReasonMaxSteps`。注意 `MaxSteps` 不能小于循环检测窗口（默认 4）。

**Q: 装配 Agent 之后往 Registry 里再注册工具，Agent 能看到吗？**
看不到。`New` 时会创建不可变的 `ToolSet` 快照，之后修改 Registry 不影响已装配的 Agent。这是刻意设计：运行中的工具集合必须稳定可审计。

**Q: 模型反复调用同一个工具怎么办？**
内置循环检测：最近 `LoopDetectWindow` 步内同一（工具名 + 参数）出现 `LoopDetectThreshold` 次即以 `ErrLoopDetected` 终止。两个参数都可在 `Config` 中调整。

**Q: 模型给的工具参数不符合 Schema 会怎样？**
校验失败的参数会作为 error result 回灌给模型修正，最多 `ToolRepairLimit` 次（默认 1，上限 2）。超过次数仍不合法则运行失败。

**Q: `RunResult.Text` 是全部回复吗？**
只是最后一步的 assistant 文本。完整的过程消息在 `RunResult.Messages`，逐步细节（每步的工具调用、结果、usage）在 `RunResult.Steps`。

**Q: 想在每一步动态换模型或收窄工具？**
用 `RunRequest.StepPolicy`。它可以为下一步选择工具子集、tool choice、模型名和采样参数，但不能改写历史或替换 `Model` 实例。

## 相关文档

- 接真实模型：[providers/openaicompat](openaicompat.md)
- 流协议细节、停止语义、恢复状态机：[运行循环内部机制](../internals.md)
- 崩溃恢复与 checkpoint：[durable](durable.md)
- 会话持久化：[session](session.md)、[message](message.md)
