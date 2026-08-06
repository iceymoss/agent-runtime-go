# skills

The `skills` package provides a tenant-isolated, immutable skill catalog: it loads "untrusted instruction content" from multiple sources, validates it, digests it, and publishes it by generation. It only handles storage and validation—**it never executes skill content and grants no tool permissions**.

## What it is

The core of this package is the `Catalog` interface (implemented by `*Manager` from `NewCatalog`). A Catalog aggregates multiple `Source`s (skill origins). Each `Refresh` loads, validates, and merges all sources into an immutable snapshot (`Snapshot`) with content digests, uniquely identified by `Generation` (content digest):

```go
type Source interface {
    Name() string
    Kind() SourceKind
    Load(context.Context, Scope) (SourceSnapshot, error)
}

type Catalog interface {
    StartScope(context.Context, Scope) error
    Current(Scope) (Snapshot, bool)
    Refresh(context.Context, Scope) (Snapshot, error)
    Resolve(ResolveRequest) (Selection, error)
    Read(context.Context, ReadRequest) (Resource, error)
    // 以及 CloseScope / Acquire / Revoke / Status / Close
}
```

Two built-in Source implementations: `NewFilesystemSource` (reads a directory; one subdirectory per skill containing a `SKILL.md` with YAML front matter) and `NewRepositorySource` (reads from a database implementing `Repository`). Sources have fixed priority by kind: `platform_filesystem` < `tenant_filesystem` < `tenant_db`. A higher-priority source that wants to override a same-named lower-priority skill must explicitly declare `Replaces` with a matching version range; otherwise the skill is blocked entirely and a `CodePrecedenceConflict` diagnostic is emitted.

All content is tenant-scoped: each `Scope{TenantKey}` starts (`StartScope`) and refreshes independently; skills are invisible across tenants.

## Why you need it

Skills are essentially "Markdown instructions from users or tenants"—classic untrusted input. Without this package you would need to handle: directory/path escape (symlink, hard link, `..`), TOCTOU (file replaced during read), size and format limits, multi-source merge and override conflicts, content-digest immutability of snapshots, and tenant isolation. This package builds those checks in:

- Filesystem sources reject symlink skill directories, reject hard-linked files, and use `os.OpenRoot` to confine reads to the root; any escape returns `ErrResourceEscape`;
- Every `Read` re-validates the content digest; mismatch with the snapshot returns `ErrResourceChanged`;
- Trust levels (`TrustPlatform` / `TrustTenantReviewed` / `TrustTenantUnreviewed`); by default only the first two are allowed, and non-platform sources declaring `platform` trust are rejected (`ErrTrustDenied`);
- Returned `Resource.Trusted` is always `false`, and `ContentClass` is always untrusted, reminding callers to treat content as prompt material, not executable code.

When you do not need it: if skills are compile-time constants, tenants cannot upload them, and hot reload is unnecessary, embed instructions in the system prompt and skip Catalog.

## How to use it

Skill directory layout: `<root>/<skill-name>/SKILL.md`; front matter `name` must match the directory name:

```markdown
---
name: code-review
description: 审查代码变更并给出改进建议
version: 1.0.0
schema_version: 1
---
这里是指令正文（Markdown）。
```

Minimal working example:

