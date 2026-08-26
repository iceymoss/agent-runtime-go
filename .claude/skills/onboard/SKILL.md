---
name: onboard
description: 帮助新成员上手 agent-runtime-go。当用户说新人上手、了解项目、项目介绍时触发。
---

帮助新成员快速理解 agent-runtime-go 的架构与开发流程。

## 步骤

1. 读根 `CLAUDE.md` 和 `README.md`，提炼项目简介
2. 说明这是**库不是服务**：运行时只负责可靠地跑模型/工具循环；模型凭据、提示词、工具实现、权限、存储、业务 API 都属于调用方
3. 说明两个 module：根模块（库，Go 1.25，仅两个直接依赖）与 `demo/icoder`（独立 module，`replace => ../..`，需要 CGO+SQLite）
4. 说明分层与依赖铁律（`deps_test.go` 强制）：根包 `agent` 不 import 任何子包；子包依赖根包；具体实现留给应用
5. 说明运行循环骨架与两个正交的终态字段（`Outcome` / `StopReason`）
6. 说明子包是**可选、渐进采用**的，按需求挑（对照 README 的"选择子包"表）
7. 列出最常见任务及入口（见输出格式）
8. 指向最重要的规则文件（不复制内容）

## 输出格式

```
## 项目简介
（2-3 句：可组合、与厂商无关的 Go agent 运行时库，v0.x）

## 架构
（一段话或 ASCII 图：RunRequest → build GenerateRequest → Model.Stream → 聚合校验 → 工具 schema+allowlist 校验 → Tool.Execute → 回灌消息 → 循环；
 根包是可移植内核，session/durable/permission/event/skills/mcp/coordinator/subagent 等是可选子包）

## 上手验证（不需要 API key）
- go run ./examples/hello
- go run ./examples/tool-agent

## 开发环境
- Go 1.25+；demo/icoder 需要 CGO_ENABLED=1 + C 编译器
- 文档站需要 bun
- CI 门槛: gofmt -l . 为空 / go vet ./... / go test ./... -count=1 / go test -race ./... -count=1 / demo/icoder 单独 vet+test

## 必须知道的三条铁律
1. 根包 agent 不得 import 本模块任何子包（共享类型放根包，子包用 type alias）
2. 根包不读环境变量、不选凭据、不连数据库、不注册隐式全局工具
3. 公开 API 变更要同步：中英双语文档 + 两个 README + CHANGELOG 的 Unreleased

## 日常开发入口
- 新工具:   /new-tool
- 新适配器: /new-provider
- 新子包:   /new-subpackage
- 跑示例/demo/文档站: /run-demo
- 跑测试:   /gotest
- 审 diff:  /goreview
- 写 spec:  /spec
- TDD:      /tdd

## 必读规则
（每条一行 + 链接到 .claude/rules/<file>.md：runtime-core / layering / providers-and-tools / go-style / test-files / docs）
```

简洁输出，不展开细节，需要时让用户读对应规则文件或 `docs/`（中文）/ `docs/en/`（英文）。
