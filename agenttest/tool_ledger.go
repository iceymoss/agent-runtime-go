package agenttest

import (
	"context"
	"errors"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	"github.com/iceymoss/agent-runtime-go/tool"
)

// ToolLedgerFactory returns a fresh, empty execution ledger for one subtest.
type ToolLedgerFactory func(t *testing.T) tool.ExecutionLedger

// TestToolExecutionLedger runs the tool execution ledger conformance suite.
//
// The ledger is what stands between a model's tool call and the outside world.
// Its whole job is to make one authorized invocation reach the effect boundary
// exactly once and to record what happened, so the failures that matter are:
// beginning an execution that was already settled, letting a superseded fence
// report an outcome, and losing the prepared identity a suspended approval must
// later be revalidated against.
func TestToolExecutionLedger(t *testing.T, factory ToolLedgerFactory) {
	t.Helper()
	t.Run("preparing is idempotent by execution key", func(t *testing.T) { testLedgerPrepare(t, factory) })
	t.Run("an execution crosses the effect boundary once", func(t *testing.T) { testLedgerBegin(t, factory) })
	t.Run("only the current fence may settle an execution", func(t *testing.T) { testLedgerSettle(t, factory) })
	t.Run("a refused execution never runs", func(t *testing.T) { testLedgerReject(t, factory) })
	t.Run("an ambiguous outcome is recorded as unknown", func(t *testing.T) { testLedgerUnknown(t, factory) })
	t.Run("a parked execution keeps its resume handle", func(t *testing.T) { testLedgerSuspend(t, factory) })
}

func ledgerPrepared() tool.PreparedExecution {
	return tool.PreparedExecution{
		TenantKey: "tenant-1", RunKey: "run-1", AttemptKey: "attempt-1", FenceToken: 2,
		StepNumber: 0, Ordinal: 0, CallID: "call-1", ToolName: "write_file",
		RawInput: `{"path":"a.go"}`, CanonicalInput: `{"path":"a.go"}`, InputDigest: "sha256:input",
		ExecutionKey: "execution-1", EffectDigest: "sha256:effect",
		ToolGeneration: "tools-v1", DefinitionDigest: "sha256:definition",
		SchemaVersion: "schema-v1", ToolVersion: "tool-v1", Action: "workspace.write",
		EffectClass: tool.EffectWrite, Idempotency: tool.IdempotencyNone, ReplayPolicy: agent.ReplayPolicyNever,
		PrincipalKey: "principal-1", SessionRef: "session-1",
		Resource: permission.Resource{Kind: "workspace", Key: "/repo"},
	}
}

func prepareExecution(t *testing.T, ledger tool.ExecutionLedger) tool.ExecutionRecord {
	t.Helper()
	record, created, err := ledger.Prepare(context.Background(), ledgerPrepared())
	if err != nil || !created || record.Status != tool.StatusPrepared {
		t.Fatalf("Prepare() = %+v, created %v, error %v", record, created, err)
	}
	return record
}

func testLedgerPrepare(t *testing.T, factory ToolLedgerFactory) {
	ledger := factory(t)
	ctx := context.Background()
	first := prepareExecution(t, ledger)

	// A retried preparation is the same invocation, not a second one.
	replayed, created, err := ledger.Prepare(ctx, ledgerPrepared())
	if err != nil || created || replayed.Revision != first.Revision {
		t.Fatalf("replayed Prepare() = %+v, created %v, error %v", replayed, created, err)
	}
	// The execution key is derived from the call, so two different calls sharing
	// one means something upstream computed it wrong.
	changed := ledgerPrepared()
	changed.InputDigest = "sha256:other"
	if _, _, err := ledger.Prepare(ctx, changed); !errors.Is(err, tool.ErrExecutionConflict) {
		t.Fatalf("Prepare() with a different input error = %v, want tool.ErrExecutionConflict", err)
	}

	// The prepared identity has to survive intact: a suspended approval is
	// revalidated against exactly this record, not a re-derived guess.
	loaded, err := ledger.Load(ctx, "execution-1")
	if err != nil || loaded.Prepared != ledgerPrepared() {
		t.Fatalf("Load() = %+v, error %v", loaded.Prepared, err)
	}
	if _, err := ledger.Load(ctx, "absent"); !errors.Is(err, tool.ErrToolNotFound) {
		t.Fatalf("Load(absent) error = %v, want tool.ErrToolNotFound", err)
	}
}

