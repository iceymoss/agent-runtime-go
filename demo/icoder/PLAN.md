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

- [x] 建立项目理解、单文件修复、多文件功能、编译错误、测试失败、重构、review 和新增测试等任务集。
- [x] 记录任务完成率、工具次数、token 和耗时；修改文件、验证命令、循环和安全违规继续从可靠事件聚合。
- [x] 为任务提供确定性断言，而不是仅判断模型最终文本。

验收：

- [x] 至少 10 个可重复运行的本地任务。
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

当前状态：M1 功能实现完成，任务级成功率将在 M0 eval harness 建立后持续量化。

### M2：项目理解和上下文

目标：在较大仓库和较长会话中保持正确方向。

任务：

- [x] 分层加载 workspace 和子目录 `AGENTS.md`。
- [x] Skills 初始只暴露 metadata，通过 `list_skills`/`load_skill` 按需读取。
- [x] 使用 runtime 保守 estimator；provider 精确 tokenizer 保留为可插拔升级。
- [x] 动态计算 tool schema token 预算。
- [x] 接通 prepare、compact、persist artifact、reprepare 流程。
- [x] 持久化轻量任务状态：goal、changed files、checks 和 open issues。

验收：

- 子目录指令只影响对应路径。
- 未选择的 Skill 不占用完整 instructions token。
- 长历史压缩后保留最新用户意图、关键事实和完整近期工具交换。
- 进程重启后能够恢复 summary artifact 和任务状态。

当前状态：持久化 context 与动态 tool schema token 预算完成。context 诊断已落地：`agent.context.prepared` 事件持久化每次 run 的 plan digest、估算、预算与 compaction 事实，terminal 事件携带每步真实 prompt token，`icoder context show` 输出估算与实际的对照。provider 精确 tokenizer 保留为可插拔升级：counter 是 App 装配时的单一 `TokenCounter` 值，诊断报告负责提供何时值得替换它的证据。

### M3：可靠运行和事件

目标：失败、取消和恢复后的状态可信。

任务：

- [x] 持久化 run started/completed/failed/canceled。
- [x] 持久化 tool call/result、approval 和 usage facts。
- [x] 使用 SQLite event/outbox 和 runtime Dispatcher，而不是依赖 lossy Observation。
- [x] 增加 retryable model error 的 backoff、jitter 和 Retry-After。
- [x] 冻结 run-scoped tenant/session/revision/runtime generation。
- [x] 接入高级 tool executor 和 effect ledger，表达 unknown effect。
- [x] 补齐根库 `durable.CheckpointAdapter`（`durable.Store` → `agent.CheckpointStore`）与 `durable.EffectReplayer`。
- [x] SQLite 实现 `durable.Store` / `ExecutionLedger` / `UsageLedger` / `tool.ExecutionLedger` / `permission.Store`。
- [x] 每次任务走 durable run；run 终态与会话 turn 在同一事务提交。
- [x] 审批 blocker 触发 durable suspension，`icoder runs approve/deny/abandon/effects` 完成人工裁决与恢复。

依赖说明：交互式 TUI 仍在 preflight 拦截器里同步询问，拒绝对模型可见（`ToolResult{IsError, StopTurn}`）；非交互 `run` 走 durable suspension，由另一个进程批准后精确恢复。两条路径共用同一个 `permission.Service` 与同一份 grant 记录。

当前状态：M3 完成。

验收：

- 模型失败、命令超时和用户取消都有可回放终态。
- 重试不会盲目重复已发生的副作用。
- 运行中切换 UI session 不会改变当前 run 的权限和提交归属。
- Observation 丢失不影响最终事实和恢复。

### M4：统一 MCP 和真实 Sub-Agent

目标：安全扩展远端工具和独立 child run。

任务：

