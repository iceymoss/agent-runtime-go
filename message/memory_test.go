package message_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/message"
)

func TestMemoryCreateIdempotencyAndDeepClone(t *testing.T) {
	service := message.NewMemory()
	call := &agent.ToolCall{ID: "call-1", Name: "lookup", Input: `{"city":"Beijing"}`}
	command := createCommand("tenant-a", "message-1", 0, agent.RoleAssistant, []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: call}})
	command.AdapterState = []byte("opaque")

	created, err := service.Create(context.Background(), command)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if created.Revision != 1 || created.BranchOrdinal != 1 || created.State != message.StateBuilding {
		t.Fatalf("Create() = revision %d ordinal %d state %q", created.Revision, created.BranchOrdinal, created.State)
	}

	call.Name = "mutated"
	command.AdapterState[0] = 'X'
	created.Parts[0].ToolCall.Name = "returned mutation"
	created.AdapterState[0] = 'Y'
	got, err := service.Get(context.Background(), message.GetQuery{TenantKey: "tenant-a", MessageKey: "message-1"})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Parts[0].ToolCall.Name != "lookup" || string(got.AdapterState) != "opaque" {
		t.Fatalf("stored snapshot was aliased: %+v %q", got.Parts[0].ToolCall, got.AdapterState)
	}

	replay := createCommand("tenant-a", "message-1", 0, agent.RoleAssistant, []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &agent.ToolCall{ID: "call-1", Name: "lookup", Input: `{"city":"Beijing"}`}}})
	replay.AdapterState = []byte("opaque")
	replayed, err := service.Create(context.Background(), replay)
	if err != nil {
		t.Fatalf("idempotent Create() error = %v", err)
	}
	if replayed.Revision != 1 || replayed.BranchOrdinal != 1 {
		t.Fatalf("idempotent Create() = %+v", replayed)
	}

	replay.StepIndex++
	if _, err := service.Create(context.Background(), replay); !errors.Is(err, message.ErrIdempotencyConflict) {
		t.Fatalf("mismatched Create() error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestMemoryImageBytesDoNotAliasCreateOrGet(t *testing.T) {
	service := message.NewMemory()
	data := []byte{1, 2, 3}
	command := createCommand("tenant-a", "image-1", 0, agent.RoleUser, []agent.ContentPart{{Type: agent.PartImage, Image: &agent.ImageContent{MediaType: "image/png", Data: data}}})
	created, err := service.Create(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 9
	created.Parts[0].Image.Data[1] = 9
	loaded, err := service.Get(context.Background(), message.GetQuery{TenantKey: "tenant-a", MessageKey: "image-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Parts[0].Image.Data; !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("stored image aliased: %v", got)
	}
}

func TestMemoryCreateValidationAndOrdinalScope(t *testing.T) {
	service := message.NewMemory()
	tests := []struct {
		name    string
		command message.CreateCommand
		want    error
	}{
		{name: "tenant required", command: createCommand("", "m", 0, agent.RoleUser, textParts("hello")), want: message.ErrInvalidCommand},
		{name: "invalid role", command: createCommand("tenant", "m", 0, agent.Role("alien"), textParts("hello")), want: message.ErrSnapshotInvariant},
		{name: "empty parts", command: createCommand("tenant", "m", 0, agent.RoleUser, nil), want: message.ErrSnapshotInvariant},
		{name: "terminal initial state", command: func() message.CreateCommand {
			command := createCommand("tenant", "m", 0, agent.RoleUser, textParts("hello"))
			command.State = message.StateComplete
			return command
		}(), want: message.ErrInvalidCommand},
		{name: "duplicate calls", command: createCommand("tenant", "m", 0, agent.RoleAssistant, []agent.ContentPart{
			{Type: agent.PartToolCall, ToolCall: &agent.ToolCall{ID: "same", Name: "one"}},
			{Type: agent.PartToolCall, ToolCall: &agent.ToolCall{ID: "same", Name: "two"}},
		}), want: message.ErrSnapshotInvariant},
		{name: "orphan result", command: createCommand("tenant", "m", 0, agent.RoleTool, []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &agent.ToolResult{ToolCallID: "missing", Name: "one"}}}), want: message.ErrSnapshotInvariant},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := service.Create(context.Background(), test.command); !errors.Is(err, test.want) {
				t.Fatalf("Create() error = %v, want %v", err, test.want)
			}
		})
	}

	first, err := service.Create(context.Background(), createCommand("tenant", "first", 4, agent.RoleUser, textParts("one")))
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	if first.BranchOrdinal != 4 {
		t.Fatalf("first ordinal = %d", first.BranchOrdinal)
	}
	if _, err := service.Create(context.Background(), createCommand("tenant", "duplicate", 4, agent.RoleUser, textParts("two"))); !errors.Is(err, message.ErrOrdinalConflict) {
		t.Fatalf("duplicate ordinal error = %v", err)
	}
	second, err := service.Create(context.Background(), createCommand("tenant", "second", 0, agent.RoleUser, textParts("two")))
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if second.BranchOrdinal != 5 {
		t.Fatalf("allocated ordinal = %d, want 5", second.BranchOrdinal)
	}
	otherBranch := createCommand("tenant", "other-branch", 0, agent.RoleUser, textParts("other"))
	otherBranch.BranchKey = "branch-2"
	created, err := service.Create(context.Background(), otherBranch)
	if err != nil || created.BranchOrdinal != 1 {
		t.Fatalf("other branch Create() = ordinal %d, error %v", created.BranchOrdinal, err)
	}
}

