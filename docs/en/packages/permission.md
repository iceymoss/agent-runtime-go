# permission

The `permission` package provides portable authorization and persistent approval contracts for Agent tool side effects. It makes an allow / deny / ask decision for each exact tool invocation and manages the complete lifecycle of approval requests, resolutions, and grants.

## What it is

The package has three core interfaces: `Policy` makes decisions, `Store` persists them, and `Service` orchestrates both into a complete authorization flow.

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

`CheckRequest` describes exactly which effect is being authorized. It binds the tenant, subject, resource, tool name, action, input digest (`InputDigest`), policy version (`PolicyVersion`), and tool generation (`ToolGeneration`). These fields are immutable bindings: approval applies to this exact set of values, and changing any one requires authorization again.

There are three decision results: `DecisionAllow` proceeds immediately; `DecisionDeny` rejects the operation; and `DecisionAsk` requires human approval. For ask, `Service.Check` automatically persists an `ApprovalRequest` and returns a `SuspensionBlocker` containing a randomly generated `ResumeToken`, which the caller uses to suspend the current run. An approver uses `Resolve` to approve or deny it, and the caller must use `Revalidate` with the `ResumeToken` before resuming execution.

The package provides `NewMemoryStore` as an in-memory `Store` reference implementation for tests and examples. Production systems should implement `Store` with a database to persist approval and grant records.

## Why you need it

Without this package, you must solve an entire set of difficult problems yourself: idempotently creating approval requests so retries do not duplicate them, selecting a winner when two approvers act concurrently (first-wins plus optimistic versioning), preventing input tampering after approval through immutable-binding validation on resume, atomically decrementing limited-use grants under concurrent consumption, and consistently expiring approvals and grants through `ExpireDue`. These are among the easiest parts of a distributed approval system to implement incorrectly.

When you do not need it: if all tools are read-only or have no side effects, and no dangerous operation requires human approval, skip this package and execute tools without permission checks.

## How to use it

The following example defines a policy that requires approval when no Grant exists, triggers ask, simulates approval, then obtains an allow result through `Revalidate`:

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

Output:

```text
ask
allow
```

Key behavior:

- `Check` strictly validates required `CheckRequest` fields (`RequestKey`, `Subject.TenantKey`, `Subject.PrincipalKey`, `RunRef`, `AttemptRef`, `ExecutionRef`, `InputDigest`, `PolicyVersion`, `ToolName`, and `Action`); if any is missing, it returns `ErrInvalidRequest`.
- For an ask decision, `Check` internally calls `Store.CreateAndSuspend` to persist an approval request. A new request has `Revision` 1, which is the source of `ExpectedRevision: 1` in the subsequent `Resolve`.
- The second return value from `Resolve` indicates whether the state actually changed. Retrying the exact same command returns `(snapshot, false, nil)`, making it naturally idempotent.
- `Revalidate` checks the resume token, immutable bindings (`InputDigest`/`PolicyVersion`/`ToolGeneration`), approval state, and current policy in sequence. Only if all pass does it converge the result to `DecisionAllow`.
- The example does not configure `ServiceOptions.Fences`, so fence validation is skipped. Production systems should provide a `FenceValidator` so the durable owner rejects operations from stale attempts.

## FAQ

**Q: Why does `Check` return `ErrPolicyUnavailable`?**
A: `CheckRequest.PolicyVersion` does not match `Policy.Version()`. Authorization binds to an exact policy version; after a policy upgrade, an old-version request cannot be evaluated with the new policy. Rebuild the request using the current version.

**Q: What happens if two approvers call `Resolve` concurrently for the same request?**
A: First-wins. The first command takes effect, and a different second command returns `ErrAlreadyResolved` (or `ErrRevisionConflict` if its `ExpectedRevision` is already stale). Replaying a command exactly identical to the applied resolution succeeds idempotently.

**Q: Why does `Revalidate` return `ErrResumeRevalidation` even though the request was approved?**
A: Check two things: whether `ResumeToken` matches the value returned in the `Blocker` by `Check`, and whether the three immutable bindings `InputDigest`, `PolicyVersion`, and `ToolGeneration` exactly match the original approval request. If any differs, the approved effect is not this effect, and authorization must run again.

**Q: What is the difference between setting `GrantSpec.MaxUses` to nil and setting it to 0?**
A: nil means unlimited uses. A non-nil 0 is valid and represents an immediately exhausted grant (`Grant.State` is directly `GrantExhausted`). Concurrent `ConsumeGrant` calls use `ExpectedRevision` optimistic locking to ensure a limited-use grant is consumed successfully only the permitted number of times; losing callers receive `ErrRevisionConflict` or `ErrGrantExhausted`.

**Q: Can a denial resolution also issue a denial-type Grant?**
A: No. `ResolveCommand.Grant` is valid only when `Kind` is `ResolutionApprove`; a deny command carrying a Grant returns `ErrInvalidRequest`. A Grant's `ExpiresAt` must also be in the future or it returns `ErrGrantExpired`, and its fields (PrincipalKey, ToolName, Action, resource, policy version, and so on) must match the approval request or it returns `ErrGrantNotApplicable`.

**Q: Can `MemoryStore` be used directly in production?**
A: It is a locked in-process implementation whose approval data is not persisted or shared across processes, making it suitable for tests and single-process prototypes. Production systems should implement `Store` with a database, persist approval requests, resolutions, and Grants, and preserve the idempotency semantics of `CreateAndSuspend` and atomicity of `ConsumeGrant`.
