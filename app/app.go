// Package app coordinates Agent runtime component readiness and shutdown ordering.
package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type State string

const (
	StateNew           State = "new"
	StateStarting      State = "starting"
	StateReady         State = "ready"
	StateDegraded      State = "degraded"
	StateDraining      State = "draining"
	StateCanceling     State = "canceling"
	StateCheckpointing State = "checkpointing"
	StateWaiting       State = "waiting"
	StateStopping      State = "stopping"
	StateStopped       State = "stopped"
	StateFailed        State = "failed"
)

type Criticality string

const (
	Required Criticality = "required"
	Optional Criticality = "optional"
)

var (
	ErrConfigInvalid      = errors.New("agent/app: config invalid")
	ErrNotReady           = errors.New("agent/app: not ready")
	ErrInvalidTransition  = errors.New("agent/app: invalid lifecycle transition")
	ErrStaleReport        = errors.New("agent/app: stale component health report")
	ErrReporterClosed     = errors.New("agent/app: component health reporter closed")
	ErrStartupFailed      = errors.New("agent/app: startup failed")
	ErrShutdownIncomplete = errors.New("agent/app: shutdown incomplete")
)

type Component interface {
	Name() string
	Dependencies() []string
	Criticality() Criticality
	Start(context.Context, Reporter) error
	Ready(context.Context) ComponentHealth
	Close(context.Context) error
}

type Reporter interface{ Report(ComponentHealth) error }

type ComponentHealth struct {
	Configured bool
	Ready      bool
	Degraded   bool
	Revision   uint64
	Generation string
	Reason     string
	CheckedAt  time.Time
}

type ComponentReport struct {
	Name string
	ComponentHealth
}

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

type Budgets struct {
	Startup         time.Duration
	StartupRollback time.Duration
	Drain           time.Duration
	Cancel          time.Duration
	Checkpoint      time.Duration
	Wait            time.Duration
	Flush           time.Duration
	Close           time.Duration
}

type Config struct {
	Budgets Budgets
	Clock   Clock
}

type RunRef struct{ RunKey string }
type AdmissionAvailability struct {
	Generation uint64
	Available  bool
}
type AdmissionSnapshot struct {
	Generation uint64
	Available  bool
	Closed     bool
	Runs       []RunRef
}
type DrainReport struct {
	Remaining []RunRef
	Err       error
}
type CancelReason string
type CancelReport struct {
	Remaining []RunRef
	Err       error
}
type WaitReport struct {
	Remaining []RunRef
	Err       error
}
type CheckpointReason string
type CheckpointReport struct {
	Remaining []RunRef
	Err       error
}
type RevokeReason string
type RevokeReport struct {
	Remaining []RunRef
	Err       error
}
type FlushReport struct{ Err error }

type RunController interface {
	SetAvailability(AdmissionAvailability) AdmissionSnapshot
	StopAdmission() AdmissionSnapshot
	Drain(context.Context, AdmissionSnapshot) DrainReport
	Cancel(context.Context, []RunRef, CancelReason) CancelReport
	Wait(context.Context, []RunRef) WaitReport
}

type Checkpointer interface {
	Checkpoint(context.Context, []RunRef, CheckpointReason) CheckpointReport
	Revoke(context.Context, []RunRef, RevokeReason) RevokeReport
}

type Flusher interface {
	Flush(context.Context) FlushReport
}

type Dependencies struct {
	Runs        RunController
	Checkpoints Checkpointer
	Events      Flusher
}

type Health struct {
	State      State
	Live       bool
	Ready      bool
	Degraded   bool
	Admission  bool
	Generation uint64
	Components []ComponentReport
	ChangedAt  time.Time
}

type Phase string

const (
	PhaseDrain      Phase = "drain"
	PhaseCancel     Phase = "cancel"
	PhaseCheckpoint Phase = "checkpoint"
	PhaseWait       Phase = "wait"
	PhaseFlush      Phase = "flush"
	PhaseClose      Phase = "close"
)

