# session

The `session` package owns a portable Session and Branch aggregate contract: it models "the authoritative state of a multi-turn conversation" as a versioned snapshot, and uses branches plus fast-forward merge to resolve write contention when concurrent runs update the same session.

## What it is

The core abstraction is the `session.Service` interface: the full read/write boundary of the session aggregate. The session itself is a `Snapshot`—the complete authoritative value at a revision—including the state machine (`StatusActive`/`StatusSuspended`/`StatusCompleted`/`StatusAbandoned`), cumulative usage (PromptTokens/CompletionTokens/CostMicros), summary pointers, and context pivot. Every successful write increments `Revision`; historical revisions can be fetched via `GetRevision`.

Concurrent writes do not land on the session directly; they go through branches: each run (`RunKey`) opens a `Branch` at a fixed `BaseRevision` via `CreateBranch`, calls `MarkBranchReady` when finished, then `CommitMerge` fast-forwards it back into the session. The branch itself uses a `Version` field for CAS (compare-and-swap) so state-machine transitions are not corrupted by concurrent writers.

```go
type Service interface {
    Create(context.Context, CreateCommand) (Snapshot, error)
    Get(context.Context, GetQuery) (Snapshot, error)
    GetRevision(context.Context, RevisionQuery) (Snapshot, error)
    CreateBranch(context.Context, CreateBranchCommand) (Branch, error)
    GetBranch(context.Context, GetBranchQuery) (Branch, error)
    MarkBranchReady(context.Context, BranchCommand) (Branch, error)
    MarkBranchConflict(context.Context, ConflictCommand) (Branch, error)
    AbandonBranch(context.Context, BranchCommand) (Branch, error)
    CommitMerge(context.Context, MergeCommit) (MergeResult, error)
    AddUsage(context.Context, UsageCommand) (Snapshot, error)
    SetSummary(context.Context, SummaryCommand) (Snapshot, error)
    Transition(context.Context, TransitionCommand) (Snapshot, error)
}
```

`NewMemory()` returns a thread-safe in-memory reference implementation `*Memory`. The package also includes a run-host layer (`Host`, `Store`, `RunRequest`, and so on; see `host_contracts.go`) for queues, worker leases, and merge orchestration—that belongs to production host assembly; this document focuses on the aggregate contract itself.

## Why you need it

Without this package you have to solve three problems yourself: **concurrent merge**—when two runs produce results from the same history, whose write wins and how the other detects conflict and retries; **idempotent replay**—when network retries or crash recovery deliver the same `Create`/`AddUsage` twice, how to avoid double billing and duplicate sessions; **optimistic concurrency**—every write carries `ExpectedRevision` or `ExpectedVersion`, and the store commits only after an atomic compare, so you do not need distributed locks.

**When you do not need it**: if your app is single-process, serial per session, and you do not need historical revisions, the root package `agent.Agent` plus your own session table is enough—no branch/merge model required.

## How to use it

```go
package main

import (
	"context"
	"fmt"
	"log"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/session"
)

func main() {
	ctx := context.Background()
	svc := session.NewMemory()

	snap, err := svc.Create(ctx, session.CreateCommand{
		TenantKey:  agent.TenantKey("tenant-a"),
		SessionKey: session.SessionKey("chat-001"),
		UserKey:    "user-1",
		AgentKey:   "assistant",
		Identity:   "user-1@tenant-a",
	})
	if err != nil {
		log.Fatal(err)
	}

	branch, err := svc.CreateBranch(ctx, session.CreateBranchCommand{
		TenantKey:    snap.TenantKey,
		SessionKey:   snap.SessionKey,
		RunKey:       session.RunKey("run-001"),
		BaseRevision: snap.Revision,
	})
	if err != nil {
		log.Fatal(err)
	}

	ready, err := svc.MarkBranchReady(ctx, session.BranchCommand{
		TenantKey:       branch.TenantKey,
		SessionKey:      branch.SessionKey,
		BranchKey:       branch.BranchKey,
		ExpectedVersion: branch.Version,
	})
	if err != nil {
		log.Fatal(err)
	}

	merged, err := svc.CommitMerge(ctx, session.MergeCommit{
		TenantKey:       ready.TenantKey,
		SessionKey:      ready.SessionKey,
		BranchKey:       ready.BranchKey,
		ExpectedVersion: ready.Version,
		MergeKind:       session.MergeKindFastForward,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("revision: %d -> %d\n", merged.PreviousRevision, merged.SessionRevision)
}
```

Output:

```text
revision: 0 -> 1
```

Key behavior:

- `Create` requires non-empty `UserKey`, `AgentKey`, and `Identity`, otherwise it returns `ErrInvalidCommand`. New sessions start at `Revision == 0` and `StatusActive`.
- `CreateBranch` requires `BaseRevision` equal to the session's current revision, otherwise it returns `ErrRevisionConflict`. `BranchKey` is derived by the implementation from tenant + `RunKey`; callers cannot set it.
- `MarkBranchReady` and `CommitMerge` both CAS on branch `ExpectedVersion`; each successful transition increments branch `Version`, so you must continue with the `Version` returned by the previous step.
- `CommitMerge` only supports fast-forward (an empty `MergeKind` defaults to `MergeKindFastForward`): the branch must be `BranchStatusReadyToMerge`, and the session revision must still equal the branch `BaseRevision`.
- `NewMemory` is a reference implementation only; data is lost on process restart. Production needs a database-backed `Service` (and host-layer `Store`) with the same CAS and idempotency semantics.

## FAQ

**Q: What happens if `Create` is called twice with the same `SessionKey`?**
A: If the immutable inputs are identical (`MutationMeta` is not compared), the second call is an idempotent replay: it returns the current snapshot with no error. Any differing field returns `ErrIdempotencyConflict`. `CreateBranch` (by `RunKey`) and `AddUsage` (by `UsageFactKey`) follow the same rule.

**Q: How should I handle `ErrRevisionConflict`?**
A: It is the normal optimistic-concurrency signal: the `ExpectedRevision` you submitted is stale. `Get` the current snapshot, recompute against the new revision, and submit again. Do not blindly retry the old command.

**Q: Why does only one of two concurrent branch merges succeed?**
A: Fast-forward requires "session revision still equals branch base". After the first `CommitMerge` succeeds, session revision increments and the second branch's precondition fails, returning `*MergeConflictError` (check with `errors.Is(err, session.ErrMergeConflict)`; the error carries `BaseRevision`/`CurrentRevision`/`BranchVersion`). The loser should rebase and re-run or `AbandonBranch`, depending on business rules.

**Q: Can I still write after the session enters `StatusCompleted`?**
A: No. `StatusCompleted` and `StatusAbandoned` are terminal: `Transition` back to active returns `ErrInvalidSessionTransition`, and `AddUsage`/`SetSummary`/`CreateBranch` are also rejected. Legal transitions are only active ↔ suspended, and either to completed/abandoned.

**Q: Why does `SetSummary` return `ErrSnapshotInvariant`?**
A: Summary fields must be paired and consistent: `SummaryMessageKey` and `SummaryAtRevision` are both set or both empty; when the key is set, `SummaryVisibleAtRevision` must be non-zero, not greater than `SummaryAtRevision`, and `SummaryAtRevision` must not exceed `ExpectedRevision`. Passing only the key without a revision is the most common mistake.

**Q: Can a branch still merge after `MarkBranchConflict`?**
A: Yes. The branch state machine allows `conflicted -> ready_to_merge`: after resolving the conflict, call `MarkBranchReady` again with the latest `Version`. But `merged` and `abandoned` are terminal; further operations return `ErrBranchClosed`.
