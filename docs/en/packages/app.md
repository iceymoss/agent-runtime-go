# app

The `app` package orchestrates Agent runtime process lifecycle: components start in dependency order, admission opens only after readiness, and shutdown walks fixed phases each with its own time budget—never waiting forever.

## What it is

The core type is `App`. You wrap process infrastructure (database, provider catalog, MCP connections, and so on) as `Component`s, and wrap "how to drain/cancel/wait for in-flight Agent runs" as three narrow dependency ports; `App` coordinates them.

```go
type Component interface {
	Name() string
	Dependencies() []string
	Criticality() Criticality // Required or Optional
	Start(context.Context, Reporter) error
	Ready(context.Context) ComponentHealth
	Close(context.Context) error
}

type Reporter interface{ Report(ComponentHealth) error }
```

The `Reporter` passed to `Component.Start` is the runtime health-reporting channel: when a background probe sees a state change, the component calls `Report(ComponentHealth)`; `App` migrates between `StateReady` and `StateDegraded` and toggles admission accordingly. `ComponentHealth.Revision` must strictly increase; stale reports are rejected.

Three dependency ports are injected via `Dependencies`: `RunController` (admission gate plus run drain/cancel/wait), `Checkpointer` (checkpoint and revoke runs that could not finish), `Flusher` (event flush to durable storage). `App` itself does not execute Agents and owns no run or event data.

The state machine is described by `State` constants: `StateNew → StateStarting → StateReady/StateDegraded`; on shutdown, `StateDraining → StateCanceling → StateCheckpointing → StateWaiting → StateStopping → StateStopped` (or `StateFailed`). `Health()` always returns an immutable health snapshot.

## Why you need it

Writing process lifecycle yourself hits the pitfalls this package addresses one by one: component start order maintained only by statement order in `main`, where adding a component can introduce implicit dependency breakage (here explicit `Dependencies()` drives topological sort; cycles and unknown deps report `ErrConfigInvalid` in `New`); accepting traffic before readiness (here admission opens only after all required components pass the first readiness probe); canceling the `context` aborting all cleanup (here each phase uses an independent timeout budget on top of `context.WithoutCancel`, so caller cancel does not stop shutdown); and graceful shutdown turning into an infinite wait (each phase times out, records, and advances; finally reports `LeakedRuns`).

**When you do not need it**: a single-component tool process with no long-running work is fine with `defer close()`; introduce this package when the process has multiple interdependent components and in-flight Agent runs that must drain safely.

## How to use it

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
		databaseComponent, // implements app.Component
		catalogComponent,  // Dependencies() returns []string{"database"}
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

	// ... after SIGTERM:
	report, err := application.Shutdown(context.Background())
	log.Printf("shutdown: final=%s leaked=%d err=%v", report.FinalState, len(report.LeakedRuns), err)
}
```

Output:

```text
2026/08/06 14:27:02 ready: state=ready admission=true
2026/08/06 14:27:02 shutdown: final=stopped leaked=0 err=<nil>
```

Key behavior:

- **Startup**: components `Start` then `Ready`-probe in dependency topological order. A failed `Required` component aborts startup; already-started components `Close` in reverse order within the `StartupRollback` budget, and `Start` returns an error wrapping `ErrStartupFailed`. A failed `Optional` component is recorded in the health report only; the process becomes ready in `StateDegraded` with admission still open.
- **`WaitReady`**: blocks until startup completes and returns a health snapshot. Calling before `Start` returns `ErrNotReady` immediately; runtime degradation after a successful start does not make it block again.
- **Runtime**: a required component reporting unhealthy via `Reporter` → `StateDegraded` and admission closed; a recovery report returns to `StateReady` and reopens admission.
- **Shutdown**: `Shutdown` runs drain → cancel → checkpoint (residual runs also revoked) → wait → flush → close, six phases, each producing a `PhaseReport` (start/end, budget, timed out or not, remaining runs). If runs still live after wait, that is unsafe: skip component `Close` (recorded in `SkippedClose`), put leaked runs in `LeakedRuns`, and revoke once more. Components close in reverse of start order.
- **Idempotency**: `Shutdown` and `StopAdmission` each run only once; concurrent and later callers get a defensive copy of the same result.

## FAQ

**Q: What causes `New` to return `ErrConfigInvalid`?**
A: Any non-positive `Budgets` field; any of the three `Dependencies` ports nil; empty component list; empty or duplicate component names; `Criticality()` not `Required`/`Optional`; a dependency pointing at an unregistered component or itself; a dependency cycle.

**Q: How should `Reporter.Report` handle `ErrStaleReport`/`ErrReporterClosed`?**
A: `ErrStaleReport` means `Revision` was not strictly greater than the last accepted value—normal under concurrent reporting; discard it, but the component must keep revisions monotonically increasing. `ErrReporterClosed` means `App` has entered shutdown or failed state; the component should stop its probe loop. Also note validation: `CheckedAt` must not be zero; when `Ready: true`, `Configured: true` and non-empty `Generation` are required, otherwise `ErrConfigInvalid`.

**Q: If an `Optional` component fails, how does `Health()` change?**
A: `Ready` stays `true` (admission unaffected), but `Degraded` becomes `true`, `State` becomes `StateDegraded`, and the component's failure reason appears in `Components[i].Reason`. Any component reporting `Degraded: true` also degrades the whole app.

**Q: If the caller's `ctx` is canceled, does `Shutdown` stop?**
A: No. Internally `Shutdown` derives each phase context with `context.WithoutCancel(ctx)` and is constrained only by each phase's budget timeout. That is intentional: shutdown often happens after the upstream context is already canceled.

**Q: What does `Shutdown` returning `ErrShutdownIncomplete` mean?**
A: At least one phase errored or timed out; `report.FinalState` is `StateFailed`. Inspect `report.Phases` (which phase, whether `TimedOut`), `report.LeakedRuns` (runs that did not finish), and `report.Unclosed` (components not closed). The error chain retains each phase's original errors for `errors.Is` matching.

**Q: Can I `Start` again after `StopAdmission`?**
A: No. Closing admission is permanent; later `Start` returns `ErrInvalidTransition`. `StopAdmission` fits a two-step rollout: drain traffic first, then `Shutdown` when ready.
