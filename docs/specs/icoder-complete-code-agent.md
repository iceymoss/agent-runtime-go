# iCoder 完整 Code Agent（M3 / M4 / M5）

## 状态

- 创建日期: 2026-08-26
- 状态: M3 / M4 已完成，M5 部分完成（见下方实现步骤勾选）
- 关联: `demo/icoder/PLAN.md` 的 M3、M4、M5

## 目标

把 `demo/icoder` 从"单进程同步 Code Agent"补完为 **production-oriented 参考应用**：
运行事实可恢复、工具副作用有账本、审批可挂起可恢复、runtime generation 可审计重建、
子 Agent 可异步、进程生命周期有 readiness 与有界关闭。

完成后 `agent-runtime-go` 的每一个子包都在 iCoder 中被真实使用：
根包 · `context` · `event` · `mcp` · `message` · `permission` · `prompt` · `provider` ·
`providers/openaicompat` · `session` · `skills` · `subagent` · `tool` · `durable` ·
`coordinator` · `app`。

## 非目标

- 完整 PTY / 任意 shell、多 Agent 并发写同一 workspace、向量检索、自动 push、Web UI、多 provider 路由。
- 不在库里塞策略：模型凭据、审批 UI、命令白名单、SQL schema 仍归 iCoder。
- 不改 `Agent.Run` 的既有语义（`Outcome` / `StopReason` 常量与含义保持不变）。

## 调用方故事

- 作为使用者，进程崩溃或被 `Ctrl+C` 后重启，我希望上一次 run 能从最后一个持久化边界继续，而不是从零重跑一遍模型和工具。
- 作为使用者，写入类工具弹出审批时，我希望**运行被真正挂起**（而不是阻塞一个 goroutine），批准后精确从那一个工具调用继续。
- 作为审计者，我希望知道"这次 run 用的到底是哪一版 prompt / tools / policy / model"，并能用同一份 manifest 重建它。
- 作为运维者，我希望 `icoder daemon` 有 readiness 探针，收到关闭信号时按 drain → cancel → checkpoint → wait → flush → close 有界收尾。

## 归属

| 能力 | 归属 | 理由 |
|---|---|---|
| `durable.Store` → `agent.CheckpointStore` 桥接 | 根库子包 `durable` | 任何用 `durable` 的应用都要写同一份映射；`PLAN.md` 4.1 已列为"runtime 应补充的通用桥接" |
| SQLite adapters（durable/session/message/subagent/permission/coordinator） | `demo/icoder` | 具体存储实现按分层铁律不进库 |
| tool.Executor 组装、审批交互、命令白名单 | `demo/icoder` | 策略与 UI 属于调用方 |
| coordinator Builder / Reconstructor | `demo/icoder` | 库已声明 Builder 是 application-owned |

## 公开 API

### 根库新增（M3-1）

| 符号 | 类型 | 说明 |
|---|---|---|
| `durable.CheckpointAdapter` | struct | 在 `durable.Store` + `durable.ExecutionLedger` 之上实现 `agent.CheckpointStore` |
| `durable.NewCheckpointAdapter(CheckpointAdapterOptions) (*CheckpointAdapter, error)` | func | 构造；校验 store/ledger/clock |
| `durable.CheckpointAdapterOptions` | struct | `Store`、`Ledger`、`TenantKey`、`AttemptKey`、`Now` |

语义要点：

- `Begin` / `Load` / `Acquire` 直接委托 `Store`，把 `durable.Snapshot` 投影成 `agent.RunSnapshot`。
- `ModelInflight` / `CommitModelResponse` / `PrepareTools` / `CommitTool` 映射成一次 `Store.Save`，
  `Status`/`Phase` 按根包状态机取值，revision 冲突原样返回 `agent.ErrCheckpointConflict`。
- `PrepareTools` 对每个 `agent.ToolExecution` 调 `Ledger.PrepareEffect`；`BeginTool` 调 `BeginEffect`；
  `CommitTool` 调 `CompleteEffect`。
- `BeginTool` 遇到已有 `EffectUnknown` 记录且 `safeReplay == false` 时返回 `agent.ErrToolExecutionUnknown`。
- `ExecutionKey` 复用 `durable.ToolExecutionKey`，不引入第二套 key 规则。

### 兼容性影响

- 无 BREAKING。纯新增导出符号，`agent` 根包不变。
- 复用现有类型：`agent.CheckpointStore`、`agent.RunSnapshot`、`agent.Checkpoint`、`agent.ErrCheckpointConflict`、
  `agent.ErrToolExecutionUnknown`、`durable.ExecutionKey`。

### iCoder 内部（不导出到库）

