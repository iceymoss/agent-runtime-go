# Message、Session 与 SQLite

## Canonical Messages

根 `agent.Message` 是 provider/runtime/persistence 共享的 canonical value。assistant tool calls 和 tool results 必须按顺序持久化，保持 call ID 和 name 配对。

`RunResult.Messages` 不含当前 user message。一个 turn 的保存顺序是：

```text
user message
assistant tool_call
tool result
...
assistant final text
```

## Message Aggregate

`agent/message.Service` 适合需要以下能力的 host：stable message key、branch ordinal、snapshot replacement、revision CAS、attempt fence、visibility revision 和 tombstone。

实现 SQLite adapter 时必须保持：

- `(tenant, message key)` idempotency。
- `(tenant, session, branch, ordinal)` uniqueness。
- `ExpectedRevision` CAS。
- stale fence rejection。
- tool call/result correlation。
- detached copies。

## Session Aggregate

`agent/session.Service` 拥有 authoritative session revision、branch、usage fact 和 fast-forward merge。需要队列、worker claim、cancel/resume 和 crash reconciliation 时使用更高层 `session.SessionAgent`，它要求实现自己的 `session.Store`、`DefinitionResolver` 和 `AttemptRunner`。

普通单进程 CLI 不需要伪装成 worker host。iCoder 使用 demo-owned `CommitTurn`：

```text
BEGIN
  verify request idempotency
  allocate message ordinals
  insert user + RunResult.Messages
  UPDATE session WHERE revision = expected
  add usage exactly once
  insert terminal event
COMMIT
```

iCoder REPL 用 `/sessions`、`/use`、`/new`、`/history` 和 `/clear` 管理这个 aggregate。默认 session ID 是由 `crypto/rand` 生成的 8 字节随机值，经 hex 编码后得到 16 位字符串；`/new` 省略名称时使用同一生成方式。

Session 切换由 host 完成，随后 context planner 和 `/history` 都只读取 active session 的 revision 和 messages。SQLite 查询始终包含 `session_id`，Slash command 本身不进入模型历史。显式 `--session <id>` 或 `/use <id>` 用于恢复已有会话。

Schema 位于 `demo/icoder/internal/icoder/store.go`，不会修改产品数据库。

## Production Upgrade

当需求出现 detached workers、concurrent branches、approval resume 或 durable checkpoint 时，迁移到：

```text
session.SessionAgent
  + session.Store adapter
  + message.Service adapter
  + durable CheckpointStore
  + event outbox transaction
```

不要在 DB transaction 内执行模型请求或文件/网络副作用。
