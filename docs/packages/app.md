# app

`app` 包负责 Agent 运行时进程的生命周期编排：组件按依赖顺序启动、就绪后才开放请求准入（admission），停机时按固定阶段、每阶段带独立时间预算地有界退出，绝不无限等待。

## 是什么

核心类型是 `App`。你把进程里的各个基础设施（数据库、provider catalog、MCP 连接等）包装成 `Component`，把"正在运行的 Agent run 如何排空/取消/等待"包装成三个窄依赖端口，`App` 负责协调它们。

```go
type Component interface {
	Name() string
	Dependencies() []string
	Criticality() Criticality // Required 或 Optional
	Start(context.Context, Reporter) error
	Ready(context.Context) ComponentHealth
	Close(context.Context) error
}

type Reporter interface{ Report(ComponentHealth) error }
```

`Component.Start` 收到的 `Reporter` 是运行期健康上报通道：组件后台探活发现状态变化时调用 `Report(ComponentHealth)`，`App` 据此在 `StateReady` 与 `StateDegraded` 之间迁移，并同步开关 admission。`ComponentHealth` 的 `Revision` 必须严格递增，旧报告被拒绝。

三个依赖端口通过 `Dependencies` 注入：`RunController`（准入开关与 run 的 drain/cancel/wait）、`Checkpointer`（对未能结束的 run 做 checkpoint 与 revoke）、`Flusher`（事件落盘）。`App` 自己不执行 Agent，也不拥有任何 run 或事件数据。

状态机由 `State` 常量描述：`StateNew → StateStarting → StateReady/StateDegraded`，停机时依次 `StateDraining → StateCanceling → StateCheckpointing → StateWaiting → StateStopping → StateStopped`（或 `StateFailed`）。`Health()` 随时返回不可变的健康快照。

## 为什么需要它

自己写进程生命周期，常见的坑正是这个包逐条解决的：组件启动顺序靠 `main` 函数里的书写顺序维持，加一个组件就可能引入隐式依赖破坏（这里用显式 `Dependencies()` 做拓扑排序，环和未知依赖在 `New` 就报 `ErrConfigInvalid`）；服务还没就绪就开始接流量（这里 required 组件全部通过首次就绪探测后才打开 admission）；停机时 `context` 一取消所有清理工作全部中断（这里每个阶段用 `context.WithoutCancel` 基础上的独立超时预算，调用方取消不影响停机推进）；以及优雅停机变成无限等待（每阶段超时就记录并进入下一阶段，最终报告 `LeakedRuns`）。

**什么时候不需要它**：单组件、无长时任务的小工具进程，`defer close()` 就够了；只有当进程里有多个有依赖关系的组件、且有正在运行的 Agent run 需要安全排空时才值得引入。

## 怎么用

```go
package main

import (
	"context"
	"log"
	"time"

	"github.com/iceymoss/agent-runtime-go/app"
)

func main() {
	application, err := app.New(
		app.Config{Budgets: app.Budgets{
			Startup: 30 * time.Second, StartupRollback: 10 * time.Second,
			Drain: 30 * time.Second, Cancel: 10 * time.Second,
			Checkpoint: 10 * time.Second, Wait: 10 * time.Second,
			Flush: 5 * time.Second, Close: 10 * time.Second,
		}},
		app.Dependencies{Runs: runController, Checkpoints: checkpointer, Events: flusher},
		databaseComponent, // 实现 app.Component
		catalogComponent,  // Dependencies() 返回 []string{"database"}
	)
	if err != nil {
		log.Fatal(err)
	}

	if err := application.Start(context.Background()); err != nil {
		log.Fatal(err)
	}
	health, err := application.WaitReady(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("ready: state=%s admission=%v", health.State, health.Admission)

	// ... 收到 SIGTERM 后：
	report, err := application.Shutdown(context.Background())
	log.Printf("shutdown: final=%s leaked=%d err=%v", report.FinalState, len(report.LeakedRuns), err)
}
```

