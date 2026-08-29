# providers/openaicompat

官方模型适配器：任何 OpenAI 兼容的 `/chat/completions` API（OpenAI、DeepSeek、Qwen、Kimi、OpenRouter、vLLM、Ollama 等）不写一行协议代码直接接入。

```go
import "github.com/iceymoss/agent-runtime-go/providers/openaicompat"
```

## 是什么

`openaicompat.Model` 是 `agent.Model` 接口的完整实现。你给它 base URL 和 API key，它负责：

- **SSE 流式**：默认以流式请求上游，把文本增量和工具调用碎片实时转成规范的 `agent.StreamChunk`；
- **结构化输出**：`ResponseFormat` 投影到 `response_format`（`json_object` 或 `json_schema`），默认声明 `Capabilities.StructuredOutput`；端点不支持时用 `WithCapabilities` 收窄，这样请求会在装配期被拒绝而不是静默返回自由文本；
- **推理内容**：读取 `reasoning_content` 与 `reasoning` 两种字段名，转成 `PartReasoning`，且**不会回传给上游**——产生它的供应商会拒绝把它当 assistant 输入；
- **工具调用重组**：上游把一个 tool call 的 JSON 参数拆成多个 delta 下发，适配器按 index 重组为完整调用后才交给运行时；
- **usage 归一化**：把各家不一致的 token 字段（含 cached tokens、reasoning tokens）归一到 `agent.Usage`，保证分量总和等于 `TotalTokens`；
- **重试提示**：解析 `Retry-After`（秒数与 HTTP 日期两种写法）填入 `ModelError.RetryAfter`，供应商说什么时候能再来就按它来；
- **错误分类**：HTTP 失败被分类为带重试语义的 `agent.ModelError`——401/403 → 认证错误（不可重试）、429 → 限流（可重试）、5xx → 传输错误（可重试）、其余 4xx → 请求被拒（不可重试）。

```go
func New(baseURL, apiKey string, opts ...Option) *Model
```

`baseURL` 是 API 根路径（如 `https://api.openai.com/v1`），适配器自动追加 `/chat/completions`。`apiKey` 为空则不发 `Authorization` 头，适合 Ollama、vLLM 等本地服务。

## 为什么需要它

自己实现 `agent.Model` 意味着要处理 SSE 解析、分片重组、终止一致性（流里发出的所有增量必须与最终 Response 完全一致，运行时会校验）、usage 字段的各家方言，以及把 HTTP 错误翻译成运行时能理解的重试语义。这些代码每个接入方都会重复写、重复踩坑。

**什么时候不需要它**：上游不是 OpenAI 协议时（如 Anthropic Messages API），需要自己实现 `agent.Model` 接口，见 [agent](agent.md)。

## 怎么用

最小接入（三行）：

```go
model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))
runner, err := agent.New(agent.Config{
	Key: "app.assistant", ModelName: "deepseek-chat", MaxSteps: 8,
}, model, registry)
```

`Config.ModelName` 会作为请求里的 `model` 字段发给上游，所以同一个适配器实例可以服务不同模型名的多个 Agent。

常见端点写法：

```go
// OpenAI
openaicompat.New("https://api.openai.com/v1", key)
// DeepSeek
openaicompat.New("https://api.deepseek.com/v1", key)
// 本地 Ollama（无鉴权）
openaicompat.New("http://127.0.0.1:11434/v1", "")
// OpenRouter（附加归因头）
openaicompat.New("https://openrouter.ai/api/v1", key,
	openaicompat.WithHeader("HTTP-Referer", "https://your-app.example"),
	openaicompat.WithHeader("X-Title", "Your App"))
```

### 选项一览

| 选项 | 用途 |
|---|---|
| `WithName(name)` | 覆盖 `Model.Name()` 返回的标识（默认 `openai-compatible`） |
| `WithHTTPClient(c)` | 自定义超时、代理、传输层重试（默认 client 超时 5 分钟） |
| `WithCapabilities(caps)` | 上游能力与默认值不同时显式声明（默认声明支持 tools 和全部 tool choice 模式） |
| `WithHeader(k, v)` | 每个请求附加自定义头 |
| `WithoutStreaming()` | 改用非流式请求，从完整响应合成 chunk 序列；用于 SSE 实现不可靠的上游 |
| `WithoutStreamUsage()` | 不发送 `stream_options.include_usage`；用于会拒绝该字段的上游（代价是流式步骤的 usage 为零） |

可运行示例（仓库自带，接任意 OpenAI 兼容端点）：

```bash
OPENAI_API_KEY=sk-... go run ./examples/openai-compat
```

输出示意（实际内容因模型而异）：

```text
[tool call] get_time {"timezone":"Asia/Shanghai"}
The current date and time in Shanghai is Thu, 06 Aug 2026 12:45:10 CST.

[gpt-4o-mini] stop=complete steps=2 tokens=421
```

## 常见问题

**Q: 上游流式接口经常断流或格式不标准怎么办？**
加 `openaicompat.WithoutStreaming()`。适配器会改发非流式请求，拿到完整响应后在本地合成 chunk 序列，对运行时完全透明——代价是没有逐 token 的实时输出。

**Q: 请求报错说不认识 `stream_options` 字段？**
部分自建服务（旧版 vLLM 等）不支持这个 OpenAI 扩展字段。加 `openaicompat.WithoutStreamUsage()` 去掉它；流式步骤的 usage 会是零，计费统计需要从上游日志获取。

**Q: 如何判断错误能不能重试？**
所有上游失败都是 `agent.ModelError`，用 `errors.As` 取出后看 `Retryable` 字段和 `Kind`（auth / rate_limit / transport / rejected / protocol）。429 和 5xx 标记为可重试，认证错误和 4xx 拒绝不可重试。

**Q: 模型不支持指定工具（named tool choice）怎么办？**
用 `WithCapabilities` 声明真实能力，例如 `agent.Capabilities{Tools: true, ToolChoiceNone: true}`。运行时会在装配期校验配置需要的能力是否被声明，不符合的组合直接拒绝，而不是运行中静默降级。

**Q: 支持图片输入吗？**
当前 wire 投影只覆盖文本、工具调用和工具结果。需要多模态时自行实现 `agent.Model`，或等后续版本。

**Q: 一个进程要接多家 provider 怎么组织？**
每家一个 `openaicompat.New` 实例（不同 baseURL/key），用 `WithName` 区分标识。需要按名字路由和目录管理时引入 [provider 子包](provider.md)。
