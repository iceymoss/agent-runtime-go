package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

type testClock struct{ now time.Time }

func (c testClock) Now() time.Time { return c.now }

type testComponent struct {
	name        string
	deps        []string
	criticality Criticality
	start       func(context.Context, Reporter) error
	ready       func(context.Context) ComponentHealth
	close       func(context.Context) error
}

func (c *testComponent) Name() string             { return c.name }
func (c *testComponent) Dependencies() []string   { return append([]string(nil), c.deps...) }
func (c *testComponent) Criticality() Criticality { return c.criticality }
func (c *testComponent) Start(ctx context.Context, reporter Reporter) error {
	if c.start != nil {
		return c.start(ctx, reporter)
	}
	return nil
}
func (c *testComponent) Ready(ctx context.Context) ComponentHealth {
	if c.ready != nil {
		return c.ready(ctx)
	}
	return readyHealth(1)
}
func (c *testComponent) Close(ctx context.Context) error {
	if c.close != nil {
		return c.close(ctx)
	}
	return nil
}

func readyHealth(revision uint64) ComponentHealth {
	return ComponentHealth{Configured: true, Ready: true, Revision: revision, Generation: "generation", CheckedAt: time.Unix(int64(revision), 0)}
}

type noopDeps struct{}

func (noopDeps) SetAvailability(availability AdmissionAvailability) AdmissionSnapshot {
	return AdmissionSnapshot{Generation: availability.Generation, Available: availability.Available}
}
func (noopDeps) StopAdmission() AdmissionSnapshot                            { return AdmissionSnapshot{} }
func (noopDeps) Drain(context.Context, AdmissionSnapshot) DrainReport        { return DrainReport{} }
func (noopDeps) Cancel(context.Context, []RunRef, CancelReason) CancelReport { return CancelReport{} }
func (noopDeps) Wait(context.Context, []RunRef) WaitReport                   { return WaitReport{} }
func (noopDeps) Checkpoint(context.Context, []RunRef, CheckpointReason) CheckpointReport {
	return CheckpointReport{}
}
func (noopDeps) Revoke(context.Context, []RunRef, RevokeReason) RevokeReport { return RevokeReport{} }
func (noopDeps) Flush(context.Context) FlushReport                           { return FlushReport{} }

func validConfig() Config {
	return Config{Budgets: Budgets{time.Second, time.Second, time.Second, time.Second, time.Second, time.Second, time.Second, time.Second}, Clock: testClock{now: time.Unix(100, 0)}}
}
func validDeps() Dependencies {
	dep := noopDeps{}
	return Dependencies{Runs: dep, Checkpoints: dep, Events: dep}
}

type recordingRuns struct {
	mu           sync.Mutex
	app          *App
	availability []AdmissionAvailability
}

func (r *recordingRuns) SetAvailability(availability AdmissionAvailability) AdmissionSnapshot {
	if r.app != nil {
		_ = r.app.State()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.availability = append(r.availability, availability)
	return AdmissionSnapshot{Generation: availability.Generation, Available: availability.Available}
}
func (*recordingRuns) StopAdmission() AdmissionSnapshot { return AdmissionSnapshot{} }
func (*recordingRuns) Drain(context.Context, AdmissionSnapshot) DrainReport {
	return DrainReport{}
}
func (*recordingRuns) Cancel(context.Context, []RunRef, CancelReason) CancelReport {
	return CancelReport{}
}
func (*recordingRuns) Wait(context.Context, []RunRef) WaitReport { return WaitReport{} }

func depsWithRuns(runs RunController) Dependencies {
	dep := noopDeps{}
	return Dependencies{Runs: runs, Checkpoints: dep, Events: dep}
}

func TestNewValidatesAndOrdersComponents(t *testing.T) {
	db := &testComponent{name: "db", criticality: Required}
	events := &testComponent{name: "events", deps: []string{"db"}, criticality: Required}
	app, err := New(validConfig(), validDeps(), events, db)
	if err != nil {
		t.Fatal(err)
	}
	if app.State() != StateNew || !reflect.DeepEqual([]string{app.components[0].Name(), app.components[1].Name()}, []string{"db", "events"}) {
		t.Fatalf("app = %+v", app)
	}
	health := app.Health()
	health.Components = append(health.Components, ComponentReport{Name: "changed"})
	if len(app.Health().Components) != 0 {
		t.Fatal("Health returned aliased components")
	}
}

func TestNewRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		config     Config
		deps       Dependencies
		components []Component
	}{
		{name: "missing dependency ports", config: validConfig(), components: []Component{&testComponent{name: "db", criticality: Required}}},
		{name: "no components", config: validConfig(), deps: validDeps()},
		{name: "duplicate", config: validConfig(), deps: validDeps(), components: []Component{&testComponent{name: "db", criticality: Required}, &testComponent{name: "db", criticality: Required}}},
		{name: "unknown dependency", config: validConfig(), deps: validDeps(), components: []Component{&testComponent{name: "db", deps: []string{"missing"}, criticality: Required}}},
		{name: "cycle", config: validConfig(), deps: validDeps(), components: []Component{&testComponent{name: "a", deps: []string{"b"}, criticality: Required}, &testComponent{name: "b", deps: []string{"a"}, criticality: Required}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.config, tt.deps, tt.components...); !errors.Is(err, ErrConfigInvalid) {
				t.Fatalf("New() error = %v", err)
			}
		})
	}
}

