# 3. 接上真实模型

## 你现在遇到的问题

前两章用的是写死回答的假模型。现在要换成真的——而且不想被某一家供应商绑死。

## 接任何 OpenAI 兼容的服务

OpenAI、DeepSeek、Qwen、Kimi、OpenRouter、vLLM、Ollama 都说 OpenAI 的 `/chat/completions` 协议，官方适配器直接可用，不用自己实现 `Model`：

```go
import "github.com/iceymoss/agent-runtime-go/providers/openaicompat"

model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

runner, err := agent.New(agent.Config{
	Key: "my.assistant", ModelName: "deepseek-chat", MaxSteps: 8,
}, model, registry)
```

换供应商就是换 `baseURL` 和 `ModelName` 两个值。本地 Ollama 把 apiKey 留空即可（`openaicompat.New("http://localhost:11434/v1", "")`），适配器不会发 `Authorization` 头。

适配器负责 SSE 流式、把拆散的工具调用分片重组、归一化 usage（含缓存和推理 token）、解析 `Retry-After`、把 HTTP 失败分类成带重试语义的 `agent.ModelError`。

把第 1 章的假模型换成这一行，加上第 2 章的工具，你就有了一个真正能用的 agent。完整程序见 [`examples/openai-compat`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/openai-compat/main.go)。

## 加上重试

供应商会抖。包一层就行：

```go
import "github.com/iceymoss/agent-runtime-go/providers/retry"

model := retry.New(openaicompat.New(baseURL, apiKey), retry.Options{})
```

零值 `Options{}` 是 3 次、200ms 起、上限 30s、带抖动。供应商给了 `Retry-After` 就按它的来。

自己写这一层很容易写错：**只包住 `Stream()` 的返回值几乎什么都没重试到**。SSE 请求通常会成功——HTTP 响应到了、channel 也拿到了——然后连接在中途断掉，这个失败出现在流**里面**，包装器根本看不见。这个包两种都重试。

它也**不会**在已经向下游交付过内容之后重试：那时候接一个新流上去，要么让运行时的一致性校验失败，要么更糟——通过了但文本重复了一遍。这种情况错误原样上报，由你决定是否整轮重跑。

## 多个模型

一个应用常常要用不止一个模型：主力模型回答，便宜模型做摘要。`provider` 子包管理这个目录，按角色取模型，凭据由你显式传入：

```go
import "github.com/iceymoss/agent-runtime-go/provider"
```

只用一个模型的话不需要它，直接持有 `agent.Model` 就够了。

## Prompt 要有版本

把 system prompt 硬编码成字符串字面量，改一次就没人知道线上跑的是哪一版。`prompt` 子包给模板和版本号，运行结果里可以记下用的是哪个版本：

```go
import "github.com/iceymoss/agent-runtime-go/prompt"
```

同样是可选的——第 1 章那样直接 `agent.NewSystemMessage("...")` 完全能跑，等你需要回答"上周那次运行用的是哪版 prompt"时再引入。

## 需要注意的

**`ModelName` 是发给上游的 `model` 字段**，所以同一个适配器实例可以服务多个用不同模型名的 Agent。

**端点不支持某个能力时用 `WithCapabilities` 收窄。** 适配器默认声明支持工具、三种 tool choice、结构化输出、推理内容。声明和现实不符时，运行时会在装配期拒绝非法请求——这比跑到一半才发现要好。

**凭据只能由你显式传入。** 适配器不读环境变量、不查配置文件。上面代码里的 `os.Getenv` 是**你的应用**在做这件事。

## 深入

- [openaicompat](../packages/openaicompat.md) —— 全部选项：非流式降级、自定义 header、超时与代理
- [retry](../packages/retry.md) —— 重试什么、不重试什么、为什么
- [provider](../packages/provider.md) —— 多模型目录与 factory
- [prompt](../packages/prompt.md) —— 模板与版本
