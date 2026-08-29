# providers/retry

## 是什么

把一个 `agent.Model` 包一层重试。

```go
import "github.com/iceymoss/agent-runtime-go/providers/retry"

model := retry.New(openaicompat.New(baseURL, apiKey), retry.Options{})
```

## 为什么需要它

自己写重试很容易写成只包住 `Stream()` 的返回值——而那几乎什么都没重试到。

SSE 请求通常**会成功**：HTTP 响应到达了，`Stream()` 正常返回 channel，然后连接在中途断掉。这个失败以 `agent.ChunkError` 的形式出现在流**里面**，不看流内部的包装器完全接不到它。也就是说，最常见的那类瞬时故障恰好是朴素实现漏掉的那一类。

这个包两种都重试，并且拒绝重试唯一一种重试会损坏结果的情况。

## 怎么用

```go
model := retry.New(openaicompat.New(baseURL, apiKey), retry.Options{
	MaxAttempts: 3,                      // 含首次；1 表示不重试
	BaseDelay:   200 * time.Millisecond, // 每次翻倍
	MaxDelay:    30 * time.Second,
})

runner, err := agent.New(agent.Config{Key: "assistant", ModelName: "gpt-4o-mini", MaxSteps: 8},
	model, registry)
```

零值 `Options{}` 使用默认值（3 次、200ms 起、上限 30s、最多 25% 抖动）。

### 什么会被重试

只有适配器**自己判定为可重试**的失败（`agent.ModelError.Retryable`）。分类是适配器的职责：它知道 429 和连接断开值得再来一次，而非法请求不值得。从错误文本去猜会把非法请求永远重试下去。

供应商给了 `Retry-After` 时以它为准，即使超过 `MaxDelay`——供应商比退避曲线更清楚自己什么时候能接活。

### 什么不会被重试

**已经向下游交付过分片之后不再重试。** 运行时会把看到的分片累加起来和终态 response 比对，把一个新的流接在半截流后面，要么会让这个校验失败，要么更糟——校验通过但文本重复了一遍。所以这种情况下错误被原样转发，由上层决定怎么办。

这意味着重试对"刚建立连接就断"最有效，对"输出到一半断掉"无能为力。后者需要的是应用层重跑整轮，而不是在流中间打补丁。

### 可选能力会被保留

如果被包的模型实现了 `agent.Generator`（非流式生成），包装结果也实现它；如果没有，包装结果也不会假装有。类型断言拿到的东西和不包装时一致，包装不会悄悄拿掉下游依赖的能力。

## 常见问题

**Q: 重试期间会不会同时开两个上游请求？**
不会。被放弃的那次 attempt 会先被取消并排空，下一次才开始。

**Q: `Sleep` 和 `Jitter` 是给谁用的？**
测试。生产留空即可；测试里换成不真正等待的实现，就不用为了验证退避行为付出真实时间。

**Q: 运行被取消时会怎样？**
退避等待受 context 约束，取消会立刻结束等待并把错误交给下游，不会让一个没人等待的运行继续付费重试。