type PhaseReport struct {
	Phase                 Phase
	StartedAt, FinishedAt time.Time
	Budget                time.Duration
	TimedOut              bool
	Error                 error
	Remaining             []RunRef
}
type ShutdownReport struct {
	StartedAt, FinishedAt  time.Time
	FinalState             State
	Admission              AdmissionSnapshot
	Phases                 []PhaseReport
	Checkpoint             CheckpointReport
	Revoked                RevokeReport
	SkippedClose, Unclosed []string
	LeakedRuns             []RunRef
}

type App struct {
	mu                sync.RWMutex
	transitionMu      sync.Mutex
	config            Config
	dependencies      Dependencies
	components        []Component
	state             State
	health            Health
	reports           map[string]ComponentHealth
	started           map[string]bool
	startupDone       chan struct{}
	startupErr        error
	admissionOnce     sync.Once
	admissionStopped  bool
	admissionSnapshot AdmissionSnapshot
	shutdownOnce      sync.Once
	shutdownReport    ShutdownReport
	shutdownErr       error
}

type componentReporter struct {
	app  *App
	name string
}

func (r componentReporter) Report(health ComponentHealth) error {
	return r.app.report(r.name, health)
}

func New(config Config, dependencies Dependencies, components ...Component) (*App, error) {
	if config.Clock == nil {
		config.Clock = realClock{}
	}
	if err := validateConfig(config, dependencies, components); err != nil {
		return nil, err
	}
	ordered, err := orderComponents(components)
	if err != nil {
		return nil, err
	}
	now := config.Clock.Now()
	return &App{
		config:       config,
		dependencies: dependencies,
		components:   ordered,
		state:        StateNew,
		health:       Health{State: StateNew, Live: true, ChangedAt: now},
		reports:      make(map[string]ComponentHealth, len(ordered)),
		started:      make(map[string]bool, len(ordered)),
		startupDone:  make(chan struct{}),
	}, nil
}

func (a *App) State() State { a.mu.RLock(); defer a.mu.RUnlock(); return a.state }

func (a *App) Health() Health {
	a.mu.RLock()
	defer a.mu.RUnlock()
	health := a.health
	health.Components = append([]ComponentReport(nil), a.health.Components...)
	return health
}

