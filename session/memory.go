package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/message"
)

type sessionIdentity struct {
	tenant  agent.TenantKey
	session SessionKey
}

type branchIdentity struct {
	tenant agent.TenantKey
	branch BranchKey
}

type runIdentity struct {
	tenant agent.TenantKey
	run    RunKey
}

type usageIdentity struct {
	tenant agent.TenantKey
	usage  UsageFactKey
}

type usageDelta struct {
	session          SessionKey
	promptTokens     int64
	completionTokens int64
	costMicros       int64
}

type storedSession struct {
	created   CreateCommand
	current   Snapshot
	revisions map[uint64]Snapshot
}

// Memory is a lock-linearized, thread-safe reference implementation of Service.
type Memory struct {
	mu       sync.RWMutex
	sessions map[sessionIdentity]*storedSession
	branches map[branchIdentity]Branch
	runs     map[runIdentity]BranchKey
	usage    map[usageIdentity]usageDelta
}

var _ Service = (*Memory)(nil)

func NewMemory() *Memory {
	return &Memory{
		sessions: make(map[sessionIdentity]*storedSession),
		branches: make(map[branchIdentity]Branch),
		runs:     make(map[runIdentity]BranchKey),
		usage:    make(map[usageIdentity]usageDelta),
	}
}

func (m *Memory) Create(ctx context.Context, command CreateCommand) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateSessionScope(command.TenantKey, command.SessionKey); err != nil {
		return Snapshot{}, err
	}
	if command.UserKey == "" || command.AgentKey == "" || command.Identity == "" {
		return Snapshot{}, fmt.Errorf("%w: user, agent, and identity are required", ErrInvalidCommand)
	}
	if command.MetadataVersion == 0 {
		command.MetadataVersion = 1
	}
	if !validPivot(command.ContextPivot, 0) {
		return Snapshot{}, fmt.Errorf("%w: invalid initial context pivot", ErrSnapshotInvariant)
	}
	command.Metadata = cloneBytes(command.Metadata)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	key := sessionIdentity{tenant: command.TenantKey, session: command.SessionKey}
	if stored, ok := m.sessions[key]; ok {
		if sameCreate(command, stored.created) {
			return cloneSnapshot(stored.current), nil
		}
		return Snapshot{}, fmt.Errorf("%w: session key %q has different immutable input", ErrIdempotencyConflict, command.SessionKey)
	}
	now := time.Now().UTC()
	snapshot := Snapshot{
		TenantKey:            command.TenantKey,
		SessionKey:           command.SessionKey,
		UserKey:              command.UserKey,
		AgentKey:             command.AgentKey,
		Identity:             command.Identity,
		Status:               StatusActive,
		Title:                command.Title,
		ContextPivot:         command.ContextPivot,
		RuntimeDefinitionKey: command.RuntimeDefinitionKey,
		MetadataVersion:      command.MetadataVersion,
		Metadata:             cloneBytes(command.Metadata),
		CreatedAt:            now,
		UpdatedAt:            now,
	}
	m.sessions[key] = &storedSession{
		created:   command,
		current:   cloneSnapshot(snapshot),
		revisions: map[uint64]Snapshot{0: cloneSnapshot(snapshot)},
	}
	return cloneSnapshot(snapshot), nil
}

