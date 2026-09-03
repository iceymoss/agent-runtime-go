package durable

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"
)

const defaultMaxOperation = 1000

type MemoryStore struct {
	mu       sync.RWMutex
	runs     map[RunKey]Snapshot
	begins   map[RunKey]BeginRequest
	pairs    map[string]RunKey
	effects  map[ExecutionKey]EffectRecord
	usage    map[string]UsageFact
	maxLimit int
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		runs: make(map[RunKey]Snapshot), begins: make(map[RunKey]BeginRequest), pairs: make(map[string]RunKey),
		effects: make(map[ExecutionKey]EffectRecord), usage: make(map[string]UsageFact), maxLimit: defaultMaxOperation,
	}
}

func (m *MemoryStore) Begin(ctx context.Context, request BeginRequest) (Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, false, err
	}
	key := RunKey(request.Identity.RunKey)
	if key == "" || request.Identity.AgentKey == "" || request.Identity.SessionID == "" || request.Identity.RequestID == "" || request.InputDigest == "" || request.ConfigDigest == "" {
		return Snapshot{}, false, durableError(ErrRunConflict, "begin", key, "missing immutable field")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.runs[key]; ok {
		if !reflect.DeepEqual(m.begins[key], request) {
			return Snapshot{}, false, durableError(ErrRunConflict, "begin", key, "immutable values differ")
		}
		return cloneSnapshot(existing), false, nil
	}
	pair := request.Identity.SessionID + "\x00" + request.Identity.RequestID
	if other, ok := m.pairs[pair]; ok && other != key {
		return Snapshot{}, false, durableError(ErrRunConflict, "begin", key, "session and request already identify another run")
	}
	snapshot := Snapshot{
		SchemaVersion: SnapshotSchemaVersion, Identity: request.Identity, InputDigest: request.InputDigest,
		ConfigDigest: request.ConfigDigest, Status: StatusClaimed, Phase: PhaseModelReady, Checkpoint: request.Checkpoint,
	}
	m.runs[key] = cloneSnapshot(snapshot)
	m.begins[key] = BeginRequest{Identity: request.Identity, InputDigest: request.InputDigest, ConfigDigest: request.ConfigDigest, Checkpoint: cloneSnapshot(snapshot).Checkpoint}
	m.pairs[pair] = key
	return cloneSnapshot(snapshot), true, nil
}

func (m *MemoryStore) Load(ctx context.Context, key RunKey) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot, ok := m.runs[key]
	if !ok {
		return Snapshot{}, durableError(ErrRunNotFound, "load", key, "")
	}
	return cloneSnapshot(snapshot), nil
}

func (m *MemoryStore) Acquire(ctx context.Context, request AcquireRequest) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if request.Owner == "" || request.Now.IsZero() || !request.LeaseUntil.After(request.Now) {
		return Snapshot{}, durableError(ErrInvalidTransition, "acquire", request.RunKey, "invalid owner or lease interval")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, ok := m.runs[request.RunKey]
	if !ok {
		return Snapshot{}, durableError(ErrRunNotFound, "acquire", request.RunKey, "")
	}
	if terminalStatus(snapshot.Status) {
		return Snapshot{}, durableError(ErrTerminal, "acquire", request.RunKey, "")
	}
	if snapshot.Status == StatusRunning && snapshot.LeaseUntil.After(request.Now) {
		return Snapshot{}, durableError(ErrLeaseHeld, "acquire", request.RunKey, "active lease")
	}
	if snapshot.Status == StatusSuspended && !request.AllowSuspended {
		return Snapshot{}, durableError(ErrInvalidTransition, "acquire", request.RunKey, "suspended recovery not authorized")
	}
	if snapshot.FenceToken == ^uint64(0) || snapshot.Revision == ^uint64(0) {
		return Snapshot{}, durableError(ErrInvalidTransition, "acquire", request.RunKey, "counter overflow")
	}
	snapshot.Status = StatusRunning
	snapshot.LeaseOwner = request.Owner
	snapshot.LeaseUntil = request.LeaseUntil
	snapshot.FenceToken++
	snapshot.Revision++
	m.runs[request.RunKey] = snapshot
	return cloneSnapshot(snapshot), nil
}

