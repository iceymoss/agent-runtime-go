package message

import (
	"fmt"
	"sort"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// This file is the message aggregate's state machine, expressed as pure
// functions over values.
//
// It is exported because a storage adapter is supposed to be a small amount of
// SQL, not a second copy of these rules. An adapter loads the current snapshot,
// calls the matching Apply function, and writes the result back under its own
// revision compare-and-set; every legality decision stays here, so two adapters
// cannot disagree about what a valid message transition is.
//
// The functions never touch storage and never read a clock: the caller supplies
// the timestamp so an adapter can make its writes reproducible.

// ApplyCreate builds the first snapshot of a message aggregate.
//
// ordinal is the branch position the caller allocated. Pass the command's own
// BranchOrdinal when it is non-zero; otherwise pass the next free ordinal in the
// branch, which only the storage layer can know.
func ApplyCreate(command CreateCommand, ordinal uint64, now time.Time) (Snapshot, error) {
	command = cloneCreateCommand(command)
	if command.State == "" {
		command.State = StateBuilding
	}
	if err := ValidateCreate(command); err != nil {
		return Snapshot{}, err
	}
	if ordinal == 0 {
		return Snapshot{}, fmt.Errorf("%w: a branch ordinal must be allocated", ErrInvalidCommand)
	}
	if now.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: a creation time is required", ErrInvalidCommand)
	}
	now = now.UTC()
	return Snapshot{
		TenantKey: command.TenantKey, MessageKey: command.MessageKey,
		SessionKey: command.SessionKey, BranchKey: command.BranchKey, BranchOrdinal: ordinal,
		Role: command.Role, Parts: cloneParts(command.Parts), State: command.State,
		FinishReason: command.FinishReason, ModelKey: command.ModelKey, ProviderKey: command.ProviderKey,
		AdapterState: cloneBytes(command.AdapterState), RunKey: command.RunKey,
		AttemptKey: command.AttemptKey, FenceToken: command.FenceToken, StepIndex: command.StepIndex,
		Revision: 1, VisibleAtRevision: command.VisibleAtRevision, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// ApplySave produces the next snapshot of an existing aggregate.
//
// The guards are the reason this is not a plain field assignment: an older fence
// means a superseded attempt is writing, an unexpected revision means someone
// else already wrote, and an attempt change without a higher fence means two
// workers believe they own the same message.
func ApplySave(current Snapshot, command SaveCommand, now time.Time) (Snapshot, error) {
	if err := ValidateSave(command); err != nil {
		return Snapshot{}, err
	}
	if err := guardMutation(current, command.AttemptKey, command.FenceToken, command.ExpectedRevision); err != nil {
		return Snapshot{}, err
	}
	if !ValidTransition(current.State, command.State) {
		return Snapshot{}, fmt.Errorf("%w: %s to %s", ErrInvalidMessageTransition, current.State, command.State)
	}
	if now.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: an update time is required", ErrInvalidCommand)
	}
	next := cloneSnapshot(current)
	next.AttemptKey, next.FenceToken = command.AttemptKey, command.FenceToken
	next.State, next.FinishReason = command.State, command.FinishReason
	next.Parts, next.AdapterState = cloneParts(command.Parts), cloneBytes(command.AdapterState)
	next.Revision++
	next.UpdatedAt = now.UTC()
	if err := ValidateSnapshot(next); err != nil {
		return Snapshot{}, err
	}
	return next, nil
}

// ApplyTombstone redacts a message through the same guards a save uses.
//
// Redaction is a state transition rather than a delete so the aggregate keeps
// its position in the branch: removing the row would silently renumber history.
func ApplyTombstone(current Snapshot, command TombstoneCommand, now time.Time) (Snapshot, error) {
	if !command.TenantKey.Valid() || command.MessageKey == "" || command.ExpectedRevision == 0 || command.AttemptKey == "" || command.FenceToken == 0 {
		return Snapshot{}, fmt.Errorf("%w: incomplete tombstone command", ErrInvalidCommand)
	}
	if err := guardMutation(current, command.AttemptKey, command.FenceToken, command.ExpectedRevision); err != nil {
		return Snapshot{}, err
	}
	if !ValidTransition(current.State, StateTombstoned) {
		return Snapshot{}, fmt.Errorf("%w: %s to %s", ErrInvalidMessageTransition, current.State, StateTombstoned)
	}
	if now.IsZero() {
		return Snapshot{}, fmt.Errorf("%w: an update time is required", ErrInvalidCommand)
	}
	next := cloneSnapshot(current)
	next.Parts, next.AdapterState, next.FinishReason = nil, nil, ""
	next.State = StateTombstoned
	next.AttemptKey, next.FenceToken = command.AttemptKey, command.FenceToken
	next.Revision++
	next.UpdatedAt = now.UTC()
	return next, nil
}

// guardMutation applies the three checks every cumulative write shares.
func guardMutation(current Snapshot, attemptKey string, fenceToken, expectedRevision uint64) error {
	if fenceToken < current.FenceToken {
		return fmt.Errorf("%w: got %d, current %d", ErrStaleFence, fenceToken, current.FenceToken)
	}
	if expectedRevision != current.Revision {
		return fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, expectedRevision, current.Revision)
	}
	if fenceToken == current.FenceToken && attemptKey != current.AttemptKey {
		return fmt.Errorf("%w: attempt changed without a higher fence", ErrStaleFence)
	}
	return nil
}

