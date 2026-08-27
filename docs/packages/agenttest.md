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

运行输出：

```text
=== RUN   TestConformance
=== RUN   TestConformance/valid_stream
=== RUN   TestConformance/missing_terminal
...（其余 case 的 RUN 行略）
--- PASS: TestConformance (0.12s)
    --- PASS: TestConformance/valid_stream (0.03s)
    --- PASS: TestConformance/missing_terminal (0.00s)
    ...（after_terminal / invalid_usage / rejected / auth / rate_limit / transport / provider_detail 均 PASS）
    --- PASS: TestConformance/cancellation (0.02s)
PASS
ok  	example/myprovider	0.127s
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

运行输出：

```text
=== RUN   TestMyTool
=== RUN   TestMyTool/echoes_input
--- PASS: TestMyTool (0.00s)
    --- PASS: TestMyTool/echoes_input (0.00s)
PASS
ok  	example/tooltest	0.006s
```

## 持久化端口一致性套件

除了 Model 和 Tool，`agenttest` 还为运行时的**持久化端口**提供了一致性套件。它们的存在理由和上面一样，但更重要：这些端口的实现错了不会报错，只会在崩溃、并发或重试的那一刻悄悄丢工作、重复副作用或多算一次钱。

| 套件 | 验证的端口 | 典型失败模式 |
|---|---|---|
| `TestCheckpointStore` | `agent.CheckpointStore` | 已提交的工具被重跑；过期 guard 仍能写入 |
| `TestDurableStore` | `durable.Store` + `ExecutionLedger` + `UsageLedger` | 僵尸 worker 覆盖新 worker；unknown 副作用被自动重放；用量重复计费 |
| `TestEventStore` | `event.Store` | 序号分配两次导致 replay 跳历史；非当前租约也能 ack 从而丢事件；重试延迟不生效变成热循环 |
| `TestToolExecutionLedger` | `tool.ExecutionLedger` | 一次授权的调用两次越过副作用边界；被拒绝或状态未知的执行仍被执行 |
| `TestPermissionStore` | `permission.Store` | 过期审批仍可批准；grant 授权了它没被授权的调用 |
| `TestMessageService` | `message.Service` | 工具结果丢失导致下一轮上下文不合法；redact 变成删除从而重排历史 |
| `TestSubagentStore` | `subagent.Store` | depth / fanout / 树预算被放宽；child 被跑两次 |
| `TestSessionService` / `TestSessionRunStore` | `session.Service` / `session.Store` | 分支重复 merge 导致 turn 重复；claim 被同时发给两个 worker |
| `TestManifestStore` | `coordinator.ManifestStore` | 一个 definition digest 描述了两套组合；跨租户解析出别人的 generation |

每个套件都接受一个工厂函数，并在**每个子测试**里要一个全新的空实现：

```go
func TestMyDurableStoreConformance(t *testing.T) {
	agenttest.TestDurableStore(t, func(t *testing.T) agenttest.DurableStore {
		return openMyStore(t)   // 每次返回一个干净实例
	})
}
```

`permission` 的工厂多接一个时钟参数，因为过期是契约的一部分，用真实时间没法确定性地测：

```go
agenttest.TestPermissionStore(t, func(t *testing.T, clock permission.Clock) permission.Store {
	return openMyApprovalStore(t, clock.Now)
})
```

库自带的内存参考实现全部跑这些套件（见各包的 `conformance_external_test.go`），所以**套件和参考实现不会各自漂移**。`demo/icoder` 的 SQLite 适配器跑的是同一份套件。

套件只断言端口承诺的可观察行为，不假设任何具体的状态/相位策略、存储结构或错误文本——判定一律用 `errors.Is` 匹配包级哨兵错误。

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
