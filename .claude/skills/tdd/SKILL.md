---
name: tdd
description: 严格测试驱动开发。当用户要求 TDD、先写测试、测试驱动、RED-GREEN-REFACTOR 时触发。
---

严格测试驱动开发。按 RED -> GREEN -> REFACTOR 循环，禁止先写实现再补测试。

## 流程

### 1. RED — 先写失败的测试
- 根据需求写一个测试，运行确认它**失败**
- 测试必须明确断言预期行为（不只是"不 panic"）
- 公开行为写外部包测试（`package agent_test` / `package <name>_test`）；只有需要触碰内部状态时才写白盒测试
- 如果测试直接通过了，说明测试写得不对，重写

### 2. GREEN — 写最少的代码让测试通过
- 只写刚好让测试通过的实现，不要多写
- 运行 `go test ./... -count=1` 确认通过
- 不要在这一步优化或重构

### 3. REFACTOR — 重构（测试必须保持通过）
- 消除重复，改善命名，简化逻辑
- 每次重构后立即跑测试；跑 `gofmt -w` 保持格式
- 测试不通过就回退

### 4. COMMIT — 提交这个循环
- 一个 RED-GREEN-REFACTOR 循环 = 一个 commit
- commit message 说明这个循环实现了什么行为（见 `/git-commit`）

## 规则

- 绝对不能先写实现再补测试
- 每个循环只关注一个行为
- 用表驱动 + `t.Run` 子测试组织多个用例，用例名描述"场景 + 预期行为"
- 测试不得依赖网络、真实模型或真实凭据；用确定性 fake model 和 `agenttest` 套件
- 临时目录用 `t.TempDir()`；并发相关的循环要在 `-race` 下验证，不要用 `time.Sleep` 做同步
- 涉及已审计的运行时不变量时，回归用例写进 `audit_regression_test.go`
- 遵守 `.claude/rules/` 的所有规则（依赖铁律、根包纯度、深拷贝、错误语义、双语文档）
- 循环结束、准备提交前跑一次完整 CI 门槛（见 `/gotest`）
