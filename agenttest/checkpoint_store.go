package agenttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// CheckpointStoreFactory returns a fresh, empty store for one subtest. Each
// subtest gets its own store so a failure cannot leak state into the next one.
type CheckpointStoreFactory func(t *testing.T) agent.CheckpointStore

// TestCheckpointStore runs the durable recovery conformance suite against a
// CheckpointStore implementation.
//
// The port's contract is unusually easy to get subtly wrong: a store that
// forgets to compare one guard field, or that refuses an already-committed tool
// execution instead of returning its stored result, still passes every ordinary
// test and then loses or repeats work only after a crash. This suite drives the
// exact sequences Agent.Run drives and asserts the properties recovery depends on.
//
// It deliberately does not assert any particular status or phase policy. Only
// the observable guarantees are checked: identity is immutable, guards are
// exact, committed work is never repeated, and terminal state is final.
func TestCheckpointStore(t *testing.T, factory CheckpointStoreFactory) {
	t.Helper()
	t.Run("begin is idempotent by immutable identity", func(t *testing.T) {
		testBeginIdempotency(t, factory)
	})
	t.Run("acquire fences and refuses a live lease", func(t *testing.T) {
		testAcquireFencing(t, factory)
	})
	t.Run("a stale guard cannot mutate", func(t *testing.T) {
		testStaleGuardRejected(t, factory)
	})
	t.Run("a committed tool is never repeated", func(t *testing.T) {
		testCommittedToolIsNotRepeated(t, factory)
	})
	t.Run("suspend keeps a resumable checkpoint", func(t *testing.T) {
		testSuspendKeepsCheckpoint(t, factory)
	})
	t.Run("terminal state is final", func(t *testing.T) {
		testTerminalStateIsFinal(t, factory)
	})
	t.Run("returned snapshots are deep copies", func(t *testing.T) {
		testSnapshotsAreDeepCopies(t, factory)
	})
}

// checkpointIdentity is the fixed run identity every subtest begins from.
func checkpointIdentity() agent.RunIdentity {
	return agent.RunIdentity{RunKey: "run-1", AgentKey: "conformance", SessionID: "session-1", RequestID: "request-1"}
}

const (
	checkpointInputDigest  = "sha256:input"
	checkpointConfigDigest = "sha256:config"
)

// initialCheckpoint is the state Begin records: the immutable input and nothing
// the run has produced yet.
func initialCheckpoint() agent.Checkpoint {
	return agent.Checkpoint{History: []agent.Message{agent.NewUserMessage("input")}}
}

// toolCallCheckpoint is the state after a model step that asked for one tool.
func toolCallCheckpoint(call agent.ToolCall) agent.Checkpoint {
	assistant := agent.Message{
		Role:         agent.RoleAssistant,
		Parts:        []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}},
		FinishReason: agent.FinishToolCalls,
	}
	step := agent.StepResult{StepNumber: 0, Message: assistant, ToolCalls: []agent.ToolCall{call}, FinishReason: agent.FinishToolCalls}
	checkpoint := initialCheckpoint()
	checkpoint.History = append(checkpoint.History, assistant)
	checkpoint.NewMessages = []agent.Message{assistant}
	checkpoint.CompletedSteps = []agent.StepResult{step}
	checkpoint.PendingToolCalls = []agent.ToolCall{call}
	return checkpoint
}

// completedCheckpoint is the state a finished run reaches: no pending calls and
// a terminal outcome.
func completedCheckpoint() agent.Checkpoint {
	answer := agent.NewAssistantMessage("done")
	checkpoint := initialCheckpoint()
	checkpoint.History = append(checkpoint.History, answer)
	checkpoint.NewMessages = []agent.Message{answer}
	checkpoint.CompletedSteps = []agent.StepResult{{StepNumber: 0, Message: answer, FinishReason: agent.FinishStop}}
	checkpoint.NextStep = 1
	checkpoint.Outcome = agent.RunResult{
		Messages: checkpoint.NewMessages, Steps: checkpoint.CompletedSteps, Text: "done",
		StopReason: agent.StopReasonComplete, Outcome: agent.OutcomeCompleted,
	}
	return checkpoint
}

