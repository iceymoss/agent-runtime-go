package agenttest

import (
	"context"
	"errors"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/session"
)

// SessionServiceFactory returns a fresh, empty session aggregate service.
type SessionServiceFactory func(t *testing.T) session.Service

// TestSessionService runs the session and branch aggregate conformance suite.
//
// The session aggregate is where a conversation's revision, its usage totals,
// and its branch merges are decided. Its failure modes are quiet and expensive:
// a merge that applies twice duplicates a turn, a usage fact counted twice
// double-bills, and a finished session that still accepts work reports a total
// that was already sent somewhere as final. Each of those is driven here.
func TestSessionService(t *testing.T, factory SessionServiceFactory) {
	t.Helper()
	t.Run("create is idempotent by session key", func(t *testing.T) { testSessionCreate(t, factory) })
	t.Run("revisions are addressable after they advance", func(t *testing.T) { testSessionRevisions(t, factory) })
	t.Run("usage accumulates exactly once per fact", func(t *testing.T) { testSessionUsage(t, factory) })
	t.Run("status transitions are one-way into terminal", func(t *testing.T) { testSessionTransitions(t, factory) })
	t.Run("a branch fast-forwards exactly once", func(t *testing.T) { testSessionMerge(t, factory) })
}

const (
	sessionTenant = agent.TenantKey("tenant-1")
	sessionKey    = session.SessionKey("session-1")
)

func sessionCreateCommand() session.CreateCommand {
	return session.CreateCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey,
		UserKey: "user-1", AgentKey: "agent-1", Identity: "identity-1", Title: "conformance",
	}
}

func createSession(t *testing.T, service session.Service) session.Snapshot {
	t.Helper()
	snapshot, err := service.Create(context.Background(), sessionCreateCommand())
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return snapshot
}

func testSessionCreate(t *testing.T, factory SessionServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	first := createSession(t, service)
	if first.Status != session.StatusActive || first.MetadataVersion == 0 {
		t.Fatalf("Create() = %+v", first)
	}
	replayed := createSession(t, service)
	if replayed.Revision != first.Revision || replayed.CreatedAt != first.CreatedAt {
		t.Fatalf("replayed Create() = %+v, first = %+v", replayed, first)
	}

	changed := sessionCreateCommand()
	changed.AgentKey = "agent-2"
	if _, err := service.Create(ctx, changed); !errors.Is(err, session.ErrIdempotencyConflict) {
		t.Fatalf("Create() with a different agent error = %v, want session.ErrIdempotencyConflict", err)
	}
	incomplete := sessionCreateCommand()
	incomplete.SessionKey, incomplete.Identity = "session-2", ""
	if _, err := service.Create(ctx, incomplete); !errors.Is(err, session.ErrInvalidCommand) {
		t.Fatalf("Create() without an identity error = %v, want session.ErrInvalidCommand", err)
	}
	// A partial context pivot would let a resumed session rebuild its history
	// from an artifact it cannot verify.
	partial := sessionCreateCommand()
	partial.SessionKey = "session-3"
	partial.ContextPivot = session.ContextPivotSnapshot{ArtifactKey: "artifact-1"}
	if _, err := service.Create(ctx, partial); !errors.Is(err, session.ErrSnapshotInvariant) {
		t.Fatalf("Create() with a partial pivot error = %v, want session.ErrSnapshotInvariant", err)
	}
	if _, err := service.Get(ctx, session.GetQuery{TenantKey: sessionTenant, SessionKey: "absent"}); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("Get(absent) error = %v, want session.ErrSessionNotFound", err)
	}
	if _, err := service.Get(ctx, session.GetQuery{TenantKey: "tenant-2", SessionKey: sessionKey}); !errors.Is(err, session.ErrSessionNotFound) {
		t.Fatalf("Get() leaked across tenants: %v", err)
	}
}