// Start starts components in dependency order and opens admission after all
// required components pass their initial readiness probe.
func (a *App) Start(ctx context.Context) error {
	a.mu.Lock()
	switch a.state {
	case StateNew:
		if a.admissionStopped {
			a.mu.Unlock()
			return fmt.Errorf("%w: admission is permanently stopped", ErrInvalidTransition)
		}
		a.state = StateStarting
		a.health.State = StateStarting
		a.health.ChangedAt = a.config.Clock.Now()
		a.mu.Unlock()
	case StateStarting:
		done := a.startupDone
		a.mu.Unlock()
		select {
		case <-done:
			a.mu.RLock()
			err := a.startupErr
			a.mu.RUnlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	default:
		state := a.state
		a.mu.Unlock()
		return fmt.Errorf("%w: cannot start app in %s state", ErrInvalidTransition, state)
	}
	a.transitionMu.Lock()
	a.mu.Lock()
	availability := AdmissionAvailability{Generation: a.health.Generation + 1, Available: false}
	a.mu.Unlock()
	snapshot := a.dependencies.Runs.SetAvailability(availability)
	a.mu.Lock()
	a.health.Generation = availability.Generation
	a.health.Admission = snapshot.Available && !snapshot.Closed
	a.health.ChangedAt = a.config.Clock.Now()
	a.mu.Unlock()
	a.transitionMu.Unlock()

	startupCtx, cancel := context.WithTimeout(ctx, a.config.Budgets.Startup)
	defer cancel()
	started := make([]Component, 0, len(a.components))
	var startupErr error
	for _, component := range a.components {
		if dependency := a.unreadyDependency(component); dependency != "" {
			err := fmt.Errorf("dependency %s is not ready", dependency)
			if component.Criticality() == Required {
				startupErr = fmt.Errorf("%w: start %s: %w", ErrStartupFailed, component.Name(), err)
				break
			}
			a.recordOptionalFailure(component.Name(), err)
			continue
		}
		reporter := componentReporter{app: a, name: component.Name()}
		if err := component.Start(startupCtx, reporter); err != nil {
			if component.Criticality() == Required {
				startupErr = fmt.Errorf("%w: start %s: %w", ErrStartupFailed, component.Name(), err)
				break
			}
			a.recordOptionalFailure(component.Name(), err)
			continue
		}
		started = append(started, component)
		a.mu.Lock()
		a.started[component.Name()] = true
		a.mu.Unlock()
		health := component.Ready(startupCtx)
		if err := reporter.Report(health); err != nil {
			if component.Criticality() == Required {
				startupErr = fmt.Errorf("%w: readiness %s: %w", ErrStartupFailed, component.Name(), err)
				break
			}
			a.recordOptionalFailure(component.Name(), err)
			continue
		}
		if component.Criticality() == Required && (!health.Configured || !health.Ready) {
			startupErr = fmt.Errorf("%w: required component %s is not ready: %w", ErrStartupFailed, component.Name(), ErrNotReady)
			break
		}
	}
	if startupErr == nil && startupCtx.Err() != nil {
		startupErr = fmt.Errorf("%w: %w", ErrStartupFailed, startupCtx.Err())
	}
	if startupErr != nil {
		a.markStartupFailed()
		startupErr = errors.Join(startupErr, a.rollback(started))
		a.finishStartup(StateFailed, startupErr)
		return startupErr
	}

	a.transitionMu.Lock()
	a.mu.Lock()
	if a.admissionStopped {
		startupErr = fmt.Errorf("%w: admission stopped during startup", ErrStartupFailed)
		a.state = StateFailed
		a.health.State = StateFailed
		a.health.Ready = false
		a.health.Admission = false
		a.health.Degraded = true
		a.health.ChangedAt = a.config.Clock.Now()
		a.startupErr = startupErr
		close(a.startupDone)
		a.mu.Unlock()
		a.transitionMu.Unlock()
		return startupErr
	}
	candidate := a.projectHealthLocked(a.reports)
	a.mu.Unlock()
	snapshot = a.dependencies.Runs.SetAvailability(AdmissionAvailability{Generation: candidate.Generation, Available: candidate.Ready})
	a.mu.Lock()
	if candidate.Degraded {
		candidate.State = StateDegraded
	} else {
		candidate.State = StateReady
	}
	candidate.Admission = snapshot.Available && !snapshot.Closed
	a.publishHealthLocked(candidate)
	a.startupErr = nil
	close(a.startupDone)
	a.mu.Unlock()
	a.transitionMu.Unlock()
	return nil
}

// WaitReady waits for startup to complete and returns the resulting immutable
// health snapshot. Runtime degradation after successful startup does not block.
func (a *App) WaitReady(ctx context.Context) (Health, error) {
	a.mu.RLock()
	state := a.state
	done := a.startupDone
	a.mu.RUnlock()
	if state == StateNew {
		return a.Health(), ErrNotReady
	}
	select {
	case <-done:
		health := a.Health()
		if health.Ready && health.Admission {
			return health, nil
		}
		a.mu.RLock()
		err := a.startupErr
		a.mu.RUnlock()
		if err != nil {
			return health, err
		}
		return health, ErrNotReady
	case <-ctx.Done():
		return a.Health(), ctx.Err()
	}
}

func (a *App) report(name string, report ComponentHealth) error {
	a.transitionMu.Lock()
	defer a.transitionMu.Unlock()

	a.mu.Lock()
	if a.admissionStopped || a.state == StateFailed || a.state == StateDraining || a.state == StateCanceling || a.state == StateCheckpointing || a.state == StateWaiting || a.state == StateStopping || a.state == StateStopped {
		a.mu.Unlock()
		return ErrReporterClosed
	}
	previous := a.reports[name]
	if report.Revision <= previous.Revision {
		a.mu.Unlock()
		return ErrStaleReport
	}
	if err := validateComponentHealth(report); err != nil {
		a.mu.Unlock()
		return err
	}
	reports := cloneReports(a.reports)
	reports[name] = report
	candidate := a.projectHealthLocked(reports)
	availabilityChanged := candidate.Ready != a.health.Ready
	starting := a.state == StateStarting
	a.mu.Unlock()

	var admission AdmissionSnapshot
	if !starting && availabilityChanged {
		admission = a.dependencies.Runs.SetAvailability(AdmissionAvailability{Generation: candidate.Generation, Available: candidate.Ready})
	}

	a.mu.Lock()
	a.reports = reports
	if !starting {
		if availabilityChanged {
			candidate.Admission = admission.Available && !admission.Closed
		} else {
			candidate.Admission = a.health.Admission
		}
	}
	a.publishHealthLocked(candidate)
	a.mu.Unlock()
	return nil
}

func validateComponentHealth(health ComponentHealth) error {
	if health.CheckedAt.IsZero() || (health.Ready && (!health.Configured || health.Generation == "")) {
		return fmt.Errorf("%w: invalid component health", ErrConfigInvalid)
	}
	return nil
}

func cloneReports(reports map[string]ComponentHealth) map[string]ComponentHealth {
	clone := make(map[string]ComponentHealth, len(reports))
	for name, report := range reports {
		clone[name] = report
	}
	return clone
}

func (a *App) projectHealthLocked(reports map[string]ComponentHealth) Health {
	health := a.health
	health.Generation++
	health.Components = make([]ComponentReport, 0, len(reports))
	ready, degraded := true, false
	for _, component := range a.components {
		report, exists := reports[component.Name()]
		if exists {
			health.Components = append(health.Components, ComponentReport{Name: component.Name(), ComponentHealth: report})
		}
		if component.Criticality() == Required && (!exists || !report.Configured || !report.Ready) {
			ready = false
			degraded = true
		}
		if component.Criticality() == Optional && (!exists || !report.Configured || !report.Ready || report.Degraded) {
			degraded = true
		}
		if report.Degraded {
			degraded = true
		}
	}
	health.Ready = ready
	health.Degraded = degraded
	if a.state != StateStarting {
		if degraded {
			health.State = StateDegraded
		} else {
			health.State = StateReady
		}
	}
	health.ChangedAt = a.config.Clock.Now()
	return health
}

func (a *App) publishHealthLocked(health Health) {
	a.health = health
	a.state = health.State
}

func (a *App) recordOptionalFailure(name string, cause error) {
	a.transitionMu.Lock()
	defer a.transitionMu.Unlock()
	a.mu.Lock()
	previous := a.reports[name]
	reports := cloneReports(a.reports)
	reports[name] = ComponentHealth{
		Configured: previous.Configured,
		Revision:   previous.Revision + 1,
		Reason:     cause.Error(),
		CheckedAt:  a.config.Clock.Now(),
	}
	a.reports = reports
	a.publishHealthLocked(a.projectHealthLocked(reports))
	a.mu.Unlock()
}

func (a *App) unreadyDependency(component Component) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, dependency := range component.Dependencies() {
		report, exists := a.reports[dependency]
		if !exists || !report.Configured || !report.Ready {
			return dependency
		}
	}
	return ""
}

