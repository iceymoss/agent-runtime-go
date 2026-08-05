package permission

import (
	"context"
	"reflect"
	"sort"
	"sync"
	"time"
)

type MemoryStore struct {
	mu       sync.RWMutex
	clock    Clock
	requests map[string]Snapshot
	grants   map[string]Grant
}

func NewMemoryStore(clocks ...Clock) *MemoryStore {
	var clock Clock = realClock{}
	if len(clocks) > 0 && clocks[0] != nil {
		clock = clocks[0]
	}
	return &MemoryStore{clock: clock, requests: make(map[string]Snapshot), grants: make(map[string]Grant)}
}

func tenantObjectKey(tenant TenantKey, key string) string { return string(tenant) + "\x00" + key }

func (m *MemoryStore) CreateAndSuspend(ctx context.Context, command CreateApprovalCommand) (Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, false, err
	}
	r := command.Request
	if !validCheck(r.Check) || r.RequestKey == "" || r.RequestKey != r.Check.RequestKey || command.ResumeToken == "" {
		return Snapshot{}, false, permissionError(ErrInvalidRequest, "create", r.RequestKey, "missing binding")
	}
	now := m.clock.Now().UTC()
	if !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(now) {
		return Snapshot{}, false, permissionError(ErrRequestExpired, "create", r.RequestKey, "")
	}
	r.State, r.Revision = ApprovalPending, 1
	if r.CreatedAt.IsZero() {
		r.CreatedAt = now
	}
	candidate := Snapshot{Request: r, ResumeToken: command.ResumeToken}
	key := tenantObjectKey(r.Check.Subject.TenantKey, string(r.RequestKey))
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.requests[key]; ok {
		if !sameImmutableRequest(existing, candidate) {
			return Snapshot{}, false, permissionError(ErrRequestConflict, "create", r.RequestKey, "immutable values differ")
		}
		return cloneSnapshot(existing), false, nil
	}
	m.requests[key] = cloneSnapshot(candidate)
	return cloneSnapshot(candidate), true, nil
}

func (m *MemoryStore) Resolve(ctx context.Context, command ResolveCommand) (Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, false, err
	}
	if !command.TenantKey.Valid() || command.RequestKey == "" || command.CommandKey == "" || command.DecisionKey == "" || command.ApproverKey == "" || command.Kind != ResolutionApprove && command.Kind != ResolutionDeny {
		return Snapshot{}, false, permissionError(ErrInvalidRequest, "resolve", command.RequestKey, "invalid decision")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantObjectKey(command.TenantKey, string(command.RequestKey))
	current, ok := m.requests[key]
	if !ok {
		return Snapshot{}, false, permissionError(ErrRequestNotFound, "resolve", command.RequestKey, "")
	}
	now := m.clock.Now().UTC()
	m.expireRequestLocked(key, &current, now)
	if current.Request.State != ApprovalPending {
		if current.Resolution != nil && sameResolution(*current.Resolution, command) && sameResolutionGrant(current.Grant, command.GrantKey, command.Grant) {
			return cloneSnapshot(current), false, nil
		}
		kind := ErrAlreadyResolved
		if current.Request.State == ApprovalExpired {
			kind = ErrRequestExpired
		}
		if current.Request.State == ApprovalCanceled {
			kind = ErrRequestCanceled
		}
		return Snapshot{}, false, permissionError(kind, "resolve", command.RequestKey, "terminal state")
	}
	if current.Request.Revision != command.ExpectedRevision {
		return Snapshot{}, false, permissionError(ErrRevisionConflict, "resolve", command.RequestKey, "")
	}
	resolution := Resolution{DecisionKey: command.DecisionKey, RequestKey: command.RequestKey, CommandKey: command.CommandKey, ApproverKey: command.ApproverKey, Kind: command.Kind, ReasonCode: command.ReasonCode, Comment: command.Comment, DecidedAt: now}
	current.Resolution, current.Request.ResolvedAt = &resolution, now
	if command.Kind == ResolutionApprove {
		current.Request.State = ApprovalApproved
	} else {
		current.Request.State = ApprovalDenied
	}
	current.Request.Revision++
	if command.Grant != nil {
		if command.Kind != ResolutionApprove {
			return Snapshot{}, false, permissionError(ErrInvalidRequest, "resolve", command.RequestKey, "deny cannot create grant")
		}
		grant := Grant{TenantKey: command.TenantKey, GrantKey: command.GrantKey, Spec: cloneGrantSpec(*command.Grant), State: GrantActive, Revision: 1, CreatedByDecisionKey: command.DecisionKey, CreatedAt: now}
		if grant.GrantKey == "" || !validGrantForRequest(grant.Spec, current.Request.Check) {
			return Snapshot{}, false, permissionError(ErrGrantNotApplicable, "resolve", command.RequestKey, "invalid grant binding")
		}
		if grant.Spec.ExpiresAt.IsZero() || !grant.Spec.ExpiresAt.After(now) {
			return Snapshot{}, false, permissionError(ErrGrantExpired, "resolve", command.RequestKey, "grant expiry is not in the future")
		}
		grantStorageKey := tenantObjectKey(command.TenantKey, string(grant.GrantKey))
		if _, exists := m.grants[grantStorageKey]; exists {
			return Snapshot{}, false, permissionError(ErrRequestConflict, "resolve", command.RequestKey, "grant key already exists")
		}
		if grant.Spec.MaxUses != nil {
			remaining := *grant.Spec.MaxUses
			grant.RemainingUses = &remaining
			if remaining == 0 {
				grant.State = GrantExhausted
			}
		}
		m.grants[grantStorageKey] = cloneGrant(grant)
		current.Grant = &grant
	}
	m.requests[key] = cloneSnapshot(current)
	return cloneSnapshot(current), true, nil
}

