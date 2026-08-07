# iCoder Code Agent 实施计划

## 1. 项目定位

`iCoder` 是 `agent-runtime-go` 的完整 Code Agent 参考项目。它的目标不是继续堆叠演示工具，而是证明 runtime 能被组合成一个能够在真实仓库中稳定完成中小型编码任务的应用。

目标执行闭环：

```text
理解任务
  -> 检索并读取相关代码
  -> 制定最小修改
  -> 冲突安全地编辑文件
  -> 运行最小必要验证
  -> 根据失败继续修复
  -> 持久化可信运行事实
  -> 汇报修改和验证结果
```

## 2. 当前基线

已经完成：

- OpenAI-compatible 流式模型接入。
- `agent-runtime-go` 驱动的多步模型和工具循环。
- Bubble Tea TUI、Cobra CLI 和运行取消。
- 文件列表、glob、搜索、范围读取、精确编辑和整文件写入。
- 受限 Go/Git 命令执行。
- 本地工具的 allow/ask/deny 和逐次审批。
- SQLite session、message、usage 和成功终态持久化。
- Filesystem Skills、可选 MCP 和 Sub-Agent 生命周期示例。

主要差距：

- 文件修改缺少可靠的批量 patch、创建、删除和移动能力。
- 命令范围过窄，无法覆盖常见非 Go 项目。
- 搜索、ignore、大文件和结构化读取语义不足。
- Context 使用粗略 token 估算，未完成持久化 compaction。
- Skills 全量静态注入，没有按需选择。
- MCP 工具未进入统一 permission/effect 生命周期。
- `delegate_review` 是 synthetic runner，不执行真实 child agent。
- 失败、取消、审批和工具事实没有形成可靠事件流。

## 3. 设计原则

1. 复用 runtime，不在 iCoder 重写 agent loop、permission 状态机、context 算法或 subagent 生命周期。
2. 先完成稳定的单 Agent 编码闭环，再引入异步 Sub-Agent 和 durable 调度。
3. 默认最小权限。文件写入、命令和未知 MCP effect 必须经过明确策略。
4. 所有文件操作限制在 workspace；prompt 和 Skills 不是安全边界。
5. 修改必须保护用户已有内容，优先 digest 校验、批量预检和可回滚写入。
6. Observation 只服务实时 UI；恢复、审计和 usage 使用可靠持久化事实。
7. 每个里程碑必须有可重复测试和任务级验收，不以工具数量作为完成标准。

## 4. 职责边界

### 4.1 `agent-runtime-go` 提供

- Model/tool loop、流式协议、停止和错误语义。
- RuntimeDefinition 和不可变 capability generation。
- Context normalize、budget、compaction 和 artifact contracts。
- Permission、approval、grant 和 resume revalidation。
- 高级 tool lifecycle、effect ledger 和 durable semantics。
- Session、message、event/outbox、MCP、Skills 和 Sub-Agent contracts。
- 应补充的通用桥接：高级 `tool.Executor` 到 root Agent、MCP 到 `agent.Tool`、Sub-Agent Runner、model retry decorator、context 高层 prepare/compact 流程。

### 4.2 `demo/icoder` 提供

- Code Agent prompt、任务策略和完成标准。
- Workspace confinement 及文件、搜索、patch、命令工具。
- 项目指令发现和 Code Agent Skills。
- 本地权限策略及 TUI 审批交互。
- reviewer/explorer 等具体 child agent 定义。
- SQLite adapters、CLI/TUI 和 coding benchmark。

## 5. 里程碑

### M0：编码任务基线

目标：使用固定 fixture 衡量能力变化。

任务：

- 建立项目理解、单文件修复、多文件功能、编译错误、测试失败、重构、review 和新增测试等任务集。
- 记录任务完成率、修改文件、验证命令、工具次数、token、循环和安全违规。
- 为每类任务提供确定性断言，而不是仅判断模型最终文本。

验收：

- 至少 10 个可重复运行的本地任务。
- 每次能力变更可以输出前后结果。
- workspace escape 和覆盖用户修改作为硬失败。

### M1：Single-Agent Coding Loop

目标：稳定完成中小型本地编码任务。

任务：

- [x] 基础文件读取、搜索、精确编辑和整文件写入。
- [x] 批量 `apply_patch`，支持 create/update/delete、digest 校验、预检和失败回滚。
- [x] 增加 move 和显式目录创建能力。
- [x] 统一 `read_file` 的行号、digest、总行数和 truncation 返回。
- [x] 支持 `.gitignore`/`.ignore`、二进制和大文件过滤。
- [x] 扩展直接 argv 命令 profile，覆盖 Go、Node、Python 和 Rust。
- [x] 返回 stdout、stderr、exit code、timeout 和 truncation 元数据。
- [x] 修改后跟踪 changed files 和 checks，未验证时明确标记。

