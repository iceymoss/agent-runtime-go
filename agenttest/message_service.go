package agenttest

import (
	"context"
	"errors"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/message"
)

// MessageServiceFactory returns a fresh, empty message service for one subtest.
type MessageServiceFactory func(t *testing.T) message.Service

// TestMessageService runs the persisted message aggregate conformance suite.
//
// A conversation transcript is the one thing a coding agent can never silently
// corrupt: a lost tool result makes the next turn incoherent, a duplicated
// ordinal reorders history, and a redaction that deletes rather than tombstones
// renumbers everything after it. The suite pins those properties.
func TestMessageService(t *testing.T, factory MessageServiceFactory) {
	t.Helper()
	t.Run("create is idempotent and allocates branch positions", func(t *testing.T) {
		testMessageCreate(t, factory)
	})
	t.Run("saving is guarded by revision, fence, and transition", func(t *testing.T) {
		testMessageSave(t, factory)
	})
	t.Run("redaction removes content but keeps the position", func(t *testing.T) {
		testMessageTombstone(t, factory)
	})
	t.Run("listings are scoped and ordered", func(t *testing.T) {
		testMessageListings(t, factory)
	})
	t.Run("tool results must match a call earlier in the branch", func(t *testing.T) {
		testMessageBranchCorrelation(t, factory)
	})
}

const (
	conformanceTenant  = agent.TenantKey("tenant-1")
	conformanceSession = "session-1"
	conformanceBranch  = "branch-1"
)

func userMessageCommand(key message.MessageKey, text string, visibleAt uint64) message.CreateCommand {
	return message.CreateCommand{
		TenantKey: conformanceTenant, MessageKey: key, SessionKey: conformanceSession, BranchKey: conformanceBranch,
		Role: agent.RoleUser, Parts: []agent.ContentPart{{Type: agent.PartText, Text: text}},
		State: message.StateBuilding, RunKey: "run-1", AttemptKey: "attempt-1", FenceToken: 1,
		VisibleAtRevision: visibleAt,
	}
}

func createMessage(t *testing.T, service message.Service, command message.CreateCommand) message.Snapshot {
	t.Helper()
	snapshot, err := service.Create(context.Background(), command)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return snapshot
}

func testMessageCreate(t *testing.T, factory MessageServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	first := createMessage(t, service, userMessageCommand("message-1", "hello", 1))
	if first.Revision != 1 || first.BranchOrdinal == 0 || first.CreatedAt.IsZero() {
		t.Fatalf("Create() = %+v", first)
	}
	// A replayed create must answer with the stored aggregate; a retried command
	// is not a new message.
	replayed := createMessage(t, service, userMessageCommand("message-1", "hello", 1))
	if replayed.Revision != first.Revision || replayed.BranchOrdinal != first.BranchOrdinal {
		t.Fatalf("replayed Create() = %+v, first = %+v", replayed, first)
	}
	if _, err := service.Create(ctx, userMessageCommand("message-1", "different", 1)); !errors.Is(err, message.ErrIdempotencyConflict) {
		t.Fatalf("Create() with different content error = %v, want message.ErrIdempotencyConflict", err)
	}

	// An unspecified ordinal appends; the branch stays densely ordered.
	second := createMessage(t, service, userMessageCommand("message-2", "second", 2))
	if second.BranchOrdinal <= first.BranchOrdinal {
		t.Fatalf("Create() did not append: %d then %d", first.BranchOrdinal, second.BranchOrdinal)
	}
	explicit := userMessageCommand("message-3", "third", 3)
	explicit.BranchOrdinal = first.BranchOrdinal
	if _, err := service.Create(ctx, explicit); !errors.Is(err, message.ErrOrdinalConflict) {
		t.Fatalf("Create() reusing an ordinal error = %v, want message.ErrOrdinalConflict", err)
	}

	incomplete := userMessageCommand("message-4", "fourth", 4)
	incomplete.FenceToken = 0
	if _, err := service.Create(ctx, incomplete); !errors.Is(err, message.ErrInvalidCommand) {
		t.Fatalf("Create() without a fence error = %v, want message.ErrInvalidCommand", err)
	}
	notBuilding := userMessageCommand("message-5", "fifth", 5)
	notBuilding.State = message.StateComplete
	if _, err := service.Create(ctx, notBuilding); !errors.Is(err, message.ErrInvalidCommand) {
		t.Fatalf("Create() in a non-building state error = %v, want message.ErrInvalidCommand", err)
	}
	if _, err := service.Get(ctx, message.GetQuery{TenantKey: conformanceTenant, MessageKey: "absent"}); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("Get(absent) error = %v, want message.ErrMessageNotFound", err)
	}
	if _, err := service.Get(ctx, message.GetQuery{TenantKey: "tenant-2", MessageKey: "message-1"}); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("Get() leaked across tenants: %v", err)
	}
}