func (a *App) rollback(started []Component) error {
	ctx, cancel := context.WithTimeout(context.Background(), a.config.Budgets.StartupRollback)
	defer cancel()
	var errs []error
	for index := len(started) - 1; index >= 0; index-- {
		result := make(chan error, 1)
		go func(component Component) {
			result <- component.Close(ctx)
		}(started[index])
		var err error
		select {
		case err = <-result:
		case <-ctx.Done():
			err = ctx.Err()
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("rollback %s: %w", started[index].Name(), err))
		}
		if err == nil {
			a.mu.Lock()
			delete(a.started, started[index].Name())
			a.mu.Unlock()
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(errs...)
}

func (a *App) markStartupFailed() {
	a.transitionMu.Lock()
	defer a.transitionMu.Unlock()
	a.mu.Lock()
	a.state = StateFailed
	a.health.State = StateFailed
	a.health.Ready = false
	a.health.Admission = false
	a.health.Degraded = true
	a.health.ChangedAt = a.config.Clock.Now()
	a.mu.Unlock()
}

func (a *App) finishStartup(state State, err error) {
	a.transitionMu.Lock()
	defer a.transitionMu.Unlock()
	a.mu.Lock()
	a.state = state
	a.health.State = state
	a.health.Ready = false
	a.health.Admission = false
	a.health.Degraded = true
	a.health.ChangedAt = a.config.Clock.Now()
	a.startupErr = err
	close(a.startupDone)
	a.mu.Unlock()
}

// StopAdmission permanently closes the session-owned admission gate. The
// component transition lock makes the close linear with health reports.
func (a *App) StopAdmission() AdmissionSnapshot {
	a.admissionOnce.Do(func() {
		a.transitionMu.Lock()
		defer a.transitionMu.Unlock()
		a.mu.Lock()
		a.admissionStopped = true
		a.health.Admission = false
		a.health.Ready = false
		a.health.Generation++
		a.health.ChangedAt = a.config.Clock.Now()
		a.mu.Unlock()

		a.admissionSnapshot = cloneAdmissionSnapshot(a.dependencies.Runs.StopAdmission())
	})
	return cloneAdmissionSnapshot(a.admissionSnapshot)
}

// Shutdown executes the bounded shutdown protocol once. Concurrent and later
// callers wait for and receive defensive copies of the same terminal report.
func (a *App) Shutdown(ctx context.Context) (ShutdownReport, error) {
	a.shutdownOnce.Do(func() {
		a.shutdownReport, a.shutdownErr = a.shutdown(ctx)
	})
	return cloneShutdownReport(a.shutdownReport), a.shutdownErr
}

func (a *App) shutdown(ctx context.Context) (ShutdownReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	shutdownCtx := context.WithoutCancel(ctx)
	report := ShutdownReport{StartedAt: a.config.Clock.Now()}

	report.Admission = a.StopAdmission()
	a.mu.RLock()
	startupInFlight := a.state == StateStarting
	a.mu.RUnlock()
	a.setShutdownState(StateDraining)
	if startupInFlight {
		<-a.startupDone
	}

	var errs []error
	drain := a.runPhase(shutdownCtx, PhaseDrain, a.config.Budgets.Drain, func(phaseCtx context.Context) ([]RunRef, error) {
		result := a.dependencies.Runs.Drain(phaseCtx, cloneAdmissionSnapshot(report.Admission))
		return result.Remaining, result.Err
	})
	report.Phases = append(report.Phases, drain)
	appendPhaseError(&errs, drain)
	remaining := cloneRunRefs(drain.Remaining)

	a.setShutdownState(StateCanceling)
	cancelPhase := a.runPhase(shutdownCtx, PhaseCancel, a.config.Budgets.Cancel, func(phaseCtx context.Context) ([]RunRef, error) {
		if len(remaining) == 0 {
			return nil, nil
		}
		result := a.dependencies.Runs.Cancel(phaseCtx, cloneRunRefs(remaining), CancelReason("app shutdown"))
		return result.Remaining, result.Err
	})
	report.Phases = append(report.Phases, cancelPhase)
	appendPhaseError(&errs, cancelPhase)
	remaining = cloneRunRefs(cancelPhase.Remaining)

	a.setShutdownState(StateCheckpointing)
	checkpointPhase := a.runPhase(shutdownCtx, PhaseCheckpoint, a.config.Budgets.Checkpoint, func(phaseCtx context.Context) ([]RunRef, error) {
		if len(remaining) == 0 {
			return nil, nil
		}
		report.Checkpoint = cloneCheckpointReport(a.dependencies.Checkpoints.Checkpoint(phaseCtx, cloneRunRefs(remaining), CheckpointReason("app shutdown")))
		checkpointRemaining := cloneRunRefs(report.Checkpoint.Remaining)
		var revokeErr error
		if len(checkpointRemaining) > 0 && phaseCtx.Err() == nil {
			report.Revoked = cloneRevokeReport(a.dependencies.Checkpoints.Revoke(phaseCtx, checkpointRemaining, RevokeReason("app shutdown safety")))
			revokeErr = report.Revoked.Err
		}
		return checkpointRemaining, errors.Join(report.Checkpoint.Err, revokeErr)
	})
	report.Phases = append(report.Phases, checkpointPhase)
	appendPhaseError(&errs, checkpointPhase)
	remaining = cloneRunRefs(checkpointPhase.Remaining)
	a.setShutdownState(StateWaiting)
	waitPhase := a.runPhase(shutdownCtx, PhaseWait, a.config.Budgets.Wait, func(phaseCtx context.Context) ([]RunRef, error) {
		result := a.dependencies.Runs.Wait(phaseCtx, cloneRunRefs(remaining))
		return result.Remaining, result.Err
	})
	report.Phases = append(report.Phases, waitPhase)
	appendPhaseError(&errs, waitPhase)
	remaining = cloneRunRefs(waitPhase.Remaining)
	unsafe := len(remaining) > 0
	if unsafe {
		report.LeakedRuns = cloneRunRefs(remaining)
		revoked := a.revoke(shutdownCtx, remaining)
		report.Revoked = mergeRevokeReports(report.Revoked, revoked)
		if revoked.Err != nil {
			errs = append(errs, fmt.Errorf("revoke leaked runs: %w", revoked.Err))
		}
	}

	a.setShutdownState(StateStopping)
	flushPhase := a.runPhase(shutdownCtx, PhaseFlush, a.config.Budgets.Flush, func(phaseCtx context.Context) ([]RunRef, error) {
		if unsafe {
			return cloneRunRefs(remaining), errors.New("event producers did not settle")
		}
		return nil, a.dependencies.Events.Flush(phaseCtx).Err
	})
	report.Phases = append(report.Phases, flushPhase)
	appendPhaseError(&errs, flushPhase)

	closePhase := a.runPhase(shutdownCtx, PhaseClose, a.config.Budgets.Close, func(phaseCtx context.Context) ([]RunRef, error) {
		if unsafe {
			report.SkippedClose = a.startedComponentNames(true)
			report.Unclosed = append([]string(nil), report.SkippedClose...)
			return cloneRunRefs(remaining), errors.New("component close skipped while runs remain")
		}
		return nil, a.closeComponents(phaseCtx, &report)
	})
	report.Phases = append(report.Phases, closePhase)
	appendPhaseError(&errs, closePhase)

	if len(errs) == 0 {
		report.FinalState = StateStopped
	} else {
		report.FinalState = StateFailed
		errs = append([]error{ErrShutdownIncomplete}, errs...)
	}
	report.FinishedAt = a.config.Clock.Now()
	a.setShutdownState(report.FinalState)
	return cloneShutdownReport(report), errors.Join(errs...)
}

func (a *App) runPhase(ctx context.Context, phase Phase, budget time.Duration, run func(context.Context) ([]RunRef, error)) PhaseReport {
	phaseReport := PhaseReport{Phase: phase, StartedAt: a.config.Clock.Now(), Budget: budget}
	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	remaining, err := run(phaseCtx)
	phaseErr := phaseCtx.Err()
	cancel()
	phaseReport.FinishedAt = a.config.Clock.Now()
	phaseReport.Remaining = cloneRunRefs(remaining)
	phaseReport.Error = err
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(phaseErr, context.DeadlineExceeded) {
		phaseReport.TimedOut = true
		phaseReport.Error = errors.Join(err, phaseErr)
	}
	return phaseReport
}

func (a *App) revoke(ctx context.Context, runs []RunRef) RevokeReport {
	revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.config.Budgets.Checkpoint)
	defer cancel()
	return a.dependencies.Checkpoints.Revoke(revokeCtx, cloneRunRefs(runs), RevokeReason("app shutdown safety"))
}

