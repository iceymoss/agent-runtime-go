# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While the project is at `v0.x`, minor versions may contain breaking changes.

## [Unreleased]

### Added

- `examples/chat`: a multi-turn conversation with a tool and persisted history -
  the program most people write second, and the one the repository was missing
  between a 55-line hello and a 12,500-line reference application. The docs gain
  a matching "Persisting a conversation" section explaining what the application
  stores and why a turn that did not complete must not be committed.
- A test that fails when the library declares something no code in the
  repository ever mentions. Capability flags nothing read, a persistence port
  nothing called, a stream consumer nothing invoked, and a conformance suite
  nothing ran were not four incidents but one: nothing proved a declaration was
  reachable from a real path. The 54 cases that already existed are frozen in a
  baseline that may only shrink, and a baseline entry that gains a test - or
  stops existing - fails the test too, so the list cannot rot into a rubber
  stamp. Clearing the first slice gave every documented `providers/openaicompat`
  option its first test.
- `providers/retry`: wraps any `agent.Model` so transient upstream failures are
  retried. The obvious implementation - wrapping what `Stream()` returns -
  retries almost nothing, because an SSE request usually succeeds and then fails
  partway through, which reaches the caller as an `agent.ChunkError` inside the
  stream. This retries both, honors the provider's `Retry-After`, preserves the
  optional `agent.Generator` capability, and deliberately refuses to retry once a
  chunk has been delivered downstream, where a second attempt would duplicate
  content the runtime already accumulated. `demo/icoder` replaced its own
  wrapper, which had exactly the gap described above.
- Structured output. `agent.ResponseFormat` (`ResponseFormatJSON` /
  `ResponseFormatJSONSchema`) on `Config` and `RunRequest`, projected into
  `GenerateRequest.ResponseFormat` and validated against
  `Capabilities.StructuredOutput` at assembly. An agent whose answer is consumed
  by code rather than read by a person - extraction, classification, routing -
  could not previously say so: the capability flag existed but nothing could
  request it. `providers/openaicompat` projects it onto `response_format` and now
  declares the capability. The format is part of `ImmutableRunConfig`, so a
  durable run cannot resume under a different output shape.
- Reasoning. `agent.PartReasoning`, `agent.ChunkReasoning`,
  `(agent.Message).Reasoning`, and `agent.ObservationReasoningDelta`. A model's
  thinking previously had nowhere to go, so adapters had to drop it or smuggle
  it into the answer. It is kept out of `Message.Text()` and out of the request
  an adapter sends upstream - providers that emit reasoning reject it as
  assistant input - and a model that produces it must declare
  `Capabilities.Reasoning`, which was until now a flag nothing read.
  `providers/openaicompat` reads both `reasoning_content` and `reasoning`.
- `agent.StreamResponse`: turns one complete response into the chunk stream the
  runtime expects. The runtime requires an adapter's chunks to add up to exactly
  the terminal response, which the obvious minimal adapter violates by emitting
  only the terminal chunk - and the resulting protocol error did not say what to
  do about it. Adapters holding a whole response (non-streaming endpoints,
  cached replies, test fakes) now call this instead of assembling chunks by
  hand; `providers/openaicompat` was rewritten on top of it, and the protocol
  error now names it.
- `agent.ObservationOptions`, `agent.NewObservationEmitterWith`, and
  `(*agent.ObservationEmitter).Dropped`. The emitter's bounded, non-blocking
  delivery is correct for telemetry and wrong for text a person is reading: any
  consumer slower than the model - which is every SSE or WebSocket UI - lost
  deltas silently, so the reader saw a truncated answer while `RunResult.Text`
  stayed complete. `Dropped` makes that loss detectable, and
  `ObservationOptions.Lossless` makes enqueueing wait for room instead, bounded
  by the run's context so a cancelled run is never held up by a stalled
  consumer. `NewObservationEmitter` keeps its existing lossy behavior.

- `durable.CheckpointAdapter` (`durable.NewCheckpointAdapter`): the official
  bridge that exposes a `durable.Store` plus its `ExecutionLedger` through the
  root `agent.CheckpointStore` port, so `Agent.Run` can execute durably against
  any durable adapter without the application re-deriving the run phase state
  machine. Store errors are wrapped so both the `durable` sentinels and the root
  sentinels (`agent.ErrCheckpointConflict`, `agent.ErrToolExecutionUnknown`,
  `agent.ErrInvalidRunTransition`) remain testable with `errors.Is`.