- [x] MCP tool 映射 action、resource、effect class 和 replay policy。
- [x] MCP 调用进入统一 permission、generation 和 result 限制。
- [x] 用真实只读 reviewer child agent 替换 synthetic runner。
- [x] 增加 explorer child agent。
- [x] 第一版同步返回 child 结果；spawn / claim / wake / reconcile / 预算与取消已走真实 `subagent.Service`。
- [x] `SQLiteSubagentStore` 实现 `subagent.Store`：relationship、树预算与 wake intent 跨进程存活，安全限额调用包导出的状态机，通过 `agenttest.TestSubagentStore`。
- [x] parent-suspending 的异步 wake：根库已补通用 tool suspension（`agent.ToolSuspensionExternal` + `tool.Executor.Resume`），delegate 工具改为返回 `ToolSuspensionExternal` 让父运行落 checkpoint 挂起，child 由 worker 推进后凭 handle 恢复。早到的 wake 会再次挂起而不是失败，重入次数由 `maxDelegationResumes` 兜底。

验收：

- 未分类或有副作用的 MCP tool 不会默认执行。
- reviewer 使用独立 context、预算和只读 tool set 输出真实 findings。
- parent 只接收结构化结果，不复制 child 全量历史。
- cancellation、depth、fanout 和预算限制生效。

当前状态：M4 已完成。reviewer 与 explorer 均为真实 child agent，父运行在 child 执行期间真正挂起并从 checkpoint 恢复，结果与 `agent.subagent.awaited` / `agent.subagent.completed` 事件持久化，`icoder delegations` 可回看。

## 5.1 下一阶段顺序

1. [x] 回补 M0 deterministic coding eval harness，建立功能成功率和安全回归基线。
2. [x] 动态 tool schema budget 与 context diagnostics 完成（`agent.context.prepared` + `step_usage` + `icoder context show`）；provider tokenizer 保留为可插拔升级，由诊断数据决定是否引入。
3. [x] 将进程内事件升级为事务 outbox，补齐 model step 和恢复投影。
4. [x] 设计并实现 root Agent 到高级 `tool.Executor` 的标准 bridge（`tool.NewAgentRegistry` + iCoder `ToolCatalog`）。
5. [x] 迁移到 RuntimeDefinition（coordinator resolve + manifest）；context-aware SessionAgent AttemptRunner 待办。
6. [x] 接入 durable suspension/resume 与 effect ledger；异步 Sub-Agent wake 已随通用 tool suspension 落地。
7. [x] `SQLiteSessionService` + `SQLiteSessionRunStore` + `session.SessionAgent` 接入（`icoder queue`，daemon 启动 worker 并在关闭时排空）。

### M5：完整参考应用

目标：展示 runtime 的 production-oriented composition。

任务：

- [x] 使用 RuntimeDefinition 冻结 model、prompt、tools、policy、Skills 和 MCP generation，并通过 `coordinator` 持久化 wire manifest、支持精确重建。
- [x] 通过 SessionAgent 和 context-aware AttemptRunner 运行任务：`icoder queue` 在提交时绑定 context plan，worker 按提交时的历史执行。
- [x] SQLite adapters：`durable.Store`/`ExecutionLedger`/`UsageLedger`、`tool.ExecutionLedger`、`permission.Store`、`coordinator.ManifestStore`、`event` outbox、context plan/artifact。
- [x] `SQLiteMessageService`（`message.Service`）与 `SQLiteSessionService` + `SQLiteSessionRunStore`（`session.Service` + `session.Store`）已实现；状态机校验入口已由库导出，适配器只做存储并通过 `agenttest` 对应套件验收。
- [x] 明确跨 aggregate 事务和 outbox 边界：turn、messages、usage、terminal event 与 durable 终态在同一个 SQLite 事务提交。
- [x] 增加 daemon 生命周期、readiness 和 bounded shutdown 示例（`icoder daemon`）。

验收：

- [x] 精确 runtime generation 可以被重建和审计（`icoder runtime show/list/verify`）。
- [x] turn、revision、messages、usage 和 event/outbox 原子提交。
- [x] 中断运行可以安全 reconcile 或明确进入人工处理状态（`icoder runs list/effects/approve/deny/abandon`）。
- [x] 文档能指导另一个项目复用同一组合方式：`docs/guide/`（中英 12 章 developer guide）+ 可运行的 `examples/guide`，从 hello 到 durability/编排/生产逐章对应真实 API。

当前状态：M5 完成。RuntimeDefinition + coordinator + daemon 生命周期、SessionAgent 队列、全部 SQLite adapters 与 developer guide 均已落地。

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