func (m *MemoryStore) Cancel(ctx context.Context, command CancelCommand) (Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantObjectKey(command.TenantKey, string(command.RequestKey))
	current, ok := m.requests[key]
	if !ok {
		return Snapshot{}, false, permissionError(ErrRequestNotFound, "cancel", command.RequestKey, "")
	}
	m.expireRequestLocked(key, &current, m.clock.Now().UTC())
	if current.Request.State == ApprovalCanceled {
		return cloneSnapshot(current), false, nil
	}
	if current.Request.State != ApprovalPending {
		return Snapshot{}, false, permissionError(ErrAlreadyResolved, "cancel", command.RequestKey, "")
	}
	if current.Request.Revision != command.ExpectedRevision {
		return Snapshot{}, false, permissionError(ErrRevisionConflict, "cancel", command.RequestKey, "")
	}
	if current.Request.Check.AttemptRef != command.AttemptRef || current.Request.Check.FenceToken != command.FenceToken {
		return Snapshot{}, false, permissionError(ErrStaleFence, "cancel", command.RequestKey, "")
	}
	current.Request.State, current.Request.ResolvedAt = ApprovalCanceled, m.clock.Now().UTC()
	current.Request.Revision++
	m.requests[key] = current
	return cloneSnapshot(current), true, nil
}

func (m *MemoryStore) GetRequest(ctx context.Context, query GetRequestQuery) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantObjectKey(query.TenantKey, string(query.RequestKey))
	current, ok := m.requests[key]
	if !ok {
		return Snapshot{}, permissionError(ErrRequestNotFound, "get", query.RequestKey, "")
	}
	m.expireRequestLocked(key, &current, m.clock.Now().UTC())
	return cloneSnapshot(current), nil
}

func (m *MemoryStore) ListPending(ctx context.Context, query ListPendingQuery) ([]Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now().UTC()
	out := make([]Snapshot, 0)
	for key, value := range m.requests {
		current := value
		m.expireRequestLocked(key, &current, now)
		if current.Request.Check.Subject.TenantKey == query.TenantKey && current.Request.State == ApprovalPending && (query.SessionRef == "" || current.Request.Check.SessionRef == query.SessionRef) {
			out = append(out, cloneSnapshot(current))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Request.RequestKey < out[j].Request.RequestKey })
	return out, nil
}

