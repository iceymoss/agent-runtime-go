# Agent Runtime for Go 速查表

本文以当前源码契约为准，供安装、选包、处理结果和排障时快速查阅。项目仍处于 `v0.x`，生产使用应固定具体版本。

## 安装

安装根 module：

```bash
go get github.com/iceymoss/agent-runtime-go@<version>
```

导入根包时通常使用别名：

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

可选子包属于同一个 module，不需要分别发布版本：

```go
import (
    agent "github.com/iceymoss/agent-runtime-go"
    agentcontext "github.com/iceymoss/agent-runtime-go/context"
    "github.com/iceymoss/agent-runtime-go/permission"
)
```

仓库内运行 iCoder：

```bash
cd demo/icoder
CGO_ENABLED=1 go test ./... -count=1
go run ./cmd/icoder --workspace ../.. --db /tmp/icoder.db --task '概览这个仓库'
```

iCoder 是独立 module，额外依赖 CGO SQLite driver；runtime Core 本身不依赖 SQLite 或 CGO。

## 最小调用

```go
registry := agent.NewRegistry()
runner, err := agent.New(agent.Config{
    Key:          "example.agent",
    ModelName:    model.Name(),
    MaxSteps:     8,
    AllowedTools: []string{}, // 空 slice 明确禁用工具；nil 表示 registry 全部工具
}, model, registry)
if err != nil {
    return err
}

result, err := runner.Run(ctx, agent.RunRequest{Messages: []agent.Message{
    agent.NewSystemMessage("Answer clearly."),
    agent.NewUserMessage("Hello"),
}})
if err != nil {
    return err
}
```

`Agent` 自身不保存 session history。应用保存 user message 和 `RunResult.Messages`，下轮再传回完整 canonical history。

## 包选择矩阵

| 需求 | 包 | 何时不要引入 |
|---|---|---|
| 消息、Model/Tool 接口、单次 model/tool loop、普通/可持久 Run | 根包 `agent` | 无法省略；这是 Core |
| Provider/model catalog、descriptor、factory | `provider` | 只有一个已注入的 `agent.Model` 时 |
| 有版本的 Prompt 模板与 render | `prompt` | 静态 system message 已足够时 |
| 历史归一化、不可变 Context Plan、预算/压缩 | `context` | 简单短上下文直接拼 messages 时 |
| 消息聚合、revision CAS | `message` | 应用已有等价会话消息存储时 |
| Session branch、claim、resume、host | `session` | 单进程一次 Run 或简单同步 API 时 |
| Checkpoint、lease、fence、reconcile ledger | `durable` 与根包 `DurableRunConfig` | 不需要跨进程恢复时 |
| allow/deny/ask、approval、grant、revalidate | `permission` | 不能省略真实 effect 边界；纯只读且应用已有授权时可不用此实现 |
| 高级 registry/executor/interceptor、effect lifecycle | `tool` | 根包 `Registry` + `Tool` 足够时 |
| 权威 envelope、outbox/inbox、dispatch/replay | `event` | 只需 UI 进度 Observation 时 |
| 非可信 Skills catalog、generation、artifact read | `skills` | 没有动态 instruction capability 时 |
| MCP discovery、transport、generation、tool call | `mcp` | 固定原生工具更简单时 |
| 不可变 capability manifest/composition | `coordinator` | composition root 静态且简单时 |
| child run、预算预留、worker/reconcile/wake | `subagent` | 普通函数调用或同一 Agent tool 已足够时 |
| readiness、组件启动和有界 shutdown | `app` | 小型 CLI 可自行管理生命周期时 |
| Model/Tool 测试替身和 conformance helper | `agenttest` | 仅测试使用，不应作为生产 runtime 依赖 |

最小原则：先从根包开始，只在出现对应持久化、调度或 capability 需求后增加子包。

<a id="result-semantics"></a>
## Outcome、StopReason、FinishReason

三者位于不同层级：`FinishReason` 描述一次模型响应；`StopReason` 描述整个 Run 为什么结束；`Outcome` 描述 Run 生命周期结果。

### Outcome

| 值 | 常量 | 含义 | 调用方动作 |
|---|---|---|---|
| `completed` | `OutcomeCompleted` | Run 已按 runtime 语义结束 | 使用结果；仍检查 `StopReason`，因为 `tool_stop_turn` 也属于 completed |
| `suspended` | `OutcomeSuspended` | 有界中断，尚非完整自然结束 | 根据 stop reason 决定扩大预算、续跑或请求用户操作 |
| `failed` | `OutcomeFailed` | Run 返回非 nil error | 以 error 为权威，使用 `errors.Is/As` 分类；result 仅供诊断/持久化协议 |

普通 `Run` 中，Go error 会把 outcome 设为 `failed`。不要只看 `result.Outcome` 而忽略返回的 `error`。