func TestStartUsesDependencyOrderAndReadinessBarrier(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	component := func(name string, deps ...string) *testComponent {
		return &testComponent{
			name: name, deps: deps, criticality: Required,
			start: func(context.Context, Reporter) error {
				mu.Lock()
				calls = append(calls, "start:"+name)
				mu.Unlock()
				return nil
			},
			ready: func(context.Context) ComponentHealth {
				mu.Lock()
				calls = append(calls, "ready:"+name)
				mu.Unlock()
				return readyHealth(1)
			},
		}
	}
	runs := &recordingRuns{}
	a, err := New(validConfig(), depsWithRuns(runs), component("api", "store"), component("store", "db"), component("db"))
	if err != nil {
		t.Fatal(err)
	}
	runs.app = a
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"start:db", "ready:db", "start:store", "ready:store", "start:api", "ready:api"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	health, err := a.WaitReady(context.Background())
	if err != nil || health.State != StateReady || !health.Ready || !health.Admission || health.Degraded {
		t.Fatalf("WaitReady() = %+v, %v", health, err)
	}
	runs.mu.Lock()
	availability := append([]AdmissionAvailability(nil), runs.availability...)
	runs.mu.Unlock()
	if len(availability) != 2 || availability[0].Available || !availability[1].Available || availability[1].Generation <= availability[0].Generation {
		t.Fatalf("availability = %+v", availability)
	}
}

func TestStartRequiredFailureRollsBackInReverseOrder(t *testing.T) {
	startErr := errors.New("start failed")
	closeErr := errors.New("close failed")
	var closed []string
	component := func(name string, deps ...string) *testComponent {
		return &testComponent{name: name, deps: deps, criticality: Required, close: func(context.Context) error {
			closed = append(closed, name)
			if name == "db" {
				return closeErr
			}
			return nil
		}}
	}
	db := component("db")
	store := component("store", "db")
	api := component("api", "store")
	api.start = func(context.Context, Reporter) error { return startErr }
	a, err := New(validConfig(), validDeps(), api, db, store)
	if err != nil {
		t.Fatal(err)
	}
	err = a.Start(context.Background())
	if !errors.Is(err, ErrStartupFailed) || !errors.Is(err, startErr) || !errors.Is(err, closeErr) {
		t.Fatalf("Start() error = %v", err)
	}
	if !reflect.DeepEqual(closed, []string{"store", "db"}) {
		t.Fatalf("closed = %v", closed)
	}
	if a.State() != StateFailed {
		t.Fatalf("state = %s", a.State())
	}
	if _, err := a.WaitReady(context.Background()); !errors.Is(err, ErrStartupFailed) {
		t.Fatalf("WaitReady() error = %v", err)
	}
}

func TestStartRollbackUsesIndependentBoundedContext(t *testing.T) {
	config := validConfig()
	config.Budgets.StartupRollback = 20 * time.Millisecond
	closed := make(chan struct{})
	db := &testComponent{name: "db", criticality: Required, close: func(ctx context.Context) error {
		<-ctx.Done()
		close(closed)
		return ctx.Err()
	}}
	failing := &testComponent{name: "api", deps: []string{"db"}, criticality: Required, start: func(context.Context, Reporter) error {
		return errors.New("boom")
	}}
	a, err := New(config, validDeps(), failing, db)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := a.Start(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("rollback took %v", elapsed)
	}
	select {
	case <-closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("rollback close did not observe its context deadline")
	}
}

