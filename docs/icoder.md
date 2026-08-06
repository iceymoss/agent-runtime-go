# iCoder 端到端教程

本文面向想看一个完整可运行组合示例的开发者：[`demo/icoder`](../demo/icoder/README.md) 是一个 Code Agent reference application，把根包的 model/tool loop 与 Prompt、Context、Skills、Permission、MCP、Sub-Agent 和 SQLite 组合在一个独立 Go module 中。本文说明如何运行它、代码结构如何映射到各子包，以及它刻意保留的 demo 边界。

## 代码结构与子包映射

```text
demo/icoder/                       # 独立 Go module（replace runtime => ../..）
├── cmd/icoder/main.go             # flags、单任务和 REPL
├── internal/icoder/
│   ├── app.go                     # composition root：prompt 渲染、context.Planner、skills 加载
│   ├── capabilities.go            # mcp bridge、delegate_review（subagent）
│   ├── config.go                  # 环境变量和默认值
│   ├── store.go                   # SQLite schema 和 turn 事务
│   ├── tools.go                   # 本地工具及 permission bridge
│   ├── weather.go                 # Open-Meteo adapter
│   └── workspace.go               # 路径约束、文件和命令
└── skills/                        # code-review、go-code-review 两个 SKILL.md
```

模型接入直接使用官方适配器 [packages/openaicompat.md](packages/openaicompat.md) 描述的 `providers/openaicompat`，默认走 SSE 流式；对 SSE 支持不完整的供应商，可在 `app.go` 改用 `openaicompat.WithoutStreaming()`。

## 前置条件

- Go 1.25 或更高版本（两个 module 的 `go.mod` 都声明 `go 1.25.0`）。
- C 编译器和 CGO：SQLite driver 是 `github.com/mattn/go-sqlite3`，必须 `CGO_ENABLED=1`。
- 一个实现 OpenAI Chat Completions、支持 function/tool calling 的兼容 endpoint，以及它的 API key、base URL 和模型 ID。
- 使用 `get_weather` 或远程 MCP 时需要对应网络出口。

```bash
go version
go env CGO_ENABLED CC   # CGO_ENABLED=0 时 SQLite 相关构建不可用
```

## 配置

三个环境变量必需；flag 可覆盖 base URL 和 model，但 CLI 没有 API key flag：

```bash
export ICODER_API_KEY='your-key'
export ICODER_BASE_URL='https://api.example.com/v1'   # 请求发送到 <base-url>/chat/completions
export ICODER_MODEL='your-model'
```

| 配置 | 来源 | 默认值或行为 |
|---|---|---|
| workspace | `--workspace` | `.`，启动时转绝对路径并解析根目录 symlink |
| SQLite | `--db` | `<workspace>/.icoder.db`；不想污染仓库时显式指定如 `--db /tmp/icoder.db` |
| Skills | `--skills` | 空，不加载 Skills |
| MCP | `--mcp-url` | 空，不连接 MCP |
| session | `--session` | `crypto/rand` 生成 16 位小写十六进制 |
| write/command | `--allow-writes` | `false` |
| context window / max output | 代码配置 | 128000 / 4096 |

以下命令默认先 `cd demo/icoder`。注意相对路径基准：`--workspace ../..` 是仓库根，`--workspace ../../..` 是仓库父目录。

## 构建、测试与单任务运行

iCoder 是独立 module，`go test ./...` 在仓库根不会覆盖它，必须分别在仓库根和 `demo/icoder` 中验证：

```bash
go test ./... -count=1
go vet ./...
go build -o /tmp/icoder ./cmd/icoder
```

审计当前仓库的单任务运行：

```bash
go run ./cmd/icoder \
  --workspace ../.. \
  --db /tmp/icoder.db \
  --skills ./skills \
  --session sdk-review \
  --task '阅读根包的 tool loop，说明模型错误和工具错误如何传播'
```

CLI 在流式打印文本后输出终态行：

```text
[completed: complete, steps=2, tokens=1234]
```

