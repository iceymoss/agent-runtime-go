package session_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/session"
)

var runLimits = session.Limits{
	MaxActiveGlobal: 4, MaxActivePerTenant: 4, MaxActivePerSession: 2,
	MaxQueuedPerTenant: 4, MaxQueuedPerSession: 4,
}

func TestMemoryRunStoreDeepCopiesImageAdmission(t *testing.T) {
	store := session.NewMemoryRunStore()
	data := []byte{1, 2, 3}
	base := uint64(0)
	_, created, err := store.AdmitAndBegin(context.Background(), session.AdmitAndBeginCommand{Admission: session.Admission{
		TenantKey: "tenant", SessionKey: "session", RunKey: "image-run", BranchKey: "image-branch", RequestID: "request",
		AgentKey: "agent", BaseRevision: &base, DefinitionDigest: "definition", ContextPlanKey: "plan", ContextPlanDigest: "plan-digest",
		InputDigest: "input", ConfigDigest: "config", CreatedAt: time.Now().UTC(), Merge: session.MergeNone,
		Messages: []agent.Message{{Role: agent.RoleUser, Parts: []agent.ContentPart{{Type: agent.PartImage, Image: &agent.ImageContent{MediaType: "image/png", Data: data}}}}},
	}, Limits: runLimits})
	if err != nil || !created {
		t.Fatalf("AdmitAndBegin() error = %v, created = %v", err, created)
	}
	data[0] = 9
	first, err := store.LoadBranchInput(context.Background(), "image-run")
	if err != nil {
		t.Fatal(err)
	}
	first[0].Parts[0].Image.Data[1] = 9
	second, err := store.LoadBranchInput(context.Background(), "image-run")
	if err != nil {
		t.Fatal(err)
	}
	if got := second[0].Parts[0].Image.Data; !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("admission image aliased: %v", got)
	}
}

func admitRun(t *testing.T, store *session.MemoryRunStore, tenant agent.TenantKey, sessionKey session.SessionKey, runKey session.RunKey, requestID string, priority int, at time.Time, merge session.MergeStrategy) session.RunReceipt {
	t.Helper()
	base := uint64(0)
	receipt, created, err := store.AdmitAndBegin(context.Background(), session.AdmitAndBeginCommand{
		Admission: session.Admission{
			TenantKey: tenant, SessionKey: sessionKey, RunKey: runKey, BranchKey: session.BranchKey("branch-" + runKey),
			RequestID: requestID, AgentKey: "agent", BaseRevision: &base, DefinitionDigest: "definition",
			ContextPlanKey: "plan", ContextPlanDigest: "plan-digest", InputDigest: "input", ConfigDigest: "config",
			Messages: []agent.Message{agent.NewUserMessage(string(runKey))}, Merge: merge, Priority: priority, CreatedAt: at,
		},
		Limits: runLimits,
	})
	if err != nil || !created {
		t.Fatalf("AdmitAndBegin(%s) = %#v, %v, created=%v", runKey, receipt, err, created)
	}
	return receipt
}

func TestMemoryRunStoreAdmissionIdempotencyLimitsAndCopies(t *testing.T) {
	store := session.NewMemoryRunStore()
	now := time.Now().UTC()
	receipt := admitRun(t, store, "opaque/tenant", "session", "run-a", "request", 0, now, session.MergeNone)
	command := session.AdmitAndBeginCommand{
		Admission: session.Admission{
			TenantKey: "opaque/tenant", SessionKey: "session", RunKey: "run-a", BranchKey: "branch-run-a",
			RequestID: "request", AgentKey: "agent", DefinitionDigest: "definition", ContextPlanKey: "plan",
			ContextPlanDigest: "plan-digest", InputDigest: "input", ConfigDigest: "config",
			Messages: []agent.Message{agent.NewUserMessage("run-a")}, Merge: session.MergeNone, CreatedAt: now,
		}, Limits: runLimits,
	}
	base := uint64(0)
	command.Admission.BaseRevision = &base
	replayed, created, err := store.AdmitAndBegin(context.Background(), command)
	if err != nil || created || replayed != receipt {
		t.Fatalf("replayed admission = %#v, %v, created=%v", replayed, err, created)
	}
	command.Admission.Priority = 1
	if _, _, err := store.AdmitAndBegin(context.Background(), command); !errors.Is(err, session.ErrRunConflict) {
		t.Fatalf("conflicting admission error = %v", err)
	}

	input, err := store.LoadBranchInput(context.Background(), "run-a")
	if err != nil {
		t.Fatal(err)
	}
	input[0].Parts[0].Text = "mutated"
	again, err := store.LoadBranchInput(context.Background(), "run-a")
	if err != nil || again[0].Text() != "run-a" {
		t.Fatalf("stored input aliased caller: %#v, %v", again, err)
	}

	limited := runLimits
	limited.MaxQueuedPerSession = 1
	command.Admission.RunKey, command.Admission.BranchKey, command.Admission.RequestID = "run-b", "branch-run-b", "request-b"
	command.Admission.Priority = 0
	command.Limits = limited
	if _, _, err := store.AdmitAndBegin(context.Background(), command); !errors.Is(err, session.ErrQueueFull) {
		t.Fatalf("queue limit error = %v", err)
	}
}