func TestOptionalFailureStartsDegradedAndKeepsAdmissionOpen(t *testing.T) {
	optionalErr := errors.New("optional unavailable")
	required := &testComponent{name: "db", criticality: Required}
	optional := &testComponent{name: "mcp", deps: []string{"db"}, criticality: Optional, start: func(context.Context, Reporter) error {
		return optionalErr
	}}
	runs := &recordingRuns{}
	a, err := New(validConfig(), depsWithRuns(runs), optional, required)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	health, err := a.WaitReady(context.Background())
	if err != nil || health.State != StateDegraded || !health.Ready || !health.Degraded || !health.Admission {
		t.Fatalf("health = %+v, error = %v", health, err)
	}
	if len(health.Components) != 2 || health.Components[1].Name != "mcp" || health.Components[1].Reason != optionalErr.Error() {
		t.Fatalf("components = %+v", health.Components)
	}
}

func TestReporterRejectsStaleAndTerminalReports(t *testing.T) {
	var reporter Reporter
	component := &testComponent{name: "db", criticality: Required, start: func(_ context.Context, r Reporter) error {
		reporter = r
		return nil
	}}
	a, err := New(validConfig(), validDeps(), component)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := a.Health()
	if err := reporter.Report(readyHealth(1)); !errors.Is(err, ErrStaleReport) {
		t.Fatalf("stale report error = %v", err)
	}
	if !reflect.DeepEqual(a.Health(), before) {
		t.Fatal("stale report changed health")
	}
	a.mu.Lock()
	a.state = StateFailed
	a.health.State = StateFailed
	a.mu.Unlock()
	if err := reporter.Report(readyHealth(2)); !errors.Is(err, ErrReporterClosed) {
		t.Fatalf("terminal report error = %v", err)
	}
}

func TestRuntimeReportsUpdateAdmissionAndRecover(t *testing.T) {
	var reporter Reporter
	component := &testComponent{name: "db", criticality: Required, start: func(_ context.Context, r Reporter) error {
		reporter = r
		return nil
	}}
	runs := &recordingRuns{}
	a, err := New(validConfig(), depsWithRuns(runs), component)
	if err != nil {
		t.Fatal(err)
	}
	runs.app = a
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	down := ComponentHealth{Configured: true, Degraded: true, Revision: 2, Reason: "lost generation", CheckedAt: time.Unix(2, 0)}
	if err := reporter.Report(down); err != nil {
		t.Fatal(err)
	}
	if health := a.Health(); health.State != StateDegraded || health.Ready || health.Admission || !health.Degraded {
		t.Fatalf("degraded health = %+v", health)
	}
	if err := reporter.Report(readyHealth(3)); err != nil {
		t.Fatal(err)
	}
	if health := a.Health(); health.State != StateReady || !health.Ready || !health.Admission || health.Degraded {
		t.Fatalf("recovered health = %+v", health)
	}
}

func TestWaitReadyBeforeStartAndCallerContext(t *testing.T) {
	readyEntered := make(chan struct{})
	release := make(chan struct{})
	component := &testComponent{name: "db", criticality: Required, ready: func(context.Context) ComponentHealth {
		close(readyEntered)
		<-release
		return readyHealth(1)
	}}
	a, err := New(validConfig(), validDeps(), component)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitReady(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("WaitReady before Start error = %v", err)
	}
	startDone := make(chan error, 1)
	go func() { startDone <- a.Start(context.Background()) }()
	<-readyEntered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitReady context error = %v", err)
	}
	close(release)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentReportsHaveStrictMonotonicRevision(t *testing.T) {
	var reporter Reporter
	component := &testComponent{name: "db", criticality: Required, start: func(_ context.Context, r Reporter) error {
		reporter = r
		return nil
	}}
	a, err := New(validConfig(), validDeps(), component)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	const reports = 100
	results := make(chan error, reports)
	var wg sync.WaitGroup
	for revision := uint64(2); revision < reports+2; revision++ {
		wg.Add(1)
		go func(revision uint64) {
			defer wg.Done()
			results <- reporter.Report(readyHealth(revision))
		}(revision)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
			continue
		}
		if !errors.Is(err, ErrStaleReport) {
			t.Fatalf("report error = %v", err)
		}
	}
	if accepted == 0 {
		t.Fatal("no report was accepted")
	}
	health := a.Health()
	if got := health.Components[0].Revision; got != reports+1 {
		t.Fatalf("final revision = %d, want %d", got, reports+1)
	}
}