func (m *MemoryStore) Renew(ctx context.Context, guard Guard, now, leaseUntil time.Time) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if now.IsZero() || !leaseUntil.After(now) {
		return Snapshot{}, durableError(ErrInvalidTransition, "renew", guard.RunKey, "invalid lease interval")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, err := m.guardedLocked(guard, "renew")
	if err != nil {
		return Snapshot{}, err
	}
	if !snapshot.LeaseUntil.After(now) {
		return Snapshot{}, durableError(ErrLeaseLost, "renew", guard.RunKey, "lease expired")
	}
	if snapshot.Revision == ^uint64(0) {
		return Snapshot{}, durableError(ErrInvalidTransition, "renew", guard.RunKey, "revision overflow")
	}
	snapshot.LeaseUntil = leaseUntil
	snapshot.Revision++
	m.runs[guard.RunKey] = snapshot
	return cloneSnapshot(snapshot), nil
}

func (m *MemoryStore) Release(ctx context.Context, guard Guard, phase Phase) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if phase != PhaseModelReady && phase != PhaseToolsReady {
		return Snapshot{}, durableError(ErrInvalidTransition, "release", guard.RunKey, "suspension requires a safe v1 phase")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, err := m.guardedLocked(guard, "release")
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.Revision == ^uint64(0) {
		return Snapshot{}, durableError(ErrInvalidTransition, "release", guard.RunKey, "revision overflow")
	}
	snapshot.Status, snapshot.Phase = StatusSuspended, phase
	snapshot.LeaseOwner, snapshot.LeaseUntil = "", time.Time{}
	snapshot.Revision++
	m.runs[guard.RunKey] = snapshot
	return cloneSnapshot(snapshot), nil
}

func (m *MemoryStore) Save(ctx context.Context, request SaveRequest) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current, err := m.guardedLocked(request.Guard, "save")
	if err != nil {
		return Snapshot{}, err
	}
	if current.Revision == ^uint64(0) {
		return Snapshot{}, durableError(ErrInvalidTransition, "save", request.Guard.RunKey, "revision overflow")
	}
	if !validStatusPhase(request.Status, request.Phase) || request.Status == StatusClaimed || !validPhaseEdge(current.Phase, request.Phase) {
		return Snapshot{}, durableError(ErrInvalidTransition, "save", request.Guard.RunKey, "illegal status or phase edge")
	}
	if request.Status == StatusCompleted && current.Phase != PhaseFinalizing {
		return Snapshot{}, durableError(ErrInvalidTransition, "save", request.Guard.RunKey, "completion requires finalizing")
	}
	if request.Status == StatusRunning && request.Failure != nil || request.Status == StatusCompleted && request.Failure != nil {
		return Snapshot{}, durableError(ErrInvalidTransition, "save", request.Guard.RunKey, "failure on non-failed state")
	}
	current.Status, current.Phase = request.Status, request.Phase
	current.Checkpoint, current.Failure = request.Checkpoint, request.Failure
	if request.Status != StatusRunning {
		current.LeaseOwner, current.LeaseUntil = "", time.Time{}
	}
	current.Revision++
	m.runs[request.Guard.RunKey] = cloneSnapshot(current)
	return cloneSnapshot(current), nil
}