// ValidateCreate reports whether a create command can produce a legal aggregate.
func ValidateCreate(command CreateCommand) error { return validateCreate(command) }

// ValidateSave reports whether a save command is well formed. It does not
// compare against stored state; ApplySave does that.
func ValidateSave(command SaveCommand) error { return validateSave(command) }

// ValidateSnapshot reports whether a snapshot's content is internally coherent:
// a redacted state carries no content, tool calls and results are unique within
// the message, and a finish reason only appears on an assistant message.
func ValidateSnapshot(snapshot Snapshot) error { return validateSnapshotContent(snapshot) }

// ValidTransition reports whether a message may move from one state to another.
// Building may be re-saved; every other state is a one-way step toward redaction.
func ValidTransition(from, to State) bool { return validTransition(from, to) }

// SameCreate reports whether a replayed create describes the identical
// aggregate, so an adapter can answer a retry with the stored snapshot instead
// of a conflict. allocatedOrdinal is the ordinal the stored aggregate received,
// which is what an unspecified ordinal in either command resolves to.
func SameCreate(left, right CreateCommand, allocatedOrdinal uint64) bool {
	return sameCreate(left, right, allocatedOrdinal)
}

// ValidateBranchCorrelation checks the invariants that span a whole branch
// rather than one message: a tool call ID appears once, every tool result
// matches an earlier call by ID and name, and no call has two results.
//
// siblings must be the other messages of the same tenant, session, and branch.
// Redacted messages carry no content and may be omitted.
func ValidateBranchCorrelation(candidate Snapshot, siblings []Snapshot) error {
	type call struct {
		name    string
		ordinal uint64
	}
	calls := make(map[string]call)
	results := make(map[string]struct{})
	snapshots := make([]Snapshot, 0, len(siblings)+1)
	for _, sibling := range siblings {
		if sibling.State == StateTombstoned || sibling.State == StateTerminal {
			continue
		}
		if sibling.TenantKey != candidate.TenantKey || sibling.SessionKey != candidate.SessionKey || sibling.BranchKey != candidate.BranchKey {
			continue
		}
		if sibling.MessageKey == candidate.MessageKey {
			continue
		}
		snapshots = append(snapshots, sibling)
	}
	if candidate.State != StateTombstoned && candidate.State != StateTerminal {
		snapshots = append(snapshots, candidate)
	}
	for _, snapshot := range snapshots {
		for _, part := range snapshot.Parts {
			if part.ToolCall == nil {
				continue
			}
			if _, exists := calls[part.ToolCall.ID]; exists {
				return fmt.Errorf("%w: duplicate branch tool call ID %q", ErrSnapshotInvariant, part.ToolCall.ID)
			}
			calls[part.ToolCall.ID] = call{name: part.ToolCall.Name, ordinal: snapshot.BranchOrdinal}
		}
	}
	for _, snapshot := range snapshots {
		for _, part := range snapshot.Parts {
			if part.ToolResult == nil {
				continue
			}
			result := part.ToolResult
			matched, exists := calls[result.ToolCallID]
			if !exists || matched.name != result.Name || matched.ordinal >= snapshot.BranchOrdinal {
				return fmt.Errorf("%w: tool result %q has no matching call", ErrSnapshotInvariant, result.ToolCallID)
			}
			if _, exists := results[result.ToolCallID]; exists {
				return fmt.Errorf("%w: tool call %q has multiple results", ErrSnapshotInvariant, result.ToolCallID)
			}
			results[result.ToolCallID] = struct{}{}
		}
	}
	return nil
}