type shutdownDeps struct {
	mu             sync.Mutex
	calls          []string
	stop           AdmissionSnapshot
	drain          func(context.Context, AdmissionSnapshot) DrainReport
	cancel         func(context.Context, []RunRef, CancelReason) CancelReport
	checkpoint     func(context.Context, []RunRef, CheckpointReason) CheckpointReport
	revoke         func(context.Context, []RunRef, RevokeReason) RevokeReport
	wait           func(context.Context, []RunRef) WaitReport
	flush          func(context.Context) FlushReport
	stopCalls      int
	shutdownActive int
}

func (d *shutdownDeps) record(call string) {
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
}

func (d *shutdownDeps) SetAvailability(availability AdmissionAvailability) AdmissionSnapshot {
	return AdmissionSnapshot{Generation: availability.Generation, Available: availability.Available}
}

func (d *shutdownDeps) StopAdmission() AdmissionSnapshot {
	d.mu.Lock()
	d.calls = append(d.calls, "stop-admission")
	d.stopCalls++
	d.mu.Unlock()
	return cloneAdmissionSnapshot(d.stop)
}

func (d *shutdownDeps) Drain(ctx context.Context, snapshot AdmissionSnapshot) DrainReport {
	d.record("drain")
	if d.drain != nil {
		return d.drain(ctx, snapshot)
	}
	return DrainReport{}
}

func (d *shutdownDeps) Cancel(ctx context.Context, runs []RunRef, reason CancelReason) CancelReport {
	d.record("cancel")
	if d.cancel != nil {
		return d.cancel(ctx, runs, reason)
	}
	return CancelReport{}
}

func (d *shutdownDeps) Checkpoint(ctx context.Context, runs []RunRef, reason CheckpointReason) CheckpointReport {
	d.record("checkpoint")
	if d.checkpoint != nil {
		return d.checkpoint(ctx, runs, reason)
	}
	return CheckpointReport{}
}

func (d *shutdownDeps) Revoke(ctx context.Context, runs []RunRef, reason RevokeReason) RevokeReport {
	d.record("revoke")
	if d.revoke != nil {
		return d.revoke(ctx, runs, reason)
	}
	return RevokeReport{}
}

func (d *shutdownDeps) Wait(ctx context.Context, runs []RunRef) WaitReport {
	d.record("wait")
	if d.wait != nil {
		return d.wait(ctx, runs)
	}
	return WaitReport{}
}

func (d *shutdownDeps) Flush(ctx context.Context) FlushReport {
	d.record("flush")
	if d.flush != nil {
		return d.flush(ctx)
	}
	return FlushReport{}
}

func shutdownDependencies(deps *shutdownDeps) Dependencies {
	return Dependencies{Runs: deps, Checkpoints: deps, Events: deps}
}

func startShutdownApp(t *testing.T, deps *shutdownDeps, components ...Component) *App {
	t.Helper()
	a, err := New(validConfig(), shutdownDependencies(deps), components...)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	deps.mu.Lock()
	deps.calls = nil
	deps.mu.Unlock()
	return a
}

