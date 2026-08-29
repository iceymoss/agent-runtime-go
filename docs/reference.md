# 速查表

本文供处理 `RunResult`、分类错误和运行验证命令时快速查阅，以当前源码契约为准。项目仍处于 `v0.x`，生产使用应固定具体版本。概念介绍见 [concepts.md](concepts.md)，包选择见[文档首页](/)与 `packages/` 目录。

## 安装

```bash
go get github.com/iceymoss/agent-runtime-go@<version>
```

```go
import (
    agent "github.com/iceymoss/agent-runtime-go"          // 根包，通常起别名
    agentcontext "github.com/iceymoss/agent-runtime-go/context"
    "github.com/iceymoss/agent-runtime-go/permission"     // 子包同属一个 module
)
```

最小可运行示例见[《构建你的 Agent》第 1 章](guide/01-first-run.md)。iCoder demo 是独立 module，额外依赖 CGO SQLite driver（见 [icoder.md](icoder.md)）；runtime Core 本身不依赖 SQLite 或 CGO。

<a id="result-semantics"></a>
## Outcome、StopReason、FinishReason

三者位于不同层级：`FinishReason` 描述一次模型响应；`StopReason` 描述整个 Run 为什么结束；`Outcome` 描述 Run 生命周期结果。

| Outcome | 常量 | 含义 | 调用方动作 |
|---|---|---|---|
| `completed` | `OutcomeCompleted` | Run 已按 runtime 语义结束 | 使用结果；仍检查 `StopReason`，`tool_stop_turn` 也属于 completed |
| `suspended` | `OutcomeSuspended` | 有界中断，尚非完整自然结束 | 按 stop reason 决定扩大预算、续跑或请求用户操作 |
| `failed` | `OutcomeFailed` | Run 返回非 nil error | 以 error 为权威，用 `errors.Is/As` 分类；result 仅供诊断 |

| StopReason | 常量 | Outcome | 触发条件 |
|---|---|---|---|
| `complete` | `StopReasonComplete` | `completed` | 模型返回 `finish_reason=stop` |
| `tool_stop_turn` | `StopReasonToolStopTurn` | `completed` | 任一工具结果设置 `StopTurn=true` |
| `max_steps` | `StopReasonMaxSteps` | `suspended` | 工具循环达到 `Config.MaxSteps` |
| `stop_condition` | `StopReasonStopCondition` | `suspended` | 自定义 stop condition 命中 |
| `context_budget` | `StopReasonContextBudget` | `suspended` | 最新请求 input token 接近 context window；Core 不自动压缩 |
| `output_limit` | `StopReasonOutputLimit` | `suspended` | 模型返回 `finish_reason=length` |
| `loop_detected` | `StopReasonLoopDetected` | `failed` | 重复工具调用被检测；同时返回 `ErrLoopDetected` |

`tool_stop_turn` 只表示工具明确要求结束当前 turn，不代表业务目标已完成（例如 iCoder 的写权限 blocker 以 `completed/tool_stop_turn` 返回，但文件没有写入）。

| FinishReason | 常量 | runtime 行为 |
|---|---|---|
| `stop` | `FinishStop` | 接受最终文本，Run `completed/complete` |
| `tool_calls` | `FinishToolCalls` | 必须含至少一个 tool call；校验并执行后继续 model loop |
| `length` | `FinishLength` | 保存已有文本，Run `suspended/output_limit` |
| `error` | `FinishError` | `ValidateResponse` 不接受它作为 terminal response；adapter 应返回 error/`ChunkError` |

Provider terminal response 只接受 `stop`、`tool_calls`、`length`，且 finish reason、assistant message 与 tool calls 必须相互一致。

## 错误分类

| 情况 | 表达 | 终止 attempt | 处理 |
|---|---|---:|---|
| 工具不可用 | 模型可见 `ToolResult{IsError:true}` | 否 | 模型可改用其他工具 |
| 工具输入不符合 JSON Schema | 模型可见错误，消耗 repair budget | 超预算后是 | 默认允许 1 次 invalid call，之后返回 `ErrToolInputInvalid`；硬上限 2 |
| 工具业务失败且可修正 | 工具返回 `ToolResult{IsError:true}, nil` | 否 | 模型读取错误并修正 |
| 工具基础设施/未知失败 | `Tool.Execute` 返回非 nil Go error | 是 | runtime 包装上下文并保留 cause |
| 模型、协议、cancellation | 非 nil Go error | 是 | 用 `errors.Is/As` 分类 |
| 有界预算结束 | nil error + `OutcomeSuspended` | 否 | 按 `StopReason` 决定续跑策略 |

根包哨兵错误：