func TestMemoryRunStoreOrderingActiveLimitsAndLeaseRecovery(t *testing.T) {
	store := session.NewMemoryRunStore()
	now := time.Now().UTC()
	admitRun(t, store, "tenant", "session", "run-low", "low", 0, now, session.MergeNone)
	admitRun(t, store, "tenant", "session", "run-high-b", "high-b", 10, now, session.MergeNone)
	admitRun(t, store, "tenant", "session", "run-high-a", "high-a", 10, now, session.MergeNone)

	claim, ok, err := store.ClaimNext(context.Background(), "worker-a", now.Add(time.Minute), runLimits)
	if err != nil || !ok || claim.RunKey != "run-high-a" {
		t.Fatalf("first ClaimNext() = %#v, %v, ok=%v", claim, err, ok)
	}
	second, ok, err := store.ClaimNext(context.Background(), "worker-b", now.Add(time.Minute), runLimits)
	if err != nil || !ok || second.RunKey != "run-high-b" {
		t.Fatalf("second ClaimNext() = %#v, %v, ok=%v", second, err, ok)
	}
	if _, ok, err := store.ClaimNext(context.Background(), "worker-c", now.Add(time.Minute), runLimits); err != nil || ok {
		t.Fatalf("session active limit ClaimNext() ok=%v err=%v", ok, err)
	}

	if err := store.Reconcile(context.Background(), now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunning(context.Background(), claim, 1); !errors.Is(err, session.ErrStaleClaim) {
		t.Fatalf("expired claim mutation error = %v", err)
	}
	recovered, ok, err := store.ClaimNext(context.Background(), "worker-new", now.Add(3*time.Minute), runLimits)
	if err != nil || !ok || recovered.ClaimToken == claim.ClaimToken {
		t.Fatalf("recovered claim = %#v, %v, ok=%v", recovered, err, ok)
	}
}

func TestMemoryRunStoreMergeConflictAndCancelPrecedence(t *testing.T) {
	store := session.NewMemoryRunStore()
	now := time.Now().UTC()
	admitRun(t, store, "tenant", "session", "run-a", "a", 0, now, session.MergeFastForward)
	admitRun(t, store, "tenant", "session", "run-b", "b", 0, now.Add(time.Nanosecond), session.MergeFastForward)
	claims := make([]session.Claim, 2)
	for i := range claims {
		claim, ok, err := store.ClaimNext(context.Background(), "worker", now.Add(time.Hour), runLimits)
		if err != nil || !ok {
			t.Fatalf("ClaimNext() = %#v, %v, ok=%v", claim, err, ok)
		}
		claims[i] = claim
		if err := store.MarkRunning(context.Background(), claim, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkMergePending(context.Background(), claim, agent.RunResult{Text: string(claim.RunKey), Outcome: agent.OutcomeCompleted}); err != nil {
			t.Fatal(err)
		}
	}
	for i, claim := range claims {
		_, err := store.MergeAndFinalize(context.Background(), session.MergeAndFinalizeCommand{
			Request: session.MergeRequest{TenantKey: "tenant", RunKey: claim.RunKey, Strategy: session.MergeFastForward, ExpectedBase: 0},
			Claim:   claim, DurableFence: uint64(i + 1), DurableResult: agent.RunResult{Text: string(claim.RunKey), Outcome: agent.OutcomeCompleted},
		})
		if i == 0 && err != nil {
			t.Fatalf("winning merge error = %v", err)
		}
		if i == 1 && !errors.Is(err, session.ErrMergeConflict) {
			t.Fatalf("losing merge error = %v", err)
		}
		var conflict *session.MergeConflictError
		if i == 1 && !errors.As(err, &conflict) {
			t.Fatalf("losing merge is not typed: %T", err)
		}
	}

	admitRun(t, store, "tenant", "cancel", "run-cancel", "cancel", 0, now, session.MergeNone)
	for _, mode := range []session.CancelMode{session.CancelAttempt, session.CancelAbandon, session.CancelSuspend} {
		_, err := store.RequestCancel(context.Background(), session.CancelRequest{TenantKey: "tenant", RunKey: "run-cancel", Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.Get(context.Background(), "run-cancel")
	if err != nil || result.Receipt.ExecutionState != session.ExecutionReleased {
		t.Fatalf("abandon precedence result = %#v, %v", result, err)
	}
}