- `durable.EffectReplayer`: optional `ExecutionLedger` extension that re-arms an
  effect a revoked lease left in `EffectUnknown`, used only for tools whose
  `ReplayPolicy` proves the retry is safe. `durable.MemoryStore` implements it.
- `agenttest` conformance suites for every persistence port, so an application
  can verify its own storage adapter against the same contract the library's
  reference implementations are held to: `TestCheckpointStore`,
  `TestDurableStore`, `TestEventStore`, `TestToolExecutionLedger`,
  `TestPermissionStore`, `TestMessageService`, `TestSubagentStore`,
  `TestSessionService`, `TestSessionRunStore`, and `TestManifestStore`. Each
  in-memory reference now runs its suite, so a suite and its reference cannot
  drift apart.
- `tool.MemoryLedger` (`tool.NewMemoryLedger`): the in-memory reference
  `ExecutionLedger` the package was missing, so an executor can be assembled and
  tested without a database and there is one authoritative statement of what the
  execution lifecycle transitions mean.
- General tool suspension. A tool can now pause for reasons other than an
  approval - a delegated child run, a webhook, a queued job - and be resumed with
  the exact handle it issued:
  - `agent.ToolSuspensionExternal` joins `agent.ToolSuspensionApproval`.
    `ToolSuspensionKind` is documented as application-extensible: the runtime
    persists the handle and hands it back without interpreting it.
  - `tool.Suspension`, `tool.SuspendExecution`, `tool.StatusSuspended`,
    `tool.ExecutionRecord.Suspension`, `tool.ExecuteResult.Suspension`,
    `tool.ResumeRequest`, `tool.ErrExecutionSuspended`, and
    `(*tool.Executor).Resume`, which dispatches on the suspension's kind:
    approvals are re-authorized against the permission record, every other kind
    is handed back to the tool.
  - `tool.NewAgentRegistry`'s bridge no longer rejects non-approval suspensions,
    and passes `ToolInvocation.Resume` through to the tool so it can claim the
    work it started instead of starting it again.
  - A resumed execution re-arms under its original fence, so its ledger writes
    are authorized the same way the first attempt's were.
- Exported state machines so a storage adapter is a small amount of SQL rather
  than a second copy of the rules:
  - `message.ApplyCreate` / `ApplySave` / `ApplyTombstone`, `SameCreate`,
    `ValidateBranchCorrelation`, the `Validate*` query helpers, `VisibleAt`,
    `SortBranch` / `SortVisible`, and `CloneSnapshot` / `CloneParts`.
  - `subagent.ValidateSpawn`, `SpecDigest`, `DeriveRunKey` /
    `DeriveRelationshipKey` / `DeriveSessionKey`, `NewChildRef`,
    `EffectiveLimits`, `ValidateDepth` / `ValidateFanout` / `ValidateNoCycle`
    (with `ParentLookup`), `ValidateUsage`, `ExceedsReservation`, and
    `BudgetSnapshot.Reserve` / `Settle` for tree-budget accounting.
  - `session.ValidStatusTransition`, `KnownStatus`, `ValidBranchTransition`,
    `ValidContextPivot`, `AddNonNegative`, `CloneSnapshot`, `CloneMessages`,
    plus `SameCreate` and `DeriveBranchKey` for the aggregate, and
    `SameAdmission`, `CancelSupersedes`, `KnownCancelMode`,
    `TerminalExecutionState`, and `ClaimBefore` for the run queue - the rules a
    `session.Store` adapter would otherwise have to restate from memory, where a
    missing case means a run executed twice or a cancellation walked backwards.
  The `message`, `subagent`, and `session` reference implementations were
  rewritten on top of these, so there is exactly one implementation of each rule.

### Changed

- **BREAKING** `tool.ExecutionLedger` gained `Suspend(context.Context,
  SuspendExecution) (ExecutionRecord, error)` and `Resume(context.Context,
  string, uint64) (ExecutionRecord, error)`. A parked execution is neither failed
  nor ambiguous, so it needs a state and a stored handle of its own.
  **Migration:** implementations must add both methods. `Suspend` requires a
  running execution at the caller's fence and a non-empty suspension kind;
  `Resume` requires a suspended execution at the same fence and returns it to
  running. `Complete` must clear the stored handle. `tool.MemoryLedger` and the
  `demo/icoder` SQLite adapter show the shape, and
  `agenttest.TestToolExecutionLedger` verifies it.

### Removed