### StopReason

| 值 | 常量 | Outcome | 触发条件 |
|---|---|---|---|
| `complete` | `StopReasonComplete` | `completed` | 模型返回 `finish_reason=stop` |
| `max_steps` | `StopReasonMaxSteps` | `suspended` | 工具循环达到 `Config.MaxSteps` |
| `stop_condition` | `StopReasonStopCondition` | `suspended` | 自定义 stop condition 命中 |
| `tool_stop_turn` | `StopReasonToolStopTurn` | `completed` | 任一工具结果设置 `StopTurn=true` |
| `loop_detected` | `StopReasonLoopDetected` | `failed` | 重复工具调用被检测；同时返回 `ErrLoopDetected` |
| `context_budget` | `StopReasonContextBudget` | `suspended` | 最新请求 input token 接近 context window；Core 不自动压缩 |
| `output_limit` | `StopReasonOutputLimit` | `suspended` | 模型返回 `finish_reason=length` |

`tool_stop_turn` 只表示工具明确要求结束当前 turn，不代表用户请求的业务目标已经完成。例如 iCoder 的写权限 blocker 就以 `completed/tool_stop_turn` 返回，但文件没有写入。

### FinishReason

| 值 | 常量 | runtime 行为 |
|---|---|---|
| `stop` | `FinishStop` | 接受最终文本，Run `completed/complete` |
| `tool_calls` | `FinishToolCalls` | 必须含至少一个 tool call；校验并执行后继续 model loop |
| `length` | `FinishLength` | 保存已有文本，Run `suspended/output_limit` |
| `error` | `FinishError` | 类型中存在，但 `ValidateResponse` 不接受它作为完整 terminal response；adapter 应返回 error/`ChunkError` |

Provider terminal response 只接受 `stop`、`tool_calls`、`length`。finish reason、assistant message 中的 finish reason、tool calls 必须相互一致。

## 错误分类

### 三类处理路径

| 情况 | 表达 | 是否终止 attempt | 处理 |
|---|---|---:|---|
| 工具不可用 | 模型可见 `ToolResult{IsError:true}` | 否 | 模型可改用其他工具 |
| 工具输入不符合 JSON Schema | 模型可见错误，消耗 repair budget | 超预算后是 | 默认允许 1 次 invalid call，之后返回 `ErrToolInputInvalid`；配置硬上限为 2 |
| 工具业务失败且可修正 | 工具返回 `ToolResult{IsError:true}, nil` | 否 | 模型读取错误并修正 |
| 工具执行基础设施/未知失败 | `Tool.Execute` 返回非 nil Go error | 是 | runtime 包装上下文并保留 cause |
| 模型、协议、context cancellation | 非 nil Go error | 是 | 使用 `errors.Is/As` 分类 |
| 有界预算结束 | nil error + `OutcomeSuspended` | 否 | 按 `StopReason` 决定续跑策略 |

### 根包哨兵错误

| 错误 | 含义 |
|---|---|
| `agent.ErrAgentConfigInvalid` | Agent、tool choice 或 durable 配置非法 |
| `agent.ErrToolNotAllowed` | 工具不在 `AllowedTools` |
| `agent.ErrToolNotFound` | 工具未注册 |
| `agent.ErrLoopDetected` | 工具调用循环 |
| `agent.ErrToolInputInvalid` | schema repair budget 耗尽 |
| `agent.ErrToolExecutionUnknown` | durable 路径中非安全重放工具的结果未知 |
| 各所有者包的 stale-fence/store errors | 例如 `message.ErrStaleFence`、`permission.ErrStaleFence`、`tool.ErrStaleFence`；用 `errors.Is` 判断 lease、fence、状态转换或幂等冲突 |

### ModelError

```go
var modelErr *agent.ModelError
if errors.As(err, &modelErr) {
    switch modelErr.Kind {
    case agent.ModelErrorKindTransport, agent.ModelErrorKindRateLimit:
        // 结合 Retryable、RetryAfter 和应用重试预算处理
    case agent.ModelErrorKindAuth, agent.ModelErrorKindRejected,
        agent.ModelErrorKindProtocol, agent.ModelErrorKindUnsupported:
        // 通常修配置、请求或 adapter，不盲目重试
    }
}
```

| Kind | 典型来源 | 通常可重试 |
|---|---|---:|
| `transport` | 网络、5xx、stream 无法打开 | adapter 决定；常为是 |
| `rate_limit` | 429 | 是 |
| `auth` | 401/403 | 否 |
| `rejected` | 其他上游拒绝 | 否 |
| `protocol` | 非法响应、chunk/terminal 不一致 | 否 |
| `unsupported` | 请求了 adapter 不支持的能力 | 否 |