验收：

- 可以在一个任务中创建、更新和删除多个文件。
- stale digest 不会覆盖并发修改。
- 测试失败后 Agent 能读取错误、继续修复并再次验证。
- 最终结果准确列出修改文件和执行的检查。
- M0 中 Go 中小型任务达到稳定可用水平。

### M2：项目理解和上下文

目标：在较大仓库和较长会话中保持正确方向。

任务：

- [x] 分层加载 workspace 和子目录 `AGENTS.md`。
- [x] Skills 初始只暴露 metadata，通过 `list_skills`/`load_skill` 按需读取。
- 使用 provider tokenizer 或 runtime 的保守 estimator。
- 动态计算 tool schema token 预算。
- 接通 prepare、compact、persist artifact、reprepare 流程。
- 持久化轻量任务状态：goal、changed files、checks 和 open issues。

验收：

- 子目录指令只影响对应路径。
- 未选择的 Skill 不占用完整 instructions token。
- 长历史压缩后保留最新用户意图、关键事实和完整近期工具交换。
- 进程重启后能够恢复 summary artifact 和任务状态。

### M3：可靠运行和事件

目标：失败、取消和恢复后的状态可信。

任务：

- 持久化 run started/completed/failed/canceled。
- 持久化 model step、tool call/result、approval 和 usage facts。
- 使用 event/outbox，而不是依赖 lossy Observation。
- 增加 retryable model error 的 backoff、jitter 和 Retry-After。
- 冻结 run-scoped tenant/session/revision/runtime generation。
- 接入高级 tool executor 和 effect ledger，表达 unknown effect。

验收：

- 模型失败、命令超时和用户取消都有可回放终态。
- 重试不会盲目重复已发生的副作用。
- 运行中切换 UI session 不会改变当前 run 的权限和提交归属。
- Observation 丢失不影响最终事实和恢复。

### M4：统一 MCP 和真实 Sub-Agent

目标：安全扩展远端工具和独立 child run。

任务：

- MCP tool 映射 action、resource、effect class 和 replay policy。
- MCP 调用进入统一 permission、generation 和 result 限制。
- 用真实只读 reviewer child agent 替换 synthetic runner。
- 增加 explorer child agent。
- 第一版同步返回 child 结果，随后接入 spawn、suspend、wake 和 resume。

验收：

- 未分类或有副作用的 MCP tool 不会默认执行。
- reviewer 使用独立 context、预算和只读 tool set 输出真实 findings。
- parent 只接收结构化结果，不复制 child 全量历史。
- cancellation、depth、fanout 和预算限制生效。

### M5：完整参考应用

目标：展示 runtime 的 production-oriented composition。

任务：

- 使用 RuntimeDefinition 冻结 model、prompt、tools、policy、Skills 和 MCP generation。
- 通过 SessionAgent 和 context-aware AttemptRunner 运行任务。
- 提供 SQLite session/message/context/permission/event/subagent/durable adapters。
- 明确跨 aggregate 事务和 outbox 边界。
- 增加 daemon 生命周期、readiness 和 bounded shutdown 示例。

验收：

- 精确 runtime generation 可以被重建和审计。
- turn、revision、messages、usage 和 event/outbox 原子提交。
- 中断运行可以安全 reconcile 或明确进入人工处理状态。
- 文档能指导另一个项目复用同一组合方式。

## 6. 首轮实施范围

首轮从 M1 的批量 patch 开始：

1. 增加结构化 `apply_patch` 工具。
2. 支持 create、update、delete。
3. 所有操作执行前完成路径、文件状态、digest 和文本匹配校验。
4. 写入使用同目录临时文件和 rename，保留现有权限。
5. 执行失败时恢复本批次已修改文件。
6. 通过现有 `workspace.write` permission wrapper 授权。
7. 增加 workspace、tool registration、permission 和回滚测试。

首轮暂不包含 move。move 会在 create/update/delete 语义稳定后作为独立增量加入，避免同时引入 source/destination 冲突和跨目录回滚复杂度。

## 7. 验证命令

每个增量至少执行：

```bash
cd demo/icoder
CGO_ENABLED=1 go test ./... -count=1
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go build ./cmd/icoder
```

涉及根 runtime 时额外执行：

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

## 8. 非当前优先级

- 完整 PTY 和任意 shell。
- 多 Agent 并发写同一 workspace。
- Vector DB 或代码 embedding。
- 自动 Git commit/push。
- Web UI 和远程 daemon。
- 多 provider 智能路由。
- 长时间无人值守自主运行。

这些能力不能替代稳定的单 Agent 检索、修改、验证和恢复闭环。
