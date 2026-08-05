# Provider Adapter

只有 `agent.Model` 的实现与具体供应商协议耦合。Runtime、Tool 和业务 facade 都只使用 canonical contract。

## 何时使用 Provider 子包

只有一个固定 endpoint 时，直接构造 `agent.Model` 即可。出现多供应商、模型角色、动态 credential、价格元数据或 generation 固定需求时，再增加 `provider` catalog 和 factory。

## Root Port

供应商 adapter 实现：

```go
type Model interface {
    Name() string
    Capabilities() Capabilities
    Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}
```

adapter 必须：

- 将 canonical messages 和 tool definitions 投影成供应商协议。
- 保留可选标量的 `nil` 与显式零值差异。
- 聚合供应商的 tool argument delta，输出完整 `ToolCall`。
- 归一化 finish reason 和 usage。
- 使用 `agent.NewModelError` 分类 transport、rate limit、auth、rejected 和 protocol errors。
- 保证增量 chunks 与最终 `Response` 完全一致。

iCoder 在 `provider.go` 实现一个小型 OpenAI-compatible Chat Completions adapter。它使用普通 HTTP response，再生成 canonical chunks，因此适合展示 wire mapping，但不等于生产流式实现。

## Provider Catalog

`provider` 不包含 vendor client。它拥有不可变 catalog、model descriptor、pricing 和 factory port：

```go
catalog, err := provider.NewCatalogSnapshot(generation, now, descriptors)
descriptor, ok := catalog.Model(provider.ModelRef{
    Provider: "openai-compatible",
    Model:    "model-id",
})
```

只有一个 endpoint 时可直接构造 `agent.Model`。需要多供应商、多模型角色、tenant credential resolution 或精确 catalog generation 时，再实现 `provider.Factory`。

## Adapter Tests

外部 adapter 应使用 test-only `agenttest.TestModel`。它验证终态、usage、错误分类和 cancellation。生产代码不能导入 `agenttest`。

## 常见错误

- 将供应商 DTO 暴露给 Tool 或业务 facade。
- 丢失 `nil` 与显式零值的差异。
- 在发送 `ChunkToolCall` 前没有拼完 argument delta。
- 最终 Response 与前面的 text/tool chunks 不一致。
- 将 rate limit、auth 或 cancellation 全部包装成普通字符串错误。
- 在 SDK 内读取 API Key；credential resolution 应留在应用 adapter。
