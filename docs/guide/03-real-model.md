# 3. 接上真实模型

## 你现在遇到的问题

前两章用的是写死回答的假模型。现在要换成真的——而且不想被某一家供应商绑死，也不想因为供应商抖一下就整轮失败。

## 接任何 OpenAI 兼容的服务

OpenAI、DeepSeek、Qwen、Kimi、OpenRouter、vLLM、Ollama 都说 OpenAI 的 `/chat/completions`，官方适配器直接可用：

```go
func NewModel() (model agent.Model, name string) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return &scriptedModel{}, "scripted-v1"
	}
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	modelName := os.Getenv("OPENAI_MODEL")
	if modelName == "" {
		modelName = "gpt-4o-mini"
	}
	// retry 包在适配器外面：它需要适配器的错误分类来判断什么值得重试
	return retry.New(openaicompat.New(baseURL, key), retry.Options{}), modelName
}
```

换供应商就是换 `baseURL` 和 `ModelName`。本地 Ollama 把 key 留空（`openaicompat.New("http://localhost:11434/v1", "")`），适配器不会发 `Authorization` 头。

**凭据是应用读的，不是库读的。** 上面的 `os.Getenv` 是你的代码——运行时不读环境变量、不查配置文件，所以这个函数是整个程序里唯一知道 key 在哪的地方。

保留一个无 key 时的假模型很有用：例子、测试、CI 都不需要真凭据。

## 加上重试

```go
model := retry.New(openaicompat.New(baseURL, key), retry.Options{})
```

零值 `Options{}` 是 3 次、200ms 起、上限 30s、带抖动。供应商给了 `Retry-After` 就按它的来。

自己写这一层很容易写错：**只包住 `Stream()` 的返回值几乎什么都没重试到**。SSE 请求通常会成功——HTTP 响应到了、channel 也拿到了——然后连接在中途断掉，这个失败出现在流**里面**，包装器根本看不见。这个包两种都重试。

它也**不会**在已经向下游交付过内容之后重试：那时候接一个新流上去，要么让运行时的一致性校验失败，要么更糟——通过了但文本重复了一遍。这种情况错误原样上报，由你决定是否整轮重跑。

## 自己写适配器时

不是 OpenAI 兼容的协议（Anthropic Messages、Bedrock）要自己实现 `Model` 的三个方法。有一条规则很容易踩：**流式分片累加后必须严格等于终态 response**。

已经拿到完整回答的适配器直接用 `agent.StreamResponse`，它会把回答拆成合规的分片序列：

```go
func say(text string) <-chan agent.StreamChunk {
	message := agent.NewAssistantMessage(text)
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{
		Message: message, FinishReason: agent.FinishStop, ModelName: "scripted-v1",
		Usage: agent.Usage{PromptTokens: 20, CompletionTokens: 12, TotalTokens: 32},
	})
}
```

真正逐 token 流式的适配器才自己发增量。写完用[第 12 章](./12-testing.md)的一致性套件验收——库自己的 `openaicompat` 接上它时抓出了三个真实缺陷。

## 多个模型 / Prompt 版本

一个应用常常要用不止一个模型：主力模型回答，便宜模型做摘要。`provider` 管理这个目录，按角色取模型。

把 system prompt 硬编码成字符串字面量，改一次就没人知道线上跑的是哪一版；`prompt` 给模板和版本号，运行结果里可以记下用的是哪版。

两个都是可选的——单模型 + `agent.NewSystemMessage("...")` 完全能跑，等你需要回答"上周那次运行用的是哪版 prompt"时再引入。

## 需要注意的

**`ModelName` 是发给上游的 `model` 字段**，所以同一个适配器实例可以服务多个用不同模型名的 Agent。

**端点不支持某个能力时用 `WithCapabilities` 收窄。** 适配器默认声明支持工具、三种 tool choice、结构化输出、推理内容。声明和现实不符时，运行时会在装配期拒绝非法请求——比跑到一半才发现好。

## 深入

- [openaicompat](../packages/openaicompat.md) —— 非流式降级、自定义 header、超时与代理
- [retry](../packages/retry.md) —— 重试什么、不重试什么、为什么
- [provider](../packages/provider.md) · [prompt](../packages/prompt.md)
- 完整可运行代码：[`examples/guide/model.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/model.go)