func TestMemorySaveCASFenceTransitionsAndReplacement(t *testing.T) {
	service := message.NewMemory()
	created, err := service.Create(context.Background(), createCommand("tenant", "message", 0, agent.RoleAssistant, textParts("partial")))
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	first := message.SaveCommand{
		TenantKey:        "tenant",
		MessageKey:       "message",
		ExpectedRevision: created.Revision,
		AttemptKey:       "attempt-2",
		FenceToken:       2,
		State:            message.StateBuilding,
		Parts:            textParts("full replacement"),
		AdapterState:     []byte("state-2"),
	}
	saved, err := service.SaveSnapshot(context.Background(), first)
	if err != nil {
		t.Fatalf("SaveSnapshot() error = %v", err)
	}
	first.Parts[0].Text = "mutated"
	first.AdapterState[0] = 'X'
	if saved.Revision != 2 || saved.FenceToken != 2 || len(saved.Parts) != 1 || saved.Parts[0].Text != "full replacement" {
		t.Fatalf("SaveSnapshot() = %+v", saved)
	}

	staleFence := first
	staleFence.ExpectedRevision = 2
	staleFence.FenceToken = 1
	if _, err := service.SaveSnapshot(context.Background(), staleFence); !errors.Is(err, message.ErrStaleFence) {
		t.Fatalf("stale fence error = %v", err)
	}
	staleRevision := first
	staleRevision.FenceToken = 3
	staleRevision.ExpectedRevision = 1
	if _, err := service.SaveSnapshot(context.Background(), staleRevision); !errors.Is(err, message.ErrRevisionConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
	wrongAttempt := first
	wrongAttempt.ExpectedRevision = 2
	wrongAttempt.AttemptKey = "other-attempt"
	if _, err := service.SaveSnapshot(context.Background(), wrongAttempt); !errors.Is(err, message.ErrStaleFence) {
		t.Fatalf("same-fence wrong attempt error = %v", err)
	}

	terminal := message.SaveCommand{
		TenantKey:        "tenant",
		MessageKey:       "message",
		ExpectedRevision: 2,
		AttemptKey:       "attempt-2",
		FenceToken:       2,
		State:            message.StateComplete,
		FinishReason:     agent.FinishStop,
		Parts:            textParts("done"),
	}
	completed, err := service.SaveSnapshot(context.Background(), terminal)
	if err != nil {
		t.Fatalf("terminal SaveSnapshot() error = %v", err)
	}
	terminal.ExpectedRevision = completed.Revision
	terminal.State = message.StateBuilding
	if _, err := service.SaveSnapshot(context.Background(), terminal); !errors.Is(err, message.ErrInvalidMessageTransition) {
		t.Fatalf("terminal regression error = %v", err)
	}
	terminal.State = message.StateComplete
	if _, err := service.SaveSnapshot(context.Background(), terminal); !errors.Is(err, message.ErrInvalidMessageTransition) {
		t.Fatalf("terminal mutation error = %v", err)
	}
}

func TestMemoryToolResultCorrelationAcrossBranch(t *testing.T) {
	service := message.NewMemory()
	callCommand := createCommand("tenant", "call-message", 0, agent.RoleAssistant, []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &agent.ToolCall{ID: "call-1", Name: "lookup"}}})
	if _, err := service.Create(context.Background(), callCommand); err != nil {
		t.Fatalf("Create(call) error = %v", err)
	}
	resultCommand := createCommand("tenant", "result-message", 0, agent.RoleTool, []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &agent.ToolResult{ToolCallID: "call-1", Name: "lookup", Content: "ok"}}})
	result, err := service.Create(context.Background(), resultCommand)
	if err != nil {
		t.Fatalf("Create(result) error = %v", err)
	}
	duplicate := createCommand("tenant", "duplicate-result", 0, agent.RoleTool, []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &agent.ToolResult{ToolCallID: "call-1", Name: "lookup"}}})
	if _, err := service.Create(context.Background(), duplicate); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("duplicate result error = %v", err)
	}
	mismatch := createCommand("tenant", "mismatch-result", 0, agent.RoleTool, []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &agent.ToolResult{ToolCallID: "call-1", Name: "other"}}})
	if _, err := service.Create(context.Background(), mismatch); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("mismatched result error = %v", err)
	}
	early := createCommand("tenant", "early-result", 1, agent.RoleTool, []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &agent.ToolResult{ToolCallID: "call-1", Name: "lookup"}}})
	early.BranchKey = "ordered-branch"
	lateCall := createCommand("tenant", "late-call", 2, agent.RoleAssistant, []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &agent.ToolCall{ID: "call-1", Name: "lookup"}}})
	lateCall.BranchKey = "ordered-branch"
	if _, err := service.Create(context.Background(), early); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("result without earlier call error = %v", err)
	}
	if _, err := service.Create(context.Background(), lateCall); err != nil {
		t.Fatalf("Create(late call) error = %v", err)
	}
	if _, err := service.Create(context.Background(), early); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("result ordered before call error = %v", err)
	}

	result.Parts[0].ToolResult.Content = "changed"
	loaded, err := service.Get(context.Background(), message.GetQuery{TenantKey: "tenant", MessageKey: "result-message"})
	if err != nil || loaded.Parts[0].ToolResult.Content != "ok" {
		t.Fatalf("Get(result) = %+v, error %v", loaded, err)
	}
	if _, err := service.Tombstone(context.Background(), message.TombstoneCommand{
		TenantKey: "tenant", MessageKey: "call-message", ExpectedRevision: 1, AttemptKey: "attempt", FenceToken: 1,
	}); !errors.Is(err, message.ErrSnapshotInvariant) {
		t.Fatalf("Tombstone(call with retained result) error = %v", err)
	}
}