func (m *MemoryStore) RevokeLease(ctx context.Context, request RevokeLeaseRequest) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, ok := m.runs[request.RunKey]
	if !ok {
		return Snapshot{}, durableError(ErrRunNotFound, "revoke", request.RunKey, "")
	}
	if snapshot.Revision != request.ExpectedRevision || snapshot.FenceToken != request.ExpectedFence || snapshot.Status != StatusRunning {
		return Snapshot{}, durableError(ErrLeaseLost, "revoke", request.RunKey, "stale revision or fence")
	}
	if request.RequireExpired && snapshot.LeaseUntil.After(request.Now) {
		return Snapshot{}, durableError(ErrLeaseHeld, "revoke", request.RunKey, "lease remains active")
	}
	if snapshot.FenceToken == ^uint64(0) || snapshot.Revision == ^uint64(0) {
		return Snapshot{}, durableError(ErrInvalidTransition, "revoke", request.RunKey, "counter overflow")
	}
	phase := safeSuspensionPhase(snapshot.Phase)
	for key, effect := range m.effects {
		if effect.RunKey == request.RunKey && effect.Status == EffectRunning {
			effect.Status, effect.FinishedAt = EffectUnknown, request.Now
			m.effects[key] = effect
			phase = PhaseToolsReady
		}
	}
	if snapshot.Phase == PhaseFinalizing {
		// Keep the finalizer-only phase while fencing its stale worker. The
		// immediately expired recovery lease preserves v1 running coherence and
		// lets the finalizer worker Acquire a fresh fence from the work hint.
		snapshot.LeaseOwner, snapshot.LeaseUntil = "recovery-revoked", request.Now
	} else {
		snapshot.Status, snapshot.Phase = StatusSuspended, phase
		snapshot.LeaseOwner, snapshot.LeaseUntil = "", time.Time{}
	}
	snapshot.FenceToken++
	snapshot.Revision++
	m.runs[request.RunKey] = snapshot
	return cloneSnapshot(snapshot), nil
}

func (m *MemoryStore) Scan(ctx context.Context, request ScanRequest) (ScanPage, error) {
	if err := ctx.Err(); err != nil {
		return ScanPage{}, err
	}
	if request.Limit <= 0 || request.Limit > m.maxLimit {
		return ScanPage{}, durableError(ErrLimitExceeded, "scan", "", "invalid limit")
	}
	statuses := make(map[Status]bool, len(request.Statuses))
	for _, status := range request.Statuses {
		statuses[status] = true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]RunKey, 0, len(m.runs))
	for key, snapshot := range m.runs {
		if key <= request.Cursor || len(statuses) > 0 && !statuses[snapshot.Status] {
			continue
		}
		if !request.ExpiredBefore.IsZero() && (snapshot.Status != StatusRunning || snapshot.LeaseUntil.After(request.ExpiredBefore)) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	page := ScanPage{}
	for _, key := range keys {
		if len(page.Snapshots) == request.Limit {
			page.Next = page.Snapshots[len(page.Snapshots)-1].IdentityKey()
			break
		}
		page.Snapshots = append(page.Snapshots, cloneSnapshot(m.runs[key]))
	}
	return page, nil
}

func (s Snapshot) IdentityKey() RunKey { return RunKey(s.Identity.RunKey) }

func (m *MemoryStore) guardedLocked(guard Guard, operation string) (Snapshot, error) {
	snapshot, ok := m.runs[guard.RunKey]
	if !ok {
		return Snapshot{}, durableError(ErrRunNotFound, operation, guard.RunKey, "")
	}
	if terminalStatus(snapshot.Status) {
		return Snapshot{}, durableError(ErrTerminal, operation, guard.RunKey, "")
	}
	if snapshot.Status != StatusRunning || snapshot.LeaseOwner != guard.LeaseOwner || snapshot.FenceToken != guard.FenceToken || snapshot.Revision != guard.Revision {
		return Snapshot{}, durableError(ErrLeaseLost, operation, guard.RunKey, "stale owner, fence, or revision")
	}
	return snapshot, nil
}

func safeSuspensionPhase(phase Phase) Phase {
	if phase == PhaseToolsReady || phase == PhaseToolInflight {
		return PhaseToolsReady
	}
	return PhaseModelReady
}

