package subagent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type treeID struct {
	tenant agent.TenantKey
	key    TreeKey
}

type relationshipID struct {
	tenant agent.TenantKey
	key    RelationshipKey
}

type requestID struct {
	tenant agent.TenantKey
	key    RequestKey
}

type usageRecord struct {
	usage  Usage
	digest string
}

// treeBudget holds one delegation tree's shared budget. The accounting lives in
// BudgetSnapshot's exported methods so an adapter cannot get it subtly wrong.
type treeBudget struct{ budget BudgetSnapshot }

type memoryRecord struct {
	snapshot   Snapshot
	specDigest string
	requestKey RequestKey
	settled    bool
	usageFacts map[UsageFactKey]usageRecord
}

// MemoryStore is a thread-safe reference implementation of Store. Multiple
// services may share one instance to model cross-worker persistence.
type MemoryStore struct {
	mu             sync.Mutex
	records        map[relationshipID]*memoryRecord
	requests       map[requestID]relationshipID
	runs           map[agent.TenantKey]map[RunKey]relationshipID
	trees          map[treeID]*treeBudget
	cancelRequests map[string]CancelResult
	facts          map[agent.TenantKey][]CommittedFact
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		records:        make(map[relationshipID]*memoryRecord),
		requests:       make(map[requestID]relationshipID),
		runs:           make(map[agent.TenantKey]map[RunKey]relationshipID),
		trees:          make(map[treeID]*treeBudget),
		cancelRequests: make(map[string]CancelResult),
		facts:          make(map[agent.TenantKey][]CommittedFact),
	}
}

