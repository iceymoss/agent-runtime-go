# message

`message` 子包（`github.com/iceymoss/agent-runtime-go/message`，注意区别于根包的 `message.go`）拥有持久化消息聚合的契约：把一条对话消息建模为带 revision 与 fence 保护的累积快照，解决流式生成、崩溃重试和内容脱敏下的一致性问题。

## 是什么

核心抽象是 `message.Service` 接口。一条持久化消息是一个 `Snapshot`：tenant 作用域内由 `MessageKey` 标识，挂在某个 session/branch 下，按 `BranchOrdinal` 在分支内排序，内容是根包的 `[]agent.ContentPart`。消息有自己的生命周期状态机（`State`）：`StateBuilding`（流式累积中）-> `StateComplete`/`StateCanceled`/`StateFailed`，任何状态都可被脱敏为 `StateTombstoned`，之后只能进入 `StateTerminal`。

写入受两层保护：`Revision` CAS 保证并发写只有一个赢家；(`AttemptKey`, `FenceToken`) 保证崩溃后接管的新尝试（更高 fence）能覆盖旧尝试，而旧尝试的迟到写入被拒绝。`SaveSnapshot` 是**全量替换**语义——每次提交完整的 `Parts`，不是追加增量。

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

这是一个叶子包：`SessionKey`、`BranchKey`、`RunKey` 故意保留为原始 `string`，不依赖 `session` 等上层包。`NewMemory()` 返回线程安全的内存参考实现。

## 为什么需要它

没有这个包，你要自己处理三类问题：**流式写入的一致性**——模型 token 逐步到达，一条消息会被写几十次，需要 revision CAS 防止乱序覆盖；**崩溃重试的正确性**——旧 worker 的迟到写入必须被 fence 挡住，否则新尝试的内容会被回滚；**内容完整性校验**——tool call 与 tool result 的配对、分支内不能有重复 call ID、result 必须出现在 call 之后，这些不变量由 `Create`/`SaveSnapshot` 在写入时强制。

什么时候不需要它：如果消息只是一次性生成后整体落库、没有流式中间态、没有并发重试，一张普通的消息表就够了，不需要 revision/fence 机制。

## 怎么用

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

运行输出：

```text
complete 2 1
```

关键行为：

- `Create` 要求 `TenantKey`、`MessageKey`、`SessionKey`、`BranchKey`、`RunKey`、`AttemptKey` 全部非空且 `FenceToken != 0`，初始 `State` 必须是 `StateBuilding`（留空会被默认填为 building）。新消息 `Revision == 1`。
- `BranchOrdinal` 传 0 表示由存储在分支内原子分配下一个序号（上例分配到 1）；显式指定已被占用的序号返回 `ErrOrdinalConflict`。
- `Create` 按 (`TenantKey`, `MessageKey`) 幂等：完全相同的不可变输入重放直接返回现有快照；任何字段不同返回 `ErrIdempotencyConflict`。
- `SaveSnapshot` 每次成功 `Revision` 加一，流式场景下要用上一次返回的 `Revision` 作为下一次的 `ExpectedRevision`。building -> building 是唯一允许的自环，用于持续累积。
- 所有边界都做深拷贝：传入的 `Parts`/`AdapterState` 和返回的快照互不共享内存，事后修改切片不会污染存储。
- `NewMemory` 仅供测试与示例；生产环境需要基于数据库实现 `Service`，并保证 revision/fence 比较与写入在同一事务内。

## 自己写持久化适配器

`Memory` 只是参考实现。要换成数据库，不需要把状态机再写一遍——规则以纯函数形式导出，适配器只做「加载 → Apply → 带 CAS 写回」：

```go
func (s *SQLStore) SaveSnapshot(ctx context.Context, command message.SaveCommand) (message.Snapshot, error) {
	// 整个循环放在一个事务里，保证状态机比对的 revision 就是落盘时的 revision
	current, err := s.load(ctx, tx, command.TenantKey, command.MessageKey)
	next, err := message.ApplySave(current, command, time.Now().UTC())   // 全部合法性判定在这里
	if err := message.ValidateBranchCorrelation(next, siblings); err != nil { ... }
	return next, s.write(ctx, tx, next)
}
```

可用的入口：`ApplyCreate` / `ApplySave` / `ApplyTombstone`（状态转移）、`SameCreate`（Create 幂等比对）、`ValidateBranchCorrelation`（跨分支的 tool call/result 配对）、`ValidateCreate` / `ValidateSave` / `ValidateSnapshot` / `ValidTransition`、`ValidateGetQuery` / `ValidateListBranchQuery` / `ValidateListVisibleQuery`、`VisibleAt`、`SortBranch` / `SortVisible`、`CloneSnapshot` / `CloneParts`。

适配器只负责存储真正拥有的三件事：**分配分支序号**（只有存储能看见整条分支）、**原子比对 revision**、**按要求的顺序读回**。

写完用 `agenttest.TestMessageService` 验收——库自带的 `Memory` 跑的是同一份套件。

## 常见问题

**Q: `Create` 报 `ErrInvalidCommand`，提示 "tenant, keys, attempt, and fence are required"？**
A: 最常漏掉的是 `FenceToken`——它必须非零（0 被视为未设置）。`RunKey` 和 `AttemptKey` 也是必填，即使你暂时没有 durable 运行时，也要提供占位的稳定值。

**Q: `ErrStaleFence` 在什么情况下出现？**
A: 两种：一是命令的 `FenceToken` 小于快照当前值（旧 worker 的迟到写入）；二是 fence 相同但 `AttemptKey` 与快照不一致——换尝试必须携带更高的 fence，同一 fence 内不允许换 attempt。遇到它说明本次写入方已丧失写权，不要重试，让新尝试继续。

**Q: 消息 `StateComplete` 之后还能改吗？**
A: 不能改回 building，也不能"complete 到 complete"再改内容——`validTransition` 只允许 building 自环，complete/canceled/failed 唯一的出边是 `StateTombstoned`。想修正内容只能走脱敏 + 新消息。

**Q: `Tombstone` 之后数据还在吗？**
A: 不在。`Tombstone` 清空 `Parts`、`AdapterState` 和 `FinishReason`，状态变为 `StateTombstoned`，且同样受 `ExpectedRevision` + fence 保护。注意：如果这条消息里的 tool call 在分支内还有存活的 tool result 引用它，脱敏会因破坏配对不变量而返回 `ErrSnapshotInvariant`——要先处理 result 消息。

**Q: `ListVisible` 为什么看不到我刚建的消息？**
A: 可见性由 `VisibleAtRevision` 控制：只有 `VisibleAtRevision > 0` 且小于等于查询的 `Revision` 的消息才会返回。`Create` 时不设该字段（保持 0）的消息永远不出现在 `ListVisible` 里——这是有意设计，消息通常在分支合并进会话后才获得可见 revision。分支内的完整列表请用 `ListBranch`（按 `BranchOrdinal` 排序）。

**Q: 写 tool result 消息时报 `ErrSnapshotInvariant`，"has no matching call"？**
A: 分支级校验要求：result 的 `ToolCallID` 必须能在同分支更早的（`BranchOrdinal` 更小）消息里找到同名 tool call，且每个 call 最多一个 result。`FinishReason` 也有类似约束——只有 `agent.RoleAssistant` 的消息允许设置。
