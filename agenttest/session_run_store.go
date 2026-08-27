package agenttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/session"
)

// SessionRunStoreFactory returns a fresh, empty durable run queue.
type SessionRunStoreFactory func(t *testing.T) session.Store

// TestSessionRunStore runs the durable run queue conformance suite.
//
// This is the queue a worker claims work from, so its contract is about who owns
// a run and what happens when that owner disappears. A store that hands the same
// run to two workers runs a task twice; one that never returns an abandoned
// claim leaves work stuck forever; one that merges a finished run twice
// duplicates a turn in the conversation. Each of those is driven here.
func TestSessionRunStore(t *testing.T, factory SessionRunStoreFactory) {
	t.Helper()
	t.Run("admission is idempotent by request", func(t *testing.T) { testRunAdmission(t, factory) })
	t.Run("a claim is exclusive and recoverable", func(t *testing.T) { testRunClaim(t, factory) })
	t.Run("a stale claim cannot report an outcome", func(t *testing.T) { testRunStaleClaim(t, factory) })
	t.Run("suspended work returns to the queue only on request", func(t *testing.T) { testRunResume(t, factory) })
	t.Run("a finished run merges exactly once", func(t *testing.T) { testRunMerge(t, factory) })
	t.Run("cancellation and abandonment are idempotent", func(t *testing.T) { testRunCancel(t, factory) })
}

func runLimits() session.Limits {
	return session.Limits{
		MaxActiveGlobal: 4, MaxActivePerTenant: 4, MaxActivePerSession: 4,
		MaxQueuedPerTenant: 4, MaxQueuedPerSession: 4,
	}
}

func runStepPolicy(t *testing.T) session.StepPolicyArtifact {
	t.Helper()
	// A step policy has to be deterministic, so every step names exactly one tool
	// or none at all. That is what lets a resumed run reproduce the same request.
	artifact, err := session.NewStepPolicyArtifact(session.StepPolicyStep{
		ActiveTools: []string{"probe"}, ToolChoice: agent.ToolChoice{Mode: agent.ToolChoiceNamed, Name: "probe"},
	})
	if err != nil {
		t.Fatalf("NewStepPolicyArtifact() error = %v", err)
	}
	return artifact
}

func runAdmission(t *testing.T, runKey session.RunKey, requestID string) session.AdmitAndBeginCommand {
	t.Helper()
	policy := runStepPolicy(t)
	return session.AdmitAndBeginCommand{
		Admission: session.Admission{
			TenantKey: sessionTenant, SessionKey: sessionKey, BranchKey: session.BranchKey("branch/" + runKey),
			RunKey: runKey, RequestID: requestID, AgentKey: "agent-1", DefinitionDigest: "sha256:definition",
			ContextPlanKey: "plan-1", ContextPlanDigest: "sha256:plan",
			InputDigest: "sha256:input", ConfigDigest: "sha256:config",
			StepPolicy: policy, StepPolicyDigest: "sha256:policy",
			Messages: []agent.Message{agent.NewUserMessage("task")}, Merge: session.MergeFastForward,
		},
		Execution: session.ResolvedExecution{DefinitionDigest: "sha256:definition", SchemaVersion: 1},
		Limits:    runLimits(),
	}
}

func admitRun(t *testing.T, store session.Store, runKey session.RunKey, requestID string) session.RunReceipt {
	t.Helper()
	receipt, created, err := store.AdmitAndBegin(context.Background(), runAdmission(t, runKey, requestID))
	if err != nil || !created {
		t.Fatalf("AdmitAndBegin() created = %v, error = %v", created, err)
	}
	return receipt
}

func claimRun(t *testing.T, store session.Store, worker string, now time.Time) session.Claim {
	t.Helper()
	claim, ok, err := store.ClaimNext(context.Background(), worker, now.Add(time.Minute), runLimits())
	if err != nil || !ok {
		t.Fatalf("ClaimNext() ok = %v, error = %v", ok, err)
	}
	return claim
}

