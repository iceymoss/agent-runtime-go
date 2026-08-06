# Cheat sheet

Quick lookup while handling `RunResult`, classifying errors, and running verification commands. Based on current source contracts. The project is still `v0.x`—pin a concrete version for production. Concepts: [concepts.md](concepts.md). Package selection: [docs home](/) and the `packages/` directory.

## Installation

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

Minimal runnable example: [quickstart.md](quickstart.md). The iCoder demo is a standalone module with an extra CGO SQLite driver dependency (see [icoder.md](icoder.md)); the runtime Core itself does not depend on SQLite or CGO.

<a id="result-semantics"></a>
## Outcome, StopReason, FinishReason

These sit at different layers: `FinishReason` describes one model response; `StopReason` describes why the whole Run ended; `Outcome` describes the Run lifecycle result.

| Outcome | Constant | Meaning | Caller action |
|---|---|---|---|
| `completed` | `OutcomeCompleted` | Run ended per runtime semantics | Use the result; still check `StopReason`—`tool_stop_turn` is also completed |
| `suspended` | `OutcomeSuspended` | Bounded interrupt, not a full natural end | By stop reason: raise budget, resume, or request user action |
| `failed` | `OutcomeFailed` | Run returned a non-nil error | Treat error as authoritative; classify with `errors.Is/As`; result is diagnostic only |

| StopReason | Constant | Outcome | Trigger |
|---|---|---|---|
| `complete` | `StopReasonComplete` | `completed` | Model returned `finish_reason=stop` |
| `tool_stop_turn` | `StopReasonToolStopTurn` | `completed` | Any tool result set `StopTurn=true` |
| `max_steps` | `StopReasonMaxSteps` | `suspended` | Tool loop hit `Config.MaxSteps` |
| `stop_condition` | `StopReasonStopCondition` | `suspended` | Custom stop condition hit |
| `context_budget` | `StopReasonContextBudget` | `suspended` | Latest request input tokens near context window; Core does not auto-compress |
| `output_limit` | `StopReasonOutputLimit` | `suspended` | Model returned `finish_reason=length` |
| `loop_detected` | `StopReasonLoopDetected` | `failed` | Repeated tool calls detected; also returns `ErrLoopDetected` |

`tool_stop_turn` only means a tool explicitly ended the current turn—not that the business goal completed (e.g. iCoder's write-permission blocker returns `completed/tool_stop_turn` without writing the file).

| FinishReason | Constant | Runtime behavior |
|---|---|---|
| `stop` | `FinishStop` | Accept final text; Run `completed/complete` |
| `tool_calls` | `FinishToolCalls` | Must include at least one tool call; validate and execute, then continue the model loop |
| `length` | `FinishLength` | Keep existing text; Run `suspended/output_limit` |
| `error` | `FinishError` | `ValidateResponse` does not accept it as a terminal response; adapters should return error/`ChunkError` |

Provider terminal responses accept only `stop`, `tool_calls`, `length`, and finish reason, assistant message, and tool calls must be mutually consistent.

## Error classification

| Case | Expression | Ends attempt | Handling |
|---|---|---:|---|
| Tool unavailable | Model-visible `ToolResult{IsError:true}` | No | Model may switch tools |
| Tool input fails JSON Schema | Model-visible error; consumes repair budget | After budget exhausted | Default allows 1 invalid call, then `ErrToolInputInvalid`; hard cap 2 |
| Tool business failure, correctable | Tool returns `ToolResult{IsError:true}, nil` | No | Model reads error and corrects |
| Tool infra / unknown failure | `Tool.Execute` returns non-nil Go error | Yes | Runtime wraps context and keeps cause |
| Model, protocol, cancellation | Non-nil Go error | Yes | Classify with `errors.Is/As` |
| Bounded budget end | nil error + `OutcomeSuspended` | No | Decide resume strategy by `StopReason` |

Root-package sentinel errors:

| Error | Meaning |
|---|---|
| `agent.ErrAgentConfigInvalid` | Illegal Agent, tool choice, or durable config |
| `agent.ErrToolNotAllowed` / `ErrToolNotFound` | Tool not in `AllowedTools` / not registered |
| `agent.ErrLoopDetected` | Tool-call loop |
| `agent.ErrToolInputInvalid` | Schema repair budget exhausted |
| `agent.ErrToolExecutionUnknown` | Result unknown for a non-safe-replay tool on the durable path |

Subpackages expose their own sentinels (e.g. `permission.ErrApprovalRequired`, `tool.ErrExecutionUnknown`, `mcp.ErrCallUnknown`, `event.ErrSequenceConflict`, `skills.ErrGenerationUnavailable`, and each package's `ErrStaleFence`). Do not branch on error strings—use `errors.Is`/`errors.As`.

`ModelError` kinds:

| Kind | Typical source | Usually retryable |
|---|---|---:|
| `transport` | Network, 5xx, stream cannot open | Adapter decides; often yes |
| `rate_limit` | 429 | Yes |
| `auth` | 401/403 | No |
| `rejected` | Other upstream rejection | No |
| `protocol` | Illegal response, chunk/terminal mismatch | No |
| `unsupported` | Requested capability the adapter does not support | No |

```go
var modelErr *agent.ModelError
if errors.As(err, &modelErr) && modelErr.Retryable {
    // 结合 RetryAfter 和应用重试预算处理
}
```

`ModelError.Error()` includes only safe metadata—it does not concatenate a raw `Cause` that may hold secrets; `Unwrap` keeps the cause for programmatic checks. `context.Canceled` and `context.DeadlineExceeded` keep standard error semantics.

## Key contracts

| Contract | Shorthand |
|---|---|
| `AllowedTools == nil` | Use all registry tools |
| `AllowedTools` empty slice | Explicitly disable all tools |
| `RunResult.Messages` | Only this turn's new assistant/tool messages—no inbound history/user message |
| `Agent` | Reusable and does not own session history; the application owns history |
| `ToolResult.IsError` | Model-visible error; not the same as a Go error |
| `ToolResult.StopTurn` | End the current turn immediately; result is completed/tool_stop_turn |
| `ReplayPolicyIdempotent` | Declaration that the runtime may safely replay per the durable protocol |
| `ReplayPolicyNever` | Do not blind-replay when result is unknown—needs reconciliation |
| `ExecutionKey` | Stable dedupe key for durable effects; downstream must still implement idempotency |
| `Usage.InputTokens()` | Current request context occupancy, including prompt/cache creation/cache read |
| Cumulative `Usage` | Billing usage—not a substitute for latest-request context occupancy |
| Observation | Bounded, non-blocking, best-effort, droppable—not an authoritative log |
| Event | Authoritative envelope persisted/dispatched by the application—not replaceable by Observation |
| Prompt/Skills/model output | None is a permission boundary |
| Durable | Provides checkpoint/lease/fence semantics—does not auto-guarantee exactly-once for external side effects |

## Verification commands

Main module, from the repo root:

```bash
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./...
```

Output:

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

iCoder standalone module, from `demo/icoder` (`go test ./...` does not cross nested modules; repo-root tests do not cover it):

```bash
CGO_ENABLED=1 go test ./... -count=1
CGO_ENABLED=1 go test -race ./... -count=1
CGO_ENABLED=1 go vet ./...
CGO_ENABLED=1 go build ./cmd/icoder
```

Output:

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

Narrow troubleshooting:

```bash
go test ./... -run 'TestName' -count=1
go test -race ./... -run 'TestName' -count=1
```

Details on the run loop, stream protocol, and durable state machine: [internals.md](internals.md).