```go
package main

import (
	"context"
	"fmt"
	"log"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/skills"
)

func main() {
	ctx := context.Background()

	source := skills.NewFilesystemSource(skills.FilesystemOptions{
		Name:       "platform",
		Kind:       skills.SourcePlatformFilesystem,
		Root:       "./skills-root",
		Generation: "v1",
	})

	catalog, err := skills.NewCatalog(skills.Options{
		Sources: []skills.SourceRegistration{{Source: source, Required: true}},
	})
	if err != nil {
		log.Fatal(err)
	}
	defer catalog.Close(ctx)

	scope := skills.Scope{TenantKey: agent.TenantKey("tenant-a")}
	if err := catalog.StartScope(ctx, scope); err != nil {
		log.Fatal(err)
	}

	snapshot, ok := catalog.Current(scope)
	if !ok {
		log.Fatal("catalog not ready")
	}

	selection, err := catalog.Resolve(skills.ResolveRequest{
		Scope:      scope,
		Generation: snapshot.Generation,
		Selectors:  []skills.Selector{{Key: "code-review"}},
	})
	if err != nil {
		log.Fatal(err)
	}

	resource, err := catalog.Read(ctx, skills.ReadRequest{
		Scope:      scope,
		Generation: snapshot.Generation,
		Skill:      "code-review",
		Version:    selection.Skills[0].Version,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("trusted=%v class=%s\n%s\n", resource.Trusted, resource.ContentClass, resource.Content)
}
```

Output:

```text
trusted=false class=untrusted_instructions
这里是指令正文（Markdown）。
```

Key behavior:

- `NewCatalog` loads nothing; `StartScope` triggers the first load for that tenant only. Construction validates sources: at least one; `Kind` and `Name` must be unique.
- Empty `Options.AllowedTrust` equals `[TrustPlatform, TrustTenantReviewed]`; to accept unreviewed tenant skills you must explicitly add `TrustTenantUnreviewed`.
- Both `Resolve` and `Read` require an explicit `Generation` (from `Current`/`Refresh`/`Acquire`) so the skill set stays consistent within a session; `Read` also requires an explicit `Version`.
- Validation failures do not fail the whole snapshot—they go into `Snapshot.Diagnostics`; when debugging "why is this skill missing?", check there first.
- To pin a generation across calls, use `Acquire(scope, generation)` for a `*Lease`, then `Release()`; unreferenced old generations are reclaimed after refresh.

## FAQ

**Q: A skill is missing from the snapshot with no error?**
A: Validation failures degrade silently into `Diagnostics` entries. Common causes: skill name does not match `^[a-z0-9]+(-[a-z0-9]+)*$` (lowercase alphanumerics and hyphens), `SKILL.md` `name` differs from the directory name, `version` is not strict semver, `schema_version` is not `1` (`CurrentSchemaVersion`), or `description` is empty or over 1024 bytes.

**Q: A tenant-DB skill and a platform skill share a name—why are both unavailable?**
A: Higher-priority override of a lower-priority skill requires an explicit `Replaces: &Replacement{Key: "same-key", VersionRange: "=1.0.0"}` matching the overridden version; otherwise a `CodePrecedenceConflict` diagnostic is emitted and that key is blocked entirely, preventing silent hijacking of platform skills. Tenant DB sources can use `Tombstone` to explicitly block lower-layer skills.

**Q: What does `Read` returning `ErrResourceChanged` mean?**
A: Filesystem sources re-read the file on every read and compare against the snapshot SHA-256 digest; if the file changed after the snapshot, the read is rejected. This is TOCTOU protection—`Refresh` to get a new generation.

**Q: Why `ErrResourceEscape`? My skill directory is a symlink.**
A: Intentional. Symlink skill directories, any symlink path segment inside, or hard-linked artifact files are rejected so files outside the root (e.g. `/etc/passwd`) cannot be exposed as skill content. Artifact `path` must also be a clean relative path without `..`, `\`, or absolute prefixes.

**Q: `Resolve` returns `ErrSkillNotFound`, but the skill is visible under another tenant?**
A: Catalog is strictly tenant-isolated; different `Scope.TenantKey` values mean fully independent snapshots. Cross-tenant lookup is treated as not found. External tests lock this behavior.

**Q: How do I enable unreviewed tenant skills (`tenant_unreviewed`)? Is it safe?**
A: Explicitly list all three levels in `Options.AllowedTrust`. The package only guarantees content integrity and provenance labeling (`Provenance`, `Trust` fields)—it does not review instruction semantics. Before injecting unreviewed instructions into prompts, the application should isolate display or require secondary confirmation.
