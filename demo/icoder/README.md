# iCoder Reference Application

`icoder` 是一个基于 Agent Runtime for Go 构建的可运行 Code Agent。它是独立 Go module，只依赖 runtime 的公开 API。

它演示以下组合：

```text
OpenAI-compatible endpoint
          |
          v
      agent.Model
          |
trusted prompt + untrusted skills + normalized SQLite history
          |
          v
   context immutable Plan
          |
          v
      agent.Agent
          |
          +-- list_files / read_file / search_code / write_file / run_command
          +-- get_weather (Open-Meteo)
          +-- permission.Service
          +-- optional MCP tools
          +-- delegate_review -> subagent.Service
          |
          v
SQLite transaction: session CAS + messages + usage + terminal event
```

## Run

```bash
cd demo/icoder
go test ./... -count=1
go build ./cmd/icoder
```

设置一个支持 Chat Completions tool calling 的 OpenAI-compatible endpoint：

```bash
export ICODER_API_KEY=your-key
export ICODER_BASE_URL=https://api.example.com/v1
export ICODER_MODEL=your-model
```

运行只读任务：

```bash
go run ./cmd/icoder \
  --workspace ../../../.. \
  --skills ./skills \
  --session sdk-review \
  --task "阅读 agent-runtime-go 的 tool loop，并说明错误如何传播"
```

## Interactive Mode

不传 `--task` 或显式使用 `-i` / `--interactive` 会启动 REPL：

```bash
go run ./cmd/icoder \
  --workspace ../../../.. \
  --skills ./skills \
  -i
```

不传 `--session` 时，启动会使用 `crypto/rand` 生成 8 字节随机数，并编码为 16 位小写十六进制字符串，例如 `4c82f7a931d65e0b`。该值同时作为 demo 的 session 名称和 SQLite ID。

要恢复已知会话，可显式传入：

```bash
go run ./cmd/icoder --workspace ../../../.. --skills ./skills --session sdk-review -i
```

```text
iCoder interactive mode. Type /help for commands.
icoder[sdk-review:.../open-source]> /pwd
/home/user/open-source
icoder[sdk-review:.../open-source]> /cd agent-runtime-go
/home/user/open-source/agent-runtime-go
icoder[sdk-review:.../agent-runtime-go]> 查看当前目录的 tool loop
```

Host commands：

| 命令 | 行为 |
|---|---|
| `/help` | 显示命令帮助 |
| `/pwd` | 显示工具实际使用的当前目录 |
| `/cd <path>` | 在 workspace 内切换目录；`/cd /` 返回 workspace root |
| `/session` | 显示当前 session ID |
| `/sessions` | 列出 SQLite 中持久化的 sessions |
| `/use <id>` | 切换或创建 session |
| `/new [id]` | 创建并切换到新 session；省略 ID 时生成随机 16 位 ID |
| `/history` | 显示当前 session 的 canonical messages |
| `/clear` | 删除当前 session 的 messages、turns 和 events |
| `/events` | 回放当前 session 的 terminal events |
| `/tools` | 列出 model-visible tools |
| `/skills` | 列出已加载的 skill capability messages |
| `/exit` | 退出 REPL |

Slash commands 由 CLI host 执行，不会发送给模型。自然语言 `cd agent-runtime-go` 只是对话文本；要真正改变后续 `list_files`、`search_code`、`read_file`、`write_file` 和 `run_command` 的执行目录，必须使用 `/cd agent-runtime-go`。

工作目录只存在于当前进程；会话历史、usage 和 terminal events 持久化到 SQLite。`/history` 始终只查询当前 active session，切换 session 不会混用历史。重新启动时若未指定 `--session`，会生成新 ID；使用 `/use <id>` 或 `--session <id>` 才会恢复指定历史。

默认情况下 `write_file` 会生成 approval blocker 并结束当前 turn，不会写文件。显式允许本次进程写入：

```bash
go run ./cmd/icoder \
  --workspace /path/to/repository \
  --allow-writes \
  --task "修改 README 中的拼写错误"
```

`run_command` 同样受 `--allow-writes` 控制，并且只接受 `go test/vet/build/fmt` 与 `git status/diff/log/show`。它不接受 Shell 文本，也不允许提交、推送或删除。

`get_weather` 是只读网络工具，使用 Open-Meteo 的 geocoding 和 current forecast API，不需要 API key：

```bash
go run ./cmd/icoder \
  --workspace /path/to/repository \
  --task "查询北京当前天气，使用摄氏度"
```

工具输入是 `{"location":"Beijing","units":"celsius"}`，其中 `units` 支持 `celsius` 和 `fahrenheit`。

查看 SQLite 中持久化的 terminal events：

```bash
go run ./cmd/icoder --workspace /path/to/repository --session default --events
```

可选接入 Streamable HTTP MCP：

```bash
go run ./cmd/icoder \
  --workspace /path/to/repository \
  --mcp-url https://approved-mcp.example.com/mcp \
  --task "使用可用工具完成调查"
```

MCP endpoint 会被加入精确 host allowlist。示例不支持从配置任意启动本地命令。

## Source Map

| 文件 | 说明 |
|---|---|
| `internal/icoder/provider.go` | `agent.Model` 的 OpenAI-compatible adapter |
| `internal/icoder/app.go` | composition root、provider catalog、context plan、prompt 和 skills |
| `internal/icoder/workspace.go` | workspace confinement、文件操作和受控命令执行 |
| `internal/icoder/tools.go` | 根 Tool adapter 与 `permission.Service` bridge |
| `internal/icoder/weather.go` | `WeatherProvider` port、Open-Meteo adapter 与 `get_weather` Tool |
| `internal/icoder/capabilities.go` | MCP tool bridge 与 `subagent.Service` bridge |
| `internal/icoder/store.go` | demo-owned SQLite schema 和原子 turn commit |
| `cmd/icoder/main.go` | CLI transport |

## SQLite Schema

Schema 只属于该 demo：

- `icoder_sessions`：revision 与累计 usage。
- `icoder_messages`：按 session ordinal 保存 canonical `agent.Message`。
- `icoder_turns`：request idempotency key 和 result snapshot。
- `icoder_events`：与 turn 同事务写入的 terminal event。

`CommitTurn` 在一个事务中执行 session revision CAS、消息追加、usage 累加、request 去重和 terminal event 写入。模型请求和工具副作用始终发生在事务外。

## Deliberate Limits

- Provider adapter 使用非流式 Chat Completions HTTP 响应，再转换为符合 SDK 约束的 `StreamChunk`；生产 adapter 应实现真正的增量流。
- byte counter 是保守演示，不是模型精确 tokenizer。
- workspace confinement 不是 OS sandbox，仍存在本地文件系统 TOCTOU 风险。
- REPL 当前工作目录是进程内状态，重启后恢复为 `--workspace` root；session history 则从 SQLite 恢复。
- permission approval 使用内存 store。默认 ask 会停止 turn；示例不伪造 approval 后自动恢复已经 prepared 的 effect。
- weather provider 访问公共 Open-Meteo 服务；生产环境应通过统一 egress policy、代理和审计控制外部网络。
- subagent 使用内存 store，展示 child run、预算和 wake intent，不承诺进程重启恢复，也不自动恢复 parent Agent。
- MCP 是可选实时 capability，示例不持久化 MCP generation。
- SQLite 主链是单进程 turn aggregate，不等同于完整的 `session.SessionAgent` worker host。

完整的包选择和生产替换路径见 [`../../docs/`](../../docs/README.md)。