func testMessageSave(t *testing.T, factory MessageServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	created := createMessage(t, service, userMessageCommand("message-1", "hello", 1))

	save := message.SaveCommand{
		TenantKey: conformanceTenant, MessageKey: "message-1", ExpectedRevision: created.Revision,
		AttemptKey: "attempt-1", FenceToken: 1, State: message.StateComplete,
		Parts: []agent.ContentPart{{Type: agent.PartText, Text: "hello there"}},
	}
	saved, err := service.SaveSnapshot(ctx, save)
	if err != nil || saved.Revision != created.Revision+1 || saved.State != message.StateComplete {
		t.Fatalf("SaveSnapshot() = %+v, error %v", saved, err)
	}

	tests := []struct {
		name    string
		mutate  func(message.SaveCommand) message.SaveCommand
		wantErr error
	}{
		{
			name:    "the already-spent revision",
			mutate:  func(c message.SaveCommand) message.SaveCommand { return c },
			wantErr: message.ErrRevisionConflict,
		},
		{
			name: "an older fence",
			mutate: func(c message.SaveCommand) message.SaveCommand {
				c.ExpectedRevision, c.FenceToken = saved.Revision, 0
				return c
			},
			wantErr: message.ErrInvalidCommand,
		},
		{
			name: "a different attempt without a higher fence",
			mutate: func(c message.SaveCommand) message.SaveCommand {
				c.ExpectedRevision, c.AttemptKey = saved.Revision, "attempt-2"
				return c
			},
			wantErr: message.ErrStaleFence,
		},
		{
			name: "a transition back to building",
			mutate: func(c message.SaveCommand) message.SaveCommand {
				c.ExpectedRevision, c.State = saved.Revision, message.StateBuilding
				return c
			},
			wantErr: message.ErrInvalidMessageTransition,
		},
		{
			name: "an unknown state",
			mutate: func(c message.SaveCommand) message.SaveCommand {
				c.ExpectedRevision, c.State = saved.Revision, message.State("invented")
				return c
			},
			wantErr: message.ErrInvalidCommand,
		},
	}
	for _, test := range tests {
		t.Run(test.name+" is rejected", func(t *testing.T) {
			if _, err := service.SaveSnapshot(ctx, test.mutate(save)); !errors.Is(err, test.wantErr) {
				t.Fatalf("SaveSnapshot() error = %v, want %v", err, test.wantErr)
			}
		})
	}
	if _, err := service.SaveSnapshot(ctx, message.SaveCommand{
		TenantKey: conformanceTenant, MessageKey: "absent", ExpectedRevision: 1,
		AttemptKey: "attempt-1", FenceToken: 1, State: message.StateComplete,
	}); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("SaveSnapshot(absent) error = %v, want message.ErrMessageNotFound", err)
	}
}

func testMessageTombstone(t *testing.T, factory MessageServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	created := createMessage(t, service, userMessageCommand("message-1", "a secret", 1))
	next := createMessage(t, service, userMessageCommand("message-2", "after", 2))

	tombstone := message.TombstoneCommand{
		TenantKey: conformanceTenant, MessageKey: "message-1", ExpectedRevision: created.Revision,
		AttemptKey: "attempt-1", FenceToken: 1,
	}
	redacted, err := service.Tombstone(ctx, tombstone)
	if err != nil || redacted.State != message.StateTombstoned {
		t.Fatalf("Tombstone() = %+v, error %v", redacted, err)
	}
	if len(redacted.Parts) != 0 || len(redacted.AdapterState) != 0 || redacted.FinishReason != "" {
		t.Fatalf("Tombstone() left content behind: %+v", redacted)
	}
	// The aggregate must keep its slot: deleting it would renumber the branch and
	// silently change what came after it.
	if redacted.BranchOrdinal != created.BranchOrdinal {
		t.Fatalf("Tombstone() moved the message: %d then %d", created.BranchOrdinal, redacted.BranchOrdinal)
	}
	listed, err := service.ListBranch(ctx, message.ListBranchQuery{TenantKey: conformanceTenant, SessionKey: conformanceSession, BranchKey: conformanceBranch})
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListBranch() after redaction = %d, error %v", len(listed), err)
	}
	if listed[1].MessageKey != next.MessageKey || listed[1].BranchOrdinal != next.BranchOrdinal {
		t.Fatalf("redaction disturbed the branch: %+v", listed)
	}
	if _, err := service.Tombstone(ctx, message.TombstoneCommand{
		TenantKey: conformanceTenant, MessageKey: "message-1", ExpectedRevision: redacted.Revision,
		AttemptKey: "attempt-1", FenceToken: 1,
	}); !errors.Is(err, message.ErrInvalidMessageTransition) {
		t.Fatalf("Tombstone() twice error = %v, want message.ErrInvalidMessageTransition", err)
	}
	if _, err := service.Tombstone(ctx, message.TombstoneCommand{
		TenantKey: conformanceTenant, MessageKey: "message-2", ExpectedRevision: 0,
		AttemptKey: "attempt-1", FenceToken: 1,
	}); !errors.Is(err, message.ErrInvalidCommand) {
		t.Fatalf("Tombstone() without an expected revision error = %v, want message.ErrInvalidCommand", err)
	}
}

