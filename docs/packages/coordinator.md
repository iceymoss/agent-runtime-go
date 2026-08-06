# coordinator

`coordinator` 包把"选哪个 agent"（Selector）解析为一份不可变的可执行 agent 定义，并且只持久化规范化的数据清单（manifest）——可执行对象永远留在内存里，需要时按清单逐项验证后重建。它解决的是"按历史某一代精确回放 agent 配置"的问题。

## 是什么

核心类型是 `Coordinator`（由 `New(Options)` 构造），它依赖三个应用侧端口：

```go
// Builder 由应用实现：配方、凭证、提示词、工具、Skills/MCP 目录都是应用的事。
type Builder interface {
    Build(context.Context, BuildRequest) (BuildResult, error)
}

// Reconstructor 按清单中记录的精确租户/generation/digest 重建可执行定义。
type Reconstructor interface {
    Reconstruct(context.Context, provider.Scope, ArtifactManifest) (Reconstruction, error)
}

type ManifestStore interface {
    SaveGeneration(context.Context, StoredGeneration, RetentionPin) error
    ResolveGeneration(context.Context, provider.Scope, string) (StoredGeneration, error)
    PinGeneration(context.Context, provider.Scope, string, RetentionPin) error
    ReleasePin(context.Context, provider.Scope, string, string) error
}
```

两条主路径：`Resolve(ctx, ResolveRequest)` 走 Builder 构建最新定义，把 `ManifestWire`（纯数据：配方版本、模型标识、提示词消息、工件引用列表等）封装成带摘要的 `ArtifactManifest` 存入 `ManifestStore`，返回 `ResolvedRuntime`；`ResolveGeneration(ctx, scope, definitionDigest)` 则按定义摘要取回历史清单，验证后交给 Reconstructor 重建出与当年逐字节等价的定义。

关键设计是序列化红线：`BuildResult.Definition`、`ResolvedRuntime.Definition`、`SystemMessages` 等可执行字段全部标注 `json:"-"`，即使调用方误把结构体整个 `json.Marshal` 也不会泄露可执行体；清单里只允许出现能被摘要校验的数据（外部测试专门断言了这一点）。包内置 `NewMemoryManifestStore()` 作为 `ManifestStore` 参考实现。

## 为什么需要它

一次 agent 运行的行为由一大堆工件共同决定：配方、提示词、模型版本、Skills/MCP 目录代……排查线上问题或续跑历史会话时，你需要"当时那一代"而不是"现在最新的"。没有这个包，你要自己解决：清单的规范化与摘要（工件排序、深拷贝防别名）、存取时的防篡改验证（租户、摘要逐项比对）、重建结果与清单的一致性核验、并发重建去重、以及"这一代还有会话在用，不能删"的保留语义。Coordinator 把这些收敛为：清单进出都过 `ValidateArtifactManifest`，重建结果的定义摘要/工件身份/提示词与清单不一致一律拒绝，同租户同清单的并发重建做 singleflight（外部测试验证 32 个并发只触发 1 次 `Reconstruct`），`RetentionPin` 保证被引用的代不会被回收。

什么时候不需要它：agent 定义是编译期写死的、不存在"按代回放"需求，直接每次现场构造 `agent.RuntimeDefinition` 即可。

## 怎么用

`Builder` 和 `Reconstructor` 是应用的核心资产（要接触你的配方库、模型凭证、工具注册表），下面示例假设它们已实现，展示 Coordinator 侧的完整调用流程：

```go
package main

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go/coordinator"
	"github.com/iceymoss/agent-runtime-go/provider"
)

func resolveAndReplay(ctx context.Context, builder coordinator.Builder, reconstructor coordinator.Reconstructor) error {
	resolver, err := coordinator.New(coordinator.Options{
		Builder:       builder,
		Reconstructor: reconstructor,
		Artifacts:     coordinator.NewMemoryManifestStore(),
	})
	if err != nil {
		return err
	}
	defer resolver.Close()

	// 路径一：构建最新一代，清单自动持久化。
	resolved, err := resolver.Resolve(ctx, coordinator.ResolveRequest{
		Selector:  coordinator.Selector{TenantKey: "tenant-a", AgentKey: "coder"},
		ModelRole: provider.RolePrimary,
	})
	if err != nil {
		return err
	}
	fmt.Println("definition digest:", resolved.DefinitionDigest)
	// resolved.Definition 即可执行的 *agent.RuntimeDefinition，直接交给会话层运行。

	// 路径二：之后任何时刻按摘要精确回放这一代。
	replayed, err := resolver.ResolveGeneration(ctx,
		provider.Scope{TenantKey: "tenant-a"}, resolved.DefinitionDigest)
	if err != nil {
		return err
	}
	fmt.Println("replayed catalog generation:", replayed.CatalogGeneration)
	return nil
}
```