func testLedgerBegin(t *testing.T, factory ToolLedgerFactory) {
	ledger := factory(t)
	ctx := context.Background()
	prepared := prepareExecution(t, ledger)

	running, err := ledger.Begin(ctx, "execution-1", prepared.Prepared.FenceToken)
	if err != nil || running.Status != tool.StatusRunning || running.FenceToken != prepared.Prepared.FenceToken {
		t.Fatalf("Begin() = %+v, error %v", running, err)
	}
	// Beginning twice would let one authorized call reach the effect boundary
	// twice, which is the exact thing this ledger exists to prevent.
	if _, err := ledger.Begin(ctx, "execution-1", prepared.Prepared.FenceToken); !errors.Is(err, tool.ErrExecutionConflict) {
		t.Fatalf("second Begin() error = %v, want tool.ErrExecutionConflict", err)
	}
	if _, err := ledger.Begin(ctx, "absent", 1); !errors.Is(err, tool.ErrToolNotFound) {
		t.Fatalf("Begin(absent) error = %v, want tool.ErrToolNotFound", err)
	}
}

func testLedgerSettle(t *testing.T, factory ToolLedgerFactory) {
	ledger := factory(t)
	ctx := context.Background()
	prepared := prepareExecution(t, ledger)
	fence := prepared.Prepared.FenceToken

	result := agent.ToolResult{ToolCallID: "call-1", Name: "write_file", Content: `{"digest":"sha256:x"}`}
	// Completing before the execution began would record an outcome for work that
	// was never authorized to start.
	if _, err := ledger.Complete(ctx, tool.CompleteExecution{ExecutionKey: "execution-1", FenceToken: fence, Result: &result}); !errors.Is(err, tool.ErrStaleFence) {
		t.Fatalf("Complete() before Begin() error = %v, want tool.ErrStaleFence", err)
	}
	if _, err := ledger.Begin(ctx, "execution-1", fence); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := ledger.Complete(ctx, tool.CompleteExecution{ExecutionKey: "execution-1", FenceToken: fence + 1, Result: &result}); !errors.Is(err, tool.ErrStaleFence) {
		t.Fatalf("Complete() under a superseded fence error = %v, want tool.ErrStaleFence", err)
	}

	completed, err := ledger.Complete(ctx, tool.CompleteExecution{ExecutionKey: "execution-1", FenceToken: fence, Result: &result})
	if err != nil || completed.Status != tool.StatusSucceeded || completed.Result == nil || completed.Result.Content != result.Content {
		t.Fatalf("Complete() = %+v, error %v", completed, err)
	}
	// A settled execution is finished. Anything that would move it again is a
	// worker acting on a state it no longer owns.
	for _, settle := range []struct {
		name string
		call func() error
	}{
		{name: "complete", call: func() error {
			_, err := ledger.Complete(ctx, tool.CompleteExecution{ExecutionKey: "execution-1", FenceToken: fence, Result: &result})
			return err
		}},
		{name: "mark unknown", call: func() error {
			_, err := ledger.MarkUnknown(ctx, "execution-1", fence, tool.Failure{Code: "interrupted"})
			return err
		}},
		{name: "begin", call: func() error {
			_, err := ledger.Begin(ctx, "execution-1", fence)
			return err
		}},
	} {
		t.Run(settle.name+" after settlement is refused", func(t *testing.T) {
			if err := settle.call(); err == nil {
				t.Fatal("a settled execution was moved again")
			}
		})
	}
	loaded, err := ledger.Load(ctx, "execution-1")
	if err != nil || loaded.Status != tool.StatusSucceeded {
		t.Fatalf("Load() after completion = %+v, error %v", loaded, err)
	}
}

func testLedgerReject(t *testing.T, factory ToolLedgerFactory) {
	ledger := factory(t)
	ctx := context.Background()
	prepared := prepareExecution(t, ledger)
	fence := prepared.Prepared.FenceToken

	if _, err := ledger.Reject(ctx, "execution-1", fence+1, tool.Failure{Code: "permission_denied"}); !errors.Is(err, tool.ErrStaleFence) {
		t.Fatalf("Reject() under a foreign fence error = %v, want tool.ErrStaleFence", err)
	}
	rejected, err := ledger.Reject(ctx, "execution-1", fence, tool.Failure{Code: "permission_denied", Message: "permission denied"})
	if err != nil || rejected.Status != tool.StatusFailed || rejected.Failure == nil || rejected.Failure.Code != "permission_denied" {
		t.Fatalf("Reject() = %+v, error %v", rejected, err)
	}
	// A refused execution must never reach the tool afterwards.
	if _, err := ledger.Begin(ctx, "execution-1", fence); err == nil {
		t.Fatal("Begin() ran an execution that authorization had refused")
	}
}