func (a *App) closeComponents(ctx context.Context, report *ShutdownReport) error {
	var errs []error
	for index := len(a.components) - 1; index >= 0; index-- {
		component := a.components[index]
		a.mu.RLock()
		started := a.started[component.Name()]
		a.mu.RUnlock()
		if !started {
			continue
		}
		if err := component.Close(ctx); err != nil {
			report.Unclosed = append(report.Unclosed, component.Name())
			errs = append(errs, fmt.Errorf("close %s: %w", component.Name(), err))
			continue
		}
		a.mu.Lock()
		delete(a.started, component.Name())
		a.mu.Unlock()
	}
	return errors.Join(errs...)
}

func (a *App) startedComponentNames(reverse bool) []string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	names := make([]string, 0, len(a.started))
	if reverse {
		for index := len(a.components) - 1; index >= 0; index-- {
			if a.started[a.components[index].Name()] {
				names = append(names, a.components[index].Name())
			}
		}
		return names
	}
	for _, component := range a.components {
		if a.started[component.Name()] {
			names = append(names, component.Name())
		}
	}
	return names
}

func (a *App) setShutdownState(state State) {
	a.mu.Lock()
	if state == StateDraining {
		a.admissionStopped = true
	}
	a.state = state
	a.health.State = state
	a.health.Ready = false
	a.health.Admission = false
	a.health.Generation++
	a.health.ChangedAt = a.config.Clock.Now()
	a.health.Live = state != StateStopped
	if state == StateFailed {
		a.health.Degraded = true
	}
	a.mu.Unlock()
}

