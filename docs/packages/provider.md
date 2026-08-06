# provider

`provider` 包提供 provider 中立的模型目录（catalog）与模型工厂抽象：把"某个租户当前可用哪些模型、参数边界是什么"固化为不可变的目录快照，再通过 `Factory` 按快照中的描述构造出可执行的 `agent.Model`。

## 是什么

这个包的核心是三层抽象：

1. **描述层**：`ModelRef`（`Provider` + `Model` 二元组）定位一个模型；`ModelDescriptor` 描述它的版本、上下文窗口、默认生成参数（`agent.GenerationOptions`）、能力声明（`agent.Capabilities`）和可选定价（`Pricing`）；`ProviderDescriptor` 把同一 provider 的多个模型聚合在一起。
2. **快照层**：`CatalogSnapshot` 是一次目录发布的不可变结果，带有 `Generation`（代号）与 `CreatedAt`。只能通过构造函数 `NewCatalogSnapshot` 创建，构造时做全量校验、排序（provider 按 ID、model 按名字）并深拷贝输入；之后所有读取（`Provider`、`Model`、`ProvidersList`、`Clone`）也都返回深拷贝，调用方改不动内部状态。
3. **构建层**：`CatalogSource` 是"取当前快照"的接口，`Factory` 负责把一条 `BuildRequest`（快照里的描述 + 角色 + 选定参数）变成真正的 `agent.Model`。

```go
type CatalogSource interface {
	Snapshot(context.Context, Scope) (CatalogSnapshot, error)
}

type Factory interface {
	Build(context.Context, BuildRequest) (agent.Model, error)
}
```

包内自带一个可直接使用的 `MemoryCatalog`：按租户（`Scope{TenantKey}`）隔离，按 generation 精确存储，`Publish` 发布新代目录，`Current` 取当前代，`Get` 取历史精确代。它实现了 `CatalogSource`（`Snapshot` 等价于 `Current`）。

角色常量 `RolePrimary`、`RoleUtility`、`RoleSummary`、`RoleTitle` 用来在 `BuildRequest.Role` 里声明这次构建的模型用途，供 `Factory` 实现做差异化处理（比如摘要模型选便宜档位）。

## 为什么需要它

没有这个包，你需要自己解决三类问题：

- **配置漂移**：模型列表、默认参数如果散落在配置文件或环境变量里，同一个会话的两次请求可能读到不一致的配置。`CatalogSnapshot` 用 generation 把"某一时刻的完整目录"钉死，历史代永远可以按代号精确取回（`MemoryCatalog.Get`），不会被后续发布覆盖。
- **参数越界**：温度写成 3、`DefaultMaxTokens` 大于 `ContextWindow` 这类错误，如果等到调用 provider API 才暴露，排障成本很高。`NewCatalogSnapshot` 在构造期就拒绝所有不一致（见下文常见问题）。
- **多租户隔离**：不同租户可用的模型集合不同。`MemoryCatalog` 以 `agent.TenantKey` 为第一层 key，跨租户读取直接返回 `ErrCatalogUnavailable`，不会泄露其他租户的目录。

**什么时候不需要它**：如果你的应用只有一个写死的模型、单租户、配置从不热更新，直接构造 `agent.Model`（例如 `providers/openaicompat`）就够了，没必要引入目录层。

## 怎么用

下面的例子构造一个含单模型的目录快照，发布到内存目录，再读回模型描述：

```go
package main

import (
	"context"
	"fmt"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/provider"
)

func main() {
	snapshot, err := provider.NewCatalogSnapshot("generation-1", time.Now().UTC(), []provider.ProviderDescriptor{{
		ID:      "openai",
		Version: "provider-config-v1",
		Models: []provider.ModelDescriptor{{
			Ref:              provider.ModelRef{Provider: "openai", Model: "gpt-4o-mini"},
			Version:          "model-config-v1",
			ContextWindow:    128000,
			DefaultMaxTokens: 4096,
			Capabilities:     agent.Capabilities{Tools: true, ToolChoiceNone: true, ToolChoiceRequired: true, ToolChoiceNamed: true, UsageDetails: true},
		}},
	}})
	if err != nil {
		panic(err)
	}

	catalog := provider.NewMemoryCatalog()
	scope := provider.Scope{TenantKey: "tenant-a"}
	if err := catalog.Publish(context.Background(), scope, snapshot); err != nil {
		panic(err)
	}

	current, err := catalog.Current(context.Background(), scope)
	if err != nil {
		panic(err)
	}
	model, ok := current.Model(provider.ModelRef{Provider: "openai", Model: "gpt-4o-mini"})
	fmt.Println(current.Generation, ok, model.ContextWindow)
}
```

