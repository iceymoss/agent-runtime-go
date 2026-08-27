# iCoder

`iCoder` 是基于 Agent Runtime for Go 构建的终端 Code Agent。它提供 Bubble Tea TUI、Cobra 命令行、流式模型输出、工具执行状态、SQLite 多会话历史、受控工作区编辑、Skills 和可选 MCP。

## 功能

- 默认启动全屏 TUI，支持多行输入、结构化工具卡片、流式回答和运行取消。
- 写入或命令执行时显示审批面板，可逐次允许或拒绝，不需要全局开放权限。
- 支持 slash command 自动补全、session picker、工具详情展开和 viewport 浏览。
- `run` 非交互模式可用于脚本和 CI，支持 stdin 与 JSON 输出。
- SQLite 持久化对话、usage、terminal events，以及可恢复的任务状态、changed files 和 validation checks。
- 每次任务都是一次 durable run：模型响应与工具副作用都写进快照和 effect 台账，进程崩溃或被中断后可以按 run key 恢复。
- 工具经过冻结的 tool generation 执行，带 action、effect class、幂等性与重放策略声明，以及可持久化的执行台账。
- 支持 ignore 规则的文件 glob/搜索、结构化范围读取、冲突安全的局部编辑、批量 patch 和全量写入。
- 不经过 shell 的 Go、Node、Python、Rust、Make 和只读 Git 命令 profile；写入和命令默认需要授权。
- Skills、OpenAI-compatible provider 和 Streamable HTTP MCP。
- 分层加载 workspace 根目录到当前工具 cwd 的 `AGENTS.md` 项目指令。
- Bash、Zsh、Fish 和 PowerShell completion。

## 安装

要求 Go 1.25+、C 编译器和 `CGO_ENABLED=1`。SQLite 驱动依赖 CGO。

```bash
cd demo/icoder
go build -o icoder ./cmd/icoder
```

配置 OpenAI-compatible endpoint：

```bash
export ICODER_API_KEY='your-key'
export ICODER_BASE_URL='https://api.example.com/v1'
export ICODER_MODEL='your-model'
```

API key 只从环境变量读取，不提供 CLI flag，避免进入 shell history。

## TUI

从目标仓库运行，或通过 `--workspace` 指定工作区：

```bash
./icoder --workspace /path/to/repository
# 等价：./icoder chat --workspace /path/to/repository
```

| 按键 | 行为 |
|---|---|
| `Enter` | 提交任务 |
| `Alt+Enter` / `Ctrl+J` | 输入换行 |
| `Ctrl+C` | 运行中取消；空闲时退出 |
| `Esc` | 取消当前运行 |
| `Ctrl+P` | 打开 slash command 补全 |
| `Ctrl+L` | 打开 session picker |
| `Ctrl+O` | 展开或折叠工具输入输出 |
| `PageUp` / `PageDown` | 浏览 transcript |
| `Tab` | 审批时在单次允许和 scoped AUTO 间切换 |

TUI slash commands：

| 命令 | 行为 |
|---|---|
| `/help` | 显示命令 |
| `/pwd` | 显示工具 cwd |
| `/cd <path>` | 在 workspace 内切换目录；`/cd /` 返回根目录 |
| `/sessions` | 打开持久化会话选择器 |
| `/use <id>` | 切换或创建会话 |
| `/new [id]` | 创建并切换会话 |
| `/clear` | 清空当前会话 |
| `/tools` | 列出模型可见工具 |
| `/details` | 展开或折叠工具详情 |
| `/diff` | 显示当前 Git workspace diff |
| `/status` | 显示当前 session 和 runtime 状态 |
| `/quit` | 退出 |

## 非交互模式

```bash
./icoder run --workspace /path/to/repository \
  '检查当前修改，修复问题并运行最小测试'

printf '%s' '解释这个项目的架构' | ./icoder run --workspace .

./icoder run --json '审查当前 git diff'
```

旧版 `--task` 已迁移到 `run --task`：

```bash
./icoder run --task '列出关键包及职责'
```

## 命令

