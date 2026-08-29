# 12. 测试你写的适配器

## 你现在遇到的问题

你写了一个自己的 `Model` 适配器（比如接 Anthropic 或 Bedrock），或者把某个存储端口接到了自己的数据库。它能跑——但"能跑"和"符合契约"是两回事，而契约里那些只在崩溃、并发、重试时才暴露的部分，你很难自己想全。

## 用库自带的一致性套件

`agenttest` 提供可复用的验收套件，你的适配器和库自带的参考实现按**同一份契约**验收：

```go
import "github.com/iceymoss/agent-runtime-go/agenttest"

func TestMyModelConformance(t *testing.T) {
	agenttest.TestModel(t, func(t *testing.T, testCase agenttest.ModelCase) agent.Model {
		// 按 testCase 造一个对应行为的上游，返回你的适配器
	})
}
```

覆盖的端口：`TestModel`、`TestTool`、`TestCheckpointStore`、`TestDurableStore`、`TestEventStore`、`TestToolExecutionLedger`、`TestPermissionStore`、`TestMessageService`、`TestSubagentStore`、`TestSessionService`、`TestSessionRunStore`、`TestManifestStore`。

这不是形式主义。库自己的 `providers/openaicompat` 接上 `TestModel` 时抓出了三个真实缺陷：`Retry-After` 从来没被解析、供应商的错误信息被丢掉、流式路径不校验 usage。写这些 fixture 的过程本身就在逼你面对那些平时想不到的边界。

`agenttest` 只能被测试代码引用——生产包引用会被 `deps_test.go` 拦下。

## 深入

- [agenttest](../packages/agenttest.md) —— 每套套件覆盖什么、怎么写 fixture