| 符号 | 说明 |
|---|---|
| `icoder.SQLiteDurableStore` | `durable.Store` + `ExecutionLedger` + `UsageLedger` 的 SQLite 实现 |
| `icoder.SQLiteSessionRunStore` | `session.Store`（17 方法）SQLite 实现 |
| `icoder.SQLiteSessionService` | `session.Service`（12 方法）SQLite 实现 |
| `icoder.SQLiteMessageService` | `message.Service` SQLite 实现 |
| `icoder.SQLiteSubagentStore` | `subagent.Store` SQLite 实现 |
| `icoder.SQLitePermissionStore` | `permission.Store` SQLite 实现 |
| `icoder.SQLiteManifestStore` | `coordinator.ManifestStore` SQLite 实现 |
| `icoder.ToolGeneration` | `tool.Registry` 装配 + `Freeze` + `tool.NewAgentRegistry` 桥接 |
| `icoder.RuntimeBuilder` / `RuntimeReconstructor` | `coordinator.Builder` / `Reconstructor` |
| `icoder.runQueue` | `session.SessionAgent` 装配；`queueDefinitions` / `queueAttempts` / `queueEvents` 分别实现 `DefinitionResolver` / `AttemptRunner` / `EventSink` |
| `icoder.Daemon` | `app.App` 组装：components + readiness + bounded shutdown |

## 语义

### 正常流程（M3 之后）

1. `App.Run` 解析 session、准备 context plan（已有）。
2. 通过 `coordinator.Resolve` 拿到冻结的 `RuntimeDefinition`（M5 之前先用直接构造）。
3. `definition.NewAgent()` → `agent.Run(RunRequest{DurableRun: ...})`。
4. 每个工具调用经 `tool.Executor`：preflight → permission → effect ledger prepare/begin → 执行 → complete。
5. 终态由 `RunResult` 决定，事实写入 SQLite（messages / usage / events outbox / effect ledger / durable snapshot）。

### 异常与终态

| 场景 | 处理方式 | Outcome / StopReason / 错误 |
|---|---|---|
| 工具需要人工审批 | executor 返回 `permission.SuspensionBlocker`，bridge 抛 `agent.ToolSuspensionError` | `suspended` + `tool_suspended` |
| 工具等待外部事件（child run、webhook） | 工具抛 `agent.ToolSuspensionError{Kind: ToolSuspensionExternal}`，executor park 并存下 handle | `suspended` + `tool_suspended` |
| 用户批准后继续 | `permission.Resolve` + `Revalidate`，再以 `DurableRun.ToolResume` 恢复同一 run | 续跑，最终 `completed` |
| 用户拒绝 | `ToolResult{IsError:true, StopTurn:true}` | `completed` + `tool_stop_turn` |
| 审批过期 | `Revalidate` 返回 `permission.ErrRequestExpired` | `ToolResult{IsError:true}`，提示重跑 |
| 模型 inflight 时崩溃 | 恢复时重发模型请求（at-least-once） | 续跑 |
| 工具 executing 时崩溃 | `RevokeLease` 把该 effect 标 `unknown`；非 `safeReplay` 工具拒绝重放 | `agent.ErrToolExecutionUnknown` → run `failed` |
| 达到 MaxSteps 仍在调工具 | 不是错误 | `suspended` + `max_steps` |
| 模型可重试错误 | `retryModel` backoff + jitter + Retry-After（已有） | 重试耗尽后 `failed` |
| 关闭进程 | `app.App.Shutdown` 有界 drain/cancel/checkpoint/wait/flush/close | `ShutdownReport` |

### 并发与恢复

- `*agent.Agent`、`*RuntimeDefinition` 无状态，可跨 goroutine 复用。
- 所有 SQLite adapter 用单写连接（`SetMaxOpenConns(1)` + `_txlock=immediate`），每个契约方法一个事务。
- 写授权一律比较 `(RunKey, LeaseOwner, Revision, FenceToken)` 四元组；不匹配返回对应包的 conflict 哨兵错误。
- 恢复锚点：`durable.Snapshot`（run 状态机）+ `EffectRecord`（工具副作用）+ `event_records` outbox（外部投递）。
- 副作用去重锚点是 `durable.ToolExecutionKey`，不改其构成规则。

## 实现步骤（每步可独立 commit）

### M3 — 可靠运行与事件（完成）

1. [x] `durable/checkpoint_adapter.go`：`CheckpointAdapter` + options + 校验；`durable/checkpoint_adapter_external_test.go`
2. [x] 中英双语文档 `docs/packages/durable.md` / `docs/en/packages/durable.md` + `CHANGELOG.md`
3. [x] iCoder `SQLiteDurableStore`（Store + ExecutionLedger + UsageLedger + EffectReplayer）+ 测试
4. [x] iCoder `ToolCatalog`：`tool.Registry` metadata、`Freeze` 拦截器（approval + audit）、
       `tool.Executor`（permission + SQLite ledger）、`tool.NewAgentRegistry` 桥接；MCP 工具 `EffectExternal` + `IdempotencyNone`
5. [x] `App.Run` 走 durable run + 审批挂起/恢复闭环
6. [x] CLI `icoder runs list|effects|approve|deny|abandon`
7. [x] 事件补齐：`agent.permission.checked`、`agent.approval.requested/resolved`、`agent.tool.started/completed/failed`、`agent.run.suspended`
8. [x] `SQLitePermissionStore` 实现 `permission.Store`（审批可跨进程回答）

### M4 — MCP 与真实 Sub-Agent（完成）