```text
icoder                         启动 TUI
icoder chat                    显式启动 TUI
icoder run [prompt]            执行单个任务
icoder eval                    使用当前模型运行 deterministic coding suite
icoder session list            列出会话
icoder session history         查看当前会话历史
icoder session clear           清空当前会话
icoder events                  回放当前会话 terminal events
icoder runs list               列出挂起或未完成的 durable run
icoder runs effects <run-key>  查看某次 run 的工具副作用台账
icoder runs approve <run-key>  批准挂起的工具调用并继续该 run
icoder runs deny <run-key>     拒绝挂起的工具调用并结束该 run
icoder runs resume <run-key>   继续一个挂起在委派工作上的 run（先推进 child）
icoder queue submit <指令>     把一条指令提交为可持久化的后台工作
icoder queue work              在当前进程执行一条排队中的 run
icoder queue show <run-key>    查看排队 run 的状态与结果
icoder queue cancel <run-key>  放弃一条排队中的 run
icoder runs abandon <run-key>  直接结束一个未完成的 run
icoder delegations             列出已记录的 sub-agent 运行与结果
icoder runtime show            显示本进程解析出的 runtime generation
icoder runtime list            列出已记录的 runtime generation
icoder runtime verify <digest> 重建某个已记录 generation 并校验是否仍然一致
icoder daemon                  以受监督进程方式运行：readiness + 有界关闭
icoder outbox dispatch         将一批可靠事件输出为 JSON Lines 并确认投递
icoder tools                   列出工具
icoder config show             显示已解析的非敏感配置
icoder config validate         验证配置
icoder completion <shell>      生成 shell completion
icoder version                 显示版本
```

使用 `icoder <command> --help` 查看完整 flags。常用全局参数包括 `--workspace`、`--session`、`--db`、`--model`、`--max-steps`、`--max-tokens`、`--skills` 和 `--mcp-url`。

真实模型评测会为每个 fixture 创建隔离 workspace 和数据库，并输出任务通过率、token、工具调用和耗时：

```bash
./icoder eval --fixtures internal/eval
```

## 工具与权限

| 工具 | 用途 | 默认权限 |
|---|---|---|
| `get_working_directory` | 返回工具 cwd | allow |
| `list_files` | 递归列文件 | allow |
| `glob_files` | 按 glob 查找文件，支持 `**` | allow |
| `read_file` | 结构化范围读取，返回行号、总行数、digest 与截断信息 | allow |
| `search_code` | 字面或正则搜索，可按 glob 过滤 | allow |
| `list_skills` | 列出可用 Skill metadata | allow |
| `load_skill` | 按需读取一个 Skill 的完整不可信指令 | allow |
| `git_status` | 查看分支和 workspace 状态 | allow |
| `git_diff` | 查看 unstaged/staged diff | allow |
| `git_commit` | 暂存明确的相对路径并创建普通 commit；拒绝无关 staged 文件，不支持 amend/push | ask |
| `edit_file` | 精确局部替换，支持 digest 冲突检查 | ask |
| `apply_patch` | 批量创建、精确更新和删除文件，支持预检与失败回滚 | ask |
| `move_file` | 在 workspace 内移动文件，不覆盖已有目标 | ask |
| `create_directory` | 创建目录，可选择创建缺失的父目录 | ask |
| `write_file` | 创建或完整替换文件 | ask |
| `run_command` | 执行受限 Go/Git 命令 | ask |
| `get_weather` | Open-Meteo 只读网络请求 | allow |
| `delegate_review` | 委派独立只读 reviewer child agent | allow |
| `delegate_explore` | 委派独立只读 explorer child agent 定位代码 | allow |

默认情况下，TUI 会在写入或执行命令前显示三项审批列表：`1. Yes`、`2. Yes, allow <具体范围> for this session` 和 `3. No`。使用上下方向键遍历，`Tab` 在单次允许和 scoped AUTO 间快速切换，`Enter` 确认；`1`、`2`、`3` 可直接确认对应选项，`y` 和 `n` 是 Yes/No 快捷键。scoped AUTO 只允许当前 session 中同一类 action，例如 workspace writes 不会同时放开 commands 或 network；状态栏会持续显示已启用的 `AUTO writes`、`AUTO commands` 等范围。使用 `/permissions` 查看当前 session 的范围，`/permissions clear` 撤销。每个自动批准的调用仍绑定 tool call、输入 digest 和当前 run，并执行 Resolve/Revalidate。审批默认有效 15 分钟，确认后进入验证状态并锁定任务输入。若审批已过期，工具不会执行；重新提交原任务即可生成新的审批请求。

选择 `3. No` 后，底部区域会切换为反馈输入。输入替代方案并按 `Enter` 后，iCoder 会先完成当前拒绝 turn，再在同一 session 中自动继续该反馈。正常运行期间也可以在底部输入下一条指令；`Enter` 将其加入队列，当前 run 结束后按顺序执行。排队不会修改正在执行的模型上下文，也不会批准被拒绝的工具调用。

非交互 `run` 不会从 stdin 隐式询问审批。遇到需要授权的工具时，它不会猜测，而是把整个 run **挂起**并落盘，然后告诉你怎么继续：