func (m *Memory) Get(ctx context.Context, query GetQuery) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateSessionScope(query.TenantKey, query.SessionKey); err != nil {
		return Snapshot{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	stored, ok := m.sessions[sessionIdentity{tenant: query.TenantKey, session: query.SessionKey}]
	if !ok {
		return Snapshot{}, ErrSessionNotFound
	}
	return cloneSnapshot(stored.current), nil
}

func (m *Memory) GetRevision(ctx context.Context, query RevisionQuery) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateSessionScope(query.TenantKey, query.SessionKey); err != nil {
		return Snapshot{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	stored, ok := m.sessions[sessionIdentity{tenant: query.TenantKey, session: query.SessionKey}]
	if !ok {
		return Snapshot{}, ErrSessionNotFound
	}
	snapshot, ok := stored.revisions[query.Revision]
	if !ok {
		return Snapshot{}, ErrRevisionNotFound
	}
	return cloneSnapshot(snapshot), nil
}

func (m *Memory) CreateBranch(ctx context.Context, command CreateBranchCommand) (Branch, error) {
	if err := contextError(ctx); err != nil {
		return Branch{}, err
	}
	if err := validateSessionScope(command.TenantKey, command.SessionKey); err != nil || !command.RunKey.Valid() {
		return Branch{}, fmt.Errorf("%w: tenant, session, and run are required", ErrInvalidCommand)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := contextError(ctx); err != nil {
		return Branch{}, err
	}
	runKey := runIdentity{tenant: command.TenantKey, run: command.RunKey}
	if existingKey, ok := m.runs[runKey]; ok {
		existing := m.branches[branchIdentity{tenant: command.TenantKey, branch: existingKey}]
		if existing.SessionKey == command.SessionKey && existing.BaseRevision == command.BaseRevision {
			return existing, nil
		}
		return Branch{}, fmt.Errorf("%w: run key %q has different immutable input", ErrIdempotencyConflict, command.RunKey)
	}
	stored, ok := m.sessions[sessionIdentity{tenant: command.TenantKey, session: command.SessionKey}]
	if !ok {
		return Branch{}, ErrSessionNotFound
	}
	if stored.current.Revision != command.BaseRevision {
		return Branch{}, fmt.Errorf("%w: base %d, current %d", ErrRevisionConflict, command.BaseRevision, stored.current.Revision)
	}
	if stored.current.Status == StatusCompleted || stored.current.Status == StatusAbandoned {
		return Branch{}, fmt.Errorf("%w: terminal session", ErrInvalidSessionTransition)
	}
	now := time.Now().UTC()
	branch := Branch{
		TenantKey:    command.TenantKey,
		BranchKey:    makeBranchKey(command.TenantKey, command.RunKey),
		SessionKey:   command.SessionKey,
		RunKey:       command.RunKey,
		BaseRevision: command.BaseRevision,
		HeadRevision: command.BaseRevision,
		Status:       BranchStatusOpen,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	m.branches[branchIdentity{tenant: branch.TenantKey, branch: branch.BranchKey}] = branch
	m.runs[runKey] = branch.BranchKey
	return branch, nil
}

func (m *Memory) GetBranch(ctx context.Context, query GetBranchQuery) (Branch, error) {
	if err := contextError(ctx); err != nil {
		return Branch{}, err
	}
	if err := validateSessionScope(query.TenantKey, query.SessionKey); err != nil || !query.BranchKey.Valid() {
		return Branch{}, fmt.Errorf("%w: tenant, session, and branch are required", ErrInvalidCommand)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	branch, ok := m.branches[branchIdentity{tenant: query.TenantKey, branch: query.BranchKey}]
	if !ok || branch.SessionKey != query.SessionKey {
		return Branch{}, ErrBranchNotFound
	}
	return branch, nil
}

func (m *Memory) MarkBranchReady(ctx context.Context, command BranchCommand) (Branch, error) {
	return m.transitionBranch(ctx, command.TenantKey, command.SessionKey, command.BranchKey, command.ExpectedVersion, BranchStatusReadyToMerge, command.HeadRevision, MergeKindNone)
}

func (m *Memory) MarkBranchConflict(ctx context.Context, command ConflictCommand) (Branch, error) {
	if command.MergeKind == MergeKindNone {
		command.MergeKind = MergeKindFastForward
	}
	if command.MergeKind != MergeKindFastForward {
		return Branch{}, fmt.Errorf("%w: unsupported merge kind %q", ErrInvalidCommand, command.MergeKind)
	}
	return m.transitionBranch(ctx, command.TenantKey, command.SessionKey, command.BranchKey, command.ExpectedVersion, BranchStatusConflicted, 0, command.MergeKind)
}

func (m *Memory) AbandonBranch(ctx context.Context, command BranchCommand) (Branch, error) {
	return m.transitionBranch(ctx, command.TenantKey, command.SessionKey, command.BranchKey, command.ExpectedVersion, BranchStatusAbandoned, 0, MergeKindNone)
}

func (m *Memory) transitionBranch(ctx context.Context, tenant agent.TenantKey, sessionKey SessionKey, key BranchKey, expected uint64, target BranchStatus, head uint64, kind MergeKind) (Branch, error) {
	if err := contextError(ctx); err != nil {
		return Branch{}, err
	}
	if err := validateSessionScope(tenant, sessionKey); err != nil || !key.Valid() {
		return Branch{}, fmt.Errorf("%w: incomplete branch command", ErrInvalidCommand)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	id := branchIdentity{tenant: tenant, branch: key}
	branch, ok := m.branches[id]
	if !ok || branch.SessionKey != sessionKey {
		return Branch{}, ErrBranchNotFound
	}
	if branch.Status == BranchStatusMerged || branch.Status == BranchStatusAbandoned {
		return Branch{}, ErrBranchClosed
	}
	if branch.Version != expected {
		return Branch{}, fmt.Errorf("%w: expected %d, current %d", ErrBranchVersionConflict, expected, branch.Version)
	}
	if !validBranchTransition(branch.Status, target) {
		return Branch{}, fmt.Errorf("%w: %s to %s", ErrInvalidBranchTransition, branch.Status, target)
	}
	if target == BranchStatusReadyToMerge {
		if head == 0 {
			head = branch.HeadRevision
		}
		if head < branch.BaseRevision {
			return Branch{}, fmt.Errorf("%w: head revision precedes base", ErrInvalidCommand)
		}
		branch.HeadRevision = head
		branch.MergeKind = MergeKindNone
	} else if target == BranchStatusConflicted {
		branch.MergeKind = kind
	}
	branch.Status = target
	branch.Version++
	branch.UpdatedAt = time.Now().UTC()
	m.branches[id] = branch
	return branch, nil
}

func (m *Memory) CommitMerge(ctx context.Context, command MergeCommit) (MergeResult, error) {
	if err := contextError(ctx); err != nil {
		return MergeResult{}, err
	}
	if err := validateSessionScope(command.TenantKey, command.SessionKey); err != nil || !command.BranchKey.Valid() {
		return MergeResult{}, fmt.Errorf("%w: incomplete merge command", ErrInvalidCommand)
	}
	if command.MergeKind == MergeKindNone {
		command.MergeKind = MergeKindFastForward
	}
	if command.MergeKind != MergeKindFastForward {
		return MergeResult{}, fmt.Errorf("%w: unsupported merge kind %q", ErrInvalidCommand, command.MergeKind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	branchID := branchIdentity{tenant: command.TenantKey, branch: command.BranchKey}
	branch, ok := m.branches[branchID]
	if !ok || branch.SessionKey != command.SessionKey {
		return MergeResult{}, ErrBranchNotFound
	}
	stored, ok := m.sessions[sessionIdentity{tenant: command.TenantKey, session: command.SessionKey}]
	if !ok {
		return MergeResult{}, ErrSessionNotFound
	}
	if branch.Status != BranchStatusReadyToMerge || branch.Version != command.ExpectedVersion || stored.current.Revision != branch.BaseRevision {
		return MergeResult{}, &MergeConflictError{
			SessionKey: command.SessionKey, BranchKey: command.BranchKey, BaseRevision: branch.BaseRevision,
			CurrentRevision: stored.current.Revision, BranchVersion: branch.Version, Reason: "fast-forward precondition failed",
		}
	}
	if stored.current.Status == StatusCompleted || stored.current.Status == StatusAbandoned {
		return MergeResult{}, &MergeConflictError{
			SessionKey: command.SessionKey, BranchKey: command.BranchKey, BaseRevision: branch.BaseRevision,
			CurrentRevision: stored.current.Revision, BranchVersion: branch.Version, Reason: "session is terminal",
		}
	}
	previous := stored.current.Revision
	next := cloneSnapshot(stored.current)
	next.Revision++
	next.UpdatedAt = time.Now().UTC()
	branch.Status = BranchStatusMerged
	branch.MergeKind = command.MergeKind
	branch.MergedRevision = next.Revision
	branch.Version++
	branch.UpdatedAt = next.UpdatedAt
	stored.current = cloneSnapshot(next)
	stored.revisions[next.Revision] = cloneSnapshot(next)
	m.branches[branchID] = branch
	return MergeResult{PreviousRevision: previous, SessionRevision: next.Revision, Session: cloneSnapshot(next), Branch: branch}, nil
}

func (m *Memory) AddUsage(ctx context.Context, command UsageCommand) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateSessionScope(command.TenantKey, command.SessionKey); err != nil || !command.UsageFactKey.Valid() {
		return Snapshot{}, fmt.Errorf("%w: tenant, session, and usage fact are required", ErrInvalidCommand)
	}
	if command.PromptTokens < 0 || command.CompletionTokens < 0 || command.CostMicros < 0 {
		return Snapshot{}, fmt.Errorf("%w: usage deltas must be nonnegative fixed-point integers", ErrInvalidCommand)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.sessions[sessionIdentity{tenant: command.TenantKey, session: command.SessionKey}]
	if !ok {
		return Snapshot{}, ErrSessionNotFound
	}
	usageKey := usageIdentity{tenant: command.TenantKey, usage: command.UsageFactKey}
	delta := usageDelta{session: command.SessionKey, promptTokens: command.PromptTokens, completionTokens: command.CompletionTokens, costMicros: command.CostMicros}
	if existing, ok := m.usage[usageKey]; ok {
		if existing == delta {
			return cloneSnapshot(stored.current), nil
		}
		return Snapshot{}, fmt.Errorf("%w: usage fact %q has different immutable input", ErrIdempotencyConflict, command.UsageFactKey)
	}
	if stored.current.Revision != command.ExpectedRevision {
		return Snapshot{}, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, command.ExpectedRevision, stored.current.Revision)
	}
	if stored.current.Status == StatusCompleted || stored.current.Status == StatusAbandoned {
		return Snapshot{}, fmt.Errorf("%w: terminal session", ErrInvalidSessionTransition)
	}
	next := cloneSnapshot(stored.current)
	var valid bool
	if next.PromptTokens, valid = addNonnegative(next.PromptTokens, command.PromptTokens); !valid {
		return Snapshot{}, fmt.Errorf("%w: prompt token overflow", ErrInvalidCommand)
	}
	if next.CompletionTokens, valid = addNonnegative(next.CompletionTokens, command.CompletionTokens); !valid {
		return Snapshot{}, fmt.Errorf("%w: completion token overflow", ErrInvalidCommand)
	}
	if next.CostMicros, valid = addNonnegative(next.CostMicros, command.CostMicros); !valid {
		return Snapshot{}, fmt.Errorf("%w: cost overflow", ErrInvalidCommand)
	}
	m.commitSessionMutation(stored, next)
	m.usage[usageKey] = delta
	return cloneSnapshot(stored.current), nil
}

func (m *Memory) SetSummary(ctx context.Context, command SummaryCommand) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateSessionScope(command.TenantKey, command.SessionKey); err != nil {
		return Snapshot{}, err
	}
	hasKey := command.SummaryMessageKey.Valid()
	hasRevision := command.SummaryAtRevision != 0
	if hasKey != hasRevision {
		return Snapshot{}, fmt.Errorf("%w: summary key and revision must be paired", ErrSnapshotInvariant)
	}
	if hasKey && (command.SummaryVisibleAtRevision == 0 || command.SummaryVisibleAtRevision > command.SummaryAtRevision || command.SummaryAtRevision > command.ExpectedRevision) {
		return Snapshot{}, fmt.Errorf("%w: summary is not visible at the expected revision", ErrSnapshotInvariant)
	}
	if !hasKey && command.SummaryVisibleAtRevision != 0 {
		return Snapshot{}, fmt.Errorf("%w: empty summary has visibility", ErrSnapshotInvariant)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.sessions[sessionIdentity{tenant: command.TenantKey, session: command.SessionKey}]
	if !ok {
		return Snapshot{}, ErrSessionNotFound
	}
	if stored.current.Revision != command.ExpectedRevision {
		return Snapshot{}, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, command.ExpectedRevision, stored.current.Revision)
	}
	if stored.current.Status == StatusCompleted || stored.current.Status == StatusAbandoned {
		return Snapshot{}, fmt.Errorf("%w: terminal session", ErrInvalidSessionTransition)
	}
	next := cloneSnapshot(stored.current)
	next.SummaryMessageKey = command.SummaryMessageKey
	next.SummaryAtRevision = command.SummaryAtRevision
	m.commitSessionMutation(stored, next)
	return cloneSnapshot(stored.current), nil
}

func (m *Memory) Transition(ctx context.Context, command TransitionCommand) (Snapshot, error) {
	if err := contextError(ctx); err != nil {
		return Snapshot{}, err
	}
	if err := validateSessionScope(command.TenantKey, command.SessionKey); err != nil || !knownStatus(command.Status) {
		return Snapshot{}, fmt.Errorf("%w: incomplete transition command", ErrInvalidCommand)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.sessions[sessionIdentity{tenant: command.TenantKey, session: command.SessionKey}]
	if !ok {
		return Snapshot{}, ErrSessionNotFound
	}
	if stored.current.Revision != command.ExpectedRevision {
		return Snapshot{}, fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, command.ExpectedRevision, stored.current.Revision)
	}
	if !validStatusTransition(stored.current.Status, command.Status) {
		return Snapshot{}, fmt.Errorf("%w: %s to %s", ErrInvalidSessionTransition, stored.current.Status, command.Status)
	}
	next := cloneSnapshot(stored.current)
	next.Status = command.Status
	if command.ContextPivot != nil {
		if !validPivot(*command.ContextPivot, next.Revision+1) {
			return Snapshot{}, fmt.Errorf("%w: invalid context pivot", ErrSnapshotInvariant)
		}
		next.ContextPivot = *command.ContextPivot
	}
	m.commitSessionMutation(stored, next)
	return cloneSnapshot(stored.current), nil
}

func (m *Memory) commitSessionMutation(stored *storedSession, next Snapshot) {
	next.Revision = stored.current.Revision + 1
	next.UpdatedAt = time.Now().UTC()
	stored.current = cloneSnapshot(next)
	stored.revisions[next.Revision] = cloneSnapshot(next)
}

func sameCreate(got, want CreateCommand) bool {
	got.MutationMeta = message.MutationMeta{}
	want.MutationMeta = got.MutationMeta
	return reflect.DeepEqual(got, want)
}

func makeBranchKey(tenant agent.TenantKey, run RunKey) BranchKey {
	digest := sha256.Sum256([]byte(string(tenant) + "\x00" + string(run)))
	return BranchKey("branch_" + hex.EncodeToString(digest[:16]))
}