func testRunAdmission(t *testing.T, factory SessionRunStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	receipt := admitRun(t, store, "run-1", "request-1")
	if receipt.ExecutionState != session.ExecutionQueued || receipt.AdmissionState != session.AdmissionQueued {
		t.Fatalf("AdmitAndBegin() = %+v", receipt)
	}
	// A retried admission must adopt the queued run rather than queue a second
	// copy of the same task.
	replayed, created, err := store.AdmitAndBegin(ctx, runAdmission(t, "run-1", "request-1"))
	if err != nil || created || replayed.RunKey != receipt.RunKey {
		t.Fatalf("replayed AdmitAndBegin() = %+v, created %v, error %v", replayed, created, err)
	}
	conflicting := runAdmission(t, "run-1", "request-1")
	conflicting.Admission.DefinitionDigest = "sha256:other"
	if _, _, err := store.AdmitAndBegin(ctx, conflicting); !errors.Is(err, session.ErrRunConflict) {
		t.Fatalf("AdmitAndBegin() with a different definition error = %v, want session.ErrRunConflict", err)
	}
	incomplete := runAdmission(t, "run-2", "request-2")
	incomplete.Admission.Messages = nil
	if _, _, err := store.AdmitAndBegin(ctx, incomplete); !errors.Is(err, session.ErrInvalidCommand) {
		t.Fatalf("AdmitAndBegin() without input error = %v, want session.ErrInvalidCommand", err)
	}

	input, err := store.LoadBranchInput(ctx, "run-1")
	if err != nil || len(input) != 1 || input[0].Text() != "task" {
		t.Fatalf("LoadBranchInput() = %+v, error %v", input, err)
	}
	policy, digest, err := store.LoadStepPolicy(ctx, "run-1")
	if err != nil || digest != "sha256:policy" || len(policy.Steps) != 1 {
		t.Fatalf("LoadStepPolicy() = %+v, digest %q, error %v", policy, digest, err)
	}
	if _, err := store.LoadBranchInput(ctx, "absent"); !errors.Is(err, session.ErrRunNotFound) {
		t.Fatalf("LoadBranchInput(absent) error = %v, want session.ErrRunNotFound", err)
	}
	if _, err := store.Get(ctx, "absent"); !errors.Is(err, session.ErrRunNotFound) {
		t.Fatalf("Get(absent) error = %v, want session.ErrRunNotFound", err)
	}
}