func conformanceCall() agent.ToolCall {
	return agent.ToolCall{ID: "call-1", Name: "probe", Input: `{"path":"a.go"}`}
}

// beginAndAcquire drives the two steps every attempt starts with and returns the
// snapshot that authorizes the first mutation.
func beginAndAcquire(t *testing.T, store agent.CheckpointStore, owner string) agent.RunSnapshot {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Begin(ctx, checkpointIdentity(), checkpointInputDigest, checkpointConfigDigest, initialCheckpoint()); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	snapshot, err := store.Acquire(ctx, checkpointIdentity().RunKey, owner, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	if snapshot.LeaseOwner != owner {
		t.Fatalf("Acquire() lease owner = %q, want %q", snapshot.LeaseOwner, owner)
	}
	if snapshot.FenceToken == 0 {
		t.Fatal("Acquire() returned a zero fence token; a fence must identify this attempt")
	}
	return snapshot
}

// reachToolsPhase drives the store through the model boundary so tools may be
// prepared, and returns the authorizing snapshot plus the prepared execution.
func reachToolsPhase(t *testing.T, store agent.CheckpointStore, snapshot agent.RunSnapshot) (agent.RunSnapshot, agent.ToolExecution, agent.Checkpoint) {
	t.Helper()
	ctx := context.Background()
	call := conformanceCall()
	checkpoint := toolCallCheckpoint(call)
	snapshot, err := store.ModelInflight(ctx, snapshot.Guard(), initialCheckpoint())
	if err != nil {
		t.Fatalf("ModelInflight() error = %v", err)
	}
	snapshot, err = store.CommitModelResponse(ctx, snapshot.Guard(), agent.Response{FinishReason: agent.FinishToolCalls}, checkpoint)
	if err != nil {
		t.Fatalf("CommitModelResponse() error = %v", err)
	}
	execution, err := agent.NewToolExecution(checkpointIdentity(), 0, 0, call)
	if err != nil {
		t.Fatalf("NewToolExecution() error = %v", err)
	}
	snapshot, err = store.PrepareTools(ctx, snapshot.Guard(), []agent.ToolExecution{execution}, checkpoint)
	if err != nil {
		t.Fatalf("PrepareTools() error = %v", err)
	}
	return snapshot, execution, checkpoint
}

func testBeginIdempotency(t *testing.T, factory CheckpointStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	first, err := store.Begin(ctx, checkpointIdentity(), checkpointInputDigest, checkpointConfigDigest, initialCheckpoint())
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if first.SchemaVersion != agent.RunSnapshotSchemaVersion {
		t.Fatalf("Begin() schema version = %d, want %d", first.SchemaVersion, agent.RunSnapshotSchemaVersion)
	}
	if first.Identity != checkpointIdentity() {
		t.Fatalf("Begin() identity = %+v", first.Identity)
	}
	second, err := store.Begin(ctx, checkpointIdentity(), checkpointInputDigest, checkpointConfigDigest, initialCheckpoint())
	if err != nil {
		t.Fatalf("replayed Begin() error = %v", err)
	}
	if second.Revision != first.Revision || second.FenceToken != first.FenceToken {
		t.Fatalf("replayed Begin() advanced the run: %+v then %+v", first, second)
	}
	// A run key must describe exactly one composition. Accepting a different
	// config digest would let a resumed run execute under settings it never
	// started with.
	if _, err := store.Begin(ctx, checkpointIdentity(), checkpointInputDigest, "sha256:different", initialCheckpoint()); err == nil {
		t.Fatal("Begin() accepted a different config digest for an existing run")
	}
	if _, err := store.Begin(ctx, checkpointIdentity(), "sha256:different", checkpointConfigDigest, initialCheckpoint()); err == nil {
		t.Fatal("Begin() accepted a different input digest for an existing run")
	}
}

func testAcquireFencing(t *testing.T, factory CheckpointStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	first := beginAndAcquire(t, store, "worker-1")

	// A live lease is what stops two workers from executing the same run at once.
	if _, err := store.Acquire(ctx, checkpointIdentity().RunKey, "worker-2", time.Now().Add(time.Minute)); err == nil {
		t.Fatal("Acquire() granted a second lease while the first was still live")
	}

	// Re-acquiring after the lease lapses must raise the fence, which is what
	// makes the previous owner's guard provably stale.
	expired, err := store.Acquire(ctx, checkpointIdentity().RunKey, "worker-1", time.Now().Add(-time.Second))
	if err == nil {
		t.Fatalf("Acquire() accepted a lease deadline in the past: %+v", expired)
	}
	if _, err := store.Load(ctx, "absent-run"); err == nil {
		t.Fatal("Load() returned a snapshot for a run that was never begun")
	}
	loaded, err := store.Load(ctx, checkpointIdentity().RunKey)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	// Load is read-only: it must not consume or advance the lease.
	if loaded.FenceToken != first.FenceToken || loaded.Revision != first.Revision {
		t.Fatalf("Load() advanced the run: %+v then %+v", first, loaded)
	}
}

func testStaleGuardRejected(t *testing.T, factory CheckpointStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	snapshot := beginAndAcquire(t, store, "worker-1")
	stale := snapshot.Guard()

	fresh, err := store.ModelInflight(ctx, stale, initialCheckpoint())
	if err != nil {
		t.Fatalf("ModelInflight() error = %v", err)
	}
	if fresh.Revision == snapshot.Revision {
		t.Fatal("a mutation did not advance the revision, so a replay could not be detected")
	}

	tests := []struct {
		name string
		call func(agent.MutationGuard) error
	}{
		{name: "model inflight", call: func(g agent.MutationGuard) error {
			_, err := store.ModelInflight(ctx, g, initialCheckpoint())
			return err
		}},
		{name: "commit model response", call: func(g agent.MutationGuard) error {
			_, err := store.CommitModelResponse(ctx, g, agent.Response{}, initialCheckpoint())
			return err
		}},
		{name: "suspend", call: func(g agent.MutationGuard) error {
			_, err := store.Suspend(ctx, g, initialCheckpoint())
			return err
		}},
		{name: "fail", call: func(g agent.MutationGuard) error {
			_, err := store.Fail(ctx, g, initialCheckpoint(), agent.RunFailure{Message: "boom"})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name+" with the previous revision", func(t *testing.T) {
			if err := test.call(stale); !errors.Is(err, agent.ErrCheckpointConflict) {
				t.Fatalf("error = %v, want agent.ErrCheckpointConflict", err)
			}
		})
	}
	for _, test := range tests {
		t.Run(test.name+" with a foreign owner", func(t *testing.T) {
			foreign := fresh.Guard()
			foreign.LeaseOwner = "worker-2"
			if err := test.call(foreign); !errors.Is(err, agent.ErrCheckpointConflict) {
				t.Fatalf("error = %v, want agent.ErrCheckpointConflict", err)
			}
		})
	}
	for _, test := range tests {
		t.Run(test.name+" with a stale fence", func(t *testing.T) {
			foreign := fresh.Guard()
			foreign.FenceToken--
			if err := test.call(foreign); !errors.Is(err, agent.ErrCheckpointConflict) {
				t.Fatalf("error = %v, want agent.ErrCheckpointConflict", err)
			}
		})
	}
}

func testCommittedToolIsNotRepeated(t *testing.T, factory CheckpointStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	snapshot := beginAndAcquire(t, store, "worker-1")
	snapshot, execution, checkpoint := reachToolsPhase(t, store, snapshot)

	snapshot, begun, err := store.BeginTool(ctx, snapshot.Guard(), execution.IdempotencyKey, false)
	if err != nil {
		t.Fatalf("BeginTool() error = %v", err)
	}
	if begun.IdempotencyKey != execution.IdempotencyKey {
		t.Fatalf("BeginTool() returned execution %q, want %q", begun.IdempotencyKey, execution.IdempotencyKey)
	}
	if begun.Status == agent.ToolExecutionCompleted {
		t.Fatal("BeginTool() reported a fresh execution as already completed")
	}

	result := agent.ToolResult{ToolCallID: "call-1", Name: "probe", Content: "ok"}
	committed := execution
	committed.Status, committed.Result = agent.ToolExecutionCompleted, &result
	withResult := checkpoint
	withResult.CompletedSteps[0].ToolResults = []agent.ToolResult{result}
	snapshot, err = store.CommitTool(ctx, snapshot.Guard(), committed, withResult)
	if err != nil {
		t.Fatalf("CommitTool() error = %v", err)
	}

	// This is the property recovery depends on. A crash between CommitTool's
	// effect record and its checkpoint leaves the runtime asking for the same
	// execution again; the store must hand back the stored result so the tool is
	// not run a second time.
	_, replayed, err := store.BeginTool(ctx, snapshot.Guard(), execution.IdempotencyKey, false)
	if err != nil {
		t.Fatalf("BeginTool() after CommitTool error = %v; a committed execution must be replayable as a result", err)
	}
	if replayed.Status != agent.ToolExecutionCompleted || replayed.Result == nil || replayed.Result.Content != "ok" {
		t.Fatalf("BeginTool() after CommitTool = %+v, want the committed result", replayed)
	}

	stored, err := store.Load(ctx, checkpointIdentity().RunKey)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(stored.Checkpoint.CompletedSteps) != 1 || len(stored.Checkpoint.CompletedSteps[0].ToolResults) != 1 {
		t.Fatalf("CommitTool() did not persist the tool result: %+v", stored.Checkpoint)
	}
}

func testSuspendKeepsCheckpoint(t *testing.T, factory CheckpointStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	snapshot := beginAndAcquire(t, store, "worker-1")
	snapshot, _, checkpoint := reachToolsPhase(t, store, snapshot)

	// A suspended run is resumed by a later attempt, so whatever it was waiting
	// on has to survive in the checkpoint. Releasing only the lease would lose it.
	suspension := agent.ToolSuspension{
		Kind: agent.ToolSuspensionApproval, ExecutionKey: "execution-1",
		RequestRef: "request-1", ResumeToken: "token-1", Revision: 1,
	}
	checkpoint.Outcome = agent.RunResult{
		StopReason: agent.StopReasonToolSuspended, Outcome: agent.OutcomeSuspended,
		Suspension: &agent.RunSuspension{Reason: agent.StopReasonToolSuspended, Tool: &suspension},
	}
	if _, err := store.Suspend(ctx, snapshot.Guard(), checkpoint); err != nil {
		t.Fatalf("Suspend() error = %v", err)
	}
	stored, err := store.Load(ctx, checkpointIdentity().RunKey)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if stored.Status != agent.RunStatusSuspended {
		t.Fatalf("Suspend() status = %s, want %s", stored.Status, agent.RunStatusSuspended)
	}
	blocker := stored.Checkpoint.Outcome.Suspension
	if blocker == nil || blocker.Tool == nil || *blocker.Tool != suspension {
		t.Fatalf("Suspend() lost the blocker a later attempt must resume from: %+v", stored.Checkpoint.Outcome)
	}
	// A suspended run must be acquirable again, or nothing could ever resume it.
	resumed, err := store.Acquire(ctx, checkpointIdentity().RunKey, "worker-2", time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("Acquire() on a suspended run error = %v", err)
	}
	if resumed.FenceToken <= stored.FenceToken {
		t.Fatalf("resuming did not raise the fence: %d then %d", stored.FenceToken, resumed.FenceToken)
	}
}

func testTerminalStateIsFinal(t *testing.T, factory CheckpointStoreFactory) {
	tests := []struct {
		name     string
		finalize func(agent.CheckpointStore, agent.MutationGuard) error
		want     agent.RunStatus
	}{
		{
			name: "complete",
			finalize: func(store agent.CheckpointStore, guard agent.MutationGuard) error {
				_, err := store.Complete(context.Background(), guard, completedCheckpoint())
				return err
			},
			want: agent.RunStatusCompleted,
		},
		{
			name: "fail",
			finalize: func(store agent.CheckpointStore, guard agent.MutationGuard) error {
				_, err := store.Fail(context.Background(), guard, completedCheckpoint(), agent.RunFailure{Code: "boom", Message: "permanent"})
				return err
			},
			want: agent.RunStatusFailed,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := factory(t)
			ctx := context.Background()
			snapshot := beginAndAcquire(t, store, "worker-1")
			// Reaching the finalizing boundary through a committed model response
			// is the only way the runtime ever gets there.
			snapshot, err := store.ModelInflight(ctx, snapshot.Guard(), initialCheckpoint())
			if err != nil {
				t.Fatalf("ModelInflight() error = %v", err)
			}
			snapshot, err = store.CommitModelResponse(ctx, snapshot.Guard(), agent.Response{FinishReason: agent.FinishStop}, completedCheckpoint())
			if err != nil {
				t.Fatalf("CommitModelResponse() error = %v", err)
			}
			if err := test.finalize(store, snapshot.Guard()); err != nil {
				t.Fatalf("finalize error = %v", err)
			}
			stored, err := store.Load(ctx, checkpointIdentity().RunKey)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if stored.Status != test.want {
				t.Fatalf("status = %s, want %s", stored.Status, test.want)
			}
			if test.want == agent.RunStatusFailed && stored.Failure == nil {
				t.Fatal("a failed run did not persist its failure")
			}
			// Terminal means terminal: neither a new lease nor a further mutation
			// may reopen a run whose outcome is already recorded.
			if _, err := store.Acquire(ctx, checkpointIdentity().RunKey, "worker-2", time.Now().Add(time.Minute)); err == nil {
				t.Fatal("Acquire() reopened a terminal run")
			}
			if _, err := store.ModelInflight(ctx, snapshot.Guard(), initialCheckpoint()); err == nil {
				t.Fatal("ModelInflight() mutated a terminal run")
			}
			// Begin after completion is how the runtime short-circuits a retry of
			// work that already finished.
			replayed, err := store.Begin(ctx, checkpointIdentity(), checkpointInputDigest, checkpointConfigDigest, initialCheckpoint())
			if err != nil {
				t.Fatalf("Begin() on a terminal run error = %v", err)
			}
			if replayed.Status != test.want {
				t.Fatalf("Begin() on a terminal run status = %s, want %s", replayed.Status, test.want)
			}
		})
	}
}

func testSnapshotsAreDeepCopies(t *testing.T, factory CheckpointStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	snapshot := beginAndAcquire(t, store, "worker-1")
	if len(snapshot.Checkpoint.History) == 0 {
		t.Fatal("Acquire() returned an empty history; the run's immutable input is missing")
	}
	// A caller that mutates what it was handed must not be able to change stored
	// state. Every boundary in this runtime returns deep copies for this reason.
	snapshot.Checkpoint.History[0] = agent.NewUserMessage("tampered")
	stored, err := store.Load(ctx, checkpointIdentity().RunKey)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if stored.Checkpoint.History[0].Text() != "input" {
		t.Fatalf("mutating a returned snapshot changed stored state: %q", stored.Checkpoint.History[0].Text())
	}
}
