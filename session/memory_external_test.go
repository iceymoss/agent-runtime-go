package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/message"
	"github.com/iceymoss/agent-runtime-go/session"
)

func createSession(t *testing.T, service session.Service, tenant agent.TenantKey, key session.SessionKey, metadata []byte) session.Snapshot {
	t.Helper()
	snapshot, err := service.Create(context.Background(), session.CreateCommand{
		TenantKey: tenant, SessionKey: key, UserKey: "user", AgentKey: "agent", Identity: "identity", Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return snapshot
}

func TestMemoryCreateHistoryIdempotencyAndCopies(t *testing.T) {
	service := session.NewMemory()
	metadata := []byte("original")
	created := createSession(t, service, "tenant-a", "session-a", metadata)
	metadata[0] = 'X'
	created.Metadata[0] = 'Y'

	again, err := service.Create(context.Background(), session.CreateCommand{
		TenantKey: "tenant-a", SessionKey: "session-a", UserKey: "user", AgentKey: "agent", Identity: "identity", Metadata: []byte("original"),
	})
	if err != nil || string(again.Metadata) != "original" || again.Revision != 0 {
		t.Fatalf("idempotent Create() = %#v, %v", again, err)
	}
	_, err = service.Create(context.Background(), session.CreateCommand{
		TenantKey: "tenant-a", SessionKey: "session-a", UserKey: "different", AgentKey: "agent", Identity: "identity", Metadata: []byte("original"),
	})
	if !errors.Is(err, session.ErrIdempotencyConflict) {
		t.Fatalf("conflicting Create() error = %v", err)
	}

	next, err := service.Transition(context.Background(), session.TransitionCommand{
		TenantKey: "tenant-a", SessionKey: "session-a", ExpectedRevision: 0, Status: session.StatusSuspended,
	})
	if err != nil || next.Revision != 1 {
		t.Fatalf("Transition() = %#v, %v", next, err)
	}
	baseline, err := service.GetRevision(context.Background(), session.RevisionQuery{TenantKey: "tenant-a", SessionKey: "session-a", Revision: 0})
	if err != nil || baseline.Status != session.StatusActive || baseline.Revision != 0 {
		t.Fatalf("GetRevision(0) = %#v, %v", baseline, err)
	}
}

func TestMemoryTenantIsolationAndBranchTransitions(t *testing.T) {
	service := session.NewMemory()
	createSession(t, service, "tenant-a", "shared", nil)
	createSession(t, service, "tenant-b", "shared", nil)
	if _, err := service.Get(context.Background(), session.GetQuery{TenantKey: "tenant-c", SessionKey: "shared"}); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("cross-tenant Get() error = %v", err)
	}
	branch, err := service.CreateBranch(context.Background(), session.CreateBranchCommand{TenantKey: "tenant-a", SessionKey: "shared", RunKey: "run", BaseRevision: 0})
	if err != nil {
		t.Fatalf("CreateBranch() error = %v", err)
	}
	replayed, err := service.CreateBranch(context.Background(), session.CreateBranchCommand{TenantKey: "tenant-a", SessionKey: "shared", RunKey: "run", BaseRevision: 0})
	if err != nil || replayed.BranchKey != branch.BranchKey {
		t.Fatalf("replayed CreateBranch() = %#v, %v", replayed, err)
	}
	ready, err := service.MarkBranchReady(context.Background(), session.BranchCommand{TenantKey: "tenant-a", SessionKey: "shared", BranchKey: branch.BranchKey, ExpectedVersion: 0})
	if err != nil || ready.Status != session.BranchStatusReadyToMerge || ready.Version != 1 {
		t.Fatalf("MarkBranchReady() = %#v, %v", ready, err)
	}
	conflicted, err := service.MarkBranchConflict(context.Background(), session.ConflictCommand{TenantKey: "tenant-a", SessionKey: "shared", BranchKey: branch.BranchKey, ExpectedVersion: 1})
	if err != nil || conflicted.Status != session.BranchStatusConflicted || conflicted.Version != 2 {
		t.Fatalf("MarkBranchConflict() = %#v, %v", conflicted, err)
	}
	ready, err = service.MarkBranchReady(context.Background(), session.BranchCommand{TenantKey: "tenant-a", SessionKey: "shared", BranchKey: branch.BranchKey, ExpectedVersion: 2})
	if err != nil || ready.Status != session.BranchStatusReadyToMerge {
		t.Fatalf("conflict retry MarkBranchReady() = %#v, %v", ready, err)
	}
	if _, err := service.GetBranch(context.Background(), session.GetBranchQuery{TenantKey: "tenant-b", SessionKey: "shared", BranchKey: branch.BranchKey}); !errors.Is(err, session.ErrBranchNotFound) {
		t.Fatalf("cross-tenant GetBranch() error = %v", err)
	}
}

func TestMemoryConcurrentFastForwardMergeHasOneWinner(t *testing.T) {
	service := session.NewMemory()
	createSession(t, service, "tenant", "session", nil)
	branches := make([]session.Branch, 2)
	for i, run := range []session.RunKey{"run-a", "run-b"} {
		branch, err := service.CreateBranch(context.Background(), session.CreateBranchCommand{TenantKey: "tenant", SessionKey: "session", RunKey: run, BaseRevision: 0})
		if err != nil {
			t.Fatalf("CreateBranch(%s) error = %v", run, err)
		}
		branches[i], err = service.MarkBranchReady(context.Background(), session.BranchCommand{TenantKey: "tenant", SessionKey: "session", BranchKey: branch.BranchKey, ExpectedVersion: 0})
		if err != nil {
			t.Fatalf("MarkBranchReady(%s) error = %v", run, err)
		}
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, branch := range branches {
		wg.Add(1)
		go func(branch session.Branch) {
			defer wg.Done()
			<-start
			_, err := service.CommitMerge(context.Background(), session.MergeCommit{
				TenantKey: "tenant", SessionKey: "session", BranchKey: branch.BranchKey, ExpectedVersion: branch.Version, MergeKind: session.MergeKindFastForward,
			})
			errs <- err
		}(branch)
	}
	close(start)
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, session.ErrMergeConflict) {
			conflicts++
		} else {
			t.Fatalf("CommitMerge() error = %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("merge outcomes wins=%d conflicts=%d", wins, conflicts)
	}
	current, err := service.Get(context.Background(), session.GetQuery{TenantKey: "tenant", SessionKey: "session"})
	if err != nil || current.Revision != 1 {
		t.Fatalf("Get() after merges = %#v, %v", current, err)
	}
}

func TestMemoryUsageReplaySummaryAndTerminal(t *testing.T) {
	service := session.NewMemory()
	createSession(t, service, "tenant", "session", nil)
	usage := session.UsageCommand{TenantKey: "tenant", SessionKey: "session", ExpectedRevision: 0, UsageFactKey: "fact", PromptTokens: 3, CompletionTokens: 5, CostMicros: 17}
	first, err := service.AddUsage(context.Background(), usage)
	if err != nil || first.Revision != 1 || first.PromptTokens != 3 || first.CostMicros != 17 {
		t.Fatalf("AddUsage() = %#v, %v", first, err)
	}
	replayed, err := service.AddUsage(context.Background(), usage)
	if err != nil || replayed.Revision != 1 || replayed.PromptTokens != 3 {
		t.Fatalf("replayed AddUsage() = %#v, %v", replayed, err)
	}
	usage.CostMicros++
	if _, err := service.AddUsage(context.Background(), usage); !errors.Is(err, session.ErrIdempotencyConflict) {
		t.Fatalf("conflicting AddUsage() error = %v", err)
	}
	if _, err := service.SetSummary(context.Background(), session.SummaryCommand{TenantKey: "tenant", SessionKey: "session", ExpectedRevision: 1, SummaryMessageKey: message.MessageKey("summary")}); !errors.Is(err, session.ErrSnapshotInvariant) {
		t.Fatalf("invalid SetSummary() error = %v", err)
	}
	completed, err := service.Transition(context.Background(), session.TransitionCommand{TenantKey: "tenant", SessionKey: "session", ExpectedRevision: 1, Status: session.StatusCompleted})
	if err != nil {
		t.Fatalf("Transition(completed) error = %v", err)
	}
	if _, err := service.Transition(context.Background(), session.TransitionCommand{TenantKey: "tenant", SessionKey: "session", ExpectedRevision: completed.Revision, Status: session.StatusActive}); !errors.Is(err, session.ErrInvalidSessionTransition) {
		t.Fatalf("terminal Transition() error = %v", err)
	}
}
