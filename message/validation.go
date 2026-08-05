package message

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
)

func validateCreate(command CreateCommand) error {
	if !command.TenantKey.Valid() || command.MessageKey == "" || command.SessionKey == "" || command.BranchKey == "" || command.RunKey == "" || command.AttemptKey == "" || command.FenceToken == 0 {
		return fmt.Errorf("%w: tenant, keys, attempt, and fence are required", ErrInvalidCommand)
	}
	if command.State != StateBuilding {
		return fmt.Errorf("%w: initial state must be %q", ErrInvalidCommand, StateBuilding)
	}
	snapshot := Snapshot{Role: command.Role, Parts: command.Parts, State: command.State, FinishReason: command.FinishReason}
	return validateSnapshotContent(snapshot)
}

func validateSave(command SaveCommand) error {
	if !command.TenantKey.Valid() || command.MessageKey == "" || command.ExpectedRevision == 0 || command.AttemptKey == "" || command.FenceToken == 0 {
		return fmt.Errorf("%w: tenant, key, expected revision, attempt, and fence are required", ErrInvalidCommand)
	}
	if !knownState(command.State) {
		return fmt.Errorf("%w: invalid state %q", ErrInvalidCommand, command.State)
	}
	return nil
}

func validateSnapshotContent(snapshot Snapshot) error {
	if snapshot.State == StateTombstoned || snapshot.State == StateTerminal {
		if len(snapshot.Parts) != 0 || len(snapshot.AdapterState) != 0 || snapshot.FinishReason != "" {
			return fmt.Errorf("%w: redacted state contains message content", ErrSnapshotInvariant)
		}
		return nil
	}
	if err := agent.ValidateMessage(agent.Message{Role: snapshot.Role, Parts: snapshot.Parts, FinishReason: snapshot.FinishReason}); err != nil {
		return fmt.Errorf("%w: %v", ErrSnapshotInvariant, err)
	}
	if snapshot.Role != agent.RoleAssistant && snapshot.FinishReason != "" {
		return fmt.Errorf("%w: finish reason is only valid for assistant messages", ErrSnapshotInvariant)
	}
	if snapshot.FinishReason != "" && snapshot.FinishReason != agent.FinishStop && snapshot.FinishReason != agent.FinishToolCalls && snapshot.FinishReason != agent.FinishLength && snapshot.FinishReason != agent.FinishError {
		return fmt.Errorf("%w: unknown finish reason %q", ErrSnapshotInvariant, snapshot.FinishReason)
	}
	calls := make(map[string]struct{})
	results := make(map[string]struct{})
	for _, part := range snapshot.Parts {
		if part.ToolCall != nil {
			if _, exists := calls[part.ToolCall.ID]; exists {
				return fmt.Errorf("%w: duplicate tool call ID %q", ErrSnapshotInvariant, part.ToolCall.ID)
			}
			calls[part.ToolCall.ID] = struct{}{}
		}
		if part.ToolResult != nil {
			if _, exists := results[part.ToolResult.ToolCallID]; exists {
				return fmt.Errorf("%w: duplicate tool result correlation %q", ErrSnapshotInvariant, part.ToolResult.ToolCallID)
			}
			results[part.ToolResult.ToolCallID] = struct{}{}
		}
	}
	return nil
}

func (m *Memory) validateBranchCorrelationsLocked(candidate Snapshot, replaced *aggregateKey) error {
	type call struct {
		name    string
		ordinal uint64
	}
	calls := make(map[string]call)
	results := make(map[string]struct{})
	snapshots := make([]Snapshot, 0, len(m.messages)+1)
	for key, stored := range m.messages {
		if replaced != nil && key == *replaced {
			continue
		}
		s := stored.snapshot
		if s.TenantKey == candidate.TenantKey && s.SessionKey == candidate.SessionKey && s.BranchKey == candidate.BranchKey && s.State != StateTombstoned && s.State != StateTerminal {
			snapshots = append(snapshots, s)
		}
	}
	if candidate.State != StateTombstoned && candidate.State != StateTerminal {
		snapshots = append(snapshots, candidate)
	}
	for _, snapshot := range snapshots {
		for _, part := range snapshot.Parts {
			if part.ToolCall != nil {
				if _, exists := calls[part.ToolCall.ID]; exists {
					return fmt.Errorf("%w: duplicate branch tool call ID %q", ErrSnapshotInvariant, part.ToolCall.ID)
				}
				calls[part.ToolCall.ID] = call{name: part.ToolCall.Name, ordinal: snapshot.BranchOrdinal}
			}
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

func validTransition(from, to State) bool {
	if from == to {
		return from == StateBuilding
	}
	switch from {
	case StateBuilding:
		return to == StateComplete || to == StateCanceled || to == StateFailed || to == StateTombstoned
	case StateComplete, StateCanceled, StateFailed:
		return to == StateTombstoned
	case StateTombstoned:
		return to == StateTerminal
	default:
		return false
	}
}

func knownState(state State) bool {
	return state == StateBuilding || state == StateComplete || state == StateCanceled || state == StateFailed || state == StateTombstoned || state == StateTerminal
}

func validateListContext(ctx context.Context, tenant agent.TenantKey, session string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if !tenant.Valid() || session == "" {
		return fmt.Errorf("%w: tenant and session are required", ErrInvalidCommand)
	}
	return nil
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidCommand)
	}
	return ctx.Err()
}

func cloneCreateCommand(command CreateCommand) CreateCommand {
	command.Parts = cloneParts(command.Parts)
	command.AdapterState = cloneBytes(command.AdapterState)
	return command
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Parts = cloneParts(snapshot.Parts)
	snapshot.AdapterState = cloneBytes(snapshot.AdapterState)
	return snapshot
}

func cloneParts(parts []agent.ContentPart) []agent.ContentPart {
	if parts == nil {
		return nil
	}
	cloned := make([]agent.ContentPart, len(parts))
	for i, part := range parts {
		cloned[i] = part
		if part.ToolCall != nil {
			call := *part.ToolCall
			cloned[i].ToolCall = &call
		}
		if part.ToolResult != nil {
			result := *part.ToolResult
			cloned[i].ToolResult = &result
		}
		if part.Image != nil {
			image := *part.Image
			image.Data = cloneBytes(part.Image.Data)
			cloned[i].Image = &image
		}
	}
	return cloned
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