func appendPhaseError(errs *[]error, report PhaseReport) {
	if report.Error != nil {
		*errs = append(*errs, fmt.Errorf("%s phase: %w", report.Phase, report.Error))
	}
}

func cloneRunRefs(runs []RunRef) []RunRef { return append([]RunRef(nil), runs...) }

func cloneAdmissionSnapshot(snapshot AdmissionSnapshot) AdmissionSnapshot {
	snapshot.Runs = cloneRunRefs(snapshot.Runs)
	return snapshot
}

func cloneCheckpointReport(report CheckpointReport) CheckpointReport {
	report.Remaining = cloneRunRefs(report.Remaining)
	return report
}

func cloneRevokeReport(report RevokeReport) RevokeReport {
	report.Remaining = cloneRunRefs(report.Remaining)
	return report
}

func mergeRevokeReports(first, second RevokeReport) RevokeReport {
	return RevokeReport{
		Remaining: cloneRunRefs(second.Remaining),
		Err:       errors.Join(first.Err, second.Err),
	}
}

func cloneShutdownReport(report ShutdownReport) ShutdownReport {
	report.Admission = cloneAdmissionSnapshot(report.Admission)
	report.Phases = append([]PhaseReport(nil), report.Phases...)
	for index := range report.Phases {
		report.Phases[index].Remaining = cloneRunRefs(report.Phases[index].Remaining)
	}
	report.Checkpoint = cloneCheckpointReport(report.Checkpoint)
	report.Revoked = cloneRevokeReport(report.Revoked)
	report.SkippedClose = append([]string(nil), report.SkippedClose...)
	report.Unclosed = append([]string(nil), report.Unclosed...)
	report.LeakedRuns = cloneRunRefs(report.LeakedRuns)
	return report
}