```bash
./icoder run --workspace /path/to/repository '把 note.txt 写成 hello'
# [suspended: tool_suspended, ...]
# A tool is waiting for approval. Review it with 'icoder runs list', then run
# 'icoder runs approve <run-key>' to continue or 'icoder runs deny <run-key>' to stop.

./icoder runs list
./icoder runs approve 'sha256:...:9f2c'
```

审批记录、resume token 和授权 grant 都存在 SQLite 里，所以批准可以由**另一个进程、另一个时间点**完成。恢复时运行时会用原始 input/config digest 校验这确实是同一次 run，并从挂起的那一个工具调用精确继续，已经完成的工具结果不会重跑。

确认工作区可信后，可以为当前进程开启所有本地写入和受限命令，跳过逐次审批：

```bash
./icoder --workspace /path/to/repository --allow-writes
./icoder run --workspace /path/to/repository --allow-writes '完成修改并测试'
```

`--allow-writes` 是进程级授权，不是 sandbox。工具使用当前用户身份运行，路径限制也不能替代操作系统隔离。`run_command` 不启动 shell，只允许预定义的 Go、Node、Python、Rust、Make 和只读 Git profile；只读审查应优先使用无需审批的 `git_status` 与 `git_diff`。用户明确要求提交时，Agent 使用独立的 `git_commit` 工具：它只接受明确的 workspace 相对路径和单行 message，保留 commit hooks，并拒绝把请求范围外的 staged 文件带入 commit。

## 可恢复执行

每次任务都是一次 durable run，记录在 `durable_runs`、`durable_effects`、`durable_usage` 三张表里：

| 记录 | 内容 | 用途 |
|---|---|---|
| run 快照 | 身份、input/config digest、状态机（status × phase）、revision、fence token、完整 checkpoint | 崩溃后从最后一个持久化边界继续，而不是从零重跑 |
| effect 台账 | 每个工具调用的 prepared → running → succeeded/failed，崩溃时转为 unknown | 判断某次写入/命令到底执行没执行；unknown 默认拒绝自动重放 |
| usage 台账 | 不可变用量事实，按 key 幂等 | 重试不会重复计费 |

写授权由 `(run key, lease owner, revision, fence token)` 四元组保护，任何一项过期即拒绝，所以僵尸 worker 无法覆盖新 worker 的数据。

工具侧还有一层 `tool_executions` 台账：它记录每次调用的冻结身份（canonical input、input digest、execution key、tool generation），审批恢复时按这份记录重新校验，而不是重新推导一遍。

一次 run 的终态与会话 turn 在**同一个 SQLite 事务**里提交，所以不会出现"对话里有这轮，但恢复记录说没发生"的分歧。

## 冻结的 runtime generation

启动时 iCoder 会把 model、prompt、tool set、执行参数和 policy 组合成一个不可变的 `agent.RuntimeDefinition`，并通过 `coordinator` 把它的 **wire manifest** 落到 SQLite。运行用的 `*agent.Agent` 由这个 definition materialize 出来，所以"实际在跑的东西"和"记录下来的东西"必然一致。

```bash
./icoder runtime show      # definition/manifest digest、model、artifacts
./icoder runtime list      # 这个工作区跑过哪些组合
./icoder runtime verify sha256:...   # 用当前二进制重建，digest 不一致就报错
```

manifest 里只有可持久化的值：definition digest、recipe 版本、provider/model、prompt 消息、执行参数、工具名清单，以及 prompt / policy / tools / toolset / skills 的 artifact 引用。可执行对象本身不会被序列化——重建走的是同一套 builder，再由 coordinator 校验 digest、prompt 和 artifact 身份是否逐项一致。

改动任何一项（换 model、改 prompt、增删工具、改 policy）都会产生新的 definition digest，从而让旧的审批 grant 和旧的 context plan 自动失效。

## 守护进程与有界关闭

`icoder daemon` 用 `app` 包把进程包成一个有生命周期的服务：

```bash
./icoder daemon --drain 30s
# ready state=ready components=4 generation=1
#   store    ready=true degraded=false
#   runtime  ready=true degraded=false
#   tools    ready=true degraded=false
#   events   ready=true degraded=false
```

- 组件按依赖顺序启动：`store` → `runtime` → `tools` / `events`；`skills` 和 `mcp` 是 optional，缺失只降级不阻止启动。
- readiness 是真实探针（ping 数据库、读事件流），并且 ready 的组件必须报出自己的 generation，探针能区分"起来了"和"起来的是你要的那一版"。
- 收到信号后按 drain → cancel → checkpoint → wait → flush → close 逐阶段收尾，每个阶段有独立预算：先停止准入、等在跑的 run 自己结束、超时才取消、取消后吊销 durable lease 让别的 worker 能立刻接手、最后把 outbox 里剩下的可靠事件投递出去。

