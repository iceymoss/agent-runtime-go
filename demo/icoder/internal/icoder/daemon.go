package icoder

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go/app"
	"github.com/iceymoss/agent-runtime-go/durable"
	"github.com/iceymoss/agent-runtime-go/event"
)

// Component generations name what each part of the process is serving. They are
// reported with readiness so a probe can tell an upgraded process from a
// restarted one.
const (
	storeSchemaVersion = "icoder-schema-v1"
	eventStreamVersion = "icoder-events-v1"
	skillsGeneration   = "icoder-skills-v1"
	mcpGeneration      = "icoder-mcp-v1"
	queueGeneration    = "icoder-queue-v1"
)

// Daemon wraps the application in a lifecycle: components come up in dependency
// order, readiness is reported rather than assumed, and shutdown is bounded.
//
// A coding agent that is only ever driven from a terminal does not strictly need
// this. It becomes necessary the moment something else decides when the process
// starts and stops - a supervisor, a container orchestrator, a CI job - because
// then "are you ready" and "stop within N seconds" have to be real answers.
type Daemon struct {
	app       *App
	lifecycle *app.App
	runs      *runController
}

// DaemonOptions bounds each shutdown phase. Zero values fall back to defaults
// that are deliberately short: an unbounded shutdown is the same as no shutdown.
type DaemonOptions struct {
	Budgets   app.Budgets
	Publisher event.Publisher
}

// NewDaemon assembles the component graph and the run controller the lifecycle
// drives.
func NewDaemon(application *App, options DaemonOptions) (*Daemon, error) {
	if application == nil {
		return nil, fmt.Errorf("daemon requires an application")
	}
	budgets := options.Budgets
	if budgets.Startup == 0 {
		budgets.Startup = 30 * time.Second
	}
	if budgets.Drain == 0 {
		budgets.Drain = 30 * time.Second
	}
	if budgets.Cancel == 0 {
		budgets.Cancel = 5 * time.Second
	}
	if budgets.Checkpoint == 0 {
		budgets.Checkpoint = 10 * time.Second
	}
	if budgets.Wait == 0 {
		budgets.Wait = 10 * time.Second
	}
	if budgets.Flush == 0 {
		budgets.Flush = 10 * time.Second
	}
	if budgets.Close == 0 {
		budgets.Close = 10 * time.Second
	}
	if budgets.StartupRollback == 0 {
		budgets.StartupRollback = 10 * time.Second
	}
	controller := application.runs
	lifecycle, err := app.New(
		app.Config{Budgets: budgets},
		app.Dependencies{
			Runs:        controller,
			Checkpoints: &leaseRevoker{store: application.durable},
			Events:      &outboxFlusher{app: application, publisher: options.Publisher},
		},
		application.components()...,
	)
	if err != nil {
		return nil, err
	}
	return &Daemon{app: application, lifecycle: lifecycle, runs: controller}, nil
}

// Start brings every component up and opens admission.
func (d *Daemon) Start(ctx context.Context) error { return d.lifecycle.Start(ctx) }

// Health reports the current lifecycle state and per-component readiness.
func (d *Daemon) Health() app.Health { return d.lifecycle.Health() }

// WaitReady blocks until startup finishes, returning why it is not ready if it
// is not.
func (d *Daemon) WaitReady(ctx context.Context) (app.Health, error) {
	return d.lifecycle.WaitReady(ctx)
}

// Shutdown closes admission and runs the bounded drain, cancel, checkpoint, wait,
// flush, and close phases exactly once.
func (d *Daemon) Shutdown(ctx context.Context) (app.ShutdownReport, error) {
	return d.lifecycle.Shutdown(ctx)
}

