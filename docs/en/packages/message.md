# message

The `message` subpackage (`github.com/iceymoss/agent-runtime-go/message`, distinct from the root package's `message.go`) owns the persistent message aggregate contract: it models one conversation message as a cumulative snapshot protected by revision and fence, addressing consistency under streaming generation, crash retries, and content redaction.

## What it is

The core abstraction is the `message.Service` interface. A persistent message is a `Snapshot`: identified by `MessageKey` within a tenant scope, attached to a session/branch, ordered within the branch by `BranchOrdinal`, with content as root-package `[]agent.ContentPart`. Messages have their own lifecycle state machine (`State`): `StateBuilding` (streaming accumulation) -> `StateComplete`/`StateCanceled`/`StateFailed`; any state can be redacted to `StateTombstoned`, after which only `StateTerminal` is allowed.

Writes are protected by two layers: `Revision` CAS so only one concurrent writer wins; (`AttemptKey`, `FenceToken`) so a crash-recovery attempt with a higher fence can override an old attempt while late writes from the old attempt are rejected. `SaveSnapshot` is **full replacement**—each commit submits complete `Parts`, not incremental appends.

```go
type Service interface {
    Create(context.Context, CreateCommand) (Snapshot, error)
    SaveSnapshot(context.Context, SaveCommand) (Snapshot, error)
    Get(context.Context, GetQuery) (Snapshot, error)
    ListBranch(context.Context, ListBranchQuery) ([]Snapshot, error)
    ListVisible(context.Context, ListVisibleQuery) ([]Snapshot, error)
    Tombstone(context.Context, TombstoneCommand) (Snapshot, error)
}
```

This is a leaf package: `SessionKey`, `BranchKey`, and `RunKey` are intentionally raw `string`s and do not depend on upper packages like `session`. `NewMemory()` returns a thread-safe in-memory reference implementation.

## Why you need it

Without this package you have to handle three classes of problems: **streaming write consistency**—tokens arrive gradually and one message may be written dozens of times, needing revision CAS against out-of-order overwrites; **crash-retry correctness**—late writes from an old worker must be blocked by the fence, or the new attempt's content is rolled back; **content integrity checks**—tool call/result pairing, no duplicate call IDs within a branch, and results must appear after calls; these invariants are enforced by `Create`/`SaveSnapshot` at write time.

**When you do not need it**: if messages are written once as a whole after generation, with no streaming intermediate state and no concurrent retries, an ordinary message table is enough—no revision/fence mechanism required.

## How to use it

```go
package main

import (
	"context"
	"fmt"
	"log"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/message"
)

func main() {
	ctx := context.Background()
	svc := message.NewMemory()

	created, err := svc.Create(ctx, message.CreateCommand{
		TenantKey:  agent.TenantKey("tenant-a"),
		MessageKey: message.MessageKey("msg-001"),
		SessionKey: "chat-001",
		BranchKey:  "branch-main",
		Role:       agent.RoleAssistant,
		Parts:      []agent.ContentPart{{Type: agent.PartText, Text: "正在生成…"}},
		State:      message.StateBuilding,
		RunKey:     "run-001",
		AttemptKey: "attempt-1",
		FenceToken: 1,
	})
	if err != nil {
		log.Fatal(err)
	}

	completed, err := svc.SaveSnapshot(ctx, message.SaveCommand{
		TenantKey:        created.TenantKey,
		MessageKey:       created.MessageKey,
		ExpectedRevision: created.Revision,
		AttemptKey:       "attempt-1",
		FenceToken:       1,
		State:            message.StateComplete,
		FinishReason:     agent.FinishStop,
		Parts:            []agent.ContentPart{{Type: agent.PartText, Text: "最终回答。"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(completed.State, completed.Revision, completed.BranchOrdinal)
}
```

Output:

```text
complete 2 1
```

Key behavior:

- `Create` requires non-empty `TenantKey`, `MessageKey`, `SessionKey`, `BranchKey`, `RunKey`, and `AttemptKey`, plus `FenceToken != 0`; initial `State` must be `StateBuilding` (empty defaults to building). New messages start at `Revision == 1`.
- `BranchOrdinal` of 0 means the store atomically allocates the next ordinal within the branch (1 in the example above); explicitly specifying an already-taken ordinal returns `ErrOrdinalConflict`.
- `Create` is idempotent on (`TenantKey`, `MessageKey`): identical immutable inputs replay returns the existing snapshot; any differing field returns `ErrIdempotencyConflict`.
- Each successful `SaveSnapshot` increments `Revision`; in streaming scenarios use the previously returned `Revision` as the next `ExpectedRevision`. building -> building is the only allowed self-loop, for continued accumulation.
- All boundaries deep-copy: incoming `Parts`/`AdapterState` and returned snapshots share no memory; mutating slices afterward does not corrupt storage.
- `NewMemory` is for tests and examples only; production needs a database-backed `Service` with revision/fence compare and write in the same transaction.

## Writing your own storage adapter

`Memory` is a reference, not the only implementation. Moving to a database does not mean rewriting the state machine: the rules are exported as pure functions, so an adapter is just load, apply, and write back under compare-and-set.

```go
func (s *SQLStore) SaveSnapshot(ctx context.Context, command message.SaveCommand) (message.Snapshot, error) {
	// The whole cycle runs in one transaction, so the revision the state machine
	// compared against is the revision that is still stored when the write lands.
	current, err := s.load(ctx, tx, command.TenantKey, command.MessageKey)
	next, err := message.ApplySave(current, command, time.Now().UTC())   // every legality decision
	if err := message.ValidateBranchCorrelation(next, siblings); err != nil { ... }
	return next, s.write(ctx, tx, next)
}
```

Available entry points: `ApplyCreate` / `ApplySave` / `ApplyTombstone` (transitions), `SameCreate` (create idempotency), `ValidateBranchCorrelation` (tool call and result pairing across a branch), `ValidateCreate` / `ValidateSave` / `ValidateSnapshot` / `ValidTransition`, `ValidateGetQuery` / `ValidateListBranchQuery` / `ValidateListVisibleQuery`, `VisibleAt`, `SortBranch` / `SortVisible`, and `CloneSnapshot` / `CloneParts`.

The adapter owns only the three things storage genuinely owns: **allocating the branch position** (only storage can see the branch), **comparing revisions atomically**, and **reading rows back in the required order**.

Verify the result with `agenttest.TestMessageService` - the bundled `Memory` runs the same suite.

## FAQ

**Q: `Create` returns `ErrInvalidCommand` with "tenant, keys, attempt, and fence are required"?**
A: The most common omission is `FenceToken`—it must be non-zero (0 is treated as unset). `RunKey` and `AttemptKey` are also required; even without a durable runtime yet, provide stable placeholders.

**Q: When does `ErrStaleFence` appear?**
A: Two cases: the command's `FenceToken` is less than the snapshot's current value (late write from an old worker); or the fence matches but `AttemptKey` differs from the snapshot—switching attempts requires a higher fence; you cannot change attempt within the same fence. It means this writer has lost write authority; do not retry—let the new attempt continue.

**Q: Can a message still be changed after `StateComplete`?**
A: You cannot go back to building, and you cannot "complete to complete" to change content—`validTransition` only allows the building self-loop; the only outgoing edge from complete/canceled/failed is `StateTombstoned`. To fix content, redact and create a new message.

**Q: Does data remain after `Tombstone`?**
A: No. `Tombstone` clears `Parts`, `AdapterState`, and `FinishReason`, sets state to `StateTombstoned`, and is still protected by `ExpectedRevision` + fence. Note: if a tool call in this message still has a live tool result referencing it in the branch, redaction returns `ErrSnapshotInvariant` for breaking the pairing invariant—handle the result message first.

**Q: Why doesn't `ListVisible` show a message I just created?**
A: Visibility is controlled by `VisibleAtRevision`: only messages with `VisibleAtRevision > 0` and less than or equal to the query `Revision` are returned. Messages that leave this field unset (0) at `Create` never appear in `ListVisible`—by design; messages usually get a visible revision after the branch merges into the session. For the full branch list use `ListBranch` (ordered by `BranchOrdinal`).

**Q: Writing a tool result message returns `ErrSnapshotInvariant`, "has no matching call"?**
A: Branch-level validation requires: the result's `ToolCallID` must match a tool call in an earlier message in the same branch (smaller `BranchOrdinal`), and each call has at most one result. `FinishReason` has a similar constraint—only `agent.RoleAssistant` messages may set it.