9. [x] `delegate_explore` explorer child agent（只读检索、独立预算）
10. [x] 真实 spawn / claim / wake / reconcile；child 结果与完成事件持久化，`icoder delegations` 可回看
11. [x] `SQLiteSubagentStore` 实现 `subagent.Store`（通过 `agenttest.TestSubagentStore`）：
       异步化后 relationship / 树预算 / wake intent 必须跨进程存活，适配器只做存储，
       depth / fanout / cycle / budget 一律调用 subagent 包导出的状态机
12. [x] 根库通用 tool suspension（`agent.ToolSuspensionExternal` + `tool.Executor.Resume`）
13. [x] parent-suspending 异步 wake：`delegate_*` spawn 后返回 `ToolSuspensionExternal` 使父运行落
       checkpoint 挂起，应用推进 child 后凭工具签发的 handle 恢复并认领同一个 child；
       早到的 wake 再次挂起而非失败，重入次数由 `maxDelegationResumes` 兜底；
       `agent.subagent.awaited` 进入事件流

### M5 — 完整参考应用（完成）

14. [x] `RuntimeDefinition` 装配 + `SQLiteManifestStore` + `RuntimeBuilder`/`Reconstructor` + `coordinator.Coordinator`
15. [x] `icoder runtime show|list|verify`
16. [x] `icoder daemon`：`app.App` components / readiness / bounded shutdown + `runController` 准入闸门
17. [x] `SQLiteMessageService` 实现 `message.Service`（通过 `agenttest.TestMessageService`）
18. [x] `SQLiteSessionService`（`session.Service`，通过 `agenttest.TestSessionService`）
        + `SQLiteSessionRunStore`（`session.Store`，通过 `agenttest.TestSessionRunStore`）
        + `session.SessionAgent` 接入：`icoder queue submit|work|show|cancel`，
        daemon 启动 worker 并在关闭时 `Shutdown` 排空；
        context-aware `AttemptRunner` —— 上下文计划在提交时绑定，排队的 run 按提交时的历史执行
18. [x] `README.md` / `PLAN.md` / `CHANGELOG.md` 更新；`docs/packages/durable.md` 与英文版同步

## 参考的现有模式

- `durable/memory.go` — Store/ledger 的语义参考实现，SQLite 版本按同一状态机
- `session/memory_run_store.go` — `session.Store` 的 17 方法语义参考
- `demo/icoder/internal/icoder/event_store.go` — 已有的 SQLite outbox 实现风格（事务、claim/lease/fence）
- `demo/icoder/internal/icoder/store.go` — 迁移、CAS、事务边界的既有写法
- `tool/agent_bridge.go` — 高级 executor 到根 Agent 的官方桥接

## 测试计划

- [ ] 每个 SQLite adapter 跑与内存参考实现相同的语义用例（正常、CAS 冲突、fence 过期、幂等重入）
- [ ] `durable.CheckpointAdapter` 外部包测试：Begin 幂等、guard 冲突、unknown effect 拒绝重放
- [ ] 审批挂起/恢复端到端：fake model + fake approval，断言 `suspended`/`tool_suspended` 与恢复后 `completed`
- [ ] 崩溃恢复：中断后用同一 RunKey 重跑，断言不重复已提交的工具结果
- [ ] `-race` 下并发 worker 声明同一 run 只有一个成功
- [ ] 新工具跑 `agenttest.TestTool`
- [ ] 两个 module 的 CI 门槛：`gofmt -l .`、`go vet`、`go test -count=1`、`go test -race -count=1`

## 库侧改造（已完成）

上一轮识别出的分层缺口已经补齐，"任何人都能写自己的持久化适配器"现在是可兑现的：

1. **每个持久化端口都有可复用的一致性套件**（`agenttest`）：`TestCheckpointStore`、`TestDurableStore`、
   `TestPermissionStore`、`TestMessageService`、`TestSubagentStore`、`TestSessionService`、`TestSessionRunStore`。
   库自带的内存参考实现全部跑同一份套件，套件与参考实现不会各自漂移。
2. **状态机从非导出变为导出**：`message` 的 `Apply*` 系列、`subagent` 的限额/预算/派生函数、
   `session` 的转移表。三个包的参考实现都重写在这些导出函数之上，规则只有一份实现。
3. 套件在编写过程中发现并修复了两个真实缺陷（见 CHANGELOG `Fixed`）：
   `CheckpointAdapter.BeginTool` 对已提交结果报错导致 run 永久不可恢复；
   `PrepareEffect` 把 attempt key 和时间戳算进不可变身份，导致每次 resume 都误报冲突。

## 待定事项 / 后续建议

- provider 精确 tokenizer 仍保留为可插拔升级点，本 spec 不含。
- `session.Store` / `subagent.Store` 的 SQLite 适配器现在可写了（规则已导出、验收套件已就绪），
  是 iCoder 侧的下一步。

## MVP 范围

M3、M4、M5 全部已交付并通过两个 module 的 CI 门槛。
runtime 的每一个持久化 port 现在都有一份 iCoder SQLite 适配器，
并且都按 `agenttest` 的同一份契约验收。