func (m *MemoryStore) PrepareEffect(ctx context.Context, request PrepareEffectRequest) (EffectRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecord{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, err := m.guardedLocked(request.Guard, "prepare effect")
	if err != nil {
		return EffectRecord{}, false, err
	}
	record, err := newEffectRecord(snapshot, request)
	if err != nil {
		return EffectRecord{}, false, err
	}
	if existing, ok := m.effects[record.ExecutionKey]; ok {
		if !samePreparedEffect(existing, record) {
			return EffectRecord{}, false, durableError(ErrEffectConflict, "prepare effect", request.Guard.RunKey, "execution key immutable mismatch")
		}
		return cloneEffect(existing), false, nil
	}
	for _, existing := range m.effects {
		if existing.RunKey != record.RunKey || existing.AttemptKey != record.AttemptKey || existing.StepNumber != record.StepNumber {
			continue
		}
		if existing.Ordinal == record.Ordinal || existing.ToolCall.ID == record.ToolCall.ID {
			return EffectRecord{}, false, durableError(ErrEffectConflict, "prepare effect", request.Guard.RunKey, "duplicate step ordinal or call id")
		}
	}
	m.effects[record.ExecutionKey] = cloneEffect(record)
	return cloneEffect(record), true, nil
}

func (m *MemoryStore) BeginEffect(ctx context.Context, guard Guard, key ExecutionKey, startedAt time.Time) (EffectRecord, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.guardedLocked(guard, "begin effect"); err != nil {
		return EffectRecord{}, err
	}
	record, ok := m.effects[key]
	if !ok || record.RunKey != guard.RunKey {
		return EffectRecord{}, durableError(ErrEffectNotFound, "begin effect", guard.RunKey, "")
	}
	if record.Status == EffectUnknown {
		return EffectRecord{}, durableError(ErrToolEffectUnknown, "begin effect", guard.RunKey, "automatic replay denied")
	}
	if record.Status == EffectRunning && record.FenceToken == guard.FenceToken {
		return cloneEffect(record), nil
	}
	if record.Status != EffectPrepared {
		// The common way to land here is a tool that suspended: its effect stays
		// running under the fence of the attempt that parked it, and the resumed
		// attempt holds a newer one. A tool that can suspend must either declare
		// agent.ToolExecutionLifecycleOwner, so the runtime opens no effect
		// boundary around it, or run through the tool subpackage's executor,
		// which parks and re-arms the effect itself.
		return EffectRecord{}, durableError(ErrInvalidTransition, "begin effect", guard.RunKey,
			fmt.Sprintf("effect is %s, not prepared; a suspending tool must own its execution lifecycle or use the tool subpackage's executor", record.Status))
	}
	record.Status, record.FenceToken, record.StartedAt = EffectRunning, guard.FenceToken, startedAt
	m.effects[key] = record
	return cloneEffect(record), nil
}

func (m *MemoryStore) CompleteEffect(ctx context.Context, request CompleteEffectRequest) (EffectRecord, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecord{}, err
	}
	if (request.Result == nil) == (request.Failure == nil) {
		return EffectRecord{}, durableError(ErrInvalidTransition, "complete effect", request.Guard.RunKey, "exactly one result or failure is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.guardedLocked(request.Guard, "complete effect"); err != nil {
		return EffectRecord{}, err
	}
	record, ok := m.effects[request.ExecutionKey]
	if !ok || record.RunKey != request.Guard.RunKey {
		return EffectRecord{}, durableError(ErrEffectNotFound, "complete effect", request.Guard.RunKey, "")
	}
	if (record.Status == EffectSucceeded || record.Status == EffectFailed) && record.FenceToken == request.Guard.FenceToken && reflect.DeepEqual(record.Result, request.Result) && reflect.DeepEqual(record.Failure, request.Failure) && record.FinishedAt.Equal(request.FinishedAt) {
		return cloneEffect(record), nil
	}
	if record.Status != EffectRunning || record.FenceToken != request.Guard.FenceToken {
		return EffectRecord{}, durableError(ErrLeaseLost, "complete effect", request.Guard.RunKey, "effect fence is stale")
	}
	record.Result, record.Failure, record.FinishedAt = request.Result, request.Failure, request.FinishedAt
	if request.Result != nil {
		record.Status = EffectSucceeded
	} else {
		record.Status = EffectFailed
	}
	m.effects[request.ExecutionKey] = cloneEffect(record)
	return cloneEffect(record), nil
}

func (m *MemoryStore) MarkEffectUnknown(ctx context.Context, guard Guard, key ExecutionKey, at time.Time) (EffectRecord, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.guardedLocked(guard, "mark effect unknown"); err != nil {
		return EffectRecord{}, err
	}
	record, ok := m.effects[key]
	if !ok || record.RunKey != guard.RunKey {
		return EffectRecord{}, durableError(ErrEffectNotFound, "mark effect unknown", guard.RunKey, "")
	}
	if record.Status == EffectUnknown {
		return cloneEffect(record), nil
	}
	if record.Status != EffectRunning {
		return EffectRecord{}, durableError(ErrInvalidTransition, "mark effect unknown", guard.RunKey, "effect is not running")
	}
	record.Status, record.FinishedAt = EffectUnknown, at
	m.effects[key] = record
	return cloneEffect(record), nil
}

// ReplayEffect re-arms an effect that a revoked lease left unknown so a tool
// whose ReplayPolicy proves the retry is safe can run again under the current
// fence. Callers must have established that safety; the ledger cannot know it.
func (m *MemoryStore) ReplayEffect(ctx context.Context, guard Guard, key ExecutionKey, startedAt time.Time) (EffectRecord, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecord{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.guardedLocked(guard, "replay effect"); err != nil {
		return EffectRecord{}, err
	}
	record, ok := m.effects[key]
	if !ok || record.RunKey != guard.RunKey {
		return EffectRecord{}, durableError(ErrEffectNotFound, "replay effect", guard.RunKey, "")
	}
	if record.Status == EffectRunning && record.FenceToken == guard.FenceToken {
		return cloneEffect(record), nil
	}
	if record.Status != EffectUnknown {
		return EffectRecord{}, durableError(ErrInvalidTransition, "replay effect", guard.RunKey, "effect is not unknown")
	}
	record.Status, record.FenceToken, record.StartedAt, record.FinishedAt = EffectRunning, guard.FenceToken, startedAt, time.Time{}
	m.effects[key] = record
	return cloneEffect(record), nil
}

func (m *MemoryStore) LoadEffect(ctx context.Context, key ExecutionKey) (EffectRecord, error) {
	if err := ctx.Err(); err != nil {
		return EffectRecord{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	record, ok := m.effects[key]
	if !ok {
		return EffectRecord{}, ErrEffectNotFound
	}
	return cloneEffect(record), nil
}

func (m *MemoryStore) ListEffects(ctx context.Context, runKey RunKey) ([]EffectRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	records := make([]EffectRecord, 0)
	for _, record := range m.effects {
		if record.RunKey == runKey {
			records = append(records, cloneEffect(record))
		}
	}
	sortEffects(records)
	return records, nil
}

func (m *MemoryStore) RecordUsage(ctx context.Context, fact UsageFact) (UsageFact, bool, error) {
	if err := ctx.Err(); err != nil {
		return UsageFact{}, false, err
	}
	if err := validateUsage(fact); err != nil {
		return UsageFact{}, false, err
	}
	key := string(fact.TenantKey) + "\x00" + string(fact.UsageKey)
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.usage[key]; ok {
		if !reflect.DeepEqual(existing, fact) {
			return UsageFact{}, false, durableError(ErrUsageConflict, "record usage", fact.RunKey, "immutable delta differs")
		}
		return existing, false, nil
	}
	m.usage[key] = fact
	return fact, true, nil
}

func (m *MemoryStore) LoadUsage(ctx context.Context, tenant TenantKey, key UsageKey) (UsageFact, error) {
	if err := ctx.Err(); err != nil {
		return UsageFact{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	fact, ok := m.usage[string(tenant)+"\x00"+string(key)]
	if !ok {
		return UsageFact{}, ErrUsageNotFound
	}
	return fact, nil
}

func (m *MemoryStore) ListAttemptUsage(ctx context.Context, tenant TenantKey, attempt AttemptKey) ([]UsageFact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	facts := make([]UsageFact, 0)
	for _, fact := range m.usage {
		if fact.TenantKey == tenant && fact.AttemptKey == attempt {
			facts = append(facts, fact)
		}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].UsageKey < facts[j].UsageKey })
	return facts, nil
}
