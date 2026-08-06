# Core concepts

One page on the root package's core types and execution semantics. After this, the rest of the docs are reference material.

## Overview

```text
RunRequest.Messages (full input history)
        │
        ▼
Agent.Run ──build──▶ GenerateRequest ──▶ Model.Stream (your model adapter)
        ▲                                    │
        │                              StreamChunk stream
        │                                    ▼
        │                          aggregate & validate into one-step Response
        │                                    │
        │          ┌── final text ─────────▶ RunResult
        │          │
        │          └── tool calls
        │                 │ allowlist + JSON Schema validation
        │                 ▼
        └───────── Tool.Execute (your business function)
                   wrap result as tool message, append history, next step
```

Inside one `Run`, the loop runs "model → tool → model" until the model emits final text, a stop condition hits, or an error occurs. Each model call and any following tool execution is one **step**.

## Message: the shared dialogue representation

`Message` = `Role` + ordered `Parts` + optional `FinishReason`.

- Four roles: `system`, `user`, `assistant`, `tool`;
- Four part kinds: text, tool call (assistant only), tool result (tool only), image (user only);
- Build with helpers: `NewSystemMessage` / `NewUserMessage` / `NewAssistantMessage` / `NewToolMessage`;
- `ValidateMessage` enforces structure (tool calls need ID and Name, tool results must pair `ToolCallID`, etc.); invalid messages are rejected before the loop starts.

## Model: the only model port

```go
type Model interface {
	Name() string
	Capabilities() Capabilities
	Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error)
}
```

Four chunk kinds flow on the channel from `Stream`:

| Chunk | Meaning |
|---|---|
| `ChunkText` | Text delta |
| `ChunkToolCall` | One **fully assembled** tool call (no partial JSON) |
| `ChunkFinish` | Sole terminal state, carrying the full `Response`; all prior deltas must match it |
| `ChunkError` | Carries an error, then the channel closes |

The runtime checks protocol consistency: missing terminal state, chunks after terminal, or text that does not match the terminal message are protocol errors. `Capabilities` declares what the model supports (tools, tool-choice modes, images); assembly compares this to config and rejects mismatches—never silent downgrade.

Before writing your own adapter, check whether [providers/openaicompat](packages/openaicompat.md) already covers you; protocol details are in [Run-loop internals](internals.md).

## Tool: business functions the model can call

```go
type Tool interface {
	Definition() ToolDefinition   // name, description, JSON Schema
	ReplayPolicy() ReplayPolicy   // replay on resume: never / idempotent / resolve
	Execute(context.Context, ToolInvocation) (ToolResult, error)
}
```

Most cases need no hand-written Schema; `agent.NewTool` reflects it from the handler input struct (see [root package docs](packages/agent.md)).

**Two tool-error semantics** (the easiest design point to confuse):

| Return | Semantics | Loop behavior |
|---|---|---|
| `ToolResult{IsError: true}` | Model-visible, correctable (e.g. "order not found") | Continue; model sees the error and adjusts |
| Non-nil Go error | Infrastructure failure (e.g. database down) | Abort this run immediately |

`Registry` is mutable; `Agent.New` takes an immutable `ToolSet` snapshot, so later Registry changes do not affect an assembled Agent. Three values for `Config.AllowedTools`: `nil` = all registered tools; `[]string{}` = disable tools; a name list = only those.

## Stop semantics: Outcome and StopReason

After `Run` ends, interpret these three together:

| `err` | `Outcome` | Meaning |
|---|---|---|
| nil | `OutcomeCompleted` | Normal completion (model final text, or tool-initiated `StopTurn`) |
| nil | `OutcomeSuspended` | Interrupted but resumable; `StopReason` says why (`max_steps` / `output_limit` / `context_budget` / custom) |
| non-nil | `OutcomeFailed` | Failure: model error, tool infrastructure error, loop detection (`ErrLoopDetected`), etc. |

`OutcomeSuspended` is an intentional intermediate state: the upper layer uses messages already produced to decide "run another turn" or "stop here"; the runtime does not decide for you.

## Error classification

- **Model side:** adapters wrap as `ModelError` with `Kind` (auth / rate_limit / transport / rejected / protocol) and a `Retryable` flag;
- **Invalid tool args:** fed back for the model to repair, up to `ToolRepairLimit` times (default 1);
- **Config errors:** `agent.New` returns `ErrAgentConfigInvalid` at assembly time—never at runtime;
- Sentinel errors (`ErrLoopDetected`, `ErrToolNotAllowed`, etc.) are checkable with `errors.Is`.

## Two run modes

- **Ordinary run** (default): entirely in memory; if the process dies, the turn is gone. Fits request-scoped calls.
- **Durable run:** with `RunRequest.DurableRun` set, each step is written to `CheckpointStore`; after a crash another worker can take the lease and resume. When you need that, read [durable](packages/durable.md) and [Run-loop internals](internals.md).

## Observation ≠ events

`ObservationEmitter` is live progress for the UI (text deltas, tool start/end)—**bounded, non-blocking, and droppable**. It deliberately has no "run completed" event—authoritative terminal state comes only from `RunResult` and the durable store. For reliable delivery (billing, audit, downstream notify) use the outbox pattern in the [event subpackage](packages/event.md).

## How subpackages relate

The root package imports no subpackages (enforced by tests); subpackages depend on root types and ports; your app wires them at the composition layer. So: **start from the root package; introduce a subpackage when it hurts.** One doc per subpackage; see the subpackage index on the [docs home](/).