func testRunClaim(t *testing.T, factory SessionRunStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC()
	admitRun(t, store, "run-1", "request-1")

	claim := claimRun(t, store, "worker-1", now)
	if claim.RunKey != "run-1" || claim.ClaimToken == 0 {
		t.Fatalf("ClaimNext() = %+v", claim)
	}
	// A live claim is what keeps one run from executing twice.
	if _, ok, err := store.ClaimNext(ctx, "worker-2", now.Add(time.Minute), runLimits()); err != nil || ok {
		t.Fatalf("ClaimNext() handed out a second claim: ok %v, error %v", ok, err)
	}

	// A worker that took a claim and then stopped before starting the attempt
	// leaves an expired lease over work that never began. Reconciliation must
	// return exactly that run to the queue, or it is stuck forever.
	if err := store.Reconcile(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	recovered, ok, err := store.ClaimNext(ctx, "worker-2", now.Add(3*time.Minute), runLimits())
	if err != nil || !ok || recovered.RunKey != "run-1" {
		t.Fatalf("ClaimNext() after reconcile = %+v, ok %v, error %v", recovered, ok, err)
	}
	if recovered.ClaimToken == claim.ClaimToken {
		t.Fatal("recovery reused the previous claim token, so the old worker is still authorized")
	}

	if err := store.MarkRunning(ctx, recovered, 1); err != nil {
		t.Fatalf("MarkRunning() error = %v", err)
	}

	// Once an attempt has actually started, a lapsed lease means something
	// different: the run becomes suspended rather than re-queued, because nobody
	// knows how far it got. Something outside the queue has to decide it may
	// continue, which is what Resume is for.
	if err := store.Reconcile(ctx, now.Add(10*time.Minute)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, ok, err := store.ClaimNext(ctx, "worker-4", now.Add(11*time.Minute), runLimits()); err != nil || ok {
		t.Fatalf("ClaimNext() re-queued an attempt that had already started: ok %v, error %v", ok, err)
	}
}

func testRunStaleClaim(t *testing.T, factory SessionRunStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC()
	admitRun(t, store, "run-1", "request-1")
	claim := claimRun(t, store, "worker-1", now)

	stale := claim
	stale.ClaimToken++
	tests := []struct {
		name string
		call func(session.Claim) error
	}{
		{name: "mark running", call: func(c session.Claim) error { return store.MarkRunning(ctx, c, 1) }},
		{name: "mark suspended", call: func(c session.Claim) error {
			return store.MarkSuspended(ctx, c, session.Failure{Code: "suspended"})
		}},
		{name: "mark failed", call: func(c session.Claim) error {
			return store.MarkFailed(ctx, c, session.Failure{Code: "failed"})
		}},
		{name: "mark merge pending", call: func(c session.Claim) error {
			return store.MarkMergePending(ctx, c, agent.RunResult{Outcome: agent.OutcomeCompleted})
		}},
		{name: "release", call: func(c session.Claim) error { return store.Release(ctx, c) }},
	}
	for _, test := range tests {
		t.Run(test.name+" with a foreign claim token", func(t *testing.T) {
			if err := test.call(stale); !errors.Is(err, session.ErrStaleClaim) {
				t.Fatalf("error = %v, want session.ErrStaleClaim", err)
			}
		})
	}
	foreignWorker := claim
	foreignWorker.WorkerID = "worker-2"
	if err := store.MarkRunning(ctx, foreignWorker, 1); !errors.Is(err, session.ErrStaleClaim) {
		t.Fatalf("MarkRunning() with a foreign worker error = %v, want session.ErrStaleClaim", err)
	}
}

func testRunResume(t *testing.T, factory SessionRunStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC()
	admitRun(t, store, "run-1", "request-1")
	claim := claimRun(t, store, "worker-1", now)
	if err := store.MarkRunning(ctx, claim, 1); err != nil {
		t.Fatalf("MarkRunning() error = %v", err)
	}
	if err := store.MarkSuspended(ctx, claim, session.Failure{Code: "approval_required", Retryable: true}); err != nil {
		t.Fatalf("MarkSuspended() error = %v", err)
	}

	// Suspended work is inert on purpose: something outside the queue has to
	// decide it may continue, so reconciliation must not pick it back up.
	if err := store.Reconcile(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, ok, err := store.ClaimNext(ctx, "worker-2", now.Add(3*time.Minute), runLimits()); err != nil || ok {
		t.Fatalf("ClaimNext() picked up suspended work: ok %v, error %v", ok, err)
	}

	resumed, err := store.Resume(ctx, session.ResumeRequest{TenantKey: sessionTenant, RunKey: "run-1"}, runLimits())
	if err != nil || !resumed.Resumed {
		t.Fatalf("Resume() = %+v, error %v", resumed, err)
	}
	if again, err := store.Resume(ctx, session.ResumeRequest{TenantKey: sessionTenant, RunKey: "run-1"}, runLimits()); err != nil || again.Resumed {
		t.Fatalf("replayed Resume() = %+v, error %v", again, err)
	}
	if _, ok, err := store.ClaimNext(ctx, "worker-2", now.Add(4*time.Minute), runLimits()); err != nil || !ok {
		t.Fatalf("ClaimNext() after resume: ok %v, error %v", ok, err)
	}
}

func testRunMerge(t *testing.T, factory SessionRunStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	now := time.Now().UTC()
	receipt := admitRun(t, store, "run-1", "request-1")
	claim := claimRun(t, store, "worker-1", now)
	if err := store.MarkRunning(ctx, claim, 1); err != nil {
		t.Fatalf("MarkRunning() error = %v", err)
	}
	result := agent.RunResult{
		Messages: []agent.Message{agent.NewAssistantMessage("done")}, Text: "done",
		Outcome: agent.OutcomeCompleted, StopReason: agent.StopReasonComplete,
	}
	if err := store.MarkMergePending(ctx, claim, result); err != nil {
		t.Fatalf("MarkMergePending() error = %v", err)
	}
	// A run whose attempt finished but whose merge has not landed must be
	// re-claimable: releasing it hands the finalize to another worker instead of
	// stranding a completed attempt.
	if err := store.Release(ctx, claim); err != nil {
		t.Fatalf("Release() on merge-pending work error = %v", err)
	}
	claim = claimRun(t, store, "worker-2", now.Add(time.Minute))
	command := session.MergeAndFinalizeCommand{
		Request: session.MergeRequest{
			TenantKey: sessionTenant, RunKey: "run-1",
			Strategy: session.MergeFastForward, ExpectedBase: receipt.BaseRevision,
		},
		Claim: claim, DurableFence: 1, DurableResult: result,
	}
	merged, err := store.MergeAndFinalize(ctx, command)
	if err != nil || merged.SessionRevision != receipt.BaseRevision+1 {
		t.Fatalf("MergeAndFinalize() = %+v, error %v", merged, err)
	}
	// A retried finalize is the same finalize. It must return the recorded result
	// rather than advance the session again, because advancing twice is what
	// silently duplicates a turn in the conversation.
	replayed, err := store.MergeAndFinalize(ctx, command)
	if err != nil {
		t.Fatalf("replayed MergeAndFinalize() error = %v", err)
	}
	if replayed.SessionRevision != merged.SessionRevision {
		t.Fatalf("replayed MergeAndFinalize() advanced the session again: %d then %d", merged.SessionRevision, replayed.SessionRevision)
	}
	final, err := store.Get(ctx, "run-1")
	if err != nil || final.CoreResult == nil || final.CoreResult.Text != "done" {
		t.Fatalf("Get() after merge = %+v, error %v", final, err)
	}
	if err := store.MarkRunning(ctx, claim, 1); err == nil {
		t.Fatal("MarkRunning() mutated a finalized run")
	}
}

func testRunCancel(t *testing.T, factory SessionRunStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	admitRun(t, store, "run-1", "request-1")

	request := session.CancelRequest{TenantKey: sessionTenant, RunKey: "run-1", Mode: session.CancelAbandon, Reason: "operator"}
	first, err := store.RequestCancel(ctx, request)
	if err != nil || !first.Requested {
		t.Fatalf("RequestCancel() = %+v, error %v", first, err)
	}
	// A retried cancel is the same decision, not a second one.
	second, err := store.RequestCancel(ctx, request)
	if err != nil {
		t.Fatalf("replayed RequestCancel() error = %v", err)
	}
	if second.RunKey != first.RunKey {
		t.Fatalf("replayed RequestCancel() = %+v, first = %+v", second, first)
	}
	if err := store.Abandon(ctx, "run-1", "operator"); err != nil {
		t.Fatalf("Abandon() error = %v", err)
	}
	if err := store.Abandon(ctx, "run-1", "operator"); err != nil {
		t.Fatalf("replayed Abandon() error = %v", err)
	}
	if _, err := store.RequestCancel(ctx, session.CancelRequest{TenantKey: sessionTenant, RunKey: "absent", Mode: session.CancelAbandon}); !errors.Is(err, session.ErrRunNotFound) {
		t.Fatalf("RequestCancel(absent) error = %v, want session.ErrRunNotFound", err)
	}
}