| 错误 | 含义 |
|---|---|
| `agent.ErrAgentConfigInvalid` | Agent、tool choice 或 durable 配置非法 |
| `agent.ErrToolNotAllowed` / `ErrToolNotFound` | 工具不在 `AllowedTools` / 未注册 |
| `agent.ErrLoopDetected` | 工具调用循环 |
| `agent.ErrToolInputInvalid` | schema repair budget 耗尽 |
| `agent.ErrToolExecutionUnknown` | durable 路径中非安全重放工具的结果未知 |

子包各自暴露哨兵错误（如 `permission.ErrApprovalRequired`、`tool.ErrExecutionUnknown`、`mcp.ErrCallUnknown`、`event.ErrSequenceConflict`、`skills.ErrGenerationUnavailable`，以及各包的 `ErrStaleFence`）。不要按错误字符串分支，用 `errors.Is`/`errors.As`。

`ModelError` 分类：

| Kind | 典型来源 | 通常可重试 |
|---|---|---:|
| `transport` | 网络、5xx、stream 无法打开 | adapter 决定；常为是 |
| `rate_limit` | 429 | 是 |
| `auth` | 401/403 | 否 |
| `rejected` | 其他上游拒绝 | 否 |
| `protocol` | 非法响应、chunk/terminal 不一致 | 否 |
| `unsupported` | 请求了 adapter 不支持的能力 | 否 |

```go
var modelErr *agent.ModelError
if errors.As(err, &modelErr) && modelErr.Retryable {
    // 结合 RetryAfter 和应用重试预算处理
}
```

`ModelError.Error()` 只包含 safe metadata，不拼接可能含 secret 的原始 `Cause`；`Unwrap` 保留 cause 供程序判断。`context.Canceled` 和 `context.DeadlineExceeded` 保持标准错误语义。

## 关键契约

| 契约 | 速记 |
|---|---|
| `AllowedTools == nil` | 使用 registry 的全部工具 |
| `AllowedTools` 空 slice | 明确禁用全部工具 |
| `RunResult.Messages` | 仅本轮新增 assistant/tool messages，不含传入 history/user message |
| `Agent` | 可复用且不拥有会话历史；应用负责 history |
| `ToolResult.IsError` | 模型可见错误，不等同于 Go error |
| `ToolResult.StopTurn` | 当前 turn 立即结束，结果是 completed/tool_stop_turn |
| `ReplayPolicyIdempotent` | runtime 可按 durable 协议安全重放的声明 |
| `ReplayPolicyNever` | 结果未知时不能盲目重放，需要 reconciliation |
| `ExecutionKey` | durable effect 的稳定去重键；下游仍必须实现幂等 |
| `Usage.InputTokens()` | 当前请求上下文占用，含 prompt/cache creation/cache read |
| 累计 `Usage` | 计费用量，不可代替最新请求的 context occupancy |
| Observation | 有界、非阻塞、best-effort、允许丢失，不是权威日志 |
| Event | 应用持久化/派发的权威 envelope，不能由 Observation 替代 |
| Prompt/Skills/model output | 都不是权限边界 |
| Durable | 提供 checkpoint/lease/fence 语义，不自动保证外部副作用 exactly-once |

## 验证命令

主 module，在仓库根运行：

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
```

运行输出：

```text
$ go test ./... -count=1
ok  	github.com/iceymoss/agent-runtime-go	1.171s
ok  	github.com/iceymoss/agent-runtime-go/app	0.102s
...（共 17 个包 ok；agenttest 与 3 个 examples 包无测试文件）
$ go test -race ./... -count=1
ok  	github.com/iceymoss/agent-runtime-go	2.243s
...（全部通过）
$ go vet ./...
$ go build ./...
（vet 与 build 无输出即通过）
```

iCoder 独立 module，在 `demo/icoder` 运行（`go test ./...` 不跨嵌套 module，仓库根测试不覆盖它）：

```bash
CGO_ENABLED=1 go test ./... -count=1
CGO_ENABLED=1 go test -race ./... -count=1
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go build ./cmd/icoder
```

运行输出：

```text
$ CGO_ENABLED=1 go test ./... -count=1
ok  	github.com/iceymoss/agent-runtime-go/demo/icoder/cmd/icoder	0.262s
ok  	github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder	0.686s
$ CGO_ENABLED=1 go test -race ./... -count=1
ok  	github.com/iceymoss/agent-runtime-go/demo/icoder/cmd/icoder	1.208s
ok  	github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder	1.647s
$ CGO_ENABLED=1 go vet ./...
$ CGO_ENABLED=1 go build ./cmd/icoder
（vet 与 build 无输出即通过）
```

窄范围排障：

```bash
go test ./... -run 'TestName' -count=1
go test -race ./... -run 'TestName' -count=1
```

运行循环、stream 协议和 durable 状态机的细节见 [internals.md](internals.md)。
