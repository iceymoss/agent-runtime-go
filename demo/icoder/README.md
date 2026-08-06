# iCoder 可运行参考应用

`icoder` 是基于 Agent Runtime for Go 的 Code Agent demo，也是一个独立 Go module。它只通过 runtime 的公开 API 组合 OpenAI-compatible model、8 个内置工具、Permission、Skills、可选 MCP、synthetic Sub-Agent 和 SQLite 会话历史。

完整源码导读、请求时序和扩展路线见 [iCoder 教程](../../docs/icoder.md)。

## 先看路径

以下命令都从当前目录 `demo/icoder` 运行：

```bash
cd demo/icoder
```

```text
--workspace ../..    -> agent-runtime-go 仓库根
--workspace ../../.. -> 仓库父目录 open-source
--skills ./skills    -> demo/icoder/skills
```

仓库根是 `../..`，不是 `../../../..`。本 module 的 `go.mod` 也使用 `replace github.com/iceymoss/agent-runtime-go => ../..`。

## 前置条件

- Go 1.25+。
- C 编译器和 `CGO_ENABLED=1`。`github.com/mattn/go-sqlite3` 需要 CGO；这只影响 iCoder demo，不代表 runtime Core 依赖 CGO。
- 支持 Chat Completions tool calling 的 OpenAI-compatible endpoint。

```bash
go version
go env CGO_ENABLED CC
```

## 配置与验证

```bash
export ICODER_API_KEY='your-key'
export ICODER_BASE_URL='https://api.example.com/v1'
export ICODER_MODEL='your-model'

CGO_ENABLED=1 go test ./... -count=1
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go build -o /tmp/icoder ./cmd/icoder
```

三个环境变量都必填。也可用 `--base-url` 和 `--model` 覆盖后两项；API key 没有对应 flag。

## 运行示例

只读审计当前仓库，并把 SQLite 放在仓库外：

```bash
go run ./cmd/icoder \
  --workspace ../.. \
  --db /tmp/icoder.db \
  --skills ./skills \
  --session sdk-review \
  --task '阅读 tool loop，说明模型错误和工具错误如何传播'
```

审计 `open-source` 下多个仓库：

```bash
go run ./cmd/icoder \
  --workspace ../../.. \
  --db /tmp/icoder-open-source.db \
  --skills ./skills \
  --task '找到 agent-runtime-go 并列出它的 Go modules'
```

查询天气：

```bash
go run ./cmd/icoder \
  --workspace ../.. \
  --db /tmp/icoder-weather.db \
  --task '查询北京当前天气，使用摄氏度'
```

`get_weather` 使用 Open-Meteo，不需要额外 API key，但需要访问公共网络。工具输入为 `{"location":"Beijing","units":"celsius"}`，单位只支持 `celsius` 和 `fahrenheit`。

## REPL

不传 `--task`，或显式传 `-i` / `--interactive`：

```bash
go run ./cmd/icoder \
  --workspace ../../.. \
  --db /tmp/icoder-repl.db \
  --skills ./skills \
  --session sdk-review \
  -i
```

```text
iCoder interactive mode. Type /help for commands.
icoder[sdk-review:.../open-source]> /pwd
/home/user/open-source
icoder[sdk-review:.../open-source]> /cd agent-runtime-go
/home/user/open-source/agent-runtime-go
icoder[sdk-review:.../agent-runtime-go]> 查看当前目录的 runtime 入口
```

| 命令 | 行为 |
|---|---|
| `/help` | 显示帮助 |
| `/pwd` | 显示工具当前目录 |
| `/cd <path>` | 在 workspace 内切换；`/cd /` 返回 root |
| `/session` | 显示 active session |
| `/sessions` | 列出 SQLite sessions |
| `/use <id>` | 切换或创建 session |
| `/new [id]` | 新建并切换；省略 ID 时生成随机 16 位 hex ID |
| `/history` | 显示当前 session messages |
| `/clear` | 删除当前 session 及其 history、turns、events |
| `/events` | 回放当前 session terminal events |
| `/tools` | 列出模型可见工具 |
| `/skills` | 列出已加载 Skills |
| `/exit` | 退出 |

Slash command 由 CLI 执行，不发送给模型。自然语言“进入某目录”不会改变工具 cwd，必须用 `/cd`。cwd 不持久化；session history、usage 和 terminal events 持久化到 SQLite。未指定 `--session` 时，每次启动生成新的随机 ID。

直接回放事件：

