# Sub-Agent

`agent/subagent` 编排独立 child run，而不是在 parent goroutine 中递归调用函数。

应用实现：

```go
type Runner interface {
    Run(context.Context, subagent.RunRequest) (subagent.RunResult, error)
}

type ParentWaker interface {
    Wake(context.Context, subagent.WakeRequest) error
}
```

然后组装：

```go
service, err := subagent.New(subagent.Options{
    Store: subagent.NewMemoryStore(),
    Runner: childRunner,
    ParentWaker: parentWaker,
    WorkerID: "worker-1",
    LeaseDuration: time.Minute,
})
```

典型流程：

```text
Spawn stable request
  -> reserve depth/fanout/token/cost/tool/runtime budget
RunNext
  -> claim one child
  -> execute Runner
  -> commit terminal or suspended state
Reconcile
  -> recover leases
  -> deliver idempotent parent wake intent
```

iCoder 的 `delegate_review` 展示这一流程。child runner 是确定性 reviewer reference，memory store 不跨重启，parent waker 也不自动恢复根 Agent。

生产实现应把 child 映射为独立 durable run，并让 `ParentWaker` 连接 session admission/resume。Wake 必须按 `WakeKey` 幂等。