func testSessionRevisions(t *testing.T, factory SessionServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	created := createSession(t, service)
	advanced, err := service.AddUsage(ctx, session.UsageCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: created.Revision,
		UsageFactKey: "usage-1", PromptTokens: 10, CompletionTokens: 4, CostMicros: 25,
	})
	if err != nil || advanced.Revision != created.Revision+1 {
		t.Fatalf("AddUsage() = %+v, error %v", advanced, err)
	}
	// A superseded revision must remain readable: a run that started against it
	// has to be able to prove what it was based on.
	historical, err := service.GetRevision(ctx, session.RevisionQuery{TenantKey: sessionTenant, SessionKey: sessionKey, Revision: created.Revision})
	if err != nil || historical.Revision != created.Revision || historical.PromptTokens != 0 {
		t.Fatalf("GetRevision(previous) = %+v, error %v", historical, err)
	}
	current, err := service.GetRevision(ctx, session.RevisionQuery{TenantKey: sessionTenant, SessionKey: sessionKey, Revision: advanced.Revision})
	if err != nil || current.PromptTokens != 10 {
		t.Fatalf("GetRevision(current) = %+v, error %v", current, err)
	}
	if _, err := service.GetRevision(ctx, session.RevisionQuery{TenantKey: sessionTenant, SessionKey: sessionKey, Revision: advanced.Revision + 5}); !errors.Is(err, session.ErrRevisionNotFound) {
		t.Fatalf("GetRevision(future) error = %v, want session.ErrRevisionNotFound", err)
	}
}

func testSessionUsage(t *testing.T, factory SessionServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	created := createSession(t, service)
	command := session.UsageCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: created.Revision,
		UsageFactKey: "usage-1", PromptTokens: 10, CompletionTokens: 4, CostMicros: 25,
	}
	first, err := service.AddUsage(ctx, command)
	if err != nil || first.PromptTokens != 10 || first.CostMicros != 25 {
		t.Fatalf("AddUsage() = %+v, error %v", first, err)
	}
	// A usage fact is counted once no matter how often the command is retried,
	// including after the revision it was issued against has moved on.
	replayed, err := service.AddUsage(ctx, command)
	if err != nil || replayed.PromptTokens != 10 || replayed.Revision != first.Revision {
		t.Fatalf("replayed AddUsage() = %+v, error %v", replayed, err)
	}
	conflicting := command
	conflicting.PromptTokens = 11
	if _, err := service.AddUsage(ctx, conflicting); !errors.Is(err, session.ErrIdempotencyConflict) {
		t.Fatalf("AddUsage() with different numbers error = %v, want session.ErrIdempotencyConflict", err)
	}
	stale := command
	stale.UsageFactKey, stale.ExpectedRevision = "usage-2", created.Revision
	if _, err := service.AddUsage(ctx, stale); !errors.Is(err, session.ErrRevisionConflict) {
		t.Fatalf("AddUsage() against a superseded revision error = %v, want session.ErrRevisionConflict", err)
	}
	negative := command
	negative.UsageFactKey, negative.ExpectedRevision, negative.CostMicros = "usage-3", first.Revision, -1
	if _, err := service.AddUsage(ctx, negative); !errors.Is(err, session.ErrInvalidCommand) {
		t.Fatalf("AddUsage() with a negative delta error = %v, want session.ErrInvalidCommand", err)
	}
}

func testSessionTransitions(t *testing.T, factory SessionServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	created := createSession(t, service)

	suspended, err := service.Transition(ctx, session.TransitionCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: created.Revision, Status: session.StatusSuspended,
	})
	if err != nil || suspended.Status != session.StatusSuspended {
		t.Fatalf("Transition(suspended) = %+v, error %v", suspended, err)
	}
	resumed, err := service.Transition(ctx, session.TransitionCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: suspended.Revision, Status: session.StatusActive,
	})
	if err != nil || resumed.Status != session.StatusActive {
		t.Fatalf("Transition(active) = %+v, error %v", resumed, err)
	}
	completed, err := service.Transition(ctx, session.TransitionCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: resumed.Revision, Status: session.StatusCompleted,
	})
	if err != nil || completed.Status != session.StatusCompleted {
		t.Fatalf("Transition(completed) = %+v, error %v", completed, err)
	}

	// Completed is final. A session whose totals were already reported as final
	// must not quietly accept more work.
	if _, err := service.Transition(ctx, session.TransitionCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: completed.Revision, Status: session.StatusActive,
	}); !errors.Is(err, session.ErrInvalidSessionTransition) {
		t.Fatalf("Transition() out of a terminal status error = %v, want session.ErrInvalidSessionTransition", err)
	}
	if _, err := service.AddUsage(ctx, session.UsageCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: completed.Revision,
		UsageFactKey: "usage-late", PromptTokens: 1,
	}); !errors.Is(err, session.ErrInvalidSessionTransition) {
		t.Fatalf("AddUsage() on a terminal session error = %v, want session.ErrInvalidSessionTransition", err)
	}
	if _, err := service.Transition(ctx, session.TransitionCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, ExpectedRevision: completed.Revision, Status: session.Status("invented"),
	}); !errors.Is(err, session.ErrInvalidCommand) {
		t.Fatalf("Transition() to an unknown status error = %v, want session.ErrInvalidCommand", err)
	}
}

