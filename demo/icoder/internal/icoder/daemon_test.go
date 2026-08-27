package icoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/app"
	"github.com/iceymoss/agent-runtime-go/event"
)

// flushPublisher captures whatever the shutdown flush delivers.
type flushPublisher struct{ delivered []event.Envelope }

func (p *flushPublisher) Publish(_ context.Context, envelope event.Envelope) error {
	p.delivered = append(p.delivered, envelope)
	return nil
}

func newDaemonApp(t *testing.T) *App {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		writeSSEResponse(t, response,
			`{"model":"fixture","choices":[{"delta":{"content":"done"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			`[DONE]`,
		)
	}))
	t.Cleanup(server.Close)
	application, err := NewApp(context.Background(), Config{
		APIKey: "fixture-key", BaseURL: server.URL, Model: "fixture",
		Workspace: t.TempDir(), Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "daemon",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := application.Close(context.Background()); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Error(err)
		}
	})
	return application
}

func TestDaemonReportsReadinessPerComponent(t *testing.T) {
	application := newDaemonApp(t)
	daemon, err := NewDaemon(application, DaemonOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := daemon.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	health, err := daemon.WaitReady(ctx)
	if err != nil {
		t.Fatalf("WaitReady() error = %v, health = %+v", err, health)
	}
	if health.State != app.StateReady || !health.Ready || !health.Admission {
		t.Fatalf("health = %+v", health)
	}
	required := map[string]bool{"store": false, "runtime": false, "tools": false, "events": false}
	for _, component := range health.Components {
		if _, ok := required[component.Name]; ok {
			required[component.Name] = component.Ready
		}
	}
	for name, ready := range required {
		if !ready {
			t.Fatalf("required component %q is not ready: %+v", name, health.Components)
		}
	}
	// The runtime component must report which generation it is serving, so a
	// readiness probe can tell two deployments apart.
	for _, component := range health.Components {
		if component.Name == "runtime" && component.Generation == "" {
			t.Fatal("runtime component reported no generation")
		}
	}
}

func TestDaemonShutdownClosesAdmissionAndRunsEveryPhase(t *testing.T) {
	application := newDaemonApp(t)
	publisher := &flushPublisher{}
	daemon, err := NewDaemon(application, DaemonOptions{
		Budgets:   app.Budgets{Drain: 2 * time.Second, Cancel: time.Second, Wait: time.Second},
		Publisher: publisher,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := daemon.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	// A completed run leaves reliable events behind, which is what the flush phase
	// is supposed to deliver before the process exits.
	if _, err := application.Run(ctx, "say done", nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	report, err := daemon.Shutdown(ctx)
	if err != nil {
		t.Fatalf("Shutdown() error = %v, report = %+v", err, report)
	}
	if report.FinalState != app.StateStopped {
		t.Fatalf("final state = %s", report.FinalState)
	}
	phases := map[app.Phase]bool{}
	for _, phase := range report.Phases {
		phases[phase.Phase] = true
		if phase.TimedOut {
			t.Fatalf("phase %s timed out: %+v", phase.Phase, phase)
		}
	}
	for _, phase := range []app.Phase{app.PhaseDrain, app.PhaseCancel, app.PhaseCheckpoint, app.PhaseWait, app.PhaseFlush, app.PhaseClose} {
		if !phases[phase] {
			t.Fatalf("phase %s did not run: %+v", phase, report.Phases)
		}
	}
	if len(publisher.delivered) == 0 {
		t.Fatal("shutdown did not flush any reliable events")
	}

	// Admission is permanently closed after shutdown, so a late request is
	// refused rather than started against a closing process.
	if _, err := application.Run(ctx, "too late", nil); err == nil {
		t.Fatal("a run was admitted after shutdown")
	}
}

func TestRunControllerRefusesWorkAfterAdmissionStops(t *testing.T) {
	controller := newRunController()
	controller.SetAvailability(app.AdmissionAvailability{Generation: 1, Available: true})
	ctx, finish, admitted := controller.admit(context.Background(), "run-1")
	if !admitted {
		t.Fatal("an open controller refused a run")
	}
	snapshot := controller.StopAdmission()
	if !snapshot.Closed || len(snapshot.Runs) != 1 || snapshot.Runs[0].RunKey != "run-1" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if _, _, admitted := controller.admit(context.Background(), "run-2"); admitted {
		t.Fatal("a closed controller admitted a run")
	}

	// Draining must not cancel the in-flight run; only the cancel phase may.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelDrain()
	if report := controller.Drain(drainCtx, snapshot); len(report.Remaining) != 1 || report.Err == nil {
		t.Fatalf("Drain() = %+v", report)
	}
	if ctx.Err() != nil {
		t.Fatal("Drain canceled a run it was only supposed to wait for")
	}
	controller.Cancel(context.Background(), snapshot.Runs, "shutdown")
	if ctx.Err() == nil {
		t.Fatal("Cancel did not interrupt the in-flight run")
	}
	finish()
	if report := controller.Wait(context.Background(), snapshot.Runs); len(report.Remaining) != 0 || report.Err != nil {
		t.Fatalf("Wait() = %+v", report)
	}
}
