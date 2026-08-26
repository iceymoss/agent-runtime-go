---
name: run-demo
description: 本地运行示例、iCoder demo 应用和文档站。当用户说跑起来、运行示例、启动 icoder、看文档站、本地预览时触发。
---

本仓库是**库**，没有常驻服务。本地可运行的东西有三类：确定性示例、iCoder demo 应用、文档站。

## 1. 示例（`examples/`）

前两个是确定性的，不需要任何 API key：

```bash
go run ./examples/hello        # 最小 Model 适配器 + 一次 run，输出 "Hello from Agent Runtime for Go."
go run ./examples/tool-agent   # 完整模型/工具循环（脚本化模型，两步）
```

接真实模型（任意 OpenAI 兼容端点）：

```bash
OPENAI_API_KEY=sk-... go run ./examples/openai-compat
```

示例用来验证改动没破坏对外契约，改根包后建议顺手跑一遍前两个。

## 2. iCoder demo（`demo/icoder`）

独立 module，需要 Go 1.25+、C 编译器和 `CGO_ENABLED=1`（SQLite 驱动）。

```bash
cd demo/icoder
go build -o icoder ./cmd/icoder
```

配置 OpenAI 兼容端点（**只从环境变量读，没有 CLI flag，避免进 shell history**）：

```bash
export ICODER_API_KEY='your-key'
export ICODER_BASE_URL='https://api.example.com/v1'
export ICODER_MODEL='your-model'
```

运行：

```bash
./icoder --workspace /path/to/repository          # 全屏 TUI（等价于 icoder chat）
./icoder run --workspace /path/to/repo '修复失败的测试'   # 非交互模式，可用于脚本/CI
```

TUI 里 `Ctrl+C` 运行中取消 / 空闲时退出，`Esc` 取消当前运行。详细按键与 slash command 见 `demo/icoder/README.md`。

**不要拿本仓库当 workspace 做破坏性试验**，指向一个临时目录或专门的测试仓库。

## 3. 文档站（`docs-site`）

VitePress，包管理器只用 **bun**。srcDir 是 `docs/`，outDir 是 `cmd/docs/dist`。

```bash
cd docs-site
bun install
bun run dev        # 开发服务器，默认 http://127.0.0.1:5173
bun run build      # 产物输出到 ../cmd/docs/dist
bun run typecheck
```

用 Go 服务端预览（`cmd/docs`）：

```bash
go run ./cmd/docs --dev-url http://127.0.0.1:5173   # 代理开发服务器
go build -tags docsprod ./cmd/docs                  # 把 dist 用 go:embed 打进二进制
```

不带 `docsprod` tag 且不给 `--dev-url` 会直接报错，这是预期行为。

## 端口冲突排查

```bash
lsof -i :5173
```

## 严格约束
- 不要 `pkill -f go` / `kill -9 -1`，会误伤其他进程；用 `Ctrl+C` 或对具体 pid `kill`
- 不要把 `ICODER_API_KEY` 写进文件或提交；只用环境变量
- 不要手改 `cmd/docs/dist`（构建产物）
- 包管理器只用 bun，不要 npm / pnpm / yarn
