---
name: gotest
description: 运行 Go 测试与完整 CI 门槛并分析结果。当用户说跑测试、运行测试、go test、验证、检查 CI 时触发。
---

运行测试并分析结果。本仓库有**两个 module**，CI 门槛缺一不可。

## 完整验证（等价于 CI）

```bash
test -z "$(gofmt -l .)" || gofmt -l .
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
cd demo/icoder && go vet ./... && go test ./... -count=1
```

`-count=1` 必须带，本仓库不接受缓存结果。`demo/icoder` 需要 `CGO_ENABLED=1`（SQLite 驱动）。

## 快速迭代

只跑相关范围，最后再跑一次完整门槛：

```bash
go test . -run TestRunReturnsTerminalStateForEveryOutcome -count=1
go test ./session/... -run TestSessionAgent -v -count=1
go test . -run 'TestRun.*' -race -count=1
```

## 步骤

1. 按改动范围先跑针对性测试，定位问题
2. 有失败：读失败输出定位根因并修复，不要靠改测试来"通过"
3. 修完跑完整门槛（含 `-race` 和 demo module）
4. 汇报：跑了哪些命令、结果如何、失败是否已修

## 注意

- `gofmt -l .` 输出非空就是 CI 红灯，先 `gofmt -w` 再继续
- 依赖相关改动会触发守卫测试 `TestRootAgentDoesNotImportChildPackages` / `TestAgentPackagesDoNotImportInternalPackages` / `TestProductionAgentPackagesDoNotImportAgenttest`，失败说明依赖方向错了，不要通过改测试绕过
- `-race` 下不稳定的用例是真问题（多为用 sleep 做同步），不要重跑掩盖
