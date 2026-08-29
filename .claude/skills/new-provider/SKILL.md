---
name: new-provider
description: 新增一个 Model 适配器（实现 agent.Model 接口对接新模型协议）。当用户说接模型、新增 provider、对接 OpenAI/Anthropic、加适配器时触发。
---

新增 Model 适配器，把上游协议投影成运行时的规范契约。

## 步骤

### 0. 先确认真的需要新适配器
OpenAI 兼容端点（OpenAI / DeepSeek / Qwen / Kimi / vLLM / Ollama / OpenRouter ...）**一律复用** `providers/openaicompat`，通过 Option 调整：

```go
model := openaicompat.New(baseURL, apiKey,
	openaicompat.WithoutStreaming(),        // SSE 不可靠的端点
	openaicompat.WithCapabilities(caps),    // 能力与默认值不同
	openaicompat.WithHeader(k, v),          // 自定义请求头
	openaicompat.WithHTTPClient(client),    // 超时 / 代理 / transport
)
```

只有协议本身不同（如 Anthropic Messages API）才写新适配器，放 `providers/<name>/`。

### 1. 实现三个方法

```go
func (m *Model) Name() string { return m.model }

func (m *Model) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true, ImageInput: true /* ... */}
}

func (m *Model) Stream(ctx context.Context, req *agent.GenerateRequest) (<-chan agent.StreamChunk, error)
```

`Capabilities()` 是能力声明的唯一出口：不支持图片输入或 tool choice 就如实声明，运行时会在装配期/请求校验时拒绝非法请求，好过运行到一半报错。

### 2. Stream 的硬性规则
- 只发送规范化的 `StreamChunk`；`ChunkFinish` 之后不得再发任何 chunk
- tool call 分片必须在适配器内部拼装完整后再发出，不要把碎片交给运行时
- usage 归一化（含 cache token）；拿不到的字段留零值，不要估算
- 尊重 `ctx.Done()`，确保 channel 一定会 close（`defer close(chunks)`）

### 3. 错误分类
所有上游失败包装成 `agent.ModelError`：

```go
return agent.NewModelError(kind, retryable, httpStatus, retryAfter, safeDetail, cause)
```

`safeDetail` **不得**包含 API key、完整 URL、请求体或堆栈。`retryable` 要与上游语义一致（429/5xx 可重试，4xx 参数错不可重试）。

### 4. 凭据
只从调用方显式传入。适配器不读环境变量、不查配置文件、不做凭据发现。

### 5. 一致性测试（必须）

```go
func TestModel(t *testing.T) {
	agenttest.TestModel(t, func(t *testing.T, c agenttest.ModelCase) agent.Model {
		// 按 case 返回接了 httptest fixture 的适配器
	})
}
```

测试用 `httptest` fixture，不得依赖网络和真实凭据。测试文件用 `package <name>_test`。

### 6. 文档与 CHANGELOG
新增 `docs/packages/<name>.md` 和 `docs/en/packages/<name>.md`（是什么 → 为什么需要 → 怎么用 → FAQ），在 `docs-site/.vitepress/config.*` 加侧边栏，更新两个 README 的适配器说明，并在 `CHANGELOG.md` 的 `## [Unreleased]` 记一条。

### 7. 验证
跑完整 CI 门槛（见 `/gotest`）。

## 严格约束
- 适配器只做协议投影，不承载业务逻辑、不做提示词拼装
- 不新增第三方依赖（根模块只有两个直接依赖），用标准库 `net/http` + `encoding/json`
- 不在适配器里打印日志或写文件；错误通过 `ModelError` 上报
- `providers/` 下的包属于生产包，不得 import `agenttest`（只能在 `_test.go` 里用）
