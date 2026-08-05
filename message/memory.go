package message

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type aggregateKey struct {
	tenant  agent.TenantKey
	message MessageKey
}

type branchKey struct {
	tenant  agent.TenantKey
	session string
	branch  string
}

type storedMessage struct {
	snapshot Snapshot
	created  CreateCommand
}

// Memory is a thread-safe in-memory reference implementation of Service.
type Memory struct {
	mu       sync.RWMutex
	messages map[aggregateKey]storedMessage
}

var _ Service = (*Memory)(nil)

func NewMemory() *Memory {
	return &Memory{messages: make(map[aggregateKey]storedMessage)}
}

func (m *Memory) Create(ctx context.Context, command CreateCommand) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	command = cloneCreateCommand(command)
	if command.State == "" {
		command.State = StateBuilding
	}
	if err := validateCreate(command); err != nil {
		return Snapshot{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}

	key := aggregateKey{tenant: command.TenantKey, message: command.MessageKey}
	if stored, ok := m.messages[key]; ok {
		if sameCreate(command, stored.created, stored.snapshot.BranchOrdinal) {
			return cloneSnapshot(stored.snapshot), nil
		}
		return Snapshot{}, fmt.Errorf("%w: message key %q has different immutable input", ErrIdempotencyConflict, command.MessageKey)
	}

	ordinal := command.BranchOrdinal
	if ordinal == 0 {
		ordinal = m.nextOrdinalLocked(branchKey{tenant: command.TenantKey, session: command.SessionKey, branch: command.BranchKey})
	} else if m.ordinalUsedLocked(command.TenantKey, command.SessionKey, command.BranchKey, ordinal) {
		return Snapshot{}, fmt.Errorf("%w: branch ordinal %d is already used", ErrOrdinalConflict, ordinal)
	}

	now := time.Now().UTC()
	snapshot := Snapshot{
		TenantKey:         command.TenantKey,
		MessageKey:        command.MessageKey,
		SessionKey:        command.SessionKey,
		BranchKey:         command.BranchKey,
		BranchOrdinal:     ordinal,
		Role:              command.Role,
		Parts:             cloneParts(command.Parts),
		State:             command.State,
		FinishReason:      command.FinishReason,
		ModelKey:          command.ModelKey,
		ProviderKey:       command.ProviderKey,
		AdapterState:      cloneBytes(command.AdapterState),
		RunKey:            command.RunKey,
		AttemptKey:        command.AttemptKey,
		FenceToken:        command.FenceToken,
		StepIndex:         command.StepIndex,
		Revision:          1,
		VisibleAtRevision: command.VisibleAtRevision,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := m.validateBranchCorrelationsLocked(snapshot, nil); err != nil {
		return Snapshot{}, err
	}
	m.messages[key] = storedMessage{snapshot: cloneSnapshot(snapshot), created: command}
	return cloneSnapshot(snapshot), nil
}

func (m *Memory) SaveSnapshot(ctx context.Context, command SaveCommand) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	command.Parts = cloneParts(command.Parts)
	command.AdapterState = cloneBytes(command.AdapterState)
	if err := validateSave(command); err != nil {
		return Snapshot{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	key := aggregateKey{tenant: command.TenantKey, message: command.MessageKey}
	stored, ok := m.messages[key]
	if !ok {
		return Snapshot{}, ErrMessageNotFound
	}
	if command.FenceToken < stored.snapshot.FenceToken {
		return Snapshot{}, fmt.Errorf("%w: got %d, current %d", ErrStaleFence, command.FenceToken, stored.snapshot.FenceToken)
	}
	if command.ExpectedRevision != stored.snapshot.Revision {
		return Snapshot{}, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, command.ExpectedRevision, stored.snapshot.Revision)
	}
	if command.FenceToken == stored.snapshot.FenceToken && command.AttemptKey != stored.snapshot.AttemptKey {
		return Snapshot{}, fmt.Errorf("%w: attempt changed without a higher fence", ErrStaleFence)
	}
	if !validTransition(stored.snapshot.State, command.State) {
		return Snapshot{}, fmt.Errorf("%w: %s to %s", ErrInvalidMessageTransition, stored.snapshot.State, command.State)
	}

	next := cloneSnapshot(stored.snapshot)
	next.AttemptKey = command.AttemptKey
	next.FenceToken = command.FenceToken
	next.State = command.State
	next.FinishReason = command.FinishReason
	next.Parts = cloneParts(command.Parts)
	next.AdapterState = cloneBytes(command.AdapterState)
	next.Revision++
	next.UpdatedAt = time.Now().UTC()
	if err := validateSnapshotContent(next); err != nil {
		return Snapshot{}, err
	}
	if err := m.validateBranchCorrelationsLocked(next, &key); err != nil {
		return Snapshot{}, err
	}
	stored.snapshot = cloneSnapshot(next)
	m.messages[key] = stored
	return cloneSnapshot(next), nil
}

func (m *Memory) Get(ctx context.Context, query GetQuery) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if !query.TenantKey.Valid() || query.MessageKey == "" {
		return Snapshot{}, ErrInvalidCommand
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	stored, ok := m.messages[aggregateKey{tenant: query.TenantKey, message: query.MessageKey}]
	if !ok {
		return Snapshot{}, ErrMessageNotFound
	}
	return cloneSnapshot(stored.snapshot), nil
}

func (m *Memory) ListBranch(ctx context.Context, query ListBranchQuery) ([]Snapshot, error) {
	if err := validateListContext(ctx, query.TenantKey, query.SessionKey); err != nil {
		return nil, err
	}
	if query.BranchKey == "" {
		return nil, fmt.Errorf("%w: branch key is required", ErrInvalidCommand)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	result := make([]Snapshot, 0)
	for _, stored := range m.messages {
		snapshot := stored.snapshot
		if snapshot.TenantKey == query.TenantKey && snapshot.SessionKey == query.SessionKey && snapshot.BranchKey == query.BranchKey {
			result = append(result, cloneSnapshot(snapshot))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].BranchOrdinal != result[j].BranchOrdinal {
			return result[i].BranchOrdinal < result[j].BranchOrdinal
		}
		return result[i].MessageKey < result[j].MessageKey
	})
	return result, nil
}

func (m *Memory) ListVisible(ctx context.Context, query ListVisibleQuery) ([]Snapshot, error) {
	if err := validateListContext(ctx, query.TenantKey, query.SessionKey); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	result := make([]Snapshot, 0)
	for _, stored := range m.messages {
		snapshot := stored.snapshot
		if snapshot.TenantKey == query.TenantKey && snapshot.SessionKey == query.SessionKey && snapshot.VisibleAtRevision > 0 && snapshot.VisibleAtRevision <= query.Revision {
			result = append(result, cloneSnapshot(snapshot))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].VisibleAtRevision != result[j].VisibleAtRevision {
			return result[i].VisibleAtRevision < result[j].VisibleAtRevision
		}
		if result[i].BranchKey != result[j].BranchKey {
			return result[i].BranchKey < result[j].BranchKey
		}
		if result[i].BranchOrdinal != result[j].BranchOrdinal {
			return result[i].BranchOrdinal < result[j].BranchOrdinal
		}
		return result[i].MessageKey < result[j].MessageKey
	})
	return result, nil
}