func (m *MemoryStore) FindGrants(ctx context.Context, query GrantQuery) ([]Grant, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now().UTC()
	out := []Grant{}
	for key, value := range m.grants {
		grant := value
		m.expireGrantLocked(key, &grant, now)
		if grant.TenantKey == query.TenantKey && grantApplicable(grant, query.Check) {
			out = append(out, cloneGrant(grant))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GrantKey < out[j].GrantKey })
	return out, nil
}

func (m *MemoryStore) ConsumeGrant(ctx context.Context, command ConsumeGrantCommand) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantObjectKey(command.TenantKey, string(command.GrantKey))
	grant, ok := m.grants[key]
	if !ok {
		return Grant{}, permissionError(ErrGrantNotFound, "consume grant", "", "")
	}
	m.expireGrantLocked(key, &grant, m.clock.Now().UTC())
	if grant.Revision != command.ExpectedRevision {
		return Grant{}, permissionError(ErrRevisionConflict, "consume grant", "", "")
	}
	if grant.State == GrantExpired {
		return Grant{}, ErrGrantExpired
	}
	if grant.State == GrantRevoked {
		return Grant{}, ErrGrantRevoked
	}
	if grant.State == GrantExhausted {
		return Grant{}, ErrGrantExhausted
	}
	if !grantApplicable(grant, command.Check) {
		return Grant{}, ErrGrantNotApplicable
	}
	grant.Used++
	grant.Revision++
	if grant.RemainingUses != nil {
		if *grant.RemainingUses == 0 {
			return Grant{}, ErrGrantExhausted
		}
		remaining := *grant.RemainingUses - 1
		grant.RemainingUses = &remaining
		if remaining == 0 {
			grant.State = GrantExhausted
		}
	}
	m.grants[key] = cloneGrant(grant)
	return cloneGrant(grant), nil
}