func TestShutdownExecutesOrderedPhasesAndReverseClose(t *testing.T) {
	run := RunRef{RunKey: "run-1"}
	deps := &shutdownDeps{
		stop: AdmissionSnapshot{Generation: 7, Closed: true, Runs: []RunRef{run}},
		drain: func(context.Context, AdmissionSnapshot) DrainReport {
			return DrainReport{Remaining: []RunRef{run}}
		},
		cancel: func(context.Context, []RunRef, CancelReason) CancelReport {
			return CancelReport{Remaining: []RunRef{run}}
		},
		checkpoint: func(context.Context, []RunRef, CheckpointReason) CheckpointReport {
			return CheckpointReport{Remaining: []RunRef{run}}
		},
		wait: func(context.Context, []RunRef) WaitReport { return WaitReport{} },
	}
	component := func(name string, dependencies ...string) Component {
		return &testComponent{name: name, deps: dependencies, criticality: Required, close: func(context.Context) error {
			deps.record("close:" + name)
			return nil
		}}
	}
	a := startShutdownApp(t, deps, component("api", "store"), component("db"), component("store", "db"))

	report, err := a.Shutdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{"stop-admission", "drain", "cancel", "checkpoint", "revoke", "wait", "flush", "close:api", "close:store", "close:db"}
	deps.mu.Lock()
	calls := append([]string(nil), deps.calls...)
	deps.mu.Unlock()
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
	if report.FinalState != StateStopped || a.State() != StateStopped {
		t.Fatalf("state = %s, report = %+v", a.State(), report)
	}
	wantPhases := []Phase{PhaseDrain, PhaseCancel, PhaseCheckpoint, PhaseWait, PhaseFlush, PhaseClose}
	for index, phase := range report.Phases {
		if phase.Phase != wantPhases[index] || phase.Budget <= 0 || phase.StartedAt.IsZero() || phase.FinishedAt.IsZero() {
			t.Fatalf("phase %d = %+v", index, phase)
		}
	}
}

func TestShutdownUsesIndependentContextsWhenCallerCanceled(t *testing.T) {
	config := validConfig()
	config.Budgets.Drain = 15 * time.Millisecond
	config.Budgets.Cancel = 40 * time.Millisecond
	run := RunRef{RunKey: "run-1"}
	var cancelLifetime time.Duration
	deps := &shutdownDeps{
		stop: AdmissionSnapshot{Closed: true, Runs: []RunRef{run}},
		drain: func(ctx context.Context, _ AdmissionSnapshot) DrainReport {
			<-ctx.Done()
			return DrainReport{Remaining: []RunRef{run}, Err: ctx.Err()}
		},
		cancel: func(ctx context.Context, _ []RunRef, _ CancelReason) CancelReport {
			started := time.Now()
			if ctx.Err() != nil {
				return CancelReport{Err: ctx.Err()}
			}
			<-ctx.Done()
			cancelLifetime = time.Since(started)
			return CancelReport{Err: ctx.Err()}
		},
	}
	a, err := New(config, shutdownDependencies(deps), &testComponent{name: "db", criticality: Required})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), struct{}{}, "trace"))
	cancel()
	report, err := a.Shutdown(ctx)
	if !errors.Is(err, ErrShutdownIncomplete) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if !report.Phases[0].TimedOut || !report.Phases[1].TimedOut {
		t.Fatalf("phases = %+v", report.Phases)
	}
	if cancelLifetime < config.Budgets.Cancel/2 {
		t.Fatalf("cancel phase lifetime = %v, budget = %v", cancelLifetime, config.Budgets.Cancel)
	}
}

