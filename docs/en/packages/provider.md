# provider

The `provider` package provides a provider-neutral model catalog and model factory abstraction: it freezes "which models a tenant can use right now, and what the parameter bounds are" into an immutable catalog snapshot, then uses a `Factory` to build executable `agent.Model` instances from descriptors in that snapshot.

## What it is

The package has three layers of abstraction:

1. **Description layer**: `ModelRef` (`Provider` + `Model` pair) locates a model; `ModelDescriptor` describes its version, context window, default generation options (`agent.GenerationOptions`), capability flags (`agent.Capabilities`), and optional pricing (`Pricing`); `ProviderDescriptor` groups multiple models under one provider.
2. **Snapshot layer**: `CatalogSnapshot` is the immutable result of one catalog publication, with a `Generation` (label) and `CreatedAt`. It can only be created via the constructor `NewCatalogSnapshot`, which validates fully, sorts (providers by ID, models by name), and deep-copies inputs; every subsequent read (`Provider`, `Model`, `ProvidersList`, `Clone`) also returns deep copies, so callers cannot mutate internal state.
3. **Build layer**: `CatalogSource` is the "get current snapshot" interface; `Factory` turns a `BuildRequest` (snapshot descriptor + role + selected options) into a real `agent.Model`.

```go
type CatalogSource interface {
	Snapshot(context.Context, Scope) (CatalogSnapshot, error)
}

type Factory interface {
	Build(context.Context, BuildRequest) (agent.Model, error)
}
```

The package ships a ready-to-use `MemoryCatalog`: isolated by tenant (`Scope{TenantKey}`), stored by exact generation, `Publish` publishes a new generation, `Current` returns the current one, `Get` returns a historical exact generation. It implements `CatalogSource` (`Snapshot` is equivalent to `Current`).

Role constants `RolePrimary`, `RoleUtility`, `RoleSummary`, and `RoleTitle` declare the model purpose in `BuildRequest.Role` so `Factory` implementations can differentiate (for example, picking a cheaper tier for summarization).

## Why you need it

Without this package you have to solve three problems yourself:

- **Config drift**: if model lists and defaults live in scattered config files or env vars, two requests in the same session may see inconsistent config. `CatalogSnapshot` pins "the full catalog at a moment" with a generation; historical generations can always be fetched by label (`MemoryCatalog.Get`) and are never overwritten by later publishes.
- **Out-of-range parameters**: temperature set to 3, or `DefaultMaxTokens` greater than `ContextWindow`, is expensive to debug if it only surfaces at the provider API. `NewCatalogSnapshot` rejects all inconsistencies at construction time (see FAQ below).
- **Multi-tenant isolation**: different tenants have different available models. `MemoryCatalog` uses `agent.TenantKey` as the first-level key; cross-tenant reads return `ErrCatalogUnavailable` and never leak another tenant's catalog.

**When you do not need it**: if your app has a single hard-coded model, one tenant, and config never hot-reloads, construct `agent.Model` directly (for example `providers/openaicompat`)—no catalog layer required.

## How to use it

The example below builds a single-model catalog snapshot, publishes it to the in-memory catalog, then reads the model descriptor back:

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

Output:

```text
generation-1 true 128000
```

Key behavior:

- `NewCatalogSnapshot` deep-copies its inputs. Mutating the passed `[]ProviderDescriptor` after construction (including `Pricing` pointers and pointer fields inside `DefaultOptions`) does not change the snapshot; mutating values read back does not write through either.
- An empty model `Ref.Provider` is filled with the owning provider's ID; a non-empty value that disagrees with the owning provider is an error.
- `Publish` is idempotent for the same generation: republishing a snapshot with identical content (same canonical digest and equal `CreatedAt`) succeeds; different content returns `ErrCatalogConflict`. Published generations are never rewritten.
- `Current` returns `ErrCatalogUnavailable` if that tenant has never published a catalog.
- After you have a `ModelDescriptor`, put it with `Role` and `SelectedOptions` into a `BuildRequest` and pass it to your own `Factory.Build` to construct an `agent.Model`. The package does not ship any concrete provider `Factory` implementations.

## FAQ

**Q: What inputs does `NewCatalogSnapshot` reject?**
A: Blank generation or zero `createdAt`; empty providers; provider missing ID or Version; duplicate providers; a provider with no models; duplicate models; model missing Version; `ContextWindow <= 0`, `DefaultMaxTokens <= 0`, or `DefaultMaxTokens > ContextWindow`; `DefaultOptions.MaxTokens` outside `(0, ContextWindow]`, `Temperature` outside `[0, 2]`, `TopP` outside `[0, 1]`; `Capabilities.Validate()` failure (for example declaring `ToolChoiceNamed` without `Tools`). All match `ErrCatalogInvalid` via `errors.Is`.

**Q: What validation applies to `Pricing` fields?**
A: Once `Pricing` is non-nil, `Currency` and `Version` are required; the four unit-price fields (`InputPerMillion` and so on) may be empty, but if set must be non-negative decimal digit strings (parsed with `big.Rat` internally). Prices use strings instead of float to keep floating-point precision out of billing paths.

**Q: Why does republishing the same generation sometimes succeed and sometimes return `ErrCatalogConflict`?**
A: `Publish` is create-or-verify: same generation, same content → idempotent success and that generation becomes current; same generation, different content → conflict. If you change catalog content, you must use a new generation name.

**Q: After publishing generation-2, can I still read generation-1?**
A: Yes. `MemoryCatalog.Get(ctx, scope, "generation-1")` always returns the exact content from publish time; `Current` only points at the latest published generation. Running sessions can stay pinned to an older catalog generation and are unaffected by hot updates.

**Q: How should errors be classified?**
A: All errors are `*provider.Error` with a `Code` (such as `CodeCatalogInvalid`, `CodeCatalogConflict`, `CodeCatalogUnavailable`) and a `Retryable` field; they `Unwrap` to the matching sentinel (`ErrCatalogInvalid`, etc.) and work with both `errors.Is` and `errors.As`. `CodeProviderNotFound`, `CodeModelNotFound`, `CodeModelBuildFailed`, `CodeCapabilityMismatch`, and `CodeCredentialUnavailable` are for `Factory` implementers to reuse; the package itself does not return those.

**Q: Can `Scope.TenantKey` be empty?**
A: No. `Publish`/`Current`/`Get` all check `TenantKey.Valid()` first (non-empty string); invalid keys fail immediately. Single-tenant apps should still use a fixed constant key such as `"default"`.
