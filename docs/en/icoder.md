# iCoder end-to-end tutorial

This document is for developers who want a complete runnable composition example: [`demo/icoder`](https://github.com/iceymoss/agent-runtime-go/tree/main/demo/icoder) is a Code Agent reference application that combines the root-package model/tool loop with Prompt, Context, Skills, Permission, MCP, Sub-Agent, and SQLite in a standalone Go module. It covers how to run it, how the code maps to subpackages, and the demo boundaries it deliberately keeps.

## Code layout and subpackage mapping

```text
demo/icoder/                       # standalone Go module (replace runtime => ../..)
├── cmd/icoder/main.go             # flags, single-task and REPL
├── internal/icoder/
│   ├── app.go                     # composition root: prompt render, context.Planner, skills load
│   ├── capabilities.go            # mcp bridge, delegate_review (subagent)
│   ├── config.go                  # env vars and defaults
│   ├── store.go                   # SQLite schema and turn transactions
│   ├── tools.go                   # local tools and permission bridge
│   ├── weather.go                 # Open-Meteo adapter
│   └── workspace.go               # path constraints, files, and commands
└── skills/                        # code-review and go-code-review SKILL.md files
```

Model access uses the official adapter `providers/openaicompat` described in [packages/openaicompat.md](packages/openaicompat.md), defaulting to SSE streaming; for providers with incomplete SSE support, switch to `openaicompat.WithoutStreaming()` in `app.go`.

## Prerequisites

- Go 1.25 or newer (both modules' `go.mod` declare `go 1.25.0`).
- A C compiler and CGO: the SQLite driver is `github.com/mattn/go-sqlite3`, requiring `CGO_ENABLED=1`.
- An OpenAI Chat Completions-compatible endpoint that supports function/tool calling, plus its API key, base URL, and model ID.
- Network egress when using `get_weather` or remote MCP.

```bash
go version
go env CGO_ENABLED CC   # SQLite-related builds unavailable when CGO_ENABLED=0
```

## Configuration

Three environment variables are required; flags can override base URL and model, but the CLI has no API key flag:

```bash
export ICODER_API_KEY='your-key'
export ICODER_BASE_URL='https://api.example.com/v1'   # requests go to <base-url>/chat/completions
export ICODER_MODEL='your-model'
```

| Setting | Source | Default or behavior |
|---|---|---|
| workspace | `--workspace` | `.`, converted to absolute path and root symlink resolved at startup |
| SQLite | `--db` | `<workspace>/.icoder.db`; use e.g. `--db /tmp/icoder.db` to avoid polluting the repo |
| Skills | `--skills` | empty—Skills not loaded |
| MCP | `--mcp-url` | empty—no MCP connection |
| session | `--session` | 16 lowercase hex digits from `crypto/rand` |
| write/command | `--allow-writes` | `false` |
| context window / max output | code config | 128000 / 4096 |

Commands below assume `cd demo/icoder` first. Relative-path base: `--workspace ../..` is the repo root; `--workspace ../../..` is the repo's parent.

## Build, test, and single-task runs

iCoder is a standalone module—`go test ./...` at the repo root does not cover it. Verify separately at the repo root and in `demo/icoder`:

```bash
go test ./... -count=1
go vet ./...
go build -o /tmp/icoder ./cmd/icoder
```

Single-task run auditing the current repo:

```bash
go run ./cmd/icoder \
  --workspace ../.. \
  --db /tmp/icoder.db \
  --skills ./skills \
  --session sdk-review \
  --task '阅读根包的 tool loop，说明模型错误和工具错误如何传播'
```

After streaming text, the CLI prints a terminal-status line:

```text
[completed: complete, steps=2, tokens=1234]
```

Exact meanings of `Outcome` and `StopReason` are in the [cheat sheet](reference.md#result-semantics).

## REPL

Omit `--task`, or use `-i` / `--interactive`, to enter the REPL:

| Command | Host-side behavior |
|---|---|
| `/help`, `/?` | Show help |
| `/pwd`, `/cd <path>` | Show/switch local tool cwd; `/cd /` returns to workspace root |
| `/session`, `/sessions`, `/use <id>`, `/new [id]` | View, list, switch, create SQLite session |
| `/history`, `/clear` | Show canonical message summary / delete all data for current session |
| `/events` | Replay terminal events for the current session |
| `/tools`, `/skills` | List model-visible tools / loaded skill capabilities |
| `/exit`, `/quit`, `/q` | Exit |

Slash commands are handled by the CLI host and are not sent to the model; natural-language "change directory" does not change tool cwd. cwd is in-process state and resets to workspace root on restart; session history restores from SQLite via the same `--session`. You can also pass `--events` to replay without entering the REPL; even for replay-only, `NewApp` still requires the three provider env vars and full initialization.

## Skills

`--skills ./skills` loads each valid `SKILL.md` under the directory via `skills.FilesystemSource` (the demo ships `code-review` and `go-code-review`). Load pins a snapshot generation; instructions are wrapped as `<untrusted-skill key="..." version="...">...</untrusted-skill>` and enter the Context Plan as capability system messages. They are untrusted instructions, not a permission boundary; frontmatter `tool_requirements` does not replace the tool allowlist or `permission.Service`. Resolved at startup—no hot reload during a run.

## Eight built-in tools

With no MCP configured, the model-visible tools are exactly:

| Tool | Input notes | Behavior and limits | ReplayPolicy |
|---|---|---|---|
| `get_working_directory` | `{}` | Returns tool cwd | `idempotent` |
| `list_files` | optional `limit`, 1..1000 | Recursive list, default 200, skips `.git`, `node_modules` | `idempotent` |
| `read_file` | `path` | Reads an existing file under workspace, max 64 KiB | `idempotent` |
| `search_code` | `pattern`, optional `limit` | Literal substring search, default 50, max 200; not regex | `idempotent` |
| `write_file` | `path`, `content`, optional `expected_digest` | Full replace or create one file as `0644`; digest is `sha256:<hex>` | `never` |
| `run_command` | `program`, `args`, optional `timeout_seconds` | Only allowlisted Go/Git subcommands; output max 64 KiB | `never` |
| `get_weather` | `location`, `units` | Open-Meteo current weather; `units` must be `celsius`/`fahrenheit`; no API key | `idempotent` |
| `delegate_review` | `task` | In-memory child workflow; returns a synthetic result ref | `idempotent` |

The first seven are wrapped by `authorizedTool`: read, search, and weather default to allow; write and command default to ask. `delegate_review` registers directly without `authorizedTool`; although the policy switch lists `subagent.spawn`, the current bridge does not call that permission action.

`run_command` does not take a shell string; allowed subcommands are only `go test|vet|build|fmt` and `git status|diff|log|show`; remaining args pass straight to the program. This is a demo allowlist, not a complete command-security policy.

## Write-permission blocker

By default, calling `write_file` or `run_command` makes the permission policy return `ask`, and the bridge converts it to a model-visible error result:

```text
approval required: request=<request-ref> token=<resume-token>
```

It also sets `ToolResult.IsError=true` and `StopTurn=true`, ending the turn as `completed / tool_stop_turn` with no side effect executed. This is a blocker demo, not a full approval flow: the permission store is in-memory, the CLI has no approve command, prepared effects are not persisted, and the token is not used to resume the original turn. To write, you must enable `--allow-writes` for the whole process—it allows both `workspace.write` and `workspace.command`, not one-shot or per-file approval.

## MCP

Only one Streamable HTTP endpoint is supported:

```bash
go run ./cmd/icoder --workspace ../.. --db /tmp/icoder-mcp.db \
  --mcp-url 'https://approved-mcp.example.com/mcp' \
  --task '列出可用工具，并使用 MCP 工具完成调查'
```

At startup the manager connects, takes a capability snapshot, and registers discovered tools into the same registry. HTTP policy puts the URL's exact `host[:port]` on the allowlist, caps responses and tool results at 64 KiB, and allows insecure loopback; it does not start local stdio commands from MCP config.

Important boundary: dynamic MCP tools' `Execute` calls `mcp.Manager.CallTool` directly and **does not** go through iCoder's `permission.Service`; `--allow-writes` has no effect on MCP. Connect only trusted endpoints. MCP generation is also not written to the demo SQLite.

## Sub-Agent

The model may call `delegate_review` (input `{"task":"..."}`). It does use `subagent.Service` `Spawn`, `RunNext`, and `Reconcile`, with depth, fanout, token, cost, tool-call, and runtime limits; store, runner, and parent waker are all demo in-memory implementations.

But `reviewRunner` does not call a model or read the repo: it only computes `review:<digest-fragment>` from task bytes, estimates input tokens, and always returns 32 output tokens. This is a **synthetic** implementation demonstrating child-run lifecycle and budget interfaces—not an independent reviewer agent. Child state is lost on process restart; the wake recorder does not resume the parent Agent.

## SQLite and CommitTurn transactions

The default SQLite DSN enables a 10-second busy timeout and foreign keys; the pool is limited to one connection. Four tables: `icoder_sessions` (revision + cumulative usage), `icoder_messages` (canonical messages appended by ordinal), `icoder_turns` (idempotent turn snapshots by `(session_id, request_id)`), `icoder_events` (sequence primary key + unique event_id terminal events).

`CommitTurn` completes in one database transaction:

1. Idempotency check on `(session_id, request_id)`; same ID with different input/result conflicts.
2. Compute the next message ordinal; append this turn's user message and `RunResult.Messages`.
3. Session revision CAS with `WHERE revision = snapshot.Revision`, and accumulate usage.
4. Write the turn result snapshot and one `agent.run.terminal` event (sequence equals the new revision).
5. Commit; any step failure rolls back.

Model requests, weather/MCP network calls, subagent execution, file writes, and command execution all happen outside the transaction: the transaction guarantees atomic publication of the demo's own session aggregate, not exactly-once for external side effects. If the Agent run returns a Go error, `App.Run` does not call `CommitTurn`—failed runs are not persisted. This is not a `session.SessionAgent` host and does not use root-package durable checkpoints; it is a single-process, optimistic revision-CAS demo-owned store.

## One request's flow

1. CLI receives `--task` or REPL input and calls `App.Run(instruction, observe)`.
2. App loads session snapshot (revision, usage) and history from SQLite, then `NormalizeHistory(RepairReject)`.
3. Render the versioned system prompt; together with untrusted skill messages, history, and the user message, hand them to `context.Planner` for an immutable Plan.
4. `agent.Agent.Run(plan.Messages, ObservationEmitter)` starts the model/tool loop (max 20 model steps); `openaicompat.Model` converts SSE into canonical chunks; the CLI prints `ObservationTextDelta` live.
5. After allowlist and JSON Schema validation: the seven built-in tools go through `permission.Service` (ask/deny triggers stop-turn blocker); `delegate_review` goes through in-memory subagent; MCP tools call `CallTool` directly.
6. After terminal return, `CommitTurn` commits messages, usage CAS, turn snapshot, and terminal event in one transaction; CLI prints outcome/stop reason/steps/tokens.

Observation is a capacity-64 best-effort non-blocking notify, not an authoritative event log; authoritative terminal events are the SQLite records written in the same transaction as the session update.

## Security and reliability boundaries

- Workspace confinement uses absolute paths, `EvalSymlinks`, and `filepath.Rel` checks—not an OS sandbox; TOCTOU is possible between path checks and actual I/O.
- Tools and commands run as the same OS user that started iCoder and inherit process environment variables; there is no container, seccomp, chroot, or resource isolation.
- `read_file` does not validate UTF-8; binary content returns as a Go string and may be truncated.
- The context-budget byte counter is a conservative ~4 bytes per token estimate, not a model tokenizer.
- Permission approval, subagent store, and Context plan store are all in-memory; MCP bypasses permission; MCP generation is not persisted.
- SQLite only stores successfully committed turns—not a full durable execution ledger.

## How to extend further

Replace boundaries one by one—do not push policy into the runtime Core:

1. Production `agent.Model` adapter: correctly aggregate tool-call arguments, normalize usage and `ModelError`, pass `agenttest` conformance.
2. OS-level isolation for workspace tools: containers or restricted workers, resource limits, low-privilege identity.
3. Writes as prepare/approve/resume: durable permission store, opaque blocker, and durable checkpoint; on resume re-validate input digest, policy generation, and fence.
4. Route all effect tools (including MCP and subagent spawn) through `tool.Executor`/permission interceptors.
5. Replace `reviewRunner` with a real child Agent facade, durable subagent store, real parent wake, and independent budgets.
6. Migrate demo SQLite to application-owned migrations; when worker recovery is needed, introduce `session`, `durable`, lease, fence, and effect ledger.
7. Persist chosen Skills/MCP generation and digests so restore can detect capability drift; add structured audit.

For production composition patterns see [production.md](production.md); for return semantics and verification commands see the [cheat sheet](reference.md).