// components describes what has to be healthy for a run to be admitted, and in
// what order those things come up.
func (a *App) components() []app.Component {
	components := []app.Component{
		&lifecycleComponent{
			name: "store", criticality: app.Required,
			ready: func(ctx context.Context) app.ComponentHealth {
				// A configured database is not a working one; the readiness probe
				// actually touches it.
				err := a.store.db.PingContext(ctx)
				return componentHealth(err == nil, storeSchemaVersion, "database unreachable", err)
			},
		},
		&lifecycleComponent{
			name: "runtime", dependencies: []string{"store"}, criticality: app.Required,
			ready: func(context.Context) app.ComponentHealth {
				return componentHealth(a.resolved.Definition != nil, a.resolved.DefinitionDigest, "no runtime generation resolved", nil)
			},
		},
		&lifecycleComponent{
			name: "tools", dependencies: []string{"runtime"}, criticality: app.Required,
			ready: func(context.Context) app.ComponentHealth {
				return componentHealth(len(a.tools) > 0, a.catalog.GenerationDigest(), "no tools are registered", nil)
			},
		},
		&lifecycleComponent{
			name: "events", dependencies: []string{"store"}, criticality: app.Required,
			ready: func(ctx context.Context) app.ComponentHealth {
				_, err := a.store.ReplayEvents(ctx, a.state.Get(), 0, 1)
				return componentHealth(err == nil, eventStreamVersion, "event stream unreadable", err)
			},
		},
	}
	if a.queue != nil {
		components = append(components, &lifecycleComponent{
			name: "queue", dependencies: []string{"runtime", "tools"}, criticality: app.Required,
			start: func(ctx context.Context) error {
				// One worker, matching the admission limit: a single workspace has
				// one set of files, so a second worker would only produce
				// interleaved edits.
				return a.queue.host.StartWorkers(ctx, 1)
			},
			ready: func(context.Context) app.ComponentHealth {
				return componentHealth(true, queueGeneration, "", nil)
			},
			close: func(ctx context.Context) error {
				// Shutdown drains rather than kills: a claimed run is either
				// finished or left suspended with its place in the queue, never
				// abandoned halfway with its lease still held.
				_, err := a.queue.host.Shutdown(ctx)
				return err
			},
		})
	}
	// Skills and MCP are optional: their absence degrades capability, it does not
	// make the agent unable to work.
	if a.skills != nil {
		components = append(components, &lifecycleComponent{
			name: "skills", dependencies: []string{"store"}, criticality: app.Optional,
			ready: func(context.Context) app.ComponentHealth { return componentHealth(true, skillsGeneration, "", nil) },
			close: func(ctx context.Context) error { return a.skills.Close(ctx) },
		})
	}
	if a.mcp != nil {
		components = append(components, &lifecycleComponent{
			name: "mcp", dependencies: []string{"store"}, criticality: app.Optional,
			ready: func(context.Context) app.ComponentHealth { return componentHealth(true, mcpGeneration, "", nil) },
			close: func(ctx context.Context) error { _, err := a.mcp.Close(ctx); return err },
		})
	}
	return components
}

// componentHealth builds a report the lifecycle will accept. A ready component
// must name the generation it is serving, so a readiness probe can distinguish
// "up" from "up and running the composition you expect".
func componentHealth(ready bool, generation, reason string, cause error) app.ComponentHealth {
	health := app.ComponentHealth{Configured: true, Ready: ready, Generation: generation, CheckedAt: time.Now().UTC()}
	if !ready {
		health.Degraded = true
		health.Reason = reason
		if cause != nil {
			health.Reason = reason + ": " + cause.Error()
		}
	}
	return health
}

// lifecycleComponent adapts plain functions to the app.Component port so the
// component graph stays declarative.
type lifecycleComponent struct {
	name         string
	dependencies []string
	criticality  app.Criticality
	revision     uint64
	start        func(context.Context) error
	ready        func(context.Context) app.ComponentHealth
	close        func(context.Context) error
}

func (c *lifecycleComponent) Name() string                 { return c.name }
func (c *lifecycleComponent) Dependencies() []string       { return c.dependencies }
func (c *lifecycleComponent) Criticality() app.Criticality { return c.criticality }

func (c *lifecycleComponent) Start(ctx context.Context, reporter app.Reporter) error {
	if c.start != nil {
		if err := c.start(ctx); err != nil {
			return err
		}
	}
	return reporter.Report(c.Ready(ctx))
}

func (c *lifecycleComponent) Ready(ctx context.Context) app.ComponentHealth {
	health := c.ready(ctx)
	c.revision++
	health.Revision = c.revision
	return health
}

func (c *lifecycleComponent) Close(ctx context.Context) error {
	if c.close == nil {
		return nil
	}
	return c.close(ctx)
}

// runController is the admission gate and the register of in-flight runs.
//
// Admission is a gate rather than a flag on the App because shutdown has to be
// able to refuse new work while letting existing work finish, and those are two
// different states.
type runController struct {
	mu         sync.Mutex
	active     map[string]context.CancelFunc
	available  bool
	closed     bool
	generation uint64
	idle       chan struct{}
}

func newRunController() *runController {
	idle := make(chan struct{})
	close(idle)
	return &runController{active: make(map[string]context.CancelFunc), idle: idle}
}

// admit registers a run and returns a context that shutdown can cancel. The
// second result is false when admission is closed, which is how a shutting-down
// process refuses new work instead of starting it and then killing it.
func (c *runController) admit(ctx context.Context, runKey string) (context.Context, func(), bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ctx, func() {}, false
	}
	runCtx, cancel := context.WithCancel(ctx)
	if len(c.active) == 0 {
		c.idle = make(chan struct{})
	}
	c.active[runKey] = cancel
	return runCtx, func() { c.release(runKey, cancel) }, true
}

