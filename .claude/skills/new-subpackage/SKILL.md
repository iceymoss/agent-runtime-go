---
name: new-subpackage
description: 新增一个可选子包（session/durable/permission 这类能力包）。当用户说加子包、新增 package、加一层能力时触发。
---

新增可选子包。这是高风险改动：子包是公开 API 的一部分，且受依赖铁律约束。**动手前先确认能力无法用现有子包表达**。

## 步骤

### 1. 与用户对齐边界（必须）
- 这个能力为什么不能放进现有子包（`session` / `durable` / `permission` / `event` / `tool` / `context` / ...）？
- 它暴露的是 port（接口）还是具体实现？库里只应该有 port + 内存参考实现
- 只用根包的用户会不会被迫承担成本？子包必须是可选的、渐进采用的

### 2. 确认依赖方向
三条铁律由 `deps_test.go` 强制：
- 根包 `agent` **不得** import 任何子包 —— 如果新子包需要根包"知道"它，改成在根包定义接口、子包实现
- 任何包不得 import `internal/`
- 生产包不得 import `agenttest`

共享类型放根包，子包用 type alias 引用（参考 `durable/types.go` 的 `type Checkpoint = agent.Checkpoint`），不要复制定义。子包之间尽量不互相依赖，确需依赖时保持单向无环。

### 3. 按既有文件职责建包

```
<name>/
  contracts.go   # 接口（port）与核心类型
  errors.go      # 哨兵错误
  validation.go  # 入参校验
  copy.go        # 深拷贝（跨边界数据）
  memory.go      # 内存参考实现（必须有，让调用方无需数据库即可测试）
  <name>_external_test.go   # package <name>_test
```

### 4. 写测试
默认写外部包测试（`package <name>_test`），保证导出面诚实可用。表驱动 + `t.Run`；并发路径要在 `-race` 下稳定。

### 5. 文档（双语，必须成对）
- `docs/packages/<name>.md` 和 `docs/en/packages/<name>.md`，结构：是什么 → 为什么需要 → 怎么用 → FAQ
- `docs-site/.vitepress/config.*` 加侧边栏条目
- `README.md` 与 `README.zh-CN.md` 的"选择子包"表格加一行
- `CHANGELOG.md` 的 `## [Unreleased]` 记一条

### 6. 验证

```bash
go test . -run TestAgentPackagesDoNotImportInternalPackages -count=1
go test . -run TestRootAgentDoesNotImportChildPackages -count=1
```

再跑完整 CI 门槛（见 `/gotest`）。

## 严格约束
- 子包只暴露 port 与语义，不绑定具体存储/传输。SQLite、HTTP server、TUI 这类实现放 `demo/icoder` 或调用方
- 不引入新的第三方依赖
- 跨 API 边界的数据必须深拷贝，不与调用方共享底层切片/map
- 有状态组件自带锁，不把锁或内部可变状态泄漏给调用方
- 新导出符号都要有说明"为什么"的 doc comment
