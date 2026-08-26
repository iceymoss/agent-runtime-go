---
name: goreview
description: 审查当前 git diff 中的 Go 代码。当用户说审查 Go 代码、review Go、检查代码时触发。
---

审查当前 git diff 中的 Go 代码。本项目是可组合、与厂商无关的 Go 库，审查重心是**边界与不变量**。

## 步骤

1. 运行 `git diff HEAD`（或 `git diff main...HEAD`）获取变更
2. 检查以下方面：
   - **依赖铁律**：根包 `agent` 是否 import 了子包；是否 import `internal/`；生产包是否 import `agenttest`（`deps_test.go` 会拦，但要在审查阶段就指出）
   - **根包纯度**：是否往内核里塞了环境变量、凭据、数据库、隐式全局工具
   - **运行时不变量**：新增停止路径是否同时设置 `StopReason` + `Outcome`；`ToolResult{IsError:true}` 与 Go error 是否用对；`AllowedTools` 的 nil 与空切片是否被混淆
   - **深拷贝**：跨 API 边界的 Message / ToolDefinition / Observation / Checkpoint 是否共享了底层切片或 map；新增字段是否同步了 `Clone()` 和 digest
   - **错误处理**：是否吞掉 error；包装是否用 `%w` 保留 cause；对外错误信息是否泄露 key、完整 URL、堆栈
   - **并发安全**：共享状态是否加锁；goroutine 是否尊重 `ctx.Done()`；资源是否 defer 关闭
   - **装配期校验**：配置类问题是否推迟到了 `Run` 才报错
   - **测试与文档配套**：公开行为是否有外部包测试；导出面变更是否同步了中英双语文档和 `CHANGELOG.md`
3. 只报告真正的问题，不报告纯风格建议

## 输出格式

按严重程度排序，每个问题一行：
```
SEVERITY | file:line | 描述
```
