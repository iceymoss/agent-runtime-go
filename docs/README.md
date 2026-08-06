# Agent Runtime for Go 文档

本目录说明如何把仓库提供的 Go 组件组合成 Agent。文档以当前源码公开接口、状态机和测试约束为准；根包导入路径是 `github.com/iceymoss/agent-runtime-go`，包名是 `agent`。

## 从哪里开始

| 你的目标 | 建议阅读 |
|---|---|
| 判断这个库是否适合项目 | [简介：定位、能力与边界](01-introduction.md) |
| 立即运行一个 model 或 tool loop | [快速开始：最小可运行 Agent](02-quick-start.md) |
| 理解类型、子包及如何选组件 | [总览：核心概念与包地图](03-overview.md) |
| 设计 provider、存储和业务层的组合 | [架构：端口、适配器与组合](04-architecture.md) |
| 排查 stream、工具、停止和恢复问题 | [实现原理：运行循环与 Durable 边界](05-runtime-internals.md) |
| 查阅根包公开 API 与代码片段 | [根包核心内容](06-root-package.md) |
| 查阅全部子包及接入边界 | [子包职责与接入指南](07-subpackages.md) |
| 组合生产服务 | [生产组合模式](08-production-patterns.md) |
| 运行完整 Code Agent | [iCoder 端到端教程](09-icoder-tutorial.md) |
| 快速查找枚举、错误和命令 | [速查与术语](10-reference.md) |

## 推荐学习路线

1. 先读[简介：定位、能力与边界](01-introduction.md)，确认根包负责什么、不负责什么。
2. 运行[快速开始：最小可运行 Agent](02-quick-start.md)的两个示例，观察一次模型直答和两步工具调用。
3. 用[总览：核心概念与包地图](03-overview.md)选择需要的可选子包；普通单次请求通常只需要根包。
4. 在接入真实 provider 或数据库前，阅读[架构：端口、适配器与组合](04-architecture.md)。
5. 实现 `Model.Stream`、`Tool.Execute` 或 `CheckpointStore` 时，以[实现原理：运行循环与 Durable 边界](05-runtime-internals.md)中的协议和状态边界为准。

## 启动文档网站

开发模式需要两个终端。先启动 Vite：

```bash
npm install --prefix docs-site
npm --prefix docs-site run dev
```

再由根 module 的 Go 命令代理 Vite：

```bash
go run ./cmd/docs --dev-url http://127.0.0.1:5173
```

访问 `http://127.0.0.1:8080`。生产构建会把 React 站点和全部 Markdown 内嵌到单个 Go 二进制：

```bash
npm --prefix docs-site run build
go build -tags docsprod -o /tmp/agent-runtime-docs ./cmd/docs
/tmp/agent-runtime-docs --listen 127.0.0.1:8080
```

`GET /healthz` 用作健康检查。普通 `go test ./...` 不要求安装 Node，也不要求预先存在前端构建产物。

## 阅读约定

- “根包”指模块根目录中的 `package agent`。
- “普通运行”指 `RunRequest.DurableRun == nil` 的内存内执行。
- “durable 运行”指设置 `DurableRunConfig`、通过 `CheckpointStore` 保存检查点的执行。
- `Observation` 是可能丢弃的进度信号；`RunResult` 和 durable store 才是终态依据。
- 仓库要求 Go `1.25.0`，以根目录 `go.mod` 为准。

继续阅读：[简介：定位、能力与边界](01-introduction.md)。
