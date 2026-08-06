# agenttest

`agenttest` 包提供可复用的适配器一致性（conformance）测试套件：你写一个 provider 适配器或工具实现，它替你验证是否满足 `agent.Model` / `agent.Tool` 的公共契约。这是 test-only 包，只应出现在 `_test.go` 文件里，生产代码不得 import。

## 是什么

包里只有两个入口函数和它们的配套类型：

```go
type ModelFactory func(t *testing.T, testCase ModelCase) agent.Model

func TestModel(t *testing.T, factory ModelFactory)

type ToolCase struct {
	Name       string
	Invocation agent.ToolInvocation
	WantResult agent.ToolResult
	WantErr    error
}

func TestTool(t *testing.T, tool agent.Tool, cases []ToolCase)
```

`TestModel` 是模型侧套件。它按 `ModelCase` 枚举向你的工厂函数索要"背靠特定上游行为的适配器实例"：`ModelCaseValidStream`（正常流）、`ModelCaseMissingTerminal` / `ModelCaseAfterTerminal` / `ModelCaseInvalidUsage`（协议违规）、`ModelCaseRejected` / `ModelCaseAuth` / `ModelCaseRateLimit` / `ModelCaseTransport`（HTTP 错误分类）、`ModelCaseProviderDetail`（错误信息安全性）、`ModelCaseCancellation`（取消行为）。你的工厂负责为每个 case 搭一个假上游（通常是 `httptest.Server`），套件负责断言适配器的输出。

`TestTool` 是工具侧套件：校验 `Definition().Name` 非空、`ReplayPolicy()` 是 `agent.ReplayPolicyNever` / `ReplayPolicyIdempotent` / `ReplayPolicyResolve` 三者之一，然后逐条执行 `ToolCase`，用 `errors.Is` 匹配 `WantErr`、`reflect.DeepEqual` 匹配 `WantResult`。

## 为什么需要它

`agent.Model.Stream` 的契约细节很多：流必须关闭、失败要先发 `ChunkError` 再关流、终止 chunk 必须唯一且带完整 `Response`、HTTP 401/429/503 要映射到正确的 `agent.ModelErrorKind` 和 `Retryable`、取消必须返回 `context.Canceled` 而不能被包装成 `ModelError`。每个适配器作者各自手写这些断言，既重复又容易漏。这个包把契约固化成一份所有适配器共用的测试，`providers/openaicompat` 也是用它验收的。

**什么时候不需要它**：你不写自定义 `agent.Model` 适配器、也不需要批量验证工具实现时，不会用到这个包。它对业务逻辑测试没有帮助。

## 怎么用

模型侧的典型接法（工厂内按 case 搭假上游，此处省略 fixture 细节）：

```go
package myprovider_test

import (
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/agenttest"
)

func TestConformance(t *testing.T) {
	agenttest.TestModel(t, func(t *testing.T, testCase agenttest.ModelCase) agent.Model {
		server := newFixtureServer(t, testCase) // 按 case 返回对应响应的 httptest.Server
		return newAdapter(server.URL)
	})
}
```

工厂返回的适配器必须满足套件对每个 case 的预期：

- **`ModelCaseValidStream`**：`Name()` 非空；`Capabilities()` 通过 `Validate()` 且 `Tools`、`ToolChoiceNone`、`ToolChoiceRequired`、`ToolChoiceNamed`、`UsageDetails` 全为 `true`。流内容有硬编码约定——文本增量拼起来是 `"hello world"`；两个工具调用依次为 ID `call_a`、名字 `first`、输入 `{"a":0}` 和 ID `call_b`、名字 `second`、输入 `{"b":1}`；usage 为 `PromptTokens: 5, CompletionTokens: 4, TotalTokens: 11, CacheReadTokens: 2`；`FinishReason` 为 `agent.FinishToolCalls`。你的 fixture 上游必须产出恰好映射成这些值的响应。
- **协议违规三兄弟**：缺终止 chunk、终止后继续发数据、usage 不合法，都必须以单个 `ChunkError` 结束流，错误可 `errors.As` 出 `*agent.ModelError` 且 `Kind == agent.ModelErrorKindProtocol`、不可重试。
- **HTTP 错误分类**：400 → `ModelErrorKindRejected`（不可重试）；401 → `ModelErrorKindAuth`（不可重试）；429 → `ModelErrorKindRateLimit`（可重试，`RetryAfter` 恰为 2 秒，fixture 需返回 `Retry-After: 2`）；503 → `ModelErrorKindTransport`（可重试）。四类的 `Cause` 都不能为 nil。
- **`ModelCaseProviderDetail`**：`ModelError.SafeDetail` 和 `err.Error()` 必须包含 fixture 提供的 `safe-provider-detail`，而 `err.Error()` 不得泄露 `unsafe-cause-marker`——即 `Cause` 里的原始内容不能进入错误字符串。
- **`ModelCaseCancellation`**：上游挂起时取消 context，`Stream` 必须在 1 秒内返回满足 `errors.Is(err, context.Canceled)` 的错误，且不能被 `errors.As` 成 `*agent.ModelError`。

工具侧更直接：

```go
func TestMyTool(t *testing.T) {
	agenttest.TestTool(t, newEchoTool(), []agenttest.ToolCase{
		{
			Name:       "echoes input",
			Invocation: agent.ToolInvocation{CallID: "call-1", Name: "echo", RawInput: `{"text":"hi"}`},
			WantResult: agent.ToolResult{Content: "hi"},
		},
	})
}
```

## 常见问题

**Q: 套件发给模型的请求长什么样？**
A: 固定为 `Model: "conformance-model"`、一条 `agent.NewUserMessage("test")`、两个工具定义 `first` 和 `second`（参数 schema 均为 `{"type": "object"}`）。fixture 上游不需要理解请求内容，只需按 case 回放预设响应。

**Q: valid stream 的断言失败在 `Capabilities()` 上？**
A: 这是最常见的翻车点：套件要求 `Tools`、`ToolChoiceNone`、`ToolChoiceRequired`、`ToolChoiceNamed`、`UsageDetails` 五项全真。如果你的适配器按真实上游能力如实声明而缺了某项，valid stream case 会失败——给 conformance 测试用的适配器实例需要以完整能力配置构造。

**Q: 流卡住不动会怎样？**
A: 套件对"消费完整个流"设有 1 秒超时，超时直接 `t.Fatal("model stream did not close")`。适配器必须保证任何路径（成功、失败、取消）下都关闭 channel。

**Q: 失败 case 里能既发 `ChunkError` 又发 `ChunkFinish` 吗？**
A: 不能。失败流出现 `ChunkFinish` 或出现第二个 `ChunkError` 都会让测试失败。契约是：失败时恰好一个 `ChunkError`，然后关流。

**Q: `TestTool` 对 `WantErr` 怎么匹配？**
A: `errors.Is(err, tt.WantErr)`。期望成功就留 nil；期望特定哨兵错误就填那个哨兵。注意 `WantResult` 用 `reflect.DeepEqual` 严格比较整个 `agent.ToolResult`，包括 `IsError`、`Name` 等所有字段。