func (s *MemoryStore) Spawn(_ context.Context, request SpawnRequest, now time.Time) (SpawnReceipt, bool, error) {
	if err := ValidateSpawn(request, now); err != nil {
		return SpawnReceipt{}, false, err
	}
	specDigest, err := SpecDigest(request)
	if err != nil {
		return SpawnReceipt{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	requestIDValue := requestID{tenant: request.Parent.TenantKey, key: request.RequestKey}
	if existingID, ok := s.requests[requestIDValue]; ok {
		record := s.records[existingID]
		if record == nil {
			return SpawnReceipt{}, false, ErrStoreInvariant
		}
		if record.specDigest != specDigest {
			return SpawnReceipt{}, false, ErrIdempotencyConflict
		}
		return record.snapshot.Receipt, false, nil
	}

	depth, treeKey, tree, newTree, err := s.resolveParentLocked(request)
	if err != nil {
		return SpawnReceipt{}, false, err
	}
	child := NewChildRef(request.Parent.TenantKey, request.RequestKey, depth)
	if err := ValidateNoCycle(request.Parent.RunKey, child.RunKey, s.parentLookupLocked(request.Parent.TenantKey)); err != nil {
		return SpawnReceipt{}, false, err
	}
	if err := ValidateDepth(depth, tree.budget.Limits); err != nil {
		return SpawnReceipt{}, false, err
	}
	if err := ValidateFanout(s.directFanoutLocked(request.Parent.TenantKey, request.Parent.RunKey), tree.budget.Limits); err != nil {
		return SpawnReceipt{}, false, err
	}
	reserved, err := tree.budget.Reserve(request.Reserve)
	if err != nil {
		return SpawnReceipt{}, false, err
	}
	tree.budget = reserved
	if newTree {
		s.trees[treeID{tenant: request.Parent.TenantKey, key: treeKey}] = tree
	}
	relationshipKey := child.RelationshipKey
	receipt := SpawnReceipt{
		RequestKey:     request.RequestKey,
		Child:          child,
		State:          ChildQueued,
		SpecDigest:     specDigest,
		ParentBlocked:  true,
		Reservation:    request.Reserve,
		EffectiveLimit: EffectiveLimits(tree.budget.Limits, request.Reserve, request.Deadline, now),
		CreatedAt:      now,
	}
	parent := request.Parent
	parent.TreeKey = treeKey
	record := &memoryRecord{
		snapshot: Snapshot{
			Receipt:     receipt,
			Parent:      parent,
			AgentKey:    request.AgentKey,
			Input:       cloneBytes(request.Input),
			ContextRefs: cloneContextRefs(request.ContextRefs),
			State:       ChildQueued,
			Version:     1,
		},
		specDigest: specDigest,
		requestKey: request.RequestKey,
		usageFacts: make(map[UsageFactKey]usageRecord),
	}
	id := relationshipID{tenant: request.Parent.TenantKey, key: relationshipKey}
	s.records[id] = record
	s.requests[requestIDValue] = id
	if s.runs[request.Parent.TenantKey] == nil {
		s.runs[request.Parent.TenantKey] = make(map[RunKey]relationshipID)
	}
	s.runs[request.Parent.TenantKey][child.RunKey] = id
	s.appendFactLocked(record, FactChildAccepted, now)
	return receipt, true, nil
}

func (s *MemoryStore) Get(_ context.Context, tenantKey agent.TenantKey, relationshipKey RelationshipKey) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[relationshipID{tenant: tenantKey, key: relationshipKey}]
	if record == nil {
		return Snapshot{}, ErrNotFound
	}
	return cloneSnapshot(record.snapshot), nil
}

func (s *MemoryStore) ClaimNext(_ context.Context, request ClaimRequest) (Snapshot, bool, error) {
	if request.TenantKey == "" || request.Owner == "" || request.Now.IsZero() || request.Lease <= 0 {
		return Snapshot{}, false, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := s.sortedIDsLocked(request.TenantKey)
	for _, id := range ids {
		record := s.records[id]
		if record.snapshot.State != ChildQueued {
			continue
		}
		if !record.snapshot.Receipt.EffectiveLimit.Deadline.IsZero() && !request.Now.Before(record.snapshot.Receipt.EffectiveLimit.Deadline) {
			if err := s.cancelRecordLocked(record, CancelAbandon, request.Now, "deadline"); err != nil {
				return Snapshot{}, false, err
			}
			continue
		}
		record.snapshot.State = ChildRunning
		record.snapshot.Receipt.State = ChildRunning
		record.snapshot.Version++
		record.snapshot.ClaimOwner = request.Owner
		record.snapshot.ClaimUntil = request.Now.Add(request.Lease)
		s.appendFactLocked(record, FactChildRunning, request.Now)
		return cloneSnapshot(record.snapshot), true, nil
	}
	return Snapshot{}, false, nil
}

func (s *MemoryStore) CommitSuspended(_ context.Context, command SuspendCommand) (Snapshot, error) {
	if command.TenantKey == "" || command.RelationshipKey == "" || command.ExpectedVersion == 0 || command.Owner == "" || command.SuspendedAt.IsZero() {
		return Snapshot{}, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[relationshipID{tenant: command.TenantKey, key: command.RelationshipKey}]
	if record == nil {
		return Snapshot{}, ErrNotFound
	}
	if record.snapshot.State != ChildRunning || record.snapshot.Version != command.ExpectedVersion || record.snapshot.ClaimOwner != command.Owner {
		return Snapshot{}, ErrStaleVersion
	}
	record.snapshot.State = ChildSuspended
	record.snapshot.Receipt.State = ChildSuspended
	record.snapshot.Failure = cloneFailure(command.Failure)
	record.snapshot.ClaimOwner = ""
	record.snapshot.ClaimUntil = time.Time{}
	record.snapshot.Version++
	return cloneSnapshot(record.snapshot), nil
}

func (s *MemoryStore) CommitTerminal(_ context.Context, command TerminalCommand) (Snapshot, error) {
	if command.TenantKey == "" || command.RelationshipKey == "" || command.ExpectedVersion == 0 || command.Owner == "" || command.CompletedAt.IsZero() {
		return Snapshot{}, ErrInvalidRequest
	}
	if !command.Result.State.Terminal() || command.Result.UsageFactKey == "" {
		return Snapshot{}, fmt.Errorf("%w: terminal state and usage fact key required", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[relationshipID{tenant: command.TenantKey, key: command.RelationshipKey}]
	if record == nil {
		return Snapshot{}, ErrNotFound
	}
	if record.snapshot.State.Terminal() {
		if sameTerminal(record.snapshot, command.Result) {
			return cloneSnapshot(record.snapshot), nil
		}
		return Snapshot{}, ErrInvalidTransition
	}
	if record.snapshot.State != ChildRunning || record.snapshot.Version != command.ExpectedVersion || record.snapshot.ClaimOwner != command.Owner {
		return Snapshot{}, ErrStaleVersion
	}
	if err := s.settleLocked(record, command.Result.UsageFactKey, command.Result.Usage); err != nil {
		return Snapshot{}, err
	}
	record.snapshot.State = command.Result.State
	record.snapshot.Receipt.State = command.Result.State
	record.snapshot.ResultRef = command.Result.ResultRef
	record.snapshot.FailureRef = command.Result.FailureRef
	record.snapshot.Failure = cloneFailure(command.Result.Failure)
	record.snapshot.TerminalAt = command.CompletedAt
	record.snapshot.ClaimOwner = ""
	record.snapshot.ClaimUntil = time.Time{}
	record.snapshot.WakePending = true
	record.snapshot.Version++
	s.appendFactLocked(record, FactChildTerminal, command.CompletedAt)
	return cloneSnapshot(record.snapshot), nil
}

func (s *MemoryStore) SettleUsage(_ context.Context, command SettleCommand) (BudgetSnapshot, bool, error) {
	if command.TenantKey == "" || command.RelationshipKey == "" || command.UsageFactKey == "" {
		return BudgetSnapshot{}, false, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.records[relationshipID{tenant: command.TenantKey, key: command.RelationshipKey}]
	if record == nil {
		return BudgetSnapshot{}, false, ErrNotFound
	}
	usageDigest, err := digest(command.Usage)
	if err != nil {
		return BudgetSnapshot{}, false, err
	}
	if existing, ok := record.usageFacts[command.UsageFactKey]; ok {
		if existing.digest != usageDigest {
			return BudgetSnapshot{}, false, ErrUsageConflict
		}
		return s.budgetSnapshotLocked(record), false, nil
	}
	if err := s.settleLocked(record, command.UsageFactKey, command.Usage); err != nil {
		return BudgetSnapshot{}, false, err
	}
	return s.budgetSnapshotLocked(record), true, nil
}

func (s *MemoryStore) RequestCancel(_ context.Context, request CancelRequest, now time.Time) (CancelResult, error) {
	if request.TenantKey == "" || request.RequestKey == "" || request.RunKey == "" || (request.Mode != CancelSuspend && request.Mode != CancelAbandon) || request.MaxTraversal <= 0 {
		return CancelResult{}, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idempotencyKey := string(request.TenantKey) + "\x00" + request.RequestKey
	if existing, ok := s.cancelRequests[idempotencyKey]; ok {
		return existing, nil
	}
	queue := []RunKey{request.RunKey}
	seen := make(map[RunKey]struct{})
	records := make([]*memoryRecord, 0)
	for len(queue) > 0 {
		if len(seen) >= request.MaxTraversal {
			return CancelResult{}, ErrTraversalLimit
		}
		parentRun := queue[0]
		queue = queue[1:]
		if _, ok := seen[parentRun]; ok {
			continue
		}
		seen[parentRun] = struct{}{}
		for _, id := range s.sortedIDsLocked(request.TenantKey) {
			record := s.records[id]
			if record.snapshot.Parent.RunKey != parentRun {
				continue
			}
			records = append(records, record)
			queue = append(queue, record.snapshot.Receipt.Child.RunKey)
		}
	}
	result := CancelResult{Affected: len(records)}
	for _, record := range records {
		if record.snapshot.State.Terminal() {
			result.Terminal++
			continue
		}
		if err := s.cancelRecordLocked(record, request.Mode, now, request.Reason); err != nil {
			return CancelResult{}, err
		}
		result.Pending++
	}
	s.cancelRequests[idempotencyKey] = result
	return result, nil
}

func (s *MemoryStore) ClaimWake(_ context.Context, tenantKey agent.TenantKey) (WakeClaim, bool, error) {
	if tenantKey == "" {
		return WakeClaim{}, false, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.sortedIDsLocked(tenantKey) {
		record := s.records[id]
		if !record.snapshot.WakePending || record.snapshot.WakeDelivered {
			continue
		}
		return WakeClaim{Request: wakeRequest(record.snapshot), Version: record.snapshot.Version}, true, nil
	}
	return WakeClaim{}, false, nil
}

func (s *MemoryStore) CompleteWake(_ context.Context, tenantKey agent.TenantKey, wakeKey WakeKey, version uint64, now time.Time) error {
	if tenantKey == "" || wakeKey == "" || version == 0 || now.IsZero() {
		return ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.sortedIDsLocked(tenantKey) {
		record := s.records[id]
		if wakeRequest(record.snapshot).WakeKey != wakeKey {
			continue
		}
		if record.snapshot.WakeDelivered {
			return nil
		}
		if record.snapshot.Version != version || !record.snapshot.WakePending {
			return ErrWakeConflict
		}
		record.snapshot.WakeDelivered = true
		record.snapshot.WakePending = false
		record.snapshot.Version++
		s.appendFactLocked(record, FactChildConsumed, now)
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) Recover(_ context.Context, request ReconcileRequest) (int, error) {
	if request.TenantKey == "" || request.Now.IsZero() || request.Limit < 0 {
		return 0, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recovered := 0
	for _, id := range s.sortedIDsLocked(request.TenantKey) {
		if request.Limit > 0 && recovered >= request.Limit {
			break
		}
		record := s.records[id]
		expiredRun := record.snapshot.State == ChildRunning && !record.snapshot.ClaimUntil.After(request.Now)
		operationalSuspend := record.snapshot.State == ChildSuspended && record.snapshot.CancelMode == ""
		if expiredRun || operationalSuspend {
			record.snapshot.State = ChildQueued
			record.snapshot.Receipt.State = ChildQueued
			record.snapshot.ClaimOwner = ""
			record.snapshot.ClaimUntil = time.Time{}
			record.snapshot.Version++
			recovered++
		}
	}
	return recovered, nil
}

func (s *MemoryStore) Facts(_ context.Context, tenantKey agent.TenantKey, after uint64, limit int) ([]CommittedFact, error) {
	if tenantKey == "" || limit < 0 {
		return nil, ErrInvalidRequest
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.facts[tenantKey]
	if after >= uint64(len(all)) {
		return []CommittedFact{}, nil
	}
	end := len(all)
	if limit > 0 && int(after)+limit < end {
		end = int(after) + limit
	}
	result := make([]CommittedFact, 0, end-int(after))
	for _, fact := range all[after:end] {
		result = append(result, cloneFact(fact))
	}
	return result, nil
}

func (s *MemoryStore) resolveParentLocked(request SpawnRequest) (uint16, TreeKey, *treeBudget, bool, error) {
	if request.Parent.RelationshipKey == "" {
		treeKey := request.Parent.TreeKey
		if treeKey == "" {
			treeKey = TreeKey(request.Parent.RunKey)
		}
		id := treeID{tenant: request.Parent.TenantKey, key: treeKey}
		tree := s.trees[id]
		if tree == nil {
			tree = &treeBudget{budget: BudgetSnapshot{Limits: request.Limits}}
			return 1, treeKey, tree, true, nil
		} else if tree.budget.Limits != request.Limits {
			return 0, "", nil, false, ErrIdempotencyConflict
		}
		return 1, treeKey, tree, false, nil
	}
	parent := s.records[relationshipID{tenant: request.Parent.TenantKey, key: request.Parent.RelationshipKey}]
	if parent == nil || parent.snapshot.Receipt.Child.RunKey != request.Parent.RunKey {
		return 0, "", nil, false, ErrNotFound
	}
	if parent.snapshot.State.Terminal() {
		return 0, "", nil, false, ErrInvalidTransition
	}
	treeKey := parent.snapshot.Parent.TreeKey
	tree := s.trees[treeID{tenant: request.Parent.TenantKey, key: treeKey}]
	if tree == nil {
		return 0, "", nil, false, ErrStoreInvariant
	}
	return parent.snapshot.Receipt.Child.Depth + 1, treeKey, tree, false, nil
}

// parentLookupLocked exposes the delegation graph to the shared cycle check.
// Only storage can walk it, so storage supplies the walk and the library owns
// what counts as a cycle.
func (s *MemoryStore) parentLookupLocked(tenantKey agent.TenantKey) ParentLookup {
	return func(run RunKey) (RunKey, bool) {
		id, ok := s.runs[tenantKey][run]
		if !ok {
			return "", false
		}
		record := s.records[id]
		if record == nil {
			return "", false
		}
		return record.snapshot.Parent.RunKey, true
	}
}

func (s *MemoryStore) directFanoutLocked(tenantKey agent.TenantKey, parentRun RunKey) int {
	count := 0
	for id, record := range s.records {
		if id.tenant == tenantKey && record.snapshot.Parent.RunKey == parentRun && record.snapshot.State != ChildCanceled {
			count++
		}
	}
	return count
}

func (s *MemoryStore) settleLocked(record *memoryRecord, key UsageFactKey, usage Usage) error {
	if err := validateUsage(usage); err != nil {
		return err
	}
	usageDigest, err := digest(usage)
	if err != nil {
		return err
	}
	if existing, ok := record.usageFacts[key]; ok {
		if existing.digest != usageDigest {
			return ErrUsageConflict
		}
		return nil
	}
	if record.settled {
		return ErrUsageConflict
	}
	reservation := record.snapshot.Receipt.Reservation
	tree := s.trees[treeID{tenant: record.snapshot.Receipt.Child.TenantKey, key: record.snapshot.Parent.TreeKey}]
	if tree == nil {
		return ErrStoreInvariant
	}
	settled, err := tree.budget.Settle(reservation, usage)
	if err != nil {
		return err
	}
	tree.budget = settled
	record.settled = true
	record.usageFacts[key] = usageRecord{usage: usage, digest: usageDigest}
	record.snapshot.UsageFactKey = key
	record.snapshot.Usage = usage
	return nil
}

func (s *MemoryStore) cancelRecordLocked(record *memoryRecord, mode CancelMode, now time.Time, reason string) error {
	if record.snapshot.State.Terminal() {
		return nil
	}
	if record.snapshot.CancelMode == CancelAbandon || (record.snapshot.CancelMode == CancelSuspend && mode == CancelSuspend) {
		return nil
	}
	record.snapshot.CancelMode = mode
	record.snapshot.Version++
	s.appendFactLocked(record, FactChildCancelRequested, now)
	if mode == CancelSuspend {
		record.snapshot.State = ChildSuspended
		record.snapshot.Receipt.State = ChildSuspended
		record.snapshot.ClaimOwner = ""
		record.snapshot.ClaimUntil = time.Time{}
		return nil
	}
	usageKey := UsageFactKey("cancel/" + string(record.snapshot.Receipt.Child.RunKey))
	if err := s.settleLocked(record, usageKey, Usage{}); err != nil && !errors.Is(err, ErrUsageConflict) {
		return err
	}
	record.snapshot.State = ChildCanceled
	record.snapshot.Receipt.State = ChildCanceled
	record.snapshot.Failure = &Failure{Code: "canceled", Message: reason}
	record.snapshot.TerminalAt = now
	record.snapshot.ClaimOwner = ""
	record.snapshot.ClaimUntil = time.Time{}
	record.snapshot.WakePending = true
	record.snapshot.Version++
	s.appendFactLocked(record, FactChildTerminal, now)
	return nil
}

func (s *MemoryStore) appendFactLocked(record *memoryRecord, kind FactKind, now time.Time) {
	tenantKey := record.snapshot.Receipt.Child.TenantKey
	facts := s.facts[tenantKey]
	fact := CommittedFact{
		FactKey:         FactKey(fmt.Sprintf("fact/%s/%d", record.snapshot.Receipt.Child.RelationshipKey, record.snapshot.Version)),
		TenantKey:       tenantKey,
		Kind:            kind,
		RelationshipKey: record.snapshot.Receipt.Child.RelationshipKey,
		ParentRunKey:    record.snapshot.Parent.RunKey,
		ChildRunKey:     record.snapshot.Receipt.Child.RunKey,
		Revision:        record.snapshot.Version,
		OccurredAt:      now,
	}
	s.facts[tenantKey] = append(facts, fact)
}

func (s *MemoryStore) sortedIDsLocked(tenantKey agent.TenantKey) []relationshipID {
	ids := make([]relationshipID, 0)
	for id := range s.records {
		if id.tenant == tenantKey {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].key < ids[j].key })
	return ids
}

func (s *MemoryStore) budgetSnapshotLocked(record *memoryRecord) BudgetSnapshot {
	tree := s.trees[treeID{tenant: record.snapshot.Receipt.Child.TenantKey, key: record.snapshot.Parent.TreeKey}]
	if tree == nil {
		return BudgetSnapshot{}
	}
	return tree.budget
}

func validateSpawn(request SpawnRequest, now time.Time) error {
	if request.RequestKey == "" || request.Parent.TenantKey == "" || request.Parent.SessionKey == "" || request.Parent.RunKey == "" || request.AgentKey == "" || len(request.Input) == 0 || now.IsZero() {
		return ErrInvalidRequest
	}
	if request.Parent.RelationshipKey != "" && request.Parent.TreeKey != "" {
		return fmt.Errorf("%w: nested parent tree is store-owned", ErrInvalidRequest)
	}
	if request.Limits.MaxDepth == 0 || request.Limits.MaxFanout == 0 || request.Limits.MaxInputTokens < 0 || request.Limits.MaxOutputTokens < 0 || request.Limits.MaxCostMicros < 0 || request.Limits.MaxToolCalls < 0 || request.Limits.MaxRuntime < 0 {
		return ErrInvalidRequest
	}
	if request.Reserve.InputTokens < 0 || request.Reserve.OutputTokens < 0 || request.Reserve.CostMicros < 0 || request.Reserve.ToolCalls < 0 || request.Reserve.Runtime < 0 {
		return ErrInvalidRequest
	}
	if (!request.Deadline.IsZero() && !request.Deadline.After(now)) || (!request.Limits.Deadline.IsZero() && !request.Limits.Deadline.After(now)) {
		return ErrDeadlineExceeded
	}
	return nil
}

func effectiveLimits(limits Limits, reserve Reservation, requested, now time.Time) Limits {
	result := limits
	result.MaxInputTokens = reserve.InputTokens
	result.MaxOutputTokens = reserve.OutputTokens
	result.MaxCostMicros = reserve.CostMicros
	result.MaxToolCalls = reserve.ToolCalls
	result.MaxRuntime = reserve.Runtime
	if result.Deadline.IsZero() || (!requested.IsZero() && requested.Before(result.Deadline)) {
		result.Deadline = requested
	}
	runtimeDeadline := now.Add(reserve.Runtime)
	if reserve.Runtime > 0 && (result.Deadline.IsZero() || runtimeDeadline.Before(result.Deadline)) {
		result.Deadline = runtimeDeadline
	}
	return result
}

func validateUsage(usage Usage) error {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CostMicros < 0 || usage.ToolCalls < 0 || usage.Runtime < 0 {
		return ErrInvalidRequest
	}
	return nil
}

func exceedsReservation(usage Usage, reservation Reservation) bool {
	return usage.InputTokens > reservation.InputTokens || usage.OutputTokens > reservation.OutputTokens || usage.CostMicros > reservation.CostMicros || usage.ToolCalls > reservation.ToolCalls || usage.Runtime > reservation.Runtime
}

func addReservation(left, right Reservation) Reservation {
	return Reservation{left.InputTokens + right.InputTokens, left.OutputTokens + right.OutputTokens, left.CostMicros + right.CostMicros, left.ToolCalls + right.ToolCalls, left.Runtime + right.Runtime}
}

func subtractReservation(left, right Reservation) Reservation {
	return Reservation{left.InputTokens - right.InputTokens, left.OutputTokens - right.OutputTokens, left.CostMicros - right.CostMicros, left.ToolCalls - right.ToolCalls, left.Runtime - right.Runtime}
}

func addUsage(left, right Usage) Usage {
	return Usage{left.InputTokens + right.InputTokens, left.OutputTokens + right.OutputTokens, left.CostMicros + right.CostMicros, left.ToolCalls + right.ToolCalls, left.Runtime + right.Runtime}
}

func unusedReservation(reservation Reservation, usage Usage) Reservation {
	return Reservation{reservation.InputTokens - usage.InputTokens, reservation.OutputTokens - usage.OutputTokens, reservation.CostMicros - usage.CostMicros, reservation.ToolCalls - usage.ToolCalls, reservation.Runtime - usage.Runtime}
}

func sameTerminal(snapshot Snapshot, result RunResult) bool {
	return snapshot.State == result.State && snapshot.ResultRef == result.ResultRef && snapshot.FailureRef == result.FailureRef && snapshot.UsageFactKey == result.UsageFactKey && snapshot.Usage == result.Usage && failuresEqual(snapshot.Failure, result.Failure)
}

func failuresEqual(left, right *Failure) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func wakeRequest(snapshot Snapshot) WakeRequest {
	return WakeRequest{
		WakeKey: WakeKey("wake/" + string(snapshot.Receipt.Child.RelationshipKey)),
		Parent:  snapshot.Parent,
		Child:   snapshot.Receipt.Child,
		State:   snapshot.State,
	}
}
