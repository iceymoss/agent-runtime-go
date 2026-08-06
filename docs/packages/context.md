# context

`context` 包负责把一段消息历史变成可以安全交给模型的"上下文计划"：规范化历史、校验 tool call/result 配对、按 token 预算做估算，并在超限时通过摘要压缩出不可变的 summary artifact。它不拥有任何 session 或消息存储。

> **导入别名**：这个包与标准库 `context` 同名。同一文件里两者都要用时，惯例是给本包起别名 `agentcontext`（仓库测试代码即如此），标准库保持原名：
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
> 包内部源码则反过来，用 `stdcontext "context"` 引标准库。两种方式任选其一，关键是别让读者猜 `context.Context` 指的是谁。

## 是什么

包里有三组能力，层层递进：

1. **历史归一化**：`NormalizeHistory(NormalizeRequest) (NormalizeResult, error)` 校验一段 `[]agent.Message` 的合法性——角色是否已知、part 结构是否自洽、tool call 与 tool result 是否严格配对——并做确定性修剪：剔除空 assistant 消息、剥离 provider 扩展 part（如原始 reasoning）、补全缺失的 tool result 名字。输出带 `Digest`（canonical 摘要）、`Exchanges`（完整工具交换的区间）和 `Diagnostics`（修剪记录）。
2. **计划构建**：`Planner` 把一次调用的全部输入（系统消息、摘要、受保护事实、主线/分支/本次消息、token 预算）组装成不可变的 `Plan`。`Plan` 由内容寻址的 `PlanRef` 标识（`PlanKey = "ctx_" + digest 前 32 位`），同输入必得同计划。
3. **压缩**：`Compactor.Compact` 选出安全前缀（不砍系统消息、最近用户意图和未完成的工具交换），交给你实现的 `Summarizer` 生成摘要，产出不可变 `SummaryArtifact` 和指向它的 `PivotRef`。

```go
type TokenCounter interface {
	ID() string
	CountTokens(stdcontext.Context, []agent.Message) (int, error)
}

type Planner interface {
	Prepare(stdcontext.Context, PrepareRequest) (Plan, error)
}
```

`NewPlanner(counter, store)` 返回默认实现 `DefaultPlanner`；`NewMemoryStore`/`NewMemoryArtifactStore` 提供测试用的内存版 `PlanStore`/`ArtifactStore`，语义均为 create-or-verify（同 key 同内容幂等，同 key 不同内容返回 `ErrArtifactConflict`）。

## 为什么需要它

不用这个包，你要自己处理：脏历史（孤儿 tool result、重复 call ID、进程崩溃留下的未闭合工具调用）会被 provider API 直接拒绝；token 超限只能靠"发出去看会不会 400"；压缩历史时一不小心把系统约束或用户最新意图摘要掉。这个包把这些都变成构造期的确定性校验：坏历史返回 `ErrToolPairing`/`ErrHistoryInvalid`，超预算返回 `ErrCompactionRequired` 或 `ErrContextTooLarge`（而不是静默截断），压缩永远保住安全边界。

**什么时候不需要它**：一次性脚本、无持久化历史的短对话，直接把消息喂给根包 runner 即可；只有当历史跨进程持久化、需要精确 revision 绑定或会话长到要压缩时，才值得引入。

## 怎么用