func TestMemoryTenantQueriesVisibilityAndTombstone(t *testing.T) {
	service := message.NewMemory()
	commands := []message.CreateCommand{
		createCommand("tenant", "m3", 3, agent.RoleUser, textParts("three")),
		createCommand("tenant", "m1", 1, agent.RoleUser, textParts("one")),
		createCommand("tenant", "m2", 2, agent.RoleUser, textParts("two")),
	}
	commands[0].VisibleAtRevision = 3
	commands[1].VisibleAtRevision = 1
	commands[2].VisibleAtRevision = 0
	for _, command := range commands {
		if _, err := service.Create(context.Background(), command); err != nil {
			t.Fatalf("Create(%s) error = %v", command.MessageKey, err)
		}
	}

	branch, err := service.ListBranch(context.Background(), message.ListBranchQuery{TenantKey: "tenant", SessionKey: "session", BranchKey: "branch"})
	if err != nil {
		t.Fatalf("ListBranch() error = %v", err)
	}
	if got := keys(branch); fmt.Sprint(got) != "[m1 m2 m3]" {
		t.Fatalf("ListBranch() keys = %v", got)
	}
	visible, err := service.ListVisible(context.Background(), message.ListVisibleQuery{TenantKey: "tenant", SessionKey: "session", Revision: 2})
	if err != nil {
		t.Fatalf("ListVisible() error = %v", err)
	}
	if got := keys(visible); fmt.Sprint(got) != "[m1]" {
		t.Fatalf("ListVisible(revision 2) keys = %v", got)
	}
	visible, err = service.ListVisible(context.Background(), message.ListVisibleQuery{TenantKey: "tenant", SessionKey: "session", Revision: 3})
	if err != nil || fmt.Sprint(keys(visible)) != "[m1 m3]" {
		t.Fatalf("ListVisible(revision 3) = %v, error %v", keys(visible), err)
	}

	if _, err := service.Get(context.Background(), message.GetQuery{TenantKey: "other-tenant", MessageKey: "m1"}); !errors.Is(err, message.ErrMessageNotFound) {
		t.Fatalf("wrong-tenant Get() error = %v", err)
	}
	other, err := service.ListBranch(context.Background(), message.ListBranchQuery{TenantKey: "other-tenant", SessionKey: "session", BranchKey: "branch"})
	if err != nil || len(other) != 0 {
		t.Fatalf("wrong-tenant ListBranch() = %v, error %v", other, err)
	}

	tombstoned, err := service.Tombstone(context.Background(), message.TombstoneCommand{
		TenantKey: "tenant", MessageKey: "m1", ExpectedRevision: 1, AttemptKey: "attempt", FenceToken: 1,
	})
	if err != nil {
		t.Fatalf("Tombstone() error = %v", err)
	}
	if tombstoned.State != message.StateTombstoned || tombstoned.Revision != 2 || tombstoned.Parts != nil || tombstoned.AdapterState != nil {
		t.Fatalf("Tombstone() = %+v", tombstoned)
	}
	if _, err := service.Tombstone(context.Background(), message.TombstoneCommand{
		TenantKey: "tenant", MessageKey: "m1", ExpectedRevision: 2, AttemptKey: "attempt", FenceToken: 1,
	}); !errors.Is(err, message.ErrInvalidMessageTransition) {
		t.Fatalf("repeated Tombstone() error = %v", err)
	}
}

