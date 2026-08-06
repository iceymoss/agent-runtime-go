# permission

`permission` 包为 Agent 工具副作用（effect）提供可移植的授权与持久化审批合约：对每一次精确的工具调用做 allow / deny / ask 决策，并管理审批请求、决议（Resolution）、授权凭据（Grant）的完整生命周期。

## 是什么

这个包的核心抽象是三个接口：`Policy` 负责决策，`Store` 负责持久化，`Service` 把两者编排为一个完整的授权流程。

```go
type Policy interface {
    Version() PolicyVersion
    Evaluate(context.Context, CheckRequest, []Grant) (CheckResult, error)
}

type Service interface {
    Check(context.Context, CheckRequest) (CheckResult, error)
    Resolve(context.Context, ResolveCommand) (Snapshot, bool, error)
    Cancel(context.Context, CancelCommand) (Snapshot, bool, error)
    Revalidate(context.Context, RevalidateCommand) (CheckResult, error)
    // 以及 GetRequest / ListPending / ConsumeGrant / RevokeGrant / ExpireDue
}
```

`CheckRequest` 描述"被授权的到底是哪一次 effect"：它绑定了租户、主体、资源、工具名、动作、输入摘要（`InputDigest`）、策略版本（`PolicyVersion`）和工具代次（`ToolGeneration`）。这些字段是不可变绑定——审批通过的是这一组精确的值，任何一个变了都必须重新走授权。

决策结果有三种：`DecisionAllow` 直接放行；`DecisionDeny` 拒绝；`DecisionAsk` 表示需要人工审批——此时 `Service.Check` 会自动持久化一条 `ApprovalRequest` 并返回 `SuspensionBlocker`（含随机生成的 `ResumeToken`），调用方据此挂起当前 run。审批人通过 `Resolve` 批准或拒绝，恢复执行前必须调用 `Revalidate` 用 `ResumeToken` 重新校验。

包内提供 `NewMemoryStore` 作为 `Store` 的内存参考实现，适合测试与示例；生产环境需要基于数据库自行实现 `Store` 接口，把审批与授权记录持久化。

## 为什么需要它

没有这个包，你需要自己解决一整套难题：审批请求如何幂等创建（重试不会产生重复审批）、两个审批人并发操作时谁生效（first-wins + 乐观版本号）、审批通过后输入被篡改怎么办（resume 时的不可变绑定校验）、限次授权在并发消费下如何不超发（`ConsumeGrant` 的原子扣减）、过期的审批和授权如何统一收敛（`ExpireDue`）。这些都是分布式审批系统里最容易写错的部分。

什么时候不需要它：如果你的工具全部是只读或无副作用操作、不存在"危险操作需要人批准"的场景，直接跳过这个包，工具执行不接权限检查即可。

## 怎么用

下面的示例定义一个"没有 Grant 就要求审批"的策略，触发 ask、模拟审批人批准，再用 `Revalidate` 拿到放行结果：

```go
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go/permission"
)

func main() {
	ctx := context.Background()
	policy := permission.PolicyFunc{
		PolicyVersion: "policy-v1",
		EvaluateFunc: func(_ context.Context, req permission.CheckRequest, grants []permission.Grant) (permission.CheckResult, error) {
			if len(grants) > 0 {
				return permission.CheckResult{Decision: permission.DecisionAllow}, nil
			}
			return permission.CheckResult{Decision: permission.DecisionAsk}, nil
		},
	}
	service, err := permission.NewService(permission.ServiceOptions{
		Policy: policy,
		Store:  permission.NewMemoryStore(),
	})
	if err != nil {
		panic(err)
	}

	check := permission.CheckRequest{
		RequestKey:        "request-1",
		Subject:           permission.Subject{TenantKey: "tenant-1", PrincipalKey: "user-1"},
		Resource:          permission.Resource{Kind: "file", Key: "/data/report.txt"},
		RunRef:            "run-1",
		AttemptRef:        "attempt-1",
		ExecutionRef:      "execution-1",
		ToolName:          "write_file",
		Action:            "update",
		InputDigest:       "sha256:9f2c...",
		PolicyVersion:     "policy-v1",
		ToolGeneration:    "tools-v1",
		ApprovalExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	result, err := service.Check(ctx, check)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Decision) // ask，result.Blocker 携带 ResumeToken

	_, _, err = service.Resolve(ctx, permission.ResolveCommand{
		TenantKey: "tenant-1", RequestKey: "request-1",
		CommandKey: "cmd-1", DecisionKey: "decision-1", ApproverKey: "admin",
		ExpectedRevision: 1, Kind: permission.ResolutionApprove,
	})
	if err != nil {
		panic(err)
	}

	allowed, err := service.Revalidate(ctx, permission.RevalidateCommand{
		TenantKey: "tenant-1", RequestKey: "request-1",
		ResumeToken: result.Blocker.ResumeToken, AttemptRef: "attempt-1",
		InputDigest:    check.InputDigest,
		PolicyVersion:  check.PolicyVersion,
		ToolGeneration: check.ToolGeneration,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(allowed.Decision) // allow
}
```