func testSessionMerge(t *testing.T, factory SessionServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	created := createSession(t, service)

	branch, err := service.CreateBranch(ctx, session.CreateBranchCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, RunKey: "run-1", BaseRevision: created.Revision,
	})
	if err != nil || branch.Status != session.BranchStatusOpen || branch.BaseRevision != created.Revision {
		t.Fatalf("CreateBranch() = %+v, error %v", branch, err)
	}
	replayed, err := service.CreateBranch(ctx, session.CreateBranchCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, RunKey: "run-1", BaseRevision: created.Revision,
	})
	if err != nil || replayed.BranchKey != branch.BranchKey {
		t.Fatalf("replayed CreateBranch() = %+v, error %v", replayed, err)
	}
	if _, err := service.CreateBranch(ctx, session.CreateBranchCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, RunKey: "run-1", BaseRevision: created.Revision + 3,
	}); !errors.Is(err, session.ErrIdempotencyConflict) {
		t.Fatalf("CreateBranch() with a different base error = %v, want session.ErrIdempotencyConflict", err)
	}
	if _, err := service.CreateBranch(ctx, session.CreateBranchCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, RunKey: "run-2", BaseRevision: created.Revision + 3,
	}); !errors.Is(err, session.ErrRevisionConflict) {
		t.Fatalf("CreateBranch() from a revision that does not exist error = %v, want session.ErrRevisionConflict", err)
	}

	// A branch may only merge once it is ready, and a merge is a fast-forward:
	// the session must still be exactly at the branch's base.
	if _, err := service.CommitMerge(ctx, session.MergeCommit{
		TenantKey: sessionTenant, SessionKey: sessionKey, BranchKey: branch.BranchKey, ExpectedVersion: branch.Version,
	}); !errors.Is(err, session.ErrMergeConflict) {
		t.Fatalf("CommitMerge() on an open branch error = %v, want session.ErrMergeConflict", err)
	}
	ready, err := service.MarkBranchReady(ctx, session.BranchCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, BranchKey: branch.BranchKey,
		ExpectedVersion: branch.Version, HeadRevision: branch.BaseRevision,
	})
	if err != nil || ready.Status != session.BranchStatusReadyToMerge {
		t.Fatalf("MarkBranchReady() = %+v, error %v", ready, err)
	}
	if _, err := service.MarkBranchReady(ctx, session.BranchCommand{
		TenantKey: sessionTenant, SessionKey: sessionKey, BranchKey: branch.BranchKey,
		ExpectedVersion: branch.Version, HeadRevision: branch.BaseRevision,
	}); !errors.Is(err, session.ErrBranchVersionConflict) {
		t.Fatalf("MarkBranchReady() with a spent version error = %v, want session.ErrBranchVersionConflict", err)
	}

	merged, err := service.CommitMerge(ctx, session.MergeCommit{
		TenantKey: sessionTenant, SessionKey: sessionKey, BranchKey: branch.BranchKey, ExpectedVersion: ready.Version,
	})
	if err != nil || merged.SessionRevision != created.Revision+1 || merged.Branch.Status != session.BranchStatusMerged {
		t.Fatalf("CommitMerge() = %+v, error %v", merged, err)
	}
	// Merging again is the failure that silently duplicates a turn.
	if _, err := service.CommitMerge(ctx, session.MergeCommit{
		TenantKey: sessionTenant, SessionKey: sessionKey, BranchKey: branch.BranchKey, ExpectedVersion: ready.Version,
	}); !errors.Is(err, session.ErrMergeConflict) {
		t.Fatalf("CommitMerge() twice error = %v, want session.ErrMergeConflict", err)
	}
	if _, err := service.GetBranch(ctx, session.GetBranchQuery{TenantKey: sessionTenant, SessionKey: sessionKey, BranchKey: "absent"}); !errors.Is(err, session.ErrBranchNotFound) {
		t.Fatalf("GetBranch(absent) error = %v, want session.ErrBranchNotFound", err)
	}
}