func (m *Memory) Tombstone(ctx context.Context, command TombstoneCommand) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if !command.TenantKey.Valid() || command.MessageKey == "" || command.ExpectedRevision == 0 || command.AttemptKey == "" || command.FenceToken == 0 {
		return Snapshot{}, fmt.Errorf("%w: incomplete tombstone command", ErrInvalidCommand)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	key := aggregateKey{tenant: command.TenantKey, message: command.MessageKey}
	stored, ok := m.messages[key]
	if !ok {
		return Snapshot{}, ErrMessageNotFound
	}
	if command.FenceToken < stored.snapshot.FenceToken {
		return Snapshot{}, fmt.Errorf("%w: got %d, current %d", ErrStaleFence, command.FenceToken, stored.snapshot.FenceToken)
	}
	if command.ExpectedRevision != stored.snapshot.Revision {
		return Snapshot{}, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, command.ExpectedRevision, stored.snapshot.Revision)
	}
	if command.FenceToken == stored.snapshot.FenceToken && command.AttemptKey != stored.snapshot.AttemptKey {
		return Snapshot{}, fmt.Errorf("%w: attempt changed without a higher fence", ErrStaleFence)
	}
	if !validTransition(stored.snapshot.State, StateTombstoned) {
		return Snapshot{}, fmt.Errorf("%w: %s to %s", ErrInvalidMessageTransition, stored.snapshot.State, StateTombstoned)
	}
	next := cloneSnapshot(stored.snapshot)
	next.Parts = nil
	next.AdapterState = nil
	next.FinishReason = ""
	next.State = StateTombstoned
	next.AttemptKey = command.AttemptKey
	next.FenceToken = command.FenceToken
	next.Revision++
	next.UpdatedAt = time.Now().UTC()
	if err := m.validateBranchCorrelationsLocked(next, &key); err != nil {
		return Snapshot{}, err
	}
	stored.snapshot = cloneSnapshot(next)
	m.messages[key] = stored
	return cloneSnapshot(next), nil
}

func (m *Memory) nextOrdinalLocked(key branchKey) uint64 {
	var highest uint64
	for _, stored := range m.messages {
		s := stored.snapshot
		if s.TenantKey == key.tenant && s.SessionKey == key.session && s.BranchKey == key.branch && s.BranchOrdinal > highest {
			highest = s.BranchOrdinal
		}
	}
	return highest + 1
}

func (m *Memory) ordinalUsedLocked(tenant agent.TenantKey, session, branch string, ordinal uint64) bool {
	for _, stored := range m.messages {
		s := stored.snapshot
		if s.TenantKey == tenant && s.SessionKey == session && s.BranchKey == branch && s.BranchOrdinal == ordinal {
			return true
		}
	}
	return false
}

func sameCreate(got, want CreateCommand, allocatedOrdinal uint64) bool {
	got.MutationMeta = MutationMeta{}
	want.MutationMeta = MutationMeta{}
	if got.BranchOrdinal == 0 {
		got.BranchOrdinal = allocatedOrdinal
	}
	if want.BranchOrdinal == 0 {
		want.BranchOrdinal = allocatedOrdinal
	}
	return reflect.DeepEqual(got, want)
}