func TestMemoryContextHandling(t *testing.T) {
	service := message.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Create(ctx, createCommand("tenant", "message", 0, agent.RoleUser, textParts("hello"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create(canceled) error = %v", err)
	}
	if _, err := service.Get(nil, message.GetQuery{TenantKey: "tenant", MessageKey: "message"}); !errors.Is(err, message.ErrInvalidCommand) {
		t.Fatalf("Get(nil) error = %v", err)
	}
}

func TestMemoryConcurrentOrdinalAllocation(t *testing.T) {
	service := message.NewMemory()
	const count = 64
	ordinals := make(chan uint64, count)
	errorsCh := make(chan error, count)
	var wait sync.WaitGroup
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			created, err := service.Create(context.Background(), createCommand("tenant", message.MessageKey(fmt.Sprintf("m-%03d", index)), 0, agent.RoleUser, textParts("hello")))
			if err != nil {
				errorsCh <- err
				return
			}
			ordinals <- created.BranchOrdinal
		}(i)
	}
	wait.Wait()
	close(errorsCh)
	close(ordinals)
	for err := range errorsCh {
		t.Errorf("Create() error = %v", err)
	}
	got := make([]int, 0, count)
	for ordinal := range ordinals {
		got = append(got, int(ordinal))
	}
	sort.Ints(got)
	if len(got) != count {
		t.Fatalf("allocated %d ordinals, want %d", len(got), count)
	}
	for i, ordinal := range got {
		if ordinal != i+1 {
			t.Fatalf("ordinal[%d] = %d, want %d", i, ordinal, i+1)
		}
	}
}

func TestMemoryConcurrentCASOneWinner(t *testing.T) {
	service := message.NewMemory()
	if _, err := service.Create(context.Background(), createCommand("tenant", "message", 0, agent.RoleUser, textParts("initial"))); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	const count = 32
	var wait sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	conflicts := 0
	otherErrors := make([]error, 0)
	for i := 0; i < count; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := service.SaveSnapshot(context.Background(), message.SaveCommand{
				TenantKey: "tenant", MessageKey: "message", ExpectedRevision: 1,
				AttemptKey: "attempt", FenceToken: 1, State: message.StateBuilding,
				Parts: textParts(fmt.Sprintf("writer-%d", index)),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners++
			case errors.Is(err, message.ErrRevisionConflict):
				conflicts++
			default:
				otherErrors = append(otherErrors, err)
			}
		}(i)
	}
	wait.Wait()
	if winners != 1 || conflicts != count-1 || len(otherErrors) != 0 {
		t.Fatalf("winners=%d conflicts=%d other=%v", winners, conflicts, otherErrors)
	}
	got, err := service.Get(context.Background(), message.GetQuery{TenantKey: "tenant", MessageKey: "message"})
	if err != nil || got.Revision != 2 {
		t.Fatalf("Get() = revision %d, error %v", got.Revision, err)
	}
}

func createCommand(tenant agent.TenantKey, key message.MessageKey, ordinal uint64, role agent.Role, parts []agent.ContentPart) message.CreateCommand {
	return message.CreateCommand{
		TenantKey: tenant, MessageKey: key, SessionKey: "session", BranchKey: "branch", BranchOrdinal: ordinal,
		Role: role, Parts: parts, State: message.StateBuilding, RunKey: "run", AttemptKey: "attempt", FenceToken: 1,
	}
}

func textParts(text string) []agent.ContentPart {
	return []agent.ContentPart{{Type: agent.PartText, Text: text}}
}

func keys(snapshots []message.Snapshot) []message.MessageKey {
	result := make([]message.MessageKey, len(snapshots))
	for i, snapshot := range snapshots {
		result[i] = snapshot.MessageKey
	}
	return result
}