运行输出：

```text
ask
allow
```

关键行为：

- `Check` 会严格校验 `CheckRequest` 的必填字段（`RequestKey`、`Subject.TenantKey`、`Subject.PrincipalKey`、`RunRef`、`AttemptRef`、`ExecutionRef`、`InputDigest`、`PolicyVersion`、`ToolName`、`Action`），缺一个就返回 `ErrInvalidRequest`。
- ask 决策时 `Check` 内部调用 `Store.CreateAndSuspend` 持久化审批请求，新请求的 `Revision` 为 1——这就是随后 `Resolve` 中 `ExpectedRevision: 1` 的来源。
- `Resolve` 的第二个返回值表示状态是否真的发生了变化；用完全相同的命令重试会返回 `(snapshot, false, nil)`，即天然幂等。
- `Revalidate` 会依次校验 resume token、不可变绑定（`InputDigest`/`PolicyVersion`/`ToolGeneration`）、审批状态和当前策略，全部通过才把结果收敛为 `DecisionAllow`。
- 示例没有配置 `ServiceOptions.Fences`，fence 校验被跳过；生产环境应提供 `FenceValidator`，让 durable owner 拒绝过期 attempt 的操作。

## 常见问题

**Q: `Check` 返回 `ErrPolicyUnavailable` 是什么原因？**
A: `CheckRequest.PolicyVersion` 与 `Policy.Version()` 不一致。授权绑定的是精确的策略版本，策略升级后旧版本的请求不能继续用新策略评估，调用方需要用当前版本重建请求。

**Q: 两个审批人同时 `Resolve` 同一个请求会怎样？**
A: first-wins。第一个命令生效，第二个不同的命令返回 `ErrAlreadyResolved`（如果 `ExpectedRevision` 已经落后则返回 `ErrRevisionConflict`）。重放与已生效决议完全相同的命令则幂等返回成功。

**Q: `Revalidate` 返回 `ErrResumeRevalidation`，但审批明明已经通过了？**
A: 检查两点：一是 `ResumeToken` 是否与 `Check` 时 `Blocker` 返回的一致；二是 `InputDigest`、`PolicyVersion`、`ToolGeneration` 三个不可变绑定是否与当初审批的请求完全相同。任何一项变了都视为"批准的不是这次 effect"，必须重新授权。

**Q: `GrantSpec.MaxUses` 设为 nil 和设为 0 有什么区别？**
A: nil 表示不限次数；非 nil 的 0 是合法值，表示"立即耗尽"的授权（`Grant.State` 直接为 `GrantExhausted`）。并发调用 `ConsumeGrant` 时靠 `ExpectedRevision` 乐观锁保证限次授权只被成功消费对应次数，落败方收到 `ErrRevisionConflict` 或 `ErrGrantExhausted`。

**Q: 审批时能顺便发一个拒绝性质的 Grant 吗？**
A: 不能。`ResolveCommand.Grant` 只在 `Kind` 为 `ResolutionApprove` 时合法，deny 命令携带 Grant 会返回 `ErrInvalidRequest`。另外 Grant 的 `ExpiresAt` 必须是未来时间，否则返回 `ErrGrantExpired`；Grant 的各字段（PrincipalKey、ToolName、Action、资源、策略版本等）必须与被审批请求匹配，否则返回 `ErrGrantNotApplicable`。

**Q: `MemoryStore` 能直接用于生产吗？**
A: 不建议。它是加锁的进程内实现，审批数据不落盘、无法跨进程共享，适合测试和单进程原型。生产环境应实现 `Store` 接口，把审批请求、决议和 Grant 存入数据库，并保证 `CreateAndSuspend` 的幂等语义和 `ConsumeGrant` 的原子性。