运行输出：

```text
2026/08/06 14:27:02 ready: state=ready admission=true
2026/08/06 14:27:02 shutdown: final=stopped leaked=0 err=<nil>
```

关键行为：

- **启动**：组件按依赖拓扑序逐个 `Start` + `Ready` 探测。`Required` 组件失败即中止启动，已启动的组件在 `StartupRollback` 预算内逆序 `Close`，`Start` 返回包裹 `ErrStartupFailed` 的错误；`Optional` 组件失败只记入健康报告，进程以 `StateDegraded` 就绪，admission 照常打开。
- **`WaitReady`**：阻塞到启动完成，返回健康快照。在 `Start` 之前调用直接返回 `ErrNotReady`；启动成功后的运行期降级不会让它重新阻塞。
- **运行期**：required 组件通过 `Reporter` 上报不健康 → 状态转 `StateDegraded` 且 admission 关闭；恢复上报后回到 `StateReady` 并重新开放。
- **停机**：`Shutdown` 依次执行 drain → cancel → checkpoint（残留 run 顺带 revoke）→ wait → flush → close 六个阶段，每阶段一份 `PhaseReport`（起止时间、预算、是否超时、剩余 run）。若 wait 之后仍有 run 存活，视为不安全：跳过组件 `Close`（记入 `SkippedClose`），泄漏的 run 记入 `LeakedRuns` 并做最后一次 revoke。组件关闭与启动顺序相反。
- **幂等**：`Shutdown` 与 `StopAdmission` 都只执行一次，并发和后续调用拿到同一份结果的防御性拷贝。

## 常见问题

**Q: `New` 报 `ErrConfigInvalid`，都有哪些原因？**
A: 任一 `Budgets` 字段不为正；`Dependencies` 三个端口有 nil；组件列表为空；组件名为空或重复；`Criticality()` 不是 `Required`/`Optional`；依赖指向未注册的组件或自己；依赖成环。

**Q: `Reporter.Report` 返回 `ErrStaleReport`/`ErrReporterClosed` 怎么处理？**
A: `ErrStaleReport` 表示 `Revision` 没有严格大于上一次接受的值——并发上报时属正常竞争，丢弃即可，但组件必须保证 revision 单调递增。`ErrReporterClosed` 表示 `App` 已进入停机或失败状态，组件应停止探活循环。另外注意校验规则：`CheckedAt` 不能为零值；`Ready: true` 时必须同时 `Configured: true` 且 `Generation` 非空，否则报 `ErrConfigInvalid`。

**Q: `Optional` 组件挂了，`Health()` 会怎么变？**
A: `Ready` 保持 `true`（admission 不受影响），但 `Degraded` 变 `true`、`State` 变 `StateDegraded`，该组件的失败原因出现在 `Components[i].Reason` 里。任何组件报告 `Degraded: true` 也会让整体降级。

**Q: 调用方的 `ctx` 取消了，`Shutdown` 会中断吗？**
A: 不会。`Shutdown` 内部用 `context.WithoutCancel(ctx)` 派生各阶段上下文，只受各自的预算超时约束。这是有意设计：停机恰恰常发生在上游 context 已取消之后。

**Q: `Shutdown` 返回 `ErrShutdownIncomplete` 意味着什么？**
A: 至少一个阶段出错或超时，`report.FinalState` 为 `StateFailed`。具体看 `report.Phases`（哪个阶段、是否 `TimedOut`）、`report.LeakedRuns`（未能结束的 run）、`report.Unclosed`（未关闭的组件）。错误链保留了各阶段的原始错误，可用 `errors.Is` 逐个匹配。

**Q: `StopAdmission` 之后还能 `Start` 吗？**
A: 不能。admission 关闭是永久性的，之后 `Start` 返回 `ErrInvalidTransition`。`StopAdmission` 适合在滚动发布中先摘流量、再择机 `Shutdown` 的两段式下线。