// ValidateGetQuery reports whether a single-message lookup is addressable.
func ValidateGetQuery(query GetQuery) error {
	if !query.TenantKey.Valid() || query.MessageKey == "" {
		return ErrInvalidCommand
	}
	return nil
}

// ValidateListBranchQuery reports whether a branch listing is addressable.
func ValidateListBranchQuery(query ListBranchQuery) error {
	if !query.TenantKey.Valid() || query.SessionKey == "" {
		return fmt.Errorf("%w: tenant and session are required", ErrInvalidCommand)
	}
	if query.BranchKey == "" {
		return fmt.Errorf("%w: branch key is required", ErrInvalidCommand)
	}
	return nil
}

// ValidateListVisibleQuery reports whether a visibility listing is addressable.
func ValidateListVisibleQuery(query ListVisibleQuery) error {
	if !query.TenantKey.Valid() || query.SessionKey == "" {
		return fmt.Errorf("%w: tenant and session are required", ErrInvalidCommand)
	}
	return nil
}

// VisibleAt reports whether a snapshot is part of the conversation at the given
// session revision. A message with no visibility revision has not been published.
func VisibleAt(snapshot Snapshot, revision uint64) bool {
	return snapshot.VisibleAtRevision > 0 && snapshot.VisibleAtRevision <= revision
}

// SortBranch orders a branch listing the way readers expect it: by position, and
// by message key when two messages somehow share one.
func SortBranch(snapshots []Snapshot) {
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].BranchOrdinal != snapshots[j].BranchOrdinal {
			return snapshots[i].BranchOrdinal < snapshots[j].BranchOrdinal
		}
		return snapshots[i].MessageKey < snapshots[j].MessageKey
	})
}

// SortVisible orders a visibility listing by when each message became visible,
// then by branch position, so a conversation reads in the order it happened.
func SortVisible(snapshots []Snapshot) {
	sort.Slice(snapshots, func(i, j int) bool {
		if snapshots[i].VisibleAtRevision != snapshots[j].VisibleAtRevision {
			return snapshots[i].VisibleAtRevision < snapshots[j].VisibleAtRevision
		}
		if snapshots[i].BranchKey != snapshots[j].BranchKey {
			return snapshots[i].BranchKey < snapshots[j].BranchKey
		}
		if snapshots[i].BranchOrdinal != snapshots[j].BranchOrdinal {
			return snapshots[i].BranchOrdinal < snapshots[j].BranchOrdinal
		}
		return snapshots[i].MessageKey < snapshots[j].MessageKey
	})
}

// CloneSnapshot returns a deep copy. Every boundary in this runtime hands out
// copies so a caller that mutates what it received cannot change stored state.
func CloneSnapshot(snapshot Snapshot) Snapshot { return cloneSnapshot(snapshot) }

// CloneParts returns a deep copy of message content.
func CloneParts(parts []agent.ContentPart) []agent.ContentPart { return cloneParts(parts) }