func (m *MemoryStore) RevokeGrant(ctx context.Context, command RevokeGrantCommand) (Grant, error) {
	if err := ctx.Err(); err != nil {
		return Grant{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := tenantObjectKey(command.TenantKey, string(command.GrantKey))
	grant, ok := m.grants[key]
	if !ok {
		return Grant{}, ErrGrantNotFound
	}
	if grant.Revision != command.ExpectedRevision {
		return Grant{}, ErrRevisionConflict
	}
	if grant.State == GrantRevoked {
		return cloneGrant(grant), nil
	}
	if grant.State != GrantActive {
		return Grant{}, ErrGrantNotApplicable
	}
	grant.State, grant.RevokedAt = GrantRevoked, m.clock.Now().UTC()
	grant.Revision++
	m.grants[key] = grant
	return cloneGrant(grant), nil
}

func (m *MemoryStore) ExpireDue(ctx context.Context, command ExpireCommand) (ExpireResult, error) {
	if err := ctx.Err(); err != nil {
		return ExpireResult{}, err
	}
	if command.Limit < 0 {
		return ExpireResult{}, ErrInvalidRequest
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock.Now().UTC()
	result := ExpireResult{}
	requestKeys := make([]string, 0, len(m.requests))
	for key, value := range m.requests {
		if value.Request.Check.Subject.TenantKey == command.TenantKey {
			requestKeys = append(requestKeys, key)
		}
	}
	sort.Strings(requestKeys)
	for _, key := range requestKeys {
		if command.Limit > 0 && len(result.Approvals)+len(result.Grants) >= command.Limit {
			break
		}
		current := m.requests[key]
		before := current.Request.State
		m.expireRequestLocked(key, &current, now)
		if before != current.Request.State {
			result.Approvals = append(result.Approvals, cloneSnapshot(current))
		}
	}
	grantKeys := make([]string, 0, len(m.grants))
	for key, value := range m.grants {
		if value.TenantKey == command.TenantKey {
			grantKeys = append(grantKeys, key)
		}
	}
	sort.Strings(grantKeys)
	for _, key := range grantKeys {
		if command.Limit > 0 && len(result.Approvals)+len(result.Grants) >= command.Limit {
			break
		}
		current := m.grants[key]
		before := current.State
		m.expireGrantLocked(key, &current, now)
		if before != current.State {
			result.Grants = append(result.Grants, cloneGrant(current))
		}
	}
	return result, nil
}

func (m *MemoryStore) expireRequestLocked(key string, current *Snapshot, now time.Time) {
	if current.Request.State == ApprovalPending && !current.Request.ExpiresAt.IsZero() && !current.Request.ExpiresAt.After(now) {
		current.Request.State, current.Request.ResolvedAt = ApprovalExpired, now
		current.Request.Revision++
		m.requests[key] = cloneSnapshot(*current)
	}
}
func (m *MemoryStore) expireGrantLocked(key string, grant *Grant, now time.Time) {
	if grant.State == GrantActive && !grant.Spec.ExpiresAt.IsZero() && !grant.Spec.ExpiresAt.After(now) {
		grant.State = GrantExpired
		grant.Revision++
		m.grants[key] = cloneGrant(*grant)
	}
}

func validCheck(c CheckRequest) bool {
	return c.RequestKey != "" && c.Subject.TenantKey.Valid() && c.Subject.PrincipalKey != "" && c.RunRef != "" && c.AttemptRef != "" && c.ExecutionRef != "" && c.InputDigest != "" && c.PolicyVersion != "" && c.ToolName != "" && c.Action != ""
}
func sameImmutableRequest(a, b Snapshot) bool {
	aa, bb := a.Request, b.Request
	aa.State, bb.State = "", ""
	aa.Revision, bb.Revision = 0, 0
	aa.CreatedAt, bb.CreatedAt = time.Time{}, time.Time{}
	aa.ResolvedAt, bb.ResolvedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(aa, bb)
}
func sameResolution(r Resolution, c ResolveCommand) bool {
	return r.DecisionKey == c.DecisionKey && r.CommandKey == c.CommandKey && r.ApproverKey == c.ApproverKey && r.Kind == c.Kind && r.ReasonCode == c.ReasonCode && r.Comment == c.Comment
}
func sameResolutionGrant(grant *Grant, key GrantKey, spec *GrantSpec) bool {
	if grant == nil || spec == nil {
		return grant == nil && spec == nil
	}
	return grant.GrantKey == key && reflect.DeepEqual(grant.Spec, *spec)
}
func validGrantForRequest(s GrantSpec, c CheckRequest) bool {
	validScope := s.Scope == ScopeInvocation || s.Scope == ScopeSession || s.Scope == ScopePrincipal
	return validScope && s.PrincipalKey == c.Subject.PrincipalKey && s.ToolName == c.ToolName && s.Action == c.Action && s.ResourceKind == c.Resource.Kind && s.ResourceKey == c.Resource.Key && s.PolicyVersion == c.PolicyVersion && s.ToolGeneration == c.ToolGeneration && (s.Scope != ScopeInvocation || s.InputDigest == c.InputDigest) && (s.Scope != ScopeSession || s.SessionRef == c.SessionRef)
}
func grantApplicable(g Grant, c CheckRequest) bool {
	return g.State == GrantActive && g.TenantKey == c.Subject.TenantKey && validGrantForRequest(g.Spec, c)
}
func cloneGrantSpec(s GrantSpec) GrantSpec {
	if s.MaxUses != nil {
		v := *s.MaxUses
		s.MaxUses = &v
	}
	return s
}
func cloneGrant(g Grant) Grant {
	g.Spec = cloneGrantSpec(g.Spec)
	if g.RemainingUses != nil {
		value := *g.RemainingUses
		g.RemainingUses = &value
	}
	return g
}
func cloneSnapshot(s Snapshot) Snapshot {
	if s.Resolution != nil {
		v := *s.Resolution
		s.Resolution = &v
	}
	if s.Grant != nil {
		v := cloneGrant(*s.Grant)
		s.Grant = &v
	}
	return s
}