func testMessageListings(t *testing.T, factory MessageServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	createMessage(t, service, userMessageCommand("message-1", "first", 1))
	createMessage(t, service, userMessageCommand("message-2", "second", 3))
	other := userMessageCommand("message-3", "other branch", 1)
	other.BranchKey = "branch-2"
	createMessage(t, service, other)

	branch, err := service.ListBranch(ctx, message.ListBranchQuery{TenantKey: conformanceTenant, SessionKey: conformanceSession, BranchKey: conformanceBranch})
	if err != nil || len(branch) != 2 {
		t.Fatalf("ListBranch() = %d, error %v", len(branch), err)
	}
	if branch[0].BranchOrdinal >= branch[1].BranchOrdinal {
		t.Fatalf("ListBranch() is not ordered by position: %+v", branch)
	}
	if _, err := service.ListBranch(ctx, message.ListBranchQuery{TenantKey: conformanceTenant, SessionKey: conformanceSession}); !errors.Is(err, message.ErrInvalidCommand) {
		t.Fatalf("ListBranch() without a branch error = %v, want message.ErrInvalidCommand", err)
	}

	// Visibility is what makes a message part of the conversation. A message that
	// becomes visible at a later revision must not appear in an earlier read.
	visible, err := service.ListVisible(ctx, message.ListVisibleQuery{TenantKey: conformanceTenant, SessionKey: conformanceSession, Revision: 2})
	if err != nil || len(visible) != 2 {
		t.Fatalf("ListVisible(revision 2) = %d, error %v", len(visible), err)
	}
	all, err := service.ListVisible(ctx, message.ListVisibleQuery{TenantKey: conformanceTenant, SessionKey: conformanceSession, Revision: 3})
	if err != nil || len(all) != 3 {
		t.Fatalf("ListVisible(revision 3) = %d, error %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].VisibleAtRevision > all[i].VisibleAtRevision {
			t.Fatalf("ListVisible() is not ordered by visibility: %+v", all)
		}
	}
	if none, err := service.ListVisible(ctx, message.ListVisibleQuery{TenantKey: "tenant-2", SessionKey: conformanceSession, Revision: 9}); err != nil || len(none) != 0 {
		t.Fatalf("ListVisible() leaked across tenants: %d, error %v", len(none), err)
	}
}

func testMessageBranchCorrelation(t *testing.T, factory MessageServiceFactory) {
	service := factory(t)
	ctx := context.Background()
	call := agent.ToolCall{ID: "call-1", Name: "probe", Input: `{}`}
	assistant := message.CreateCommand{
		TenantKey: conformanceTenant, MessageKey: "message-1", SessionKey: conformanceSession, BranchKey: conformanceBranch,
		Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}},
		State: message.StateBuilding, FinishReason: agent.FinishToolCalls,
		RunKey: "run-1", AttemptKey: "attempt-1", FenceToken: 1, VisibleAtRevision: 1,
	}
	createMessage(t, service, assistant)

	orphan := agent.ToolResult{ToolCallID: "call-missing", Name: "probe", Content: "ok"}
	orphanMessage := message.CreateCommand{
		TenantKey: conformanceTenant, MessageKey: "message-orphan", SessionKey: conformanceSession, BranchKey: conformanceBranch,
		Role: agent.RoleTool, Parts: []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &orphan}},
		State: message.StateBuilding, RunKey: "run-1", AttemptKey: "attempt-1", FenceToken: 1, VisibleAtRevision: 2,
	}
	// A tool result with no matching call is exactly the shape that breaks a
	// provider request on the next turn, so it must be refused at write time.
	if _, err := service.Create(ctx, orphanMessage); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("Create() with an orphan tool result error = %v, want message.ErrSnapshotInvariant", err)
	}

	result := agent.ToolResult{ToolCallID: "call-1", Name: "probe", Content: "ok"}
	toolMessage := orphanMessage
	toolMessage.MessageKey = "message-2"
	toolMessage.Parts = []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &result}}
	createMessage(t, service, toolMessage)

	duplicate := toolMessage
	duplicate.MessageKey = "message-3"
	duplicate.VisibleAtRevision = 3
	if _, err := service.Create(ctx, duplicate); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("Create() with a second result for one call error = %v, want message.ErrSnapshotInvariant", err)
	}
	repeatedCall := assistant
	repeatedCall.MessageKey = "message-4"
	repeatedCall.VisibleAtRevision = 4
	if _, err := service.Create(ctx, repeatedCall); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("Create() reusing a tool call ID error = %v, want message.ErrSnapshotInvariant", err)
	}
}