最小示例：实现一个 `TokenCounter`，构建 `Plan`，把 `plan.Messages()` 交给模型。

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
			TokenizerID:       "chars/v1", // 必须等于 counter.ID()
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
	// plan.Messages() 即可直接作为模型请求的消息列表
}
```

关键行为：

- **组装顺序固定**：`SystemMessages` → `CapabilityMessages` → `SummaryMessages` → 受保护事实投影 → `MainlineMessages` → `BranchMessages` → `InvocationMessages`，拼好后整体走一遍 `NormalizeHistory`（`RepairReject` 策略），任何配对问题都会让 `Prepare` 失败。
- **预算口径**：`Budget.InputLimit() = ContextTokens - ReservedOutputTokens - SafetyMarginTokens`；最终估算 `Estimate.TotalInputTokens = MessageTokens + ToolSchemaTokens + MediaTokens`，超过 `InputLimit()` 时，若存在可压缩前缀返回 `ErrCompactionRequired`，否则返回 `ErrContextTooLarge`。
- **确定性**：同一个 `PrepareRequest` 重复调用得到相同 `PlanRef`；`PlanStore.Get` 用完整 `ref` 匹配，篡改 ref 中任何字段（哪怕只是 `Budget.ContextTokens` 加一）会得到 `ErrPlanDrift`。
- **不可变性**：`plan.Messages()` 等所有访问器返回深拷贝，改返回值不影响已存储的计划。`MarshalWire`/`UnmarshalPlan` 提供带自校验的持久化格式，反序列化时 digest 不匹配同样报 `ErrPlanDrift`。

崩溃恢复场景用 `RepairFromTerminal`：历史结尾有未闭合的 tool call 时，为每个 call 提供一条 `TerminalFact{CallID, Reason, SourceRevision}`（`EffectUnknown: true` 表示副作用状态未知），`NormalizeHistory` 会合成 `IsError: true, StopTurn: true` 的终止 tool result 并记录 `DiagnosticTerminalRepaired` 诊断；缺任何一个 fact 则返回 `ErrHistoryInterrupted`。

## 常见问题

**Q: 受保护事实（`ProtectedFact`）的字段都必填吗？**
A: 是。`Key`、`Kind`、`Value`、`SourceKey`、`PolicyVersion` 非空且 `SourceRevision != 0`，否则 `NewFactSet` 报 `ErrInvalidRequest`；同一 `Key` 出现两次报 `ErrProtectedFactConflict`。事实集会投影成一条 JSON 系统消息计入预算，仅事实加 tool schema/media 就超 `InputLimit()` 时直接返回 `ErrProtectedFactsTooLarge`——压缩救不了它们，只能精简事实或加预算。

**Q: `Prepare` 报"runtime tokenizer and token counter identities differ"？**
A: `PrepareRequest.Runtime.TokenizerID` 必须与 `NewPlanner` 传入的 counter 的 `ID()` 完全一致。这是刻意的：token 估算只对特定 tokenizer 有效，换模型就该换 counter 和 TokenizerID。

**Q: `SourceRef` 里 `BranchKey` 和 `BranchVersion` 能只填一个吗？**
A: 不能，必须同时填或同时不填，否则 `ErrInvalidRequest`。另外 `TenantKey`、`SessionKey`、`SessionRevision` 全部必填（revision 从 1 开始，0 视为缺失）。

**Q: 为什么 `Compact` 返回 `ErrCompactionNoProgress`？**
A: 三种情况：安全前缀为空（比如历史开头就是系统/用户消息或工具交换，没有可压缩的纯 assistant 前缀）；摘要后 `before - after < MinSavings`；摘要后总量仍大于 `TargetTokens`。另外 `Summarizer` 返回的消息里不允许出现工具内容或图片（`ErrHistoryInvalid`），`Generation` 必须非空。

**Q: 压缩会不会把用户最近的话摘要掉？**
A: 不会。`selectSafePrefix` 只选"最后一条用户消息之前、且不含系统消息和工具交换"的连续 assistant 前缀，最新用户意图必然留在 `CompactResult.Kept` 里。

**Q: `PivotRef` 由谁保存和推进？**
A: 调用方（session 的 owner）。本包只在 `CompactResult.Pivot` 里返回一个 revision 绑定的值，自己不追踪"当前 pivot"。下次 `Prepare` 时把它填进 `PrepareRequest.Pivot`，其 `FactSetDigest` 必须与当次事实集的 digest 一致，否则报 `ErrPivotInvalid`。

**Q: 归一化会保留图片吗？**
A: 会。合法的 `PartImage` 按原位置保留，字节数据深拷贝；未知类型的 part（provider 扩展、原始 reasoning）会被静默剥离，不进入 provider 中立的规范历史。
