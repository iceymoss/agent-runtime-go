# iCoder

`iCoder` 是基于 Agent Runtime for Go 构建的终端 Code Agent。它提供 Bubble Tea TUI、Cobra 命令行、流式模型输出、工具执行状态、SQLite 多会话历史、受控工作区编辑、Skills 和可选 MCP。

## 功能

- 默认启动全屏 TUI，支持多行输入、结构化工具卡片、流式回答和运行取消。
- 写入或命令执行时显示审批面板，可逐次允许或拒绝，不需要全局开放权限。
- 支持 slash command 自动补全、session picker、工具详情展开和 viewport 浏览。
- `run` 非交互模式可用于脚本和 CI，支持 stdin 与 JSON 输出。
- SQLite 持久化对话、usage 和 terminal events。
- 支持 ignore 规则的文件 glob/搜索、结构化范围读取、冲突安全的局部编辑、批量 patch 和全量写入。
- 受限的 Go/Git 命令执行；写入和命令默认需要授权。
- Skills、OpenAI-compatible provider 和 Streamable HTTP MCP。
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
icoder session list            列出会话
icoder session history         查看当前会话历史
icoder session clear           清空当前会话
icoder events                  回放当前会话 terminal events
icoder tools                   列出工具
icoder config show             显示已解析的非敏感配置
icoder config validate         验证配置
icoder completion <shell>      生成 shell completion
icoder version                 显示版本
```

使用 `icoder <command> --help` 查看完整 flags。常用全局参数包括 `--workspace`、`--session`、`--db`、`--model`、`--max-steps`、`--max-tokens`、`--skills` 和 `--mcp-url`。

## 工具与权限

| 工具 | 用途 | 默认权限 |
|---|---|---|
| `get_working_directory` | 返回工具 cwd | allow |
| `list_files` | 递归列文件 | allow |
| `glob_files` | 按 glob 查找文件，支持 `**` | allow |
| `read_file` | 结构化范围读取，返回行号、总行数、digest 与截断信息 | allow |
| `search_code` | 字面或正则搜索，可按 glob 过滤 | allow |
| `git_status` | 查看分支和 workspace 状态 | allow |
| `git_diff` | 查看 unstaged/staged diff | allow |
| `edit_file` | 精确局部替换，支持 digest 冲突检查 | ask |
| `apply_patch` | 批量创建、精确更新和删除文件，支持预检与失败回滚 | ask |
| `move_file` | 在 workspace 内移动文件，不覆盖已有目标 | ask |
| `create_directory` | 创建目录，可选择创建缺失的父目录 | ask |
| `write_file` | 创建或完整替换文件 | ask |
| `run_command` | 执行受限 Go/Git 命令 | ask |
| `get_weather` | Open-Meteo 只读网络请求 | allow |
| `delegate_review` | 演示 Sub-Agent 生命周期 | allow |

默认情况下，TUI 会在写入或执行命令前显示审批面板。选择 `y`、`1` 或 `Enter` 只允许当前精确工具调用；选择 `n`、`2` 或 `Esc` 拒绝操作。批准结果绑定 tool call、输入 digest 和当前 run，并在执行前重新验证。

非交互 `run` 不会从 stdin 隐式询问审批，默认安全拒绝副作用。确认工作区可信后，可以为当前进程开启所有本地写入和受限命令：

```bash
./icoder --workspace /path/to/repository --allow-writes
./icoder run --workspace /path/to/repository --allow-writes '完成修改并测试'
```

`--allow-writes` 是进程级授权，不是 sandbox。工具使用当前用户身份运行，路径限制也不能替代操作系统隔离。`run_command` 只允许 `go test/vet/build/fmt` 和 `git status/diff/log/show`；只读审查应优先使用无需审批的 `git_status` 与 `git_diff`。

## 数据与扩展

默认数据库为 `<workspace>/.icoder.db`。`--session` 可恢复指定会话；未指定时生成随机 session ID。建议在不希望修改仓库内容时显式指定 `--db /tmp/icoder.db`。

```bash
./icoder --skills ./skills
./icoder --mcp-url 'https://approved-mcp.example.com/mcp'
```

MCP 工具由远端服务定义，目前不经过本地 permission wrapper，只应连接受信 endpoint。`delegate_review` 当前是 synthetic lifecycle 示例，不会执行真实模型 review。

## 验证

```bash
CGO_ENABLED=1 go test ./... -count=1
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go build ./cmd/icoder
```