func TestShutdownContinuesAfterErrorsAndSkipsUnsafeClose(t *testing.T) {
	run := RunRef{RunKey: "leaked"}
	drainErr := errors.New("drain failed")
	checkpointErr := errors.New("checkpoint failed")
	revokeErr := errors.New("revoke failed")
	deps := &shutdownDeps{
		stop: AdmissionSnapshot{Closed: true, Runs: []RunRef{run}},
		drain: func(context.Context, AdmissionSnapshot) DrainReport {
			return DrainReport{Remaining: []RunRef{run}, Err: drainErr}
		},
		cancel: func(context.Context, []RunRef, CancelReason) CancelReport {
			return CancelReport{Remaining: []RunRef{run}}
		},
		checkpoint: func(context.Context, []RunRef, CheckpointReason) CheckpointReport {
			return CheckpointReport{Remaining: []RunRef{run}, Err: checkpointErr}
		},
		revoke: func(context.Context, []RunRef, RevokeReason) RevokeReport {
			return RevokeReport{Remaining: []RunRef{run}, Err: revokeErr}
		},
		wait: func(context.Context, []RunRef) WaitReport { return WaitReport{Remaining: []RunRef{run}} },
	}
	closed := false
	a := startShutdownApp(t, deps, &testComponent{name: "db", criticality: Required, close: func(context.Context) error {
		closed = true
		return nil
	}})
	report, err := a.Shutdown(context.Background())
	if !errors.Is(err, ErrShutdownIncomplete) || !errors.Is(err, drainErr) || !errors.Is(err, checkpointErr) || !errors.Is(err, revokeErr) {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if closed || report.FinalState != StateFailed || !reflect.DeepEqual(report.LeakedRuns, []RunRef{run}) || !reflect.DeepEqual(report.SkippedClose, []string{"db"}) || !reflect.DeepEqual(report.Unclosed, []string{"db"}) {
		t.Fatalf("report = %+v, closed = %v", report, closed)
	}
	deps.mu.Lock()
	calls := append([]string(nil), deps.calls...)
	deps.mu.Unlock()
	if !reflect.DeepEqual(calls, []string{"stop-admission", "drain", "cancel", "checkpoint", "revoke", "wait", "revoke"}) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestShutdownContinuesReverseCloseAfterComponentError(t *testing.T) {
	closeErr := errors.New("api close failed")
	deps := &shutdownDeps{}
	component := func(name string, dependencies ...string) Component {
		return &testComponent{name: name, deps: dependencies, criticality: Required, close: func(context.Context) error {
			deps.record("close:" + name)
			if name == "api" {
				return closeErr
			}
			return nil
		}}
	}
	a := startShutdownApp(t, deps, component("api", "store"), component("db"), component("store", "db"))
	report, err := a.Shutdown(context.Background())
	if !errors.Is(err, ErrShutdownIncomplete) || !errors.Is(err, closeErr) {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if report.FinalState != StateFailed || !reflect.DeepEqual(report.Unclosed, []string{"api"}) {
		t.Fatalf("report = %+v", report)
	}
	deps.mu.Lock()
	calls := append([]string(nil), deps.calls...)
	deps.mu.Unlock()
	wantSuffix := []string{"close:api", "close:store", "close:db"}
	if !reflect.DeepEqual(calls[len(calls)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("calls = %v", calls)
	}
}

func TestShutdownConcurrentIdempotentAndReportsAreDefensiveCopies(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	deps := &shutdownDeps{drain: func(context.Context, AdmissionSnapshot) DrainReport {
		close(entered)
		<-release
		return DrainReport{}
	}}
	a := startShutdownApp(t, deps, &testComponent{name: "db", criticality: Required})

	type result struct {
		report ShutdownReport
		err    error
	}
	results := make(chan result, 16)
	for range 16 {
		go func() {
			report, err := a.Shutdown(context.Background())
			results <- result{report: report, err: err}
		}()
	}
	<-entered
	close(release)
	var first ShutdownReport
	for index := 0; index < 16; index++ {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if index == 0 {
			first = result.report
		} else if !reflect.DeepEqual(result.report, first) {
			t.Fatalf("report %d differs: %+v != %+v", index, result.report, first)
		}
	}
	deps.mu.Lock()
	stopCalls := deps.stopCalls
	deps.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("StopAdmission calls = %d", stopCalls)
	}

	first.Phases[0].Remaining = append(first.Phases[0].Remaining, RunRef{RunKey: "mutated"})
	first.Admission.Runs = append(first.Admission.Runs, RunRef{RunKey: "mutated"})
	again, err := a.Shutdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Phases[0].Remaining) != 0 || len(again.Admission.Runs) != 0 {
		t.Fatalf("aliased report = %+v", again)
	}
}

func TestStopAdmissionIsIdempotentAndClosesReporter(t *testing.T) {
	var reporter Reporter
	deps := &shutdownDeps{stop: AdmissionSnapshot{Generation: 9, Closed: true, Runs: []RunRef{{RunKey: "run"}}}}
	a := startShutdownApp(t, deps, &testComponent{name: "db", criticality: Required, start: func(_ context.Context, value Reporter) error {
		reporter = value
		return nil
	}})

	first := a.StopAdmission()
	first.Runs[0].RunKey = "mutated"
	second := a.StopAdmission()
	if second.Runs[0].RunKey != "run" {
		t.Fatalf("snapshot = %+v", second)
	}
	deps.mu.Lock()
	stopCalls := deps.stopCalls
	deps.mu.Unlock()
	if stopCalls != 1 {
		t.Fatalf("StopAdmission calls = %d", stopCalls)
	}
	if err := reporter.Report(readyHealth(2)); !errors.Is(err, ErrReporterClosed) {
		t.Fatalf("Report() error = %v", err)
	}
	if err := a.Start(context.Background()); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Start() after StopAdmission error = %v", err)
	}
}