## 数据与扩展

默认数据库为 `<workspace>/.icoder.db`。`--session` 可恢复指定会话；未指定时生成随机 session ID。建议在不希望修改仓库内容时显式指定 `--db /tmp/icoder.db`。

```bash
./icoder --skills ./skills
./icoder --mcp-url 'https://approved-mcp.example.com/mcp'
```

MCP 工具由远端服务定义且默认按未知网络副作用逐次询问审批。连接器仍只应指向经过批准的 endpoint。

`delegate_review` 和 `delegate_explore` 是真实的 child agent：各自拥有独立 context、独立 token 预算和**只读**工具集，由 `subagent.Service` 统一管理 depth、fanout、预算预留和取消。父运行只收到结构化结果（`result_ref` + 文本），不会把 child 的完整历史复制进来。child 的结果和完成事件都落 SQLite，`icoder delegations` 可以回看。

delegate 工具是**异步**的：spawn 之后它不会阻塞等待，而是返回 `agent.ToolSuspensionExternal`，让父运行落 checkpoint 挂起（`suspended` + `tool_suspended`）。应用推进 child（`RunNext` + `Reconcile`）后，凭工具自己签发的 handle 恢复这次调用，工具据此认领它启动的那个 child 而不是再起一个。这样进程在 child 执行期间崩溃，父运行仍然可以从 checkpoint 恢复，而不是丢掉这次委派。wake 早到（child 还没跑完）时工具会再次挂起而不是失败——它没有出错，只是等待的事件还没发生；重入次数由 `maxDelegationResumes` 兜底，避免永远跑不完的 child 把 attempt 挂死。挂起与恢复都会写入事件流（`agent.subagent.awaited` / `agent.subagent.completed`）。

child 关系状态机落在 SQLite（`SQLiteSubagentStore` 实现 `subagent.Store`）。异步化之后这是必需的：父运行挂起期间，relationship、它预留的树预算、以及将要唤醒父运行的 wake intent 都必须跨进程存活，否则重启会同时泄漏预算并让父运行永远醒不过来。

适配器只负责存储真正拥有的部分——找到 parent、数兄弟、原子写入；depth / fanout / 环检测 / 预算算术全部调用 `subagent` 包导出的状态机（`ValidateDepth`、`ValidateFanout`、`ValidateNoCycle`、`BudgetSnapshot.Reserve/Settle`）。这些是安全限额而不是记账：适配器自己重写一份不会大声报错，只会悄悄放宽一个调用方从未授权的深度、宽度或花费。`agenttest.TestSubagentStore` 把这个适配器和库自带参考实现按同一份契约验收。

`icoder runs list` 会把等待 child 的运行显示为 `awaiting-delegate` 而不是 `awaiting-approval`——它需要的是有人推进工作，不是有人做决定；`icoder runs resume` 会先推进 child 再恢复父运行。

## 持久化的运行队列

`icoder queue` 背后是 runtime 的 `session.SessionAgent`，存储层是 `SQLiteSessionRunStore`（`session.Store`，17 个方法）和 `SQLiteSessionService`（`session.Service`，12 个方法），两者都通过 `agenttest.TestSessionRunStore` / `TestSessionService` 验收。

它和 `icoder run` 是两条路径，这是有意的：坐在终端前的人要的是自己这一轮马上跑，排队是给 daemon 用的，提交它的进程和执行它的进程可以不是同一个。队列提供的是交互路径没有的东西——准入配额、claim 租约与接管、取消意图、以及跨重启保留排队位置。

上下文计划在**提交时**绑定：一条排到后面的 run 回答的是它被提交时那个历史下的问题，而不是 worker 终于轮到它时会话变成的样子。这就是 `AttemptRunner` 所谓「context-aware」的含义。

准入上限刻意设成 1（`MaxActiveGlobal/PerTenant/PerSession`）：一个 workspace 只有一份文件，让多个 run 并发改它只会产生没人要的交错修改。队列在这里的作用是串行化和抗重启，不是并行化。

`icoder daemon` 会启动一个 worker，关闭时走 `Shutdown` 排空：已认领的 run 要么跑完，要么带着队列位置挂起，不会半途丢下还占着租约。

`outbox dispatch` 使用 runtime `event.Dispatcher` 的 claim/lease/fence/ack 语义。发布目标是 stdout，适合作为外部日志采集器或消息投递进程的参考：

```bash
./icoder outbox dispatch --limit 100 > events.jsonl
```

## 验证

```bash
CGO_ENABLED=1 go test ./... -count=1
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go build ./cmd/icoder
```
