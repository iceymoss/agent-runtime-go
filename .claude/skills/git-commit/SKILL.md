---
name: git-commit
description: 按本仓库历史规范生成 Git commit message 或安全创建提交。当用户说 git commit、提交代码、生成提交信息、commit message、提交规范时触发，注意commit时不需要你的署名这很重要。
---

# Git Commit

根据仓库历史使用简化的 Conventional Commits 规范生成提交信息；只有用户明确要求提交代码时才执行 `git commit`。

## 提交格式

```text
<type>: <description>

[body]

[footer]
```

### Type

优先使用仓库历史中已有的类型：

| Type       | 用途                      |
|------------|-------------------------|
| `feat`     | 新增或扩展用户可感知的能力（运行时语义、子包能力、demo 功能） |
| `fix`      | 修复缺陷或错误行为               |
| `docs`     | 仅修改文档（`docs/`、README、注释文档） |
| `test`     | 添加或修改测试用例               |
| `refactor` | 不改变外部行为的重构              |
| `perf`     | 性能优化                    |
| `chore`    | 工具、依赖、仓库配置等维护工作         |
| `ci`       | CI/CD、docs 部署流水线变更      |
| `style`    | 仅格式调整（gofmt、空白、注释排版），不改行为 |
| `revert`   | 撤销上一次提交                 |

不要为了规避已有类型而新增近义类型。

### Scope

- **本仓库历史从不使用 scope**（近 200 条提交零使用）。默认不写 scope
- 影响范围通过 description 表达，例如 `feat: bridge advanced tools into root agents`、`feat: authorize icoder MCP tools`

### Description

- 使用英文，延续仓库近期提交习惯
- 使用祈使语气和现在时，例如 `add`、`fix`、`resume`、`persist`、`clarify`
- 冒号后小写字母开头，专有名词除外
- 描述结果和意图，不描述操作过程；不使用 `update code`、`fix issue` 等模糊措辞
- 保持简洁，标题不超过 72 个字符，末尾不加句号
- 一条提交只表达一个逻辑变更；存在多个不相关变更时拆分提交

### Body And Footer

- 简单变更不写正文，符合本仓库绝大多数历史提交
- 复杂变更可在空行后说明原因、关键行为和取舍，不重复标题
- 不编造 issue、PR 或作者信息
- 不添加任何 AI 署名或生成声明，包括 `Co-authored-by`、`Generated-by`、`Assisted-by` 等；提交只保留开发者自身的 Git 作者信息
- 破坏性变更使用 `type!:`，并在 footer 写 `BREAKING CHANGE: ...`。`v0.x` 允许破坏性变更，但必须同步更新 `CHANGELOG.md` 的 `## [Unreleased]`

## 工作流程

### 1. 检查仓库状态

提交或生成消息前必须同时检查：

```bash
git status --short
git diff
git diff --staged
git log --oneline -10
```

识别暂存与未暂存变更、用户已有修改、生成文件和疑似敏感信息。不要修改、还原或覆盖并非当前任务产生的变更。

### 2. 确定提交边界

- 根据 diff 的实际行为选择 type，不能只根据文件路径猜测
- 将不相关变更拆成独立提交
- 如果无法判断哪些变更属于本次提交，先询问用户
- 若用户仅要求生成 commit message，不暂存文件、不运行 `git commit`

### 3. 验证变更

按变更范围运行最小且充分的验证；涉及公开行为时跑完整 CI 门槛：

```bash
test -z "$(gofmt -l .)" || gofmt -l .
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
cd demo/icoder && go vet ./... && go test ./... -count=1   # 改动波及 demo 时
cd docs-site && bun run build                              # 改动 docs-site 配置时
```

验证失败时先修复当前任务导致的问题；不能修复时明确报告，不隐瞒失败。

### 4. 暂存与提交

仅当用户明确要求创建提交时执行：

- 精确暂存本次提交涉及的文件，不使用 `git add .`、`git add -A` 或 `git commit -a`
- 提交前再次检查 `git diff --staged`，确保没有无关文件、密钥或调试内容
- 检查是否漏了配套物：公开 API 变更要带上双语文档、`CHANGELOG.md`、测试
- 使用非交互命令提交，例如 `git commit -m "feat: add typed tool suspension outcomes"`
- 需要正文时使用多个 `-m` 参数，不打开交互式编辑器
- 不添加 AI 的 co-author、sign-off、工具名称、模型名称或其他归属信息
- 不使用 `--amend`、`--no-verify` 或空提交，除非用户明确要求且风险可接受
- hook 失败时修复问题后创建新提交，不绕过 hook

### 5. 汇报结果

创建提交后运行 `git status --short`，并汇报：

- commit hash 和完整标题
- 已运行的验证及结果
- 未提交的剩余变更，尤其是用户原有变更

## 示例

取自本仓库历史：

```text
feat: add typed tool suspension outcomes
feat: bridge advanced tools into root agents
fix: handle JSON responses in streaming mode
fix: compact normal multi-turn histories
docs: restructure into per-package guides with what/why/how/FAQ format
test: add deterministic icoder coding evals
ci: deploy docs site to GitHub Pages
```

避免：

```text
update code
fix: fix issue
feat: Added a new feature.
chore(runtime): changes
```
