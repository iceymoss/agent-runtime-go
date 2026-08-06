# session

`session` 包拥有可移植的会话（Session）与分支（Branch）聚合契约：它把"一个多轮对话的权威状态"建模为带版本号的快照，并用分支 + fast-forward 合并解决并发运行对同一会话的写入竞争。

## 是什么

这个包的核心抽象是 `session.Service` 接口：会话聚合的完整读写边界。会话本体是 `Snapshot`——某个 revision 上的完整权威值，包含状态机（`StatusActive`/`StatusSuspended`/`StatusCompleted`/`StatusAbandoned`）、累计用量（PromptTokens/CompletionTokens/CostMicros）、摘要指针和 context pivot。每次成功写入都会使 `Revision` 加一，历史 revision 可以通过 `GetRevision` 回查。

并发写入不直接落在会话上，而是走分支：每个运行（`RunKey`）通过 `CreateBranch` 在固定的 `BaseRevision` 上开一条 `Branch`，运行结束后 `MarkBranchReady`，再由 `CommitMerge` 以 fast-forward 方式合并回会话。分支自身用 `Version` 字段做 CAS（compare-and-swap），保证状态机转换不会被并发写破坏。

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

包内提供 `NewMemory()` 返回线程安全的内存参考实现 `*Memory`。此外包里还有一层运行宿主（`Host`、`Store`、`RunRequest` 等，见 `host_contracts.go`），负责队列、worker 租约和合并编排，属于生产宿主拼装范畴，本文聚焦聚合契约本身。

## 为什么需要它

没有这个包，你要自己解决三件事：一是**并发合并**——两个运行同时基于同一份历史产出结果时，谁的写入生效、另一个如何检测到冲突并重试；二是**幂等重放**——网络重试或崩溃恢复导致同一 `Create`/`AddUsage` 请求到达两次时，如何不重复计费、不重复建会话；三是**乐观并发控制**——所有写命令都带 `ExpectedRevision` 或 `ExpectedVersion`，存储层原子比较后才提交，你不必依赖分布式锁。

什么时候不需要它：如果你的应用是单进程、单会话串行执行，且不需要回查历史 revision，直接用根包的 `agent.Agent` 加自己的一张会话表就够了，不必引入分支/合并模型。

## 怎么用

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

运行输出：

```text
revision: 0 -> 1
```

关键行为：

- `Create` 要求 `UserKey`、`AgentKey`、`Identity` 非空，否则返回 `ErrInvalidCommand`。新会话从 `Revision == 0`、`StatusActive` 开始。
- `CreateBranch` 的 `BaseRevision` 必须等于会话当前 revision，否则返回 `ErrRevisionConflict`。`BranchKey` 由实现根据 tenant + `RunKey` 派生，调用方不能自己指定。
- `MarkBranchReady` 和 `CommitMerge` 都用 `ExpectedVersion` 做分支版本 CAS，每次成功转换分支 `Version` 加一，所以必须用上一步返回的 `Version` 继续操作。
- `CommitMerge` 只支持 fast-forward（`MergeKind` 留空会被默认为 `MergeKindFastForward`）：分支必须处于 `BranchStatusReadyToMerge`，且会话当前 revision 仍等于分支的 `BaseRevision`。
- `NewMemory` 只是参考实现，进程重启即丢数据；生产环境需要用数据库自己实现 `Service`（以及宿主层的 `Store`），保持相同的 CAS 与幂等语义。

## 常见问题

**Q: `Create` 用同一个 `SessionKey` 调两次会怎样？**
A: 如果两次的不可变输入完全一致（`MutationMeta` 不参与比较），第二次是幂等重放，返回当前快照且不报错；只要有任何字段不同，返回 `ErrIdempotencyConflict`。`CreateBranch`（按 `RunKey`）和 `AddUsage`（按 `UsageFactKey`）遵循同样的规则。

**Q: 收到 `ErrRevisionConflict` 该怎么处理？**
A: 这是乐观并发控制的正常信号：你提交时带的 `ExpectedRevision` 已过期。用 `Get` 重新读取当前快照，基于新 revision 重算再提交。不要盲目用旧命令重试。

**Q: 并发合并两个分支，为什么只有一个成功？**
A: fast-forward 的前置条件是"会话 revision 仍等于分支 base"。第一个 `CommitMerge` 成功后会话 revision 加一，第二个分支的前置条件立即失效，返回 `*MergeConflictError`（可用 `errors.Is(err, session.ErrMergeConflict)` 判断，错误里带 `BaseRevision`/`CurrentRevision`/`BranchVersion`）。失败方应根据业务决定 rebase 重跑还是 `AbandonBranch`。

**Q: 会话进入 `StatusCompleted` 后还能写吗？**
A: 不能。`StatusCompleted` 和 `StatusAbandoned` 是终态：`Transition` 回 active 返回 `ErrInvalidSessionTransition`，`AddUsage`/`SetSummary`/`CreateBranch` 也都会被拒绝。合法转换只有 active ↔ suspended，以及两者到 completed/abandoned。

**Q: `SetSummary` 报 `ErrSnapshotInvariant` 是为什么？**
A: 摘要字段必须成对且自洽：`SummaryMessageKey` 与 `SummaryAtRevision` 要么都设、要么都空；设了 key 时 `SummaryVisibleAtRevision` 必须非零、不大于 `SummaryAtRevision`，且 `SummaryAtRevision` 不能超过 `ExpectedRevision`。只传 key 不传 revision 是最常见的踩坑点。

**Q: 分支的 `MarkBranchConflict` 之后还能合并吗？**
A: 可以。分支状态机允许 `conflicted -> ready_to_merge`：解决冲突后用最新 `Version` 再调一次 `MarkBranchReady` 即可。但 `merged` 和 `abandoned` 是分支终态，再操作返回 `ErrBranchClosed`。
