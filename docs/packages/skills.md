# skills

`skills` 包提供一个租户隔离、不可变的技能目录（Catalog）：从多个来源加载"未受信任的指令内容"，统一校验、计算摘要、按代（generation）发布。它只负责存取与校验，**从不执行技能内容，也不授予任何工具权限**。

## 是什么

这个包的核心是 `Catalog` 接口（由 `NewCatalog` 返回的 `*Manager` 实现）。一个 Catalog 聚合多个 `Source`（技能来源），每次 `Refresh` 会把所有来源加载、校验、合并成一个带内容摘要的不可变快照（`Snapshot`），快照由 `Generation`（内容摘要）唯一标识：

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

内置两种 Source 实现：`NewFilesystemSource`（从目录读取，每个技能一个子目录，内含带 YAML front matter 的 `SKILL.md`）和 `NewRepositorySource`（从实现了 `Repository` 接口的数据库读取）。来源按种类有固定优先级：`platform_filesystem` < `tenant_filesystem` < `tenant_db`，高优先级来源想覆盖低优先级的同名技能，必须显式声明 `Replaces` 且版本范围匹配，否则该技能会被整体屏蔽并产生 `CodePrecedenceConflict` 诊断。

所有内容都是租户作用域的：每个 `Scope{TenantKey}` 独立启动（`StartScope`）、独立刷新，租户之间的技能互不可见。

## 为什么需要它

技能本质是"用户或租户提供的 Markdown 指令"，属于典型的不受信任输入。没有这个包，你需要自己解决：目录/路径逃逸（symlink、硬链接、`..`）、TOCTOU（读取时文件被替换）、大小与格式限制、多来源合并与覆盖冲突、按内容摘要保证快照不可变、租户隔离。这个包把这些校验全部内置：

- 文件系统来源拒绝 symlink 技能目录、拒绝硬链接文件、用 `os.OpenRoot` 把读取限制在根目录内，任何逃逸返回 `ErrResourceEscape`；
- 每次 `Read` 都重新校验内容摘要，与快照记录不一致返回 `ErrResourceChanged`；
- 信任分级（`TrustPlatform` / `TrustTenantReviewed` / `TrustTenantUnreviewed`），默认只允许前两级，且非平台来源声明 `platform` 信任会被拒绝（`ErrTrustDenied`）；
- 返回的 `Resource.Trusted` 恒为 `false`，`ContentClass` 恒为 untrusted，提醒调用方把内容当提示词素材而非代码执行。

什么时候不需要它：如果技能是编译期写死的常量、不接受租户上传、也不需要热更新，直接把指令内嵌到系统提示词即可，不必引入 Catalog。

## 怎么用

技能目录结构：`<root>/<skill-name>/SKILL.md`，front matter 中 `name` 必须与目录名一致：

```markdown
---
name: code-review
description: 审查代码变更并给出改进建议
version: 1.0.0
schema_version: 1
---
这里是指令正文（Markdown）。
```

最小可用示例：

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

关键行为：

- `NewCatalog` 不加载任何数据；`StartScope` 才触发首次加载，且只加载该租户。构造时校验来源：至少一个、`Kind` 与 `Name` 均不可重复。
- `Options.AllowedTrust` 留空等价于 `[TrustPlatform, TrustTenantReviewed]`；要接受未审核的租户技能必须显式加入 `TrustTenantUnreviewed`。
- `Resolve` 与 `Read` 都要求显式传 `Generation`（来自 `Current`/`Refresh`/`Acquire`），保证一次会话内看到的技能集合一致；`Read` 还要求显式传 `Version`。
- 校验失败的技能不会让整个快照失败，而是进入 `Snapshot.Diagnostics`；排查"技能为什么没出现"应先看这里。
- 需要跨多次调用固定某一代时，用 `Acquire(scope, generation)` 拿 `*Lease`，用完 `Release()`；未被租约引用的旧代会在刷新后被回收。

## 常见问题

**Q: 技能没有出现在快照里，也没有报错？**
A: 校验失败会静默降级为 `Diagnostics` 条目。常见原因：技能名不符合 `^[a-z0-9]+(-[a-z0-9]+)*$`（小写字母数字加中划线）、`SKILL.md` 的 `name` 与目录名不一致、`version` 不是严格 semver、`schema_version` 不等于 `1`（`CurrentSchemaVersion`）、`description` 为空或超过 1024 字节。

**Q: 租户库里的技能和平台技能同名，为什么两个都不可用了？**
A: 高优先级来源覆盖低优先级技能必须显式声明 `Replaces: &Replacement{Key: "同名key", VersionRange: "=1.0.0"}` 且范围匹配被覆盖版本；否则产生 `CodePrecedenceConflict` 诊断并把该 key 整体屏蔽，防止静默劫持平台技能。租户 DB 来源可用 `Tombstone` 显式屏蔽下层技能。

**Q: `Read` 返回 `ErrResourceChanged` 是什么意思？**
A: 文件系统来源在每次读取时会重读文件并与快照记录的 SHA-256 摘要比对，文件在快照生成后被修改就会拒绝返回。这是防 TOCTOU 的设计，重新 `Refresh` 拿到新一代即可。

**Q: 为什么返回 `ErrResourceEscape`？我的技能目录是个软链接。**
A: 有意为之。技能目录本身、目录内任何路径分段是 symlink，或工件文件存在硬链接，都会被拒绝，防止把根目录外的文件（如 `/etc/passwd`）暴露成技能内容。工件 `path` 还必须是不含 `..`、`\`、绝对前缀的规范相对路径。

**Q: `Resolve` 报 `ErrSkillNotFound`，但技能明明在另一个租户下能查到？**
A: Catalog 是严格租户隔离的，`Scope.TenantKey` 不同就是两套完全独立的快照，跨租户查询按不存在处理。这是外部测试明确锁定的行为。

**Q: 未审核的租户技能（`tenant_unreviewed`）如何启用？启用后安全吗？**
A: 在 `Options.AllowedTrust` 显式列出三个级别即可。但注意包只保证内容完整性和来源标注（`Provenance`、`Trust` 字段），不会审查指令语义——把未审核指令注入提示词前，应用层应自行做隔离展示或二次确认。
