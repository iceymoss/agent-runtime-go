# coordinator

The `coordinator` package resolves "which agent to select" (Selector) into an immutable executable agent definition, and persists only a normalized data manifest—executable objects stay in memory and are rebuilt on demand by validating the manifest item by item. It solves exact replay of agent config for a historical generation.

## What it is

The core type is `Coordinator` (from `New(Options)`), depending on three application ports:

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

Two main paths: `Resolve(ctx, ResolveRequest)` uses Builder to build the latest definition, wraps `ManifestWire` (pure data: recipe version, model identity, prompt messages, artifact refs, etc.) as a digests-bearing `ArtifactManifest` into `ManifestStore`, and returns `ResolvedRuntime`; `ResolveGeneration(ctx, scope, definitionDigest)` loads the historical manifest by definition digest, validates it, and hands it to Reconstructor to rebuild a definition byte-equivalent to the original.

A hard serialization rule: executable fields such as `BuildResult.Definition`, `ResolvedRuntime.Definition`, and `SystemMessages` are all tagged `json:"-"`—even if a caller accidentally `json.Marshal`s the whole struct, executables are not leaked; the manifest may only contain data that can be digest-checked (external tests assert this). Built-in `NewMemoryManifestStore()` is the `ManifestStore` reference implementation.

## Why you need it

An agent run's behavior is jointly determined by many artifacts: recipe, prompt, model version, Skills/MCP catalog generation… When debugging production or resuming a historical session you need "that generation then," not "whatever is latest now." Without this package you would need: manifest normalization and digests (artifact ordering, deep copy against aliasing), tamper-resistant load/store validation (tenant and per-item digest checks), consistency checks between reconstruction and manifest, concurrent reconstruction dedupe, and retention so a generation still in use by sessions is not deleted. Coordinator converges this to: manifests go through `ValidateArtifactManifest` on the way in and out; reconstruction whose definition digest / artifact identity / prompts disagree with the manifest is rejected; concurrent reconstruction for the same tenant and manifest is singleflight (external tests verify 32 concurrent calls trigger `Reconstruct` once); `RetentionPin` keeps referenced generations from being reclaimed.

When you do not need it: if agent definitions are compile-time fixed and there is no "replay by generation" need, construct `agent.RuntimeDefinition` on the spot each time.

## How to use it

`Builder` and `Reconstructor` are core application assets (they touch your recipe store, model credentials, and tool registry). The example below assumes they are implemented and shows the full Coordinator-side call flow:

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

Key behavior:

- `New` requires `Builder`, `Reconstructor`, and `Artifacts`; otherwise `ErrInvalidRequest`. `Clock` is optional (default `time.Now`).
- `Resolve` normalizes the request: `TenantKey` must be valid, `AgentKey` non-empty; empty `ModelRole` defaults to `provider.RolePrimary` and only accepts primary/utility/summary/title; `Selector.Values` keys must be non-empty and unique. Builder `Wire.Role` that disagrees with the request role is rejected (`ErrDefinitionInvalid`).
- Coordinator checks Builder output: `Wire.DefinitionDigest` must equal `Definition.ArtifactVersions().Definition`, and `SystemMessages` must digest-match `Wire.PromptMessages`, so the stored manifest and the executable body cannot disagree.
- `ResolveGeneration` validates every step: manifest tenant must equal request tenant, `DefinitionDigest` must match, `ValidateArtifactManifest` must pass, and reconstructed artifact identity and prompts must match the manifest. Any failure is classified as `ErrGenerationUnavailable` (retryable) rather than returning suspicious data.
- Return values are always `Clone()`d: slices are defensive copies; the `Definition` pointer is shared (it is itself immutable).

## FAQ

**Q: Why is Definition missing from `json.Marshal(BuildResult)`?**
A: Intentional. Executable definitions hold model clients and tool closures—serializing them is both useless and dangerous—so `Definition` and `SystemMessages` are `json:"-"`. The only persistence vehicle is `ArtifactManifest`, whose contents are fully checkable via `CanonicalDigest`.

**Q: `ResolveGeneration` returns `ErrGenerationUnavailable`, but the store has the record?**
A: This error covers every "cannot safely deliver" case: cross-tenant access, `ManifestDigest` mismatch with content (tampered store—external tests use a tamper store), manifest validation failure, or Reconstructor output disagreeing with the manifest. Check that `provider.Scope.TenantKey` matches what was written, then whether the storage layer altered manifest fields.

**Q: `NewArtifactManifest` returns `ErrManifestInvalid`—which fields are required?**
A: `SchemaVersion` must equal `CurrentSchemaVersion` (1); `DefinitionDigest`, `RecipeKey`, `RecipeVersion`, `Provider`, `Model`, `ModelVersion`, `CatalogGeneration`, and `OptionsVersion` must all be non-empty; `Execution.StopConditions` must be empty (executable stop conditions must not enter the manifest); artifact ref quintuples (Kind/Key/Generation/Digest/SchemaVersion) must be complete with no duplicates; `RootToolNames` must be lexicographically ordered and declared with `RootToolsDeclared: true`.

**Q: How do I keep a generation referenced by sessions from being cleaned up?**
A: Use `RetentionPin`: `SaveGeneration` can attach an initial pin; later use `PinGeneration` / `ReleasePin` to adjust. `MemoryManifestStore.RetireGeneration` returns `ErrRetentionPinned` while pins remain; the same `PinKey` claimed by a different `OwnerKey` returns `ErrInvariantConflict`; `ReleasePin` is idempotent—repeat releases do not error.

**Q: Under high concurrency, how many times is the same generation reconstructed?**
A: Once. Concurrent `ResolveGeneration` for the same tenant and manifest digest share one in-flight entry (singleflight); others wait for the result. Failed reconstruction entries are removed from cache so the next call can retry.

**Q: Is `Resolve` returning `ErrCapabilityUnavailable` a Coordinator bug?**
A: No—it is the uniform wrap when Builder returns an error (`Retryable: true`). The root cause is in application build logic; use `errors.As(*coordinator.Error)` to get `Cause`.
