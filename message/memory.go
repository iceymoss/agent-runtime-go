package message

import (
	"context"
	"fmt"
	"reflect"
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
//
// It is deliberately thin: every legality decision is delegated to the exported
// state machine in apply.go, so this type demonstrates the shape a persistent
// adapter should have rather than owning a second copy of the rules.
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}

	key := aggregateKey{tenant: command.TenantKey, message: command.MessageKey}
	if stored, ok := m.messages[key]; ok {
		if SameCreate(command, stored.created, stored.snapshot.BranchOrdinal) {
			return CloneSnapshot(stored.snapshot), nil
		}
		return Snapshot{}, fmt.Errorf("%w: message key %q has different immutable input", ErrIdempotencyConflict, command.MessageKey)
	}

	// Allocating the ordinal is the storage layer's job: only it can see the
	// branch. Everything that follows is the shared state machine.
	ordinal := command.BranchOrdinal
	if ordinal == 0 {
		ordinal = m.nextOrdinalLocked(branchKey{tenant: command.TenantKey, session: command.SessionKey, branch: command.BranchKey})
	} else if m.ordinalUsedLocked(command.TenantKey, command.SessionKey, command.BranchKey, ordinal) {
		return Snapshot{}, fmt.Errorf("%w: branch ordinal %d is already used", ErrOrdinalConflict, ordinal)
	}
	snapshot, err := ApplyCreate(command, ordinal, time.Now().UTC())
	if err != nil {
		return Snapshot{}, err
	}
	if err := ValidateBranchCorrelation(snapshot, m.branchSiblingsLocked(snapshot)); err != nil {
		return Snapshot{}, err
	}
	normalized := cloneCreateCommand(command)
	if normalized.State == "" {
		normalized.State = StateBuilding
	}
	m.messages[key] = storedMessage{snapshot: CloneSnapshot(snapshot), created: normalized}
	return CloneSnapshot(snapshot), nil
}

func (m *Memory) SaveSnapshot(ctx context.Context, command SaveCommand) (Snapshot, error) {
	return m.mutate(ctx, command.TenantKey, command.MessageKey, func(current Snapshot) (Snapshot, error) {
		return ApplySave(current, command, time.Now().UTC())
	})
}

func (m *Memory) Tombstone(ctx context.Context, command TombstoneCommand) (Snapshot, error) {
	return m.mutate(ctx, command.TenantKey, command.MessageKey, func(current Snapshot) (Snapshot, error) {
		return ApplyTombstone(current, command, time.Now().UTC())
	})
}

// mutate is the load, apply, store cycle every cumulative write shares. A
// persistent adapter has the same shape with a transaction around it.
func (m *Memory) mutate(ctx context.Context, tenant agent.TenantKey, message MessageKey, apply func(Snapshot) (Snapshot, error)) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	key := aggregateKey{tenant: tenant, message: message}
	stored, ok := m.messages[key]
	if !ok {
		return Snapshot{}, ErrMessageNotFound
	}
	next, err := apply(stored.snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	if err := ValidateBranchCorrelation(next, m.branchSiblingsLocked(next)); err != nil {
		return Snapshot{}, err
	}
	stored.snapshot = CloneSnapshot(next)
	m.messages[key] = stored
	return CloneSnapshot(next), nil
}

func (m *Memory) Get(ctx context.Context, query GetQuery) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := ValidateGetQuery(query); err != nil {
		return Snapshot{}, err
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
	return CloneSnapshot(stored.snapshot), nil
}

func (m *Memory) ListBranch(ctx context.Context, query ListBranchQuery) ([]Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := ValidateListBranchQuery(query); err != nil {
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
		if snapshot.TenantKey == query.TenantKey && snapshot.SessionKey == query.SessionKey && snapshot.BranchKey == query.BranchKey {
			result = append(result, CloneSnapshot(snapshot))
		}
	}
	SortBranch(result)
	return result, nil
}

func (m *Memory) ListVisible(ctx context.Context, query ListVisibleQuery) ([]Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := ValidateListVisibleQuery(query); err != nil {
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
		if snapshot.TenantKey == query.TenantKey && snapshot.SessionKey == query.SessionKey && VisibleAt(snapshot, query.Revision) {
			result = append(result, CloneSnapshot(snapshot))
		}
	}
	SortVisible(result)
	return result, nil
}

// branchSiblingsLocked returns the other live messages of the candidate's branch,
// which is the context ValidateBranchCorrelation needs.
func (m *Memory) branchSiblingsLocked(candidate Snapshot) []Snapshot {
	siblings := make([]Snapshot, 0, len(m.messages))
	for _, stored := range m.messages {
		snapshot := stored.snapshot
		if snapshot.MessageKey == candidate.MessageKey && snapshot.TenantKey == candidate.TenantKey {
			continue
		}
		if snapshot.TenantKey == candidate.TenantKey && snapshot.SessionKey == candidate.SessionKey && snapshot.BranchKey == candidate.BranchKey {
			siblings = append(siblings, snapshot)
		}
	}
	return siblings
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
	got, want = cloneCreateCommand(got), cloneCreateCommand(want)
	got.MutationMeta = MutationMeta{}
	want.MutationMeta = MutationMeta{}
	if got.State == "" {
		got.State = StateBuilding
	}
	if want.State == "" {
		want.State = StateBuilding
	}
	if got.BranchOrdinal == 0 {
		got.BranchOrdinal = allocatedOrdinal
	}
	if want.BranchOrdinal == 0 {
		want.BranchOrdinal = allocatedOrdinal
	}
	return reflect.DeepEqual(got, want)
}
