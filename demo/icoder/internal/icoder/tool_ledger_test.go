package icoder

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/agenttest"
	toollifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

func newToolLedger(t *testing.T) *SQLiteToolLedger {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store.ToolLedger()
}

func preparedExecution() toollifecycle.PreparedExecution {
	return toollifecycle.PreparedExecution{
		TenantKey: tenantKey, RunKey: "run-1", AttemptKey: "attempt-1", FenceToken: 2,
		CallID: "call-1", ToolName: "write_file", RawInput: `{"path":"a.go"}`, CanonicalInput: `{"path":"a.go"}`,
		InputDigest: "sha256:input", ExecutionKey: "execution-1", EffectDigest: "sha256:effect",
		ToolGeneration: "tools-v1", DefinitionDigest: "sha256:definition", SchemaVersion: "schema-v1",
		ToolVersion: "tool-v1", Action: "workspace.write", EffectClass: toollifecycle.EffectWrite,
		Idempotency: toollifecycle.IdempotencyNone, ReplayPolicy: agent.ReplayPolicyNever,
		PrincipalKey: principalKey, SessionRef: "session-1",
	}
}

func TestSQLiteToolLedgerLifecycle(t *testing.T) {
	ledger := newToolLedger(t)
	ctx := context.Background()
	prepared := preparedExecution()

	record, created, err := ledger.Prepare(ctx, prepared)
	if err != nil || !created || record.Status != toollifecycle.StatusPrepared {
		t.Fatalf("Prepare() = %+v, created %v, error %v", record, created, err)
	}
	if _, created, err := ledger.Prepare(ctx, prepared); err != nil || created {
		t.Fatalf("replayed Prepare() created = %v, error = %v", created, err)
	}
	changed := prepared
	changed.InputDigest = "sha256:other"
	if _, _, err := ledger.Prepare(ctx, changed); !errors.Is(err, toollifecycle.ErrExecutionConflict) {
		t.Fatalf("conflicting Prepare() error = %v, want toollifecycle.ErrExecutionConflict", err)
	}

	running, err := ledger.Begin(ctx, prepared.ExecutionKey, prepared.FenceToken)
	if err != nil || running.Status != toollifecycle.StatusRunning || running.FenceToken != prepared.FenceToken {
		t.Fatalf("Begin() = %+v, error %v", running, err)
	}
	if _, err := ledger.Begin(ctx, prepared.ExecutionKey, prepared.FenceToken); !errors.Is(err, toollifecycle.ErrExecutionConflict) {
		t.Fatalf("second Begin() error = %v, want toollifecycle.ErrExecutionConflict", err)
	}
	result := agent.ToolResult{ToolCallID: "call-1", Name: "write_file", Content: `{"digest":"sha256:x"}`}
	completed, err := ledger.Complete(ctx, toollifecycle.CompleteExecution{
		ExecutionKey: prepared.ExecutionKey, FenceToken: prepared.FenceToken, Result: &result,
	})
	if err != nil || completed.Status != toollifecycle.StatusSucceeded || completed.Result == nil {
		t.Fatalf("Complete() = %+v, error %v", completed, err)
	}
	loaded, err := ledger.Load(ctx, prepared.ExecutionKey)
	if err != nil || loaded.Status != toollifecycle.StatusSucceeded || loaded.Prepared.CallID != "call-1" {
		t.Fatalf("Load() = %+v, error %v", loaded, err)
	}
}

func TestSQLiteToolLedgerGuardsTransitions(t *testing.T) {
	ctx := context.Background()
	prepared := preparedExecution()
	tests := []struct {
		name    string
		arrange func(*testing.T, *SQLiteToolLedger)
		call    func(*SQLiteToolLedger) error
		wantErr error
	}{
		{
			name:    "loading an unknown execution",
			arrange: func(*testing.T, *SQLiteToolLedger) {},
			call: func(l *SQLiteToolLedger) error {
				_, err := l.Load(ctx, "missing")
				return err
			},
			wantErr: toollifecycle.ErrToolNotFound,
		},
		{
			name: "rejecting under a stale fence",
			arrange: func(t *testing.T, l *SQLiteToolLedger) {
				if _, _, err := l.Prepare(ctx, prepared); err != nil {
					t.Fatal(err)
				}
			},
			call: func(l *SQLiteToolLedger) error {
				_, err := l.Reject(ctx, prepared.ExecutionKey, prepared.FenceToken+1, toollifecycle.Failure{Code: "denied"})
				return err
			},
			wantErr: toollifecycle.ErrStaleFence,
		},
		{
			name: "completing an execution that never began",
			arrange: func(t *testing.T, l *SQLiteToolLedger) {
				if _, _, err := l.Prepare(ctx, prepared); err != nil {
					t.Fatal(err)
				}
			},
			call: func(l *SQLiteToolLedger) error {
				result := agent.ToolResult{ToolCallID: "call-1", Name: "write_file"}
				_, err := l.Complete(ctx, toollifecycle.CompleteExecution{ExecutionKey: prepared.ExecutionKey, FenceToken: prepared.FenceToken, Result: &result})
				return err
			},
			wantErr: toollifecycle.ErrStaleFence,
		},
		{
			name: "marking an execution unknown before it ran",
			arrange: func(t *testing.T, l *SQLiteToolLedger) {
				if _, _, err := l.Prepare(ctx, prepared); err != nil {
					t.Fatal(err)
				}
			},
			call: func(l *SQLiteToolLedger) error {
				_, err := l.MarkUnknown(ctx, prepared.ExecutionKey, prepared.FenceToken, toollifecycle.Failure{Code: "interrupted"})
				return err
			},
			wantErr: toollifecycle.ErrStaleFence,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ledger := newToolLedger(t)
			test.arrange(t, ledger)
			if err := test.call(ledger); !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestSQLiteToolLedgerRecordsDenialWithoutRunning(t *testing.T) {
	ledger := newToolLedger(t)
	ctx := context.Background()
	prepared := preparedExecution()
	if _, _, err := ledger.Prepare(ctx, prepared); err != nil {
		t.Fatal(err)
	}
	rejected, err := ledger.Reject(ctx, prepared.ExecutionKey, prepared.FenceToken, toollifecycle.Failure{Code: "permission_denied", Message: "permission denied"})
	if err != nil || rejected.Status != toollifecycle.StatusFailed || rejected.Failure == nil || rejected.Failure.Code != "permission_denied" {
		t.Fatalf("Reject() = %+v, error %v", rejected, err)
	}
	if _, err := ledger.Begin(ctx, prepared.ExecutionKey, prepared.FenceToken); !errors.Is(err, toollifecycle.ErrExecutionConflict) {
		t.Fatalf("Begin() after a denial error = %v, want toollifecycle.ErrExecutionConflict", err)
	}
}

// TestSQLiteToolLedgerConformance holds this adapter to the same execution
// ledger contract as the library's reference implementation.
func TestSQLiteToolLedgerConformance(t *testing.T) {
	agenttest.TestToolExecutionLedger(t, func(t *testing.T) toollifecycle.ExecutionLedger {
		return newToolLedger(t)
	})
}