关键行为：

- `New` 要求 `Builder`、`Reconstructor`、`Artifacts` 三者齐备，否则返回 `ErrInvalidRequest`。`Clock` 可选，默认 `time.Now`。
- `Resolve` 会规范化请求：`TenantKey` 必须有效、`AgentKey` 非空；`ModelRole` 留空默认 `provider.RolePrimary`，只接受 primary/utility/summary/title 四种；`Selector.Values` 的 key 必须非空且唯一。Builder 返回的 `Wire.Role` 与请求角色不一致会被拒绝（`ErrDefinitionInvalid`）。
- Coordinator 会核验 Builder 的返回：`Wire.DefinitionDigest` 必须等于 `Definition.ArtifactVersions().Definition`，`SystemMessages` 必须与 `Wire.PromptMessages` 摘要一致，防止"存的清单"和"给的可执行体"说两套话。
- `ResolveGeneration` 每一步都验证：清单租户必须等于请求租户、`DefinitionDigest` 必须匹配、`ValidateArtifactManifest` 必须通过、重建结果的工件身份与提示词必须与清单一致。任何一步失败统一归类为 `ErrGenerationUnavailable`（可重试）而不是返回可疑数据。
- 返回值总是 `Clone()` 过的：切片是防御性拷贝，`Definition` 指针共享（它本身不可变）。

## 常见问题

**Q: 为什么 `json.Marshal(BuildResult)` 里看不到 Definition？**
A: 有意为之。可执行定义含模型客户端、工具闭包，序列化它既无意义又危险，所以 `Definition`、`SystemMessages` 都是 `json:"-"`。持久化的唯一载体是 `ArtifactManifest`，其内容全部可被 `CanonicalDigest` 校验。

**Q: `ResolveGeneration` 报 `ErrGenerationUnavailable`，但存储里明明有这条记录？**
A: 这个错误覆盖了所有"不能安全交付"的情形：跨租户访问、`ManifestDigest` 与内容不符（存储被篡改，外部测试用 tamper store 验证）、清单校验失败、Reconstructor 重建结果与清单不一致。先检查请求的 `provider.Scope.TenantKey` 是否与写入时一致，再核对存储层有没有改动过清单字段。

**Q: `NewArtifactManifest` 报 `ErrManifestInvalid`，哪些字段是必填的？**
A: `SchemaVersion` 必须等于 `CurrentSchemaVersion`(1)；`DefinitionDigest`、`RecipeKey`、`RecipeVersion`、`Provider`、`Model`、`ModelVersion`、`CatalogGeneration`、`OptionsVersion` 全部非空；`Execution.StopConditions` 必须为空（可执行的停止条件不允许进清单）；工件引用五元组（Kind/Key/Generation/Digest/SchemaVersion）齐全、无重复；`RootToolNames` 需按字典序且以 `RootToolsDeclared: true` 声明。

**Q: 怎么防止正在被会话引用的代被清理掉？**
A: 用 `RetentionPin`：`SaveGeneration` 可附带初始 pin，之后用 `PinGeneration` / `ReleasePin` 增减。`MemoryManifestStore.RetireGeneration` 在仍有 pin 时返回 `ErrRetentionPinned`；同一 `PinKey` 被不同 `OwnerKey` 抢占返回 `ErrInvariantConflict`；`ReleasePin` 幂等，重复释放不报错。

**Q: 高并发下同一代会被重建多少次？**
A: 一次。同租户同清单摘要的并发 `ResolveGeneration` 共享一个 in-flight 条目（singleflight），其余调用等待结果；重建失败的条目会从缓存移除，下次调用可以重试。

**Q: `Resolve` 报 `ErrCapabilityUnavailable` 是 Coordinator 的问题吗？**
A: 不是，这是 Builder 返回错误时的统一包装（`Retryable: true`），根因在应用的构建逻辑里，用 `errors.As(*coordinator.Error)` 取 `Cause` 排查。