`Outcome` 和 `StopReason` 的准确含义见[速查表](reference.md#result-semantics)。

## REPL

不传 `--task`，或使用 `-i` / `--interactive`，进入 REPL：

| 命令 | host 端行为 |
|---|---|
| `/help`、`/?` | 显示帮助 |
| `/pwd`、`/cd <path>` | 显示/切换本地工具 cwd；`/cd /` 返回 workspace root |
| `/session`、`/sessions`、`/use <id>`、`/new [id]` | 查看、列出、切换、新建 SQLite session |
| `/history`、`/clear` | 显示 canonical messages 摘要 / 删除当前 session 全部数据 |
| `/events` | 回放当前 session 的 terminal events |
| `/tools`、`/skills` | 列出 model-visible tools / 已加载 skill capability |
| `/exit`、`/quit`、`/q` | 退出 |

Slash command 由 CLI host 执行，不发送给模型；自然语言"切换目录"不会改变工具 cwd。cwd 是进程内状态，重启恢复到 workspace root；session 历史通过同一个 `--session` 从 SQLite 恢复。也可以不进 REPL 直接 `--events` 回放；即使只回放，`NewApp` 仍要求三个 provider 环境变量并完成全部初始化。

## Skills

`--skills ./skills` 用 `skills.FilesystemSource` 加载目录下每个合法 `SKILL.md`（demo 自带 `code-review` 和 `go-code-review`）。加载固定 snapshot generation，instructions 被包装为 `<untrusted-skill key="..." version="...">...</untrusted-skill>`，作为 capability system messages 进入 Context Plan。它们是非可信指令，不是权限边界；frontmatter 的 `tool_requirements` 不替代工具白名单或 `permission.Service`。启动时解析，运行中不热更新。

## 8 个内置工具

不配置 MCP 时，model-visible 工具恰好是：

| 工具 | 输入要点 | 行为与限制 | ReplayPolicy |
|---|---|---|---|
| `get_working_directory` | `{}` | 返回工具 cwd | `idempotent` |
| `list_files` | 可选 `limit`，1..1000 | 递归列文件，默认 200，跳过 `.git`、`node_modules` | `idempotent` |
| `read_file` | `path` | 读 workspace 内已有文件，最多 64 KiB | `idempotent` |
| `search_code` | `pattern`、可选 `limit` | 字面子串搜索，默认 50，最多 200；不是正则 | `idempotent` |
| `write_file` | `path`、`content`、可选 `expected_digest` | 全量替换或创建单文件，写为 `0644`；digest 是 `sha256:<hex>` | `never` |
| `run_command` | `program`、`args`、可选 `timeout_seconds` | 只运行 allowlist 中的 Go/Git 子命令，输出最多 64 KiB | `never` |
| `get_weather` | `location`、`units` | Open-Meteo 查询当前天气；`units` 只能是 `celsius`/`fahrenheit`，无需 API key | `idempotent` |
| `delegate_review` | `task` | 内存 child workflow，返回 synthetic result ref | `idempotent` |

前 7 个由 `authorizedTool` 包装：读取、搜索和 weather 默认 allow；写文件和命令默认 ask。`delegate_review` 直接注册，没有经过 `authorizedTool`；策略 switch 中虽列出 `subagent.spawn`，当前 bridge 并未调用该 permission action。

`run_command` 不接收 shell 字符串，允许的子命令只有 `go test|vet|build|fmt` 和 `git status|diff|log|show`；后续参数直接传给程序，这是演示用 allowlist，不是完备的命令安全策略。

## 写权限 blocker

默认调用 `write_file` 或 `run_command` 时，permission policy 返回 `ask`，bridge 把它转换为模型可见的错误结果：

```text
approval required: request=<request-ref> token=<resume-token>
```

同时设置 `ToolResult.IsError=true` 和 `StopTurn=true`，本轮以 `completed / tool_stop_turn` 结束，副作用没有执行。这是 blocker 演示，不是完整审批流程：permission store 在内存中，CLI 没有 approve 命令，不会持久化 prepared effect，也不会拿 token 自动继续原 turn。需要写入时只能为整个进程启用 `--allow-writes`——它同时允许 `workspace.write` 和 `workspace.command`，不是单次审批，也不是按文件授权。

## MCP

只支持一个 Streamable HTTP endpoint：

```bash
go run ./cmd/icoder --workspace ../.. --db /tmp/icoder-mcp.db \
  --mcp-url 'https://approved-mcp.example.com/mcp' \
  --task '列出可用工具，并使用 MCP 工具完成调查'
```

启动时 manager 连接 endpoint、获取 capability snapshot，把发现的工具注册进同一个 registry。HTTP policy 将 URL 的精确 `host[:port]` 放入 allowlist，响应和工具结果上限 64 KiB，允许 insecure loopback；不从 MCP 配置启动本地 stdio 命令。

重要边界：动态 MCP tool 的 `Execute` 直接调用 `mcp.Manager.CallTool`，**不经过** iCoder 的 `permission.Service`；`--allow-writes` 对 MCP 没有控制作用。只应连接受信 endpoint。MCP generation 也不写入 demo SQLite。

## Sub-Agent

模型可调用 `delegate_review`（输入 `{"task":"..."}`）。它确实使用 `subagent.Service` 的 `Spawn`、`RunNext` 和 `Reconcile`，设置 depth、fanout、token、cost、tool-call 和 runtime 限额；store、runner 和 parent waker 都是 demo 内存实现。

但 `reviewRunner` 不调用模型、不读取仓库：它只根据 task bytes 计算 `review:<digest-fragment>`，估算 input token，固定返回 32 output tokens。这是 **synthetic** 实现，用于演示 child run 生命周期与预算接口，不是独立 reviewer agent。进程重启后 child 状态丢失，wake recorder 不会恢复 parent Agent。

## SQLite 与 CommitTurn 事务

SQLite 默认 DSN 开启 10 秒 busy timeout 和 foreign keys，连接池限制为单连接。四张表：`icoder_sessions`（revision + 累计 usage）、`icoder_messages`（按 ordinal 追加 canonical messages）、`icoder_turns`（按 `(session_id, request_id)` 幂等的 turn 快照）、`icoder_events`（sequence 主键 + 唯一 event_id 的 terminal events）。

`CommitTurn` 在一个数据库事务中完成：

1. 按 `(session_id, request_id)` 检查幂等；相同 ID 但 input/result 不同则冲突。
2. 计算下一条 message ordinal，追加本轮 user message 和 `RunResult.Messages`。
3. 用 `WHERE revision = snapshot.Revision` 做 session revision CAS，并累加 usage。
4. 写入 turn result snapshot 和一个 `agent.run.terminal` event（sequence 等于新 revision）。
5. commit；任一步失败则 rollback。

模型请求、weather/MCP 网络请求、subagent 执行、文件写入和命令执行都发生在事务之外：事务保证 demo 自己的 session 聚合原子发布，不保证外部副作用 exactly-once。若 Agent run 返回 Go error，`App.Run` 不调用 `CommitTurn`，失败 run 不落表。这不是 `session.SessionAgent` host，也没有使用根包 durable checkpoint；它是单进程、乐观 revision CAS 的 demo-owned store。

## 一次请求的流程

1. CLI 收到 `--task` 或 REPL 输入，调用 `App.Run(instruction, observe)`。
2. App 从 SQLite 加载 session snapshot（revision、usage）和 history，执行 `NormalizeHistory(RepairReject)`。
3. 渲染 versioned system prompt，与 untrusted skill messages、history、user message 一起交给 `context.Planner`，得到不可变 Plan。
4. `agent.Agent.Run(plan.Messages, ObservationEmitter)` 启动 model/tool loop（最多 20 个 model steps），`openaicompat.Model` 通过 SSE 转换出 canonical chunks，CLI 即时打印 `ObservationTextDelta`。
5. 工具调用经 allowlist 与 JSON Schema 校验后：内置 7 工具走 `permission.Service`（ask/deny 触发 stop-turn blocker）；`delegate_review` 走内存 subagent；MCP 工具直接 `CallTool`。
6. 终态返回后 `CommitTurn` 在单事务中提交 messages、usage CAS、turn 快照和 terminal event；CLI 打印 outcome/stop reason/steps/tokens。

Observation 是容量 64 的 best-effort 非阻塞通知，不是权威事件日志；权威 terminal event 是与 session 更新同事务写入 SQLite 的记录。

## 安全与可靠性边界

- Workspace confinement 用绝对路径、`EvalSymlinks` 和 `filepath.Rel` 检查实现，不是 OS sandbox；路径检查与实际读写之间可能有 TOCTOU。
- 工具和命令以启动 iCoder 的同一 OS 用户运行，继承进程环境变量；没有容器、seccomp、chroot 或资源隔离。
- `read_file` 不验证 UTF-8；二进制内容按 Go string 返回并截断。
- context budget 的 byte counter 是约每 4 bytes 一个 token 的保守估算，不是模型 tokenizer。
- permission approval、subagent store、Context plan store 都是内存实现；MCP 不经过 permission，MCP generation 不持久化。
- SQLite 只保存成功 commit 的 turn，不是完整 durable execution ledger。

## 如何继续扩展

按边界逐项替换，不要把策略塞进 runtime Core：

1. 生产 `agent.Model` adapter：正确聚合 tool-call arguments、归一化 usage 和 `ModelError`，通过 `agenttest` conformance 测试。
2. workspace 工具加 OS 级隔离：容器或受限 worker、资源限制、低权限身份。
3. 写操作改为 prepare/approve/resume：持久化 permission store、opaque blocker 和 durable checkpoint，恢复时重新校验 input digest、policy generation 和 fence。
4. 所有 effect tool（含 MCP 和 subagent spawn）统一走 `tool.Executor`/permission interceptor。
5. `reviewRunner` 换成真实 child Agent facade，配持久 subagent store、真实 parent wake 和独立预算。
6. demo SQLite 迁移到应用自有 migration；需要 worker 恢复时引入 `session`、`durable`、lease、fence 和 effect ledger。
7. 为 Skills/MCP 保存选定 generation 和 digest，使恢复请求可检测 capability drift；增加结构化审计。

生产级组合模式见 [production.md](production.md)，返回语义和验证命令见[速查表](reference.md)。