- **BREAKING** `agent.Store`, `agent.SessionStore`, `agent.MessageStore`,
  `agent.MemoryStore`, `agent.Session`, `agent.SessionStatus` (and its
  constants), and `agent.ErrSessionNotFound`.
  Nothing in the library called any of them - not `Agent.Run`, not a subpackage,
  not an example, not the demo - so implementing the interfaces produced no
  behavior, and their `uint` identity contradicted the string keys used
  everywhere else. They were an earlier design the `message` and `session`
  subpackages superseded, left in place where a newcomer looking for
  "how do I persist a conversation" would find them and take a wrong turn.
  **Migration:** the runtime is stateless, so the application joins turns
  together itself - see the new `examples/chat` and the "Persisting a
  conversation" section of `docs/packages/agent.md`. For revision CAS and branch
  visibility use the `message` subpackage; for the session aggregate use
  `session`. Anyone using `agent.MemoryStore` as a standalone container can
  replace it with a slice, since the runtime never read from it.
- **BREAKING** `agent.Capabilities.Media`. Nothing in the library read it, and
  what it would have meant is already covered by `ImageInput`, which is checked.
  **Migration:** delete the field from adapter capability literals.
- `collectStream`, an unexported stream consumer that nothing called. The live
  consumer is `streamStep`; keeping a second near-identical copy meant a rule
  added to one could silently miss the other.

### Fixed

- `providers/openaicompat` now runs `agenttest.TestModel`, the suite the project
  tells third-party adapters to run. Writing the fixtures found three defects in
  the reference adapter, all of which every copy of it inherited:
  - `Retry-After` was never parsed, so `ModelError.RetryAfter` was always zero
    and a rate-limited provider could not tell any retry policy - including
    `providers/retry` - when to come back.
  - A failed request reported `"provider rejected request"` and nothing else. The
    provider's own message now reaches `SafeDetail`, so a 400 says why; the raw
    body stays in the cause, where it belongs.
  - The streaming path accepted usage the non-streaming path already rejected,
    so an incoherent provider total was visible only with streaming off.
  It also now refuses content that arrives after the model's finish reason
  (a usage-only trailer, which `stream_options.include_usage` produces, is still
  accepted), and declares `Capabilities.UsageDetails`, which it had always
  earned by normalizing cache and reasoning tokens.

- `durable.CheckpointAdapter.BeginTool` returned an error for an execution whose
  result was already committed. A crash between committing an effect and
  committing its checkpoint therefore left the run permanently unresumable
  instead of replaying the stored result. It now returns the completed execution.
- `event.MemoryStore` and `permission.MemoryStore` are unchanged; the new suites
  confirmed both already met their contracts.
- `durable`'s prepared-effect identity included the attempt key and the
  preparation timestamp, so `PrepareEffect` reported a conflict when a resumed
  attempt re-prepared an open tool batch - which every resume does. The effect
  digest now covers identity only; attempt key and prepared time are provenance.
  **Migration:** stored `EffectRecord.Digest` values change. The digest is only
  used for conflict detection inside a ledger, and the deduplication anchor
  (`ExecutionKey`) is unchanged, so existing records stay addressable; a store
  that persists the digest should recompute or accept a one-time mismatch.

## [0.1.1] - 2026-08-06

### Added

- Apache License 2.0 (`LICENSE`) and `CHANGELOG.md`.

## [0.1.0] - 2026-08-06

The first public release.

### Added

- Core runtime: multi-step model/tool loop with JSON Schema validation,
  tool allowlists, stop conditions, context budgets, and loop detection.
- `providers/openaicompat`: official adapter for any OpenAI-compatible
  endpoint (OpenAI, DeepSeek, Qwen, Kimi, vLLM, Ollama) with SSE streaming,
  tool-call fragment assembly, usage normalization, and classified errors.
- `agent.NewTool` / `agent.MustNewTool`: define tools from typed Go
  functions; the JSON Schema is generated from the input struct.
- Optional subpackages: `session`, `durable`, `permission`, `event`,
  `skills`, `mcp`, `coordinator`, `subagent`, `provider`, `prompt`,
  `context`, `message`, `tool`, `app`, and `agenttest`.
- Runnable examples: `examples/hello`, `examples/tool-agent`,
  `examples/openai-compat`, and the `demo/icoder` reference application.
- Bilingual README (English / Simplified Chinese) and GitHub Actions CI
  (gofmt, `go vet`, tests with race detector for both modules).

[Unreleased]: https://github.com/iceymoss/agent-runtime-go/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/iceymoss/agent-runtime-go/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/iceymoss/agent-runtime-go/releases/tag/v0.1.0