`ModelError.Error()` 只包含 safe metadata，不拼接可能含 secret 的原始 `Cause`；`Unwrap` 仍保留 cause 供程序判断。`context.Canceled` 和 `context.DeadlineExceeded` 应保持标准错误语义。

### 子包错误

各子包暴露自己的哨兵错误，例如 `permission.ErrApprovalRequired`、`tool.ErrExecutionUnknown`、`mcp.ErrCallUnknown`、`event.ErrSequenceConflict`、`skills.ErrGenerationUnavailable`。不要按错误字符串分支；使用：

```go
if errors.Is(err, permission.ErrApprovalRequired) { /* suspend */ }

var modelErr *agent.ModelError
if errors.As(err, &modelErr) { /* inspect safe fields */ }
```

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
| Event | 应用持久化/派发的权威 envelope；不能由 Observation 替代 |
| Prompt/Skills/model output | 都不是权限边界 |
| Durable | 提供 checkpoint/lease/fence 语义，不自动保证外部副作用 exactly-once |

## 关键限制

- 根包只定义 provider-neutral `Model` 接口，不附带生产 OpenAI/Anthropic adapter；应用实现 adapter。
- Core 不读取环境变量、不选择 credential、不连接数据库、不隐式注册全局工具。
- Core 不自动压缩上下文；`context_budget` 只中断，压缩策略由应用或 `context` 组合提供。
- `Model.Stream` 是 canonical stream 契约；某个 adapter 是否真实增量 streaming 取决于实现。iCoder adapter 不是。
- JSON Schema 和工具白名单不是 authorization；effect 必须在工具执行边界做 permission check。
- Workspace 路径约束不是 sandbox；命令、网络和文件工具需要 OS/容器级隔离。
- MCP capability 不天然可信，也不天然经过 Permission；composition root 必须显式连接两者。
- Session 与 Durable 解决的层次不同：session 管理会话/claim/merge，durable 管理 attempt checkpoint/lease/fence；简单同步调用不必引入它们。
- 内存 store 只适合测试和单进程演示，不能声称支持进程重启恢复。
- `v0.x` API 可能变化，应固定版本并在升级时跑全量测试和外部包 API 测试。

## 验证命令

主 module，在仓库根运行：

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
```

iCoder 独立 module，在 `demo/icoder` 运行：

```bash
CGO_ENABLED=1 go test ./... -count=1
CGO_ENABLED=1 go test -race ./... -count=1
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go build ./cmd/icoder
```

窄范围排障：

```bash
go test ./... -run 'TestName' -count=1
go test -race ./... -run 'TestName' -count=1
go test ./demo/path  # 仅当该路径属于当前 module；iCoder 必须切到其 module
```

`go test ./...` 不会跨越嵌套 module，因此仓库根测试不会覆盖 `demo/icoder`。

## 术语

| 术语 | 定义 |
|---|---|
| canonical message/chunk | runtime 与 provider 无关的规范化消息/流片段 |
| model step | 一次 `GenerateRequest` 到一个合法 terminal `Response` |
| turn / Run | 用户调用 `Agent.Run` 的一次完整 model/tool loop |
| attempt | 对某个 run 的一次执行尝试；durable 模式受 lease/fence 保护 |
| ToolSet | registry 在 Agent 构造时按 allowlist 固化的工具快照 |
| tool generation | 工具定义集合的版本/代际，用于漂移检测 |
| capability generation | Skills/MCP 等发现结果的不可变 generation |
| Context Plan | 按 runtime artifacts、history 和预算准备的不可变模型输入 |
| revision CAS | 仅在已读 revision 仍匹配时发布新状态的乐观并发控制 |
| lease | worker 在有限时间内拥有 attempt 执行权的租约 |
| fence token | 单调身份标记，用于拒绝旧 worker/旧 attempt 的写入 |
| checkpoint | 可恢复执行的权威中间状态 |
| effect | 文件写入、命令、网络调用等可观察副作用 |
| idempotency key | 重试同一逻辑操作时保持稳定的去重标识 |
| blocker | 暂停执行并等待外部条件的安全引用；不是批准本身 |
| approval/grant | 对精确请求的审批记录/可消费授权 |
| Observation | 面向进度/UI 的 best-effort 进程内通知 |
| terminal event | 与业务终态原子发布、可 replay 的权威事件 |
| MCP | Model Context Protocol；动态发现并调用外部 capability |
| Sub-Agent | 受独立预算、状态和生命周期管理的 child run |
| synthetic | 为展示契约而伪造的确定性实现，不执行名称暗示的真实业务工作 |
| sandbox | OS 级隔离边界；路径清理和命令 allowlist 都不等于 sandbox |

iCoder 的完整运行路径见[《iCoder 端到端教程》](09-icoder-tutorial.md)。