// interrupt cancels one in-flight run without touching the rest. It is what a
// queue cancellation reaches: the worker notices its context is done and reports
// the outcome itself, rather than having its bookkeeping torn out from under it.
func (c *runController) interrupt(runKey string) {
	c.mu.Lock()
	cancel := c.active[runKey]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *runController) release(runKey string, cancel context.CancelFunc) {
	cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.active, runKey)
	if len(c.active) == 0 {
		select {
		case <-c.idle:
		default:
			close(c.idle)
		}
	}
}

func (c *runController) SetAvailability(availability app.AdmissionAvailability) app.AdmissionSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation = availability.Generation
	if !c.closed {
		c.available = availability.Available
	}
	return c.snapshotLocked()
}

func (c *runController) StopAdmission() app.AdmissionSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed, c.available = true, false
	return c.snapshotLocked()
}

func (c *runController) snapshotLocked() app.AdmissionSnapshot {
	runs := make([]app.RunRef, 0, len(c.active))
	for key := range c.active {
		runs = append(runs, app.RunRef{RunKey: key})
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunKey < runs[j].RunKey })
	return app.AdmissionSnapshot{Generation: c.generation, Available: c.available, Closed: c.closed, Runs: runs}
}

// Drain waits for the runs that were already admitted. It never cancels them:
// draining means letting work finish, and cancellation is a later, separate phase.
func (c *runController) Drain(ctx context.Context, _ app.AdmissionSnapshot) app.DrainReport {
	c.mu.Lock()
	idle := c.idle
	c.mu.Unlock()
	select {
	case <-idle:
		return app.DrainReport{}
	case <-ctx.Done():
		return app.DrainReport{Remaining: c.remaining(), Err: ctx.Err()}
	}
}

// Cancel interrupts the named runs. The runtime turns a canceled attempt into a
// suspended durable snapshot, so the work is interrupted rather than lost.
func (c *runController) Cancel(_ context.Context, refs []app.RunRef, _ app.CancelReason) app.CancelReport {
	c.mu.Lock()
	for _, ref := range refs {
		if cancel, ok := c.active[ref.RunKey]; ok {
			cancel()
		}
	}
	c.mu.Unlock()
	return app.CancelReport{Remaining: c.remaining()}
}

// Wait blocks until the canceled runs have actually stopped.
func (c *runController) Wait(ctx context.Context, _ []app.RunRef) app.WaitReport {
	c.mu.Lock()
	idle := c.idle
	c.mu.Unlock()
	select {
	case <-idle:
		return app.WaitReport{}
	case <-ctx.Done():
		return app.WaitReport{Remaining: c.remaining(), Err: ctx.Err()}
	}
}

func (c *runController) remaining() []app.RunRef {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked().Runs
}

// leaseRevoker is the shutdown checkpoint hook.
//
// Every durable boundary is already persisted while a run executes, so there is
// nothing extra to write at shutdown. What is worth doing is releasing the lease
// of a run this process will not finish, so another worker can pick it up
// immediately instead of waiting for the lease to expire.
type leaseRevoker struct{ store *SQLiteDurableStore }

func (r *leaseRevoker) Checkpoint(context.Context, []app.RunRef, app.CheckpointReason) app.CheckpointReport {
	return app.CheckpointReport{}
}

func (r *leaseRevoker) Revoke(ctx context.Context, refs []app.RunRef, _ app.RevokeReason) app.RevokeReport {
	report := app.RevokeReport{}
	now := time.Now().UTC()
	for _, ref := range refs {
		snapshot, err := r.store.Load(ctx, durable.RunKey(ref.RunKey))
		if err != nil || snapshot.Terminal() || snapshot.Status != durable.StatusRunning {
			continue
		}
		if _, err := r.store.RevokeLease(ctx, durable.RevokeLeaseRequest{
			RunKey: durable.RunKey(ref.RunKey), ExpectedRevision: snapshot.Revision,
			ExpectedFence: snapshot.FenceToken, Now: now,
		}); err != nil {
			report.Remaining = append(report.Remaining, ref)
			report.Err = err
		}
	}
	return report
}

// outboxFlusher delivers pending reliable events before the process exits.
//
// Losing them would not corrupt state - the outbox keeps them until they are
// acknowledged - but flushing on the way out means a normal shutdown does not
// leave a backlog for whatever starts next.
type outboxFlusher struct {
	app       *App
	publisher event.Publisher
}

func (f *outboxFlusher) Flush(ctx context.Context) app.FlushReport {
	if f.publisher == nil {
		return app.FlushReport{}
	}
	if _, err := f.app.DispatchOutbox(ctx, f.publisher, 100); err != nil {
		return app.FlushReport{Err: err}
	}
	return app.FlushReport{}
}