func testLedgerUnknown(t *testing.T, factory ToolLedgerFactory) {
	ledger := factory(t)
	ctx := context.Background()
	prepared := prepareExecution(t, ledger)
	fence := prepared.Prepared.FenceToken
	if _, err := ledger.Begin(ctx, "execution-1", fence); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	// Unknown is the honest answer when the effect may have happened but its
	// result could not be recorded. It must be distinguishable from success and
	// from failure, because it is the one state that forbids automatic replay.
	unknown, err := ledger.MarkUnknown(ctx, "execution-1", fence, tool.Failure{Code: "interrupted", Message: "result could not be recorded"})
	if err != nil || unknown.Status != tool.StatusUnknown || unknown.Failure == nil {
		t.Fatalf("MarkUnknown() = %+v, error %v", unknown, err)
	}
	if _, err := ledger.Begin(ctx, "execution-1", fence); err == nil {
		t.Fatal("Begin() replayed an execution whose outcome is unknown")
	}
	loaded, err := ledger.Load(ctx, "execution-1")
	if err != nil || loaded.Status != tool.StatusUnknown {
		t.Fatalf("Load() = %+v, error %v", loaded, err)
	}
}

func testLedgerSuspend(t *testing.T, factory ToolLedgerFactory) {
	ledger := factory(t)
	ctx := context.Background()
	prepared := prepareExecution(t, ledger)
	fence := prepared.Prepared.FenceToken

	handle := tool.Suspension{Kind: agent.ToolSuspensionExternal, RequestRef: "child-1", ResumeToken: "token-1", Revision: 1}
	// Parking is only meaningful for work that already started, so it requires a
	// running execution at the current fence.
	if _, err := ledger.Suspend(ctx, tool.SuspendExecution{ExecutionKey: "execution-1", FenceToken: fence, Suspension: handle}); !errors.Is(err, tool.ErrStaleFence) {
		t.Fatalf("Suspend() before Begin() error = %v, want tool.ErrStaleFence", err)
	}
	if _, err := ledger.Begin(ctx, "execution-1", fence); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if _, err := ledger.Suspend(ctx, tool.SuspendExecution{ExecutionKey: "execution-1", FenceToken: fence}); err == nil {
		t.Fatal("Suspend() accepted a handle with no kind")
	}

	suspended, err := ledger.Suspend(ctx, tool.SuspendExecution{ExecutionKey: "execution-1", FenceToken: fence, Suspension: handle})
	if err != nil || suspended.Status != tool.StatusSuspended {
		t.Fatalf("Suspend() = %+v, error %v", suspended, err)
	}
	// The handle is the whole point: without it a later attempt cannot tell which
	// outside work this execution is waiting on.
	if suspended.Suspension == nil || *suspended.Suspension != handle {
		t.Fatalf("Suspend() lost the resume handle: %+v", suspended.Suspension)
	}
	loaded, err := ledger.Load(ctx, "execution-1")
	if err != nil || loaded.Suspension == nil || *loaded.Suspension != handle {
		t.Fatalf("Load() after Suspend() = %+v, error %v", loaded.Suspension, err)
	}
	// Parked is not running: completing it directly would record an outcome for
	// work that has not been re-armed.
	result := agent.ToolResult{ToolCallID: "call-1", Name: "write_file", Content: "ok"}
	if _, err := ledger.Complete(ctx, tool.CompleteExecution{ExecutionKey: "execution-1", FenceToken: fence, Result: &result}); !errors.Is(err, tool.ErrStaleFence) {
		t.Fatalf("Complete() on a parked execution error = %v, want tool.ErrStaleFence", err)
	}
	if _, err := ledger.Resume(ctx, "execution-1", fence+1); !errors.Is(err, tool.ErrStaleFence) {
		t.Fatalf("Resume() under a foreign fence error = %v, want tool.ErrStaleFence", err)
	}

	resumed, err := ledger.Resume(ctx, "execution-1", fence)
	if err != nil || resumed.Status != tool.StatusRunning {
		t.Fatalf("Resume() = %+v, error %v", resumed, err)
	}
	if _, err := ledger.Resume(ctx, "execution-1", fence); !errors.Is(err, tool.ErrStaleFence) {
		t.Fatalf("Resume() twice error = %v, want tool.ErrStaleFence", err)
	}
	completed, err := ledger.Complete(ctx, tool.CompleteExecution{ExecutionKey: "execution-1", FenceToken: fence, Result: &result})
	if err != nil || completed.Status != tool.StatusSucceeded {
		t.Fatalf("Complete() after Resume() = %+v, error %v", completed, err)
	}
	// A settled execution has nothing left to resume, so the stale handle must
	// not linger where something could act on it.
	if completed.Suspension != nil {
		t.Fatalf("Complete() kept a resume handle on a finished execution: %+v", completed.Suspension)
	}
}
