# context

The `context` package turns a message history into a "context plan" that is safe to hand to a model: normalize history, validate tool call/result pairing, estimate against a token budget, and when over limit compress via summarization into an immutable summary artifact. It owns no session or message storage.

> **Import alias**: this package shares a name with the standard library `context`. When both are needed in one file, the convention is to alias this package as `agentcontext` (as the repo's tests do) and keep the standard library under its usual name:
>
> ```go
> import (
>     "context"
>
>     agent "github.com/iceymoss/agent-runtime-go"
>     agentcontext "github.com/iceymoss/agent-runtime-go/context"
> )
> ```
>
> Inside the package sources the opposite convention is used: `stdcontext "context"` for the standard library. Either approach works; the point is that readers should not have to guess what `context.Context` refers to.

## What it is

The package has three capability groups, layered in order:

1. **History normalization**: `NormalizeHistory(NormalizeRequest) (NormalizeResult, error)` validates a `[]agent.Message`—known roles, self-consistent part structure, strict tool call/result pairing—and applies deterministic pruning: drop empty assistant messages, strip provider extension parts (such as raw reasoning), fill missing tool result names. Output includes `Digest` (canonical digest), `Exchanges` (intervals of complete tool exchanges), and `Diagnostics` (prune records).
2. **Plan building**: `Planner` assembles all inputs for one call (system messages, summary, protected facts, mainline/branch/invocation messages, token budget) into an immutable `Plan`. A `Plan` is identified by a content-addressed `PlanRef` (`PlanKey = "ctx_" + first 32 hex digits of the digest`); same input always yields the same plan.
3. **Compaction**: `Compactor.Compact` selects a safe prefix (never cutting system messages, the latest user intent, or incomplete tool exchanges), hands it to your `Summarizer` implementation, and produces an immutable `SummaryArtifact` plus a `PivotRef` pointing at it.

```go
type TokenCounter interface {
	ID() string
	CountTokens(stdcontext.Context, []agent.Message) (int, error)
}

type Planner interface {
	Prepare(stdcontext.Context, PrepareRequest) (Plan, error)
}
```

`NewPlanner(counter, store)` returns the default `DefaultPlanner`; `NewMemoryStore`/`NewMemoryArtifactStore` provide in-memory `PlanStore`/`ArtifactStore` for tests, both create-or-verify (same key + same content is idempotent; same key + different content returns `ErrArtifactConflict`).

## Why you need it

Without this package you must handle: dirty history (orphan tool results, duplicate call IDs, unfinished tool calls left by a crash) that provider APIs reject outright; token overruns only discovered by "send and see if you get a 400"; and summarization that accidentally drops system constraints or the user's latest intent. This package turns those into deterministic construction-time checks: bad history returns `ErrToolPairing`/`ErrHistoryInvalid`; over budget returns `ErrCompactionRequired` or `ErrContextTooLarge` (never silent truncation); compaction always preserves safe boundaries.

**When you do not need it**: one-shot scripts or short chats with no durable history can feed messages straight to the root-package runner; introduce this package when history is persisted across processes, needs exact revision binding, or grows long enough to require compaction.

## How to use it

Minimal example: implement a `TokenCounter`, build a `Plan`, and pass `plan.Messages()` to the model.

```go
package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
	agentcontext "github.com/iceymoss/agent-runtime-go/context"
)

type charCounter struct{}

func (charCounter) ID() string { return "chars/v1" }
func (charCounter) CountTokens(_ context.Context, messages []agent.Message) (int, error) {
	total := 0
	for _, message := range messages {
		total += len(message.Text())
	}
	return total, nil
}

func main() {
	planner, err := agentcontext.NewPlanner(charCounter{}, agentcontext.NewMemoryStore())
	if err != nil {
		panic(err)
	}
	plan, err := planner.Prepare(context.Background(), agentcontext.PrepareRequest{
		Source: agentcontext.SourceRef{TenantKey: "tenant-a", SessionKey: "session-1", SessionRevision: 7},
		Runtime: agentcontext.RuntimeArtifacts{
			DefinitionDigest:  "runtime-digest-1",
			ProjectionVersion: "messages/v1",
			TokenizerID:       "chars/v1", // must equal counter.ID()
			SystemMessages:    []agent.Message{agent.NewSystemMessage("你是订单助手。")},
		},
		InvocationMessages: []agent.Message{agent.NewUserMessage("查一下订单 42")},
		Budget: agentcontext.Budget{
			ContextTokens: 32000, ReservedOutputTokens: 2000, SafetyMarginTokens: 1000,
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(plan.Ref().PlanKey, plan.Estimate().TotalInputTokens)
	// plan.Messages() can be used directly as the model request message list
}
```

Output:

```text
ctx_sha256:d007bd48891e4438b9198a301 39
```

Key behavior:

- **Fixed assembly order**: `SystemMessages` → `CapabilityMessages` → `SummaryMessages` → protected-fact projection → `MainlineMessages` → `BranchMessages` → `InvocationMessages`, then the whole list runs through `NormalizeHistory` (`RepairReject` policy); any pairing problem fails `Prepare`.
- **Budget accounting**: `Budget.InputLimit() = ContextTokens - ReservedOutputTokens - SafetyMarginTokens`; final estimate `Estimate.TotalInputTokens = MessageTokens + ToolSchemaTokens + MediaTokens`. Over `InputLimit()`, if a compressible prefix exists return `ErrCompactionRequired`, otherwise `ErrContextTooLarge`.
- **Determinism**: repeating the same `PrepareRequest` yields the same `PlanRef`; `PlanStore.Get` matches on the full `ref`, and tampering with any field in the ref (even bumping `Budget.ContextTokens` by one) yields `ErrPlanDrift`.
- **Immutability**: accessors such as `plan.Messages()` return deep copies; mutating return values does not affect the stored plan. `MarshalWire`/`UnmarshalPlan` provide a self-checking persistence format; digest mismatch on deserialize also reports `ErrPlanDrift`.

For crash recovery use `RepairFromTerminal`: when history ends with unfinished tool calls, supply one `TerminalFact{CallID, Reason, SourceRevision}` per call (`EffectUnknown: true` means side-effect status is unknown); `NormalizeHistory` synthesizes a terminating tool result with `IsError: true, StopTurn: true` and records a `DiagnosticTerminalRepaired` diagnostic; missing any fact returns `ErrHistoryInterrupted`.

## FAQ

**Q: Are all `ProtectedFact` fields required?**
A: Yes. `Key`, `Kind`, `Value`, `SourceKey`, and `PolicyVersion` must be non-empty and `SourceRevision != 0`, or `NewFactSet` returns `ErrInvalidRequest`; the same `Key` twice returns `ErrProtectedFactConflict`. The fact set projects to one JSON system message counted against the budget; if facts alone plus tool schema/media already exceed `InputLimit()`, you get `ErrProtectedFactsTooLarge`—compaction cannot save them; shrink facts or raise the budget.

**Q: Why does `Prepare` report "runtime tokenizer and token counter identities differ"?**
A: `PrepareRequest.Runtime.TokenizerID` must exactly match the `ID()` of the counter passed to `NewPlanner`. That is intentional: token estimates are only valid for a specific tokenizer; changing models means changing both the counter and TokenizerID.

**Q: Can `SourceRef` set only one of `BranchKey` and `BranchVersion`?**
A: No—both or neither, otherwise `ErrInvalidRequest`. Also `TenantKey`, `SessionKey`, and `SessionRevision` are all required (revision starts at 1; 0 is treated as missing).

**Q: Why does `Compact` return `ErrCompactionNoProgress`?**
A: Three cases: empty safe prefix (for example history starts with system/user messages or a tool exchange, with no compressible pure-assistant prefix); after summarization `before - after < MinSavings`; after summarization total still exceeds `TargetTokens`. Also, messages returned by `Summarizer` must not contain tool content or images (`ErrHistoryInvalid`), and `Generation` must be non-empty.

**Q: Will compaction summarize away the user's latest message?**
A: No. `selectSafePrefix` only selects a contiguous assistant prefix before the last user message that contains neither system messages nor tool exchanges; the latest user intent always remains in `CompactResult.Kept`.

**Q: Who stores and advances `PivotRef`?**
A: The caller (session owner). This package only returns a revision-bound value in `CompactResult.Pivot` and does not track "current pivot". On the next `Prepare`, put it in `PrepareRequest.Pivot`; its `FactSetDigest` must match that call's fact-set digest or you get `ErrPivotInvalid`.

**Q: Does normalization keep images?**
A: Yes. Valid `PartImage` parts are kept in place with deep-copied bytes; unknown part types (provider extensions, raw reasoning) are stripped silently and do not enter provider-neutral canonical history.
