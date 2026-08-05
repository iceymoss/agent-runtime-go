# 模型提供商

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

`agent/provider` 不包含 vendor client。它拥有不可变 catalog、model descriptor、pricing 和 factory port：

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