func validateConfig(config Config, dependencies Dependencies, components []Component) error {
	budgets := []time.Duration{config.Budgets.Startup, config.Budgets.StartupRollback, config.Budgets.Drain, config.Budgets.Cancel, config.Budgets.Checkpoint, config.Budgets.Wait, config.Budgets.Flush, config.Budgets.Close}
	for _, budget := range budgets {
		if budget <= 0 {
			return fmt.Errorf("%w: budgets must be positive", ErrConfigInvalid)
		}
	}
	if dependencies.Runs == nil || dependencies.Checkpoints == nil || dependencies.Events == nil {
		return fmt.Errorf("%w: dependencies are required", ErrConfigInvalid)
	}
	if len(components) == 0 {
		return fmt.Errorf("%w: components are required", ErrConfigInvalid)
	}
	return nil
}

func orderComponents(components []Component) ([]Component, error) {
	byName := make(map[string]Component, len(components))
	for _, component := range components {
		if component == nil || component.Name() == "" {
			return nil, fmt.Errorf("%w: component name is required", ErrConfigInvalid)
		}
		if component.Criticality() != Required && component.Criticality() != Optional {
			return nil, fmt.Errorf("%w: invalid criticality for %s", ErrConfigInvalid, component.Name())
		}
		if _, exists := byName[component.Name()]; exists {
			return nil, fmt.Errorf("%w: duplicate component %s", ErrConfigInvalid, component.Name())
		}
		byName[component.Name()] = component
	}
	for name, component := range byName {
		for _, dependency := range component.Dependencies() {
			if dependency == name || byName[dependency] == nil {
				return nil, fmt.Errorf("%w: unknown dependency %s for %s", ErrConfigInvalid, dependency, name)
			}
		}
	}
	var ordered []Component
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("%w: component dependency cycle at %s", ErrConfigInvalid, name)
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		dependencies := append([]string(nil), byName[name].Dependencies()...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[name], visited[name] = false, true
		ordered = append(ordered, byName[name])
		return nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return append([]Component(nil), ordered...), nil
}