关键行为：

- `NewCatalogSnapshot` 会深拷贝入参。构造完成后再修改传入的 `[]ProviderDescriptor`（包括 `Pricing` 指针、`DefaultOptions` 里的指针字段）不影响快照内容；反过来读出来的值改了也不会写回快照。
- 模型的 `Ref.Provider` 留空时会自动填成所属 provider 的 ID；填了但和所属 provider 不一致会报错。
- `Publish` 对同一 generation 是幂等的：重发内容完全一致（canonical digest 相同且 `CreatedAt` 相等）的快照成功返回；内容不同则返回 `ErrCatalogConflict`，已发布的代永远不会被改写。
- `Current` 在该租户从未发布过任何目录时返回 `ErrCatalogUnavailable`。
- 拿到 `ModelDescriptor` 之后，把它连同 `Role`、`SelectedOptions` 填进 `BuildRequest`，交给你自己实现的 `Factory.Build` 去构造 `agent.Model`。包内不提供任何具体 provider 的 `Factory` 实现。

## 常见问题

**Q: `NewCatalogSnapshot` 会拒绝哪些输入？**
A: generation 为空白或 `createdAt` 为零值；providers 为空；provider 缺 ID 或 Version；provider 重复；某个 provider 没有模型；模型重复；模型缺 Version；`ContextWindow <= 0`、`DefaultMaxTokens <= 0` 或 `DefaultMaxTokens > ContextWindow`；`DefaultOptions.MaxTokens` 超出 `(0, ContextWindow]`、`Temperature` 超出 `[0, 2]`、`TopP` 超出 `[0, 1]`；`Capabilities.Validate()` 失败（例如声明了 `ToolChoiceNamed` 却没声明 `Tools`）。全部以 `ErrCatalogInvalid` 可被 `errors.Is` 匹配。

**Q: `Pricing` 字段有什么校验？**
A: 一旦 `Pricing` 非 nil，`Currency` 和 `Version` 必填；四个单价字段（`InputPerMillion` 等）允许留空，但填了就必须是非负十进制数字符串（内部用 `big.Rat` 解析）。价格用字符串而不是 float，是为了避免浮点精度问题进入计费路径。

**Q: 为什么重发同一个 generation 有时成功有时报 `ErrCatalogConflict`？**
A: `Publish` 的语义是 create-or-verify：同代同内容 → 幂等成功并把该代设为 current；同代不同内容 → 冲突。如果你改了目录内容，必须换一个新的 generation 名字。

**Q: 发布 generation-2 之后还能读 generation-1 吗？**
A: 能。`MemoryCatalog.Get(ctx, scope, "generation-1")` 永远返回发布时的精确内容；`Current` 只是指向最新发布的那一代。这保证正在运行的会话可以钉在旧代目录上不受热更新影响。

**Q: 错误怎么分类处理？**
A: 所有错误都是 `*provider.Error`，带 `Code`（如 `CodeCatalogInvalid`、`CodeCatalogConflict`、`CodeCatalogUnavailable`）和 `Retryable` 字段，`Unwrap` 到对应哨兵错误（`ErrCatalogInvalid` 等），用 `errors.Is`/`errors.As` 均可。`CodeProviderNotFound`、`CodeModelNotFound`、`CodeModelBuildFailed`、`CodeCapabilityMismatch`、`CodeCredentialUnavailable` 供 `Factory` 实现方复用，包内自身不返回这几类。

**Q: `Scope.TenantKey` 可以为空吗？**
A: 不可以。`Publish`/`Current`/`Get` 都先检查 `TenantKey.Valid()`（非空字符串），无效直接报错。单租户应用也要固定给一个常量 key，比如 `"default"`。