```bash
go run ./cmd/icoder \
  --workspace ../.. \
  --db /tmp/icoder.db \
  --session sdk-review \
  --events
```

## 8 个内置工具

| 工具 | 用途 | 默认权限 |
|---|---|---|
| `get_working_directory` | 返回工具 cwd | allow |
| `list_files` | 递归列文件 | allow |
| `read_file` | 读取文件，最多 64 KiB | allow |
| `search_code` | 字面子串搜索，不是正则 | allow |
| `write_file` | 全量替换/创建文件，可带 expected digest | ask |
| `run_command` | 运行受限 Go/Git 命令 | ask |
| `get_weather` | Open-Meteo 当前天气 | allow |
| `delegate_review` | 演示 child run 生命周期 | 未经过 permission wrapper |

`run_command` 只允许 `go test/vet/build/fmt` 和 `git status/diff/log/show`，输入是 program/args，不是 shell 文本。

默认的 write/command `ask` 会返回 approval blocker，设置 `StopTurn` 并结束当前 turn，不执行副作用。CLI 没有审批和恢复命令。确认风险后可为当前整个进程开启写与命令权限：

```bash
go run ./cmd/icoder \
  --workspace /path/to/repository \
  --db /tmp/icoder-write.db \
  --allow-writes \
  --task '修正 README 中一个明确的拼写错误并运行最小测试'
```

`--allow-writes` 不是单次授权，也不是 sandbox。

## Skills

```bash
go run ./cmd/icoder \
  --workspace ../.. \
  --db /tmp/icoder-skills.db \
  --skills ./skills \
  --task '按 Go code review skill 审查 store.go'
```

demo 自带 `code-review` 和 `go-code-review`。它们在启动时加载为 `<untrusted-skill>` system capability messages，不热更新，也不是权限边界。

## MCP

```bash
go run ./cmd/icoder \
  --workspace ../.. \
  --db /tmp/icoder-mcp.db \
  --mcp-url 'https://approved-mcp.example.com/mcp' \
  --task '使用可用 MCP 工具完成调查'
```

demo 只接一个 Streamable HTTP endpoint，不从配置启动本地 stdio 命令。endpoint 的精确 host 被加入 HTTP allowlist。

动态 MCP 工具直接调用 `mcp.Manager.CallTool`，**不经过 iCoder 的 `permission.Service`**；`--allow-writes` 也不控制 MCP。只连接受信服务，并在生产环境另外实施身份、权限、egress 和审计。

## Sub-Agent 的真实范围

`delegate_review` 使用内存 `subagent.Service` 展示 spawn、预算、worker claim、reconcile 和 wake intent，但 runner 是 synthetic：它不调用模型、不读取仓库、不输出真实 review，只根据 task 生成 `review:<digest>` result ref 和模拟 usage。进程重启不会恢复 child，parent 也不会被真实唤醒。

## SQLite

默认数据库是 `<workspace>/.icoder.db`，建议演示时显式传 `--db /tmp/icoder.db`。四张表分别保存：

- `icoder_sessions`：revision 和累计 usage。
- `icoder_messages`：按 ordinal 保存 canonical messages。
- `icoder_turns`：request idempotency key 和 result snapshot。
- `icoder_events`：每个已提交 turn 的 `agent.run.terminal` event。

`CommitTurn` 在同一事务中追加 messages、用 revision CAS 更新 session/usage、写 turn 和 terminal event。模型调用及文件、命令、weather、MCP、subagent 等副作用都在事务外，因此这不是外部副作用 exactly-once，也不是完整 `session`/`durable` worker host。

## 实现边界

- Provider 使用官方适配器 `providers/openaicompat`，默认走 SSE 流式并逐段转换为 `StreamChunk`；SSE 不可用时可在 `app.go` 改用 `openaicompat.WithoutStreaming()`。
- token counter 按 bytes 粗略估算 Context Plan，不是模型 tokenizer；账单 usage 取 provider 响应。
- Workspace 使用路径解析约束访问范围，但不是 OS sandbox，工具以当前用户身份执行并继承环境。
- Permission、Context plan store 和 Sub-Agent store 是内存实现。
- MCP 工具未走 permission，MCP generation 未持久化。
- 只有成功返回并提交的 turn 写入 demo SQLite。

更详细的 schema、Mermaid 时序图和生产替换建议见[完整教程](../../docs/icoder.md)；API 结果和包选择见[速查表](../../docs/reference.md)。
