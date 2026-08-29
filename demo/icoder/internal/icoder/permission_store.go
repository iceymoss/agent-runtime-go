package icoder

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/iceymoss/agent-runtime-go/permission"
)

// SQLitePermissionStore persists approval requests and grants.
//
// It exists because an approval outlives the process that asked for it. A
// non-interactive run that stops for a decision is only useful if a human can
// answer it later from another command, and that requires the pending request,
// its resume token, and any grant it produces to survive process exit.
//
// Snapshots and grants are stored as JSON with a few extracted columns. The
// extracted columns are exactly the ones queries filter on; everything else stays
// in the payload so a contract change does not silently drop a field.
type SQLitePermissionStore struct {
	db    *sql.DB
	clock func() time.Time
}

// NewSQLitePermissionStore binds the approval store to an already-migrated
// database. clock may be nil, in which case wall-clock UTC time is used.
func NewSQLitePermissionStore(db *sql.DB, clock func() time.Time) *SQLitePermissionStore {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &SQLitePermissionStore{db: db, clock: clock}
}

var _ permission.Store = (*SQLitePermissionStore)(nil)

// PermissionStore returns the approval store backed by this store's database.
func (s *Store) PermissionStore() *SQLitePermissionStore {
	return NewSQLitePermissionStore(s.db, nil)
}

var permissionMigrations = []string{
	`CREATE TABLE IF NOT EXISTS permission_requests (
		tenant_key TEXT NOT NULL,
		request_key TEXT NOT NULL,
		state TEXT NOT NULL,
		session_ref TEXT NOT NULL DEFAULT '',
		expires_at INTEGER NOT NULL DEFAULT 0,
		revision INTEGER NOT NULL DEFAULT 0,
		payload BLOB NOT NULL,
		PRIMARY KEY (tenant_key, request_key)
	)`,
	`CREATE INDEX IF NOT EXISTS permission_requests_pending ON permission_requests(tenant_key, state, request_key)`,
	`CREATE TABLE IF NOT EXISTS permission_grants (
		tenant_key TEXT NOT NULL,
		grant_key TEXT NOT NULL,
		state TEXT NOT NULL,
		principal_key TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		action TEXT NOT NULL,
		resource_kind TEXT NOT NULL,
		resource_key TEXT NOT NULL,
		expires_at INTEGER NOT NULL DEFAULT 0,
		revision INTEGER NOT NULL DEFAULT 0,
		payload BLOB NOT NULL,
		PRIMARY KEY (tenant_key, grant_key)
	)`,
	`CREATE INDEX IF NOT EXISTS permission_grants_lookup
		ON permission_grants(tenant_key, principal_key, tool_name, action, resource_kind, resource_key, state)`,
}

func permissionFailf(kind error, operation string, key permission.RequestKey, detail string) error {
	if detail == "" {
		return fmt.Errorf("icoder permission: %s request=%q: %w", operation, key, kind)
	}
	return fmt.Errorf("icoder permission: %s request=%q: %s: %w", operation, key, detail, kind)
}

func (s *SQLitePermissionStore) begin(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

func loadRequest(ctx context.Context, tx *sql.Tx, tenant permission.TenantKey, key permission.RequestKey) (permission.Snapshot, error) {
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM permission_requests WHERE tenant_key = ? AND request_key = ?`, string(tenant), string(key)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return permission.Snapshot{}, permissionFailf(permission.ErrRequestNotFound, "get", key, "")
	}
	if err != nil {
		return permission.Snapshot{}, err
	}
	var snapshot permission.Snapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return permission.Snapshot{}, err
	}
	return snapshot, nil
}

func saveRequest(ctx context.Context, tx *sql.Tx, snapshot permission.Snapshot) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO permission_requests(tenant_key, request_key, state, session_ref, expires_at, revision, payload) VALUES(?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_key, request_key) DO UPDATE SET state = excluded.state, session_ref = excluded.session_ref, expires_at = excluded.expires_at, revision = excluded.revision, payload = excluded.payload`,
		string(snapshot.Request.Check.Subject.TenantKey), string(snapshot.Request.RequestKey), string(snapshot.Request.State),
		snapshot.Request.Check.SessionRef, nanos(snapshot.Request.ExpiresAt), snapshot.Request.Revision, payload)
	return err
}

func saveGrant(ctx context.Context, tx *sql.Tx, grant permission.Grant) error {
	payload, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO permission_grants(tenant_key, grant_key, state, principal_key, tool_name, action, resource_kind, resource_key, expires_at, revision, payload) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_key, grant_key) DO UPDATE SET state = excluded.state, expires_at = excluded.expires_at, revision = excluded.revision, payload = excluded.payload`,
		string(grant.TenantKey), string(grant.GrantKey), string(grant.State), grant.Spec.PrincipalKey,
		grant.Spec.ToolName, grant.Spec.Action, grant.Spec.ResourceKind, grant.Spec.ResourceKey,
		nanos(grant.Spec.ExpiresAt), grant.Revision, payload)
	return err
}

// expireRequest applies lazy expiry. An approval that timed out must read as
// expired even if nobody ran a sweep, otherwise a stale decision could still be
// revalidated.
func (s *SQLitePermissionStore) expireRequest(ctx context.Context, tx *sql.Tx, snapshot *permission.Snapshot, now time.Time) error {
	if snapshot.Request.State != permission.ApprovalPending || snapshot.Request.ExpiresAt.IsZero() || snapshot.Request.ExpiresAt.After(now) {
		return nil
	}
	snapshot.Request.State, snapshot.Request.ResolvedAt = permission.ApprovalExpired, now
	snapshot.Request.Revision++
	return saveRequest(ctx, tx, *snapshot)
}

func (s *SQLitePermissionStore) expireGrant(ctx context.Context, tx *sql.Tx, grant *permission.Grant, now time.Time) error {
	if grant.State != permission.GrantActive || grant.Spec.ExpiresAt.IsZero() || grant.Spec.ExpiresAt.After(now) {
		return nil
	}
	grant.State = permission.GrantExpired
	grant.Revision++
	return saveGrant(ctx, tx, *grant)
}

// CreateAndSuspend records a pending approval. It is idempotent by request key so
// a retried check reuses the same request and resume token instead of stranding
// the first one.
func (s *SQLitePermissionStore) CreateAndSuspend(ctx context.Context, command permission.CreateApprovalCommand) (snapshot permission.Snapshot, created bool, err error) {
	request := command.Request
	if request.RequestKey == "" || request.RequestKey != request.Check.RequestKey || command.ResumeToken == "" {
		return permission.Snapshot{}, false, permissionFailf(permission.ErrInvalidRequest, "create", request.RequestKey, "missing binding")
	}
	now := s.clock().UTC()
	if !request.ExpiresAt.IsZero() && !request.ExpiresAt.After(now) {
		return permission.Snapshot{}, false, permissionFailf(permission.ErrRequestExpired, "create", request.RequestKey, "")
	}
	request.State, request.Revision = permission.ApprovalPending, 1
	if request.CreatedAt.IsZero() {
		request.CreatedAt = now
	}
	candidate := permission.Snapshot{Request: request, ResumeToken: command.ResumeToken}
	tx, err := s.begin(ctx)
	if err != nil {
		return permission.Snapshot{}, false, err
	}
	defer finishTx(tx, &err)
	existing, loadErr := loadRequest(ctx, tx, request.Check.Subject.TenantKey, request.RequestKey)
	if loadErr == nil {
		if !sameImmutableApproval(existing, candidate) {
			return permission.Snapshot{}, false, permissionFailf(permission.ErrRequestConflict, "create", request.RequestKey, "immutable values differ")
		}
		return existing, false, nil
	}
	if !errors.Is(loadErr, permission.ErrRequestNotFound) {
		return permission.Snapshot{}, false, loadErr
	}
	if err = saveRequest(ctx, tx, candidate); err != nil {
		return permission.Snapshot{}, false, err
	}
	return candidate, true, nil
}

// Resolve records the human decision and, when approving, the grant that
// authorizes the exact effect the decision was about.
func (s *SQLitePermissionStore) Resolve(ctx context.Context, command permission.ResolveCommand) (snapshot permission.Snapshot, applied bool, err error) {
	if !command.TenantKey.Valid() || command.RequestKey == "" || command.CommandKey == "" || command.DecisionKey == "" || command.ApproverKey == "" ||
		(command.Kind != permission.ResolutionApprove && command.Kind != permission.ResolutionDeny) {
		return permission.Snapshot{}, false, permissionFailf(permission.ErrInvalidRequest, "resolve", command.RequestKey, "invalid decision")
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return permission.Snapshot{}, false, err
	}
	defer finishTx(tx, &err)
	current, err := loadRequest(ctx, tx, command.TenantKey, command.RequestKey)
	if err != nil {
		return permission.Snapshot{}, false, err
	}
	now := s.clock().UTC()
	if err = s.expireRequest(ctx, tx, &current, now); err != nil {
		return permission.Snapshot{}, false, err
	}
	if current.Request.State != permission.ApprovalPending {
		if current.Resolution != nil && sameApprovalResolution(*current.Resolution, command) {
			return current, false, nil
		}
		kind := permission.ErrAlreadyResolved
		switch current.Request.State {
		case permission.ApprovalExpired:
			kind = permission.ErrRequestExpired
		case permission.ApprovalCanceled:
			kind = permission.ErrRequestCanceled
		}
		return permission.Snapshot{}, false, permissionFailf(kind, "resolve", command.RequestKey, "terminal state")
	}
	if current.Request.Revision != command.ExpectedRevision {
		return permission.Snapshot{}, false, permissionFailf(permission.ErrRevisionConflict, "resolve", command.RequestKey, "")
	}
	resolution := permission.Resolution{
		DecisionKey: command.DecisionKey, RequestKey: command.RequestKey, CommandKey: command.CommandKey,
		ApproverKey: command.ApproverKey, Kind: command.Kind, ReasonCode: command.ReasonCode,
		Comment: command.Comment, DecidedAt: now,
	}
	current.Resolution, current.Request.ResolvedAt = &resolution, now
	current.Request.State = permission.ApprovalDenied
	if command.Kind == permission.ResolutionApprove {
		current.Request.State = permission.ApprovalApproved
	}
	current.Request.Revision++
	if command.Grant != nil {
		grant, grantErr := s.newGrant(ctx, tx, command, current.Request.Check, now)
		if grantErr != nil {
			return permission.Snapshot{}, false, grantErr
		}
		current.Grant = &grant
	}
	if err = saveRequest(ctx, tx, current); err != nil {
		return permission.Snapshot{}, false, err
	}
	return current, true, nil
}

func (s *SQLitePermissionStore) newGrant(ctx context.Context, tx *sql.Tx, command permission.ResolveCommand, check permission.CheckRequest, now time.Time) (permission.Grant, error) {
	if command.Kind != permission.ResolutionApprove {
		return permission.Grant{}, permissionFailf(permission.ErrInvalidRequest, "resolve", command.RequestKey, "deny cannot create grant")
	}
	grant := permission.Grant{
		TenantKey: command.TenantKey, GrantKey: command.GrantKey, Spec: *command.Grant,
		State: permission.GrantActive, Revision: 1, CreatedByDecisionKey: command.DecisionKey, CreatedAt: now,
	}
	if grant.GrantKey == "" || !grantMatchesCheck(grant.Spec, check) {
		return permission.Grant{}, permissionFailf(permission.ErrGrantNotApplicable, "resolve", command.RequestKey, "invalid grant binding")
	}
	if grant.Spec.ExpiresAt.IsZero() || !grant.Spec.ExpiresAt.After(now) {
		return permission.Grant{}, permissionFailf(permission.ErrGrantExpired, "resolve", command.RequestKey, "grant expiry is not in the future")
	}
	var existing []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM permission_grants WHERE tenant_key = ? AND grant_key = ?`, string(grant.TenantKey), string(grant.GrantKey)).Scan(&existing)
	if err == nil {
		return permission.Grant{}, permissionFailf(permission.ErrRequestConflict, "resolve", command.RequestKey, "grant key already exists")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return permission.Grant{}, err
	}
	if grant.Spec.MaxUses != nil {
		remaining := *grant.Spec.MaxUses
		grant.RemainingUses = &remaining
		if remaining == 0 {
			grant.State = permission.GrantExhausted
		}
	}
	if err := saveGrant(ctx, tx, grant); err != nil {
		return permission.Grant{}, err
	}
	return grant, nil
}

// Cancel releases a pending approval nobody answered. The attempt fence is
// compared so a stale worker cannot cancel a decision a live one is waiting on.
func (s *SQLitePermissionStore) Cancel(ctx context.Context, command permission.CancelCommand) (snapshot permission.Snapshot, applied bool, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return permission.Snapshot{}, false, err
	}
	defer finishTx(tx, &err)
	current, err := loadRequest(ctx, tx, command.TenantKey, command.RequestKey)
	if err != nil {
		return permission.Snapshot{}, false, err
	}
	now := s.clock().UTC()
	if err = s.expireRequest(ctx, tx, &current, now); err != nil {
		return permission.Snapshot{}, false, err
	}
	if current.Request.State == permission.ApprovalCanceled {
		return current, false, nil
	}
	if current.Request.State != permission.ApprovalPending {
		return permission.Snapshot{}, false, permissionFailf(permission.ErrAlreadyResolved, "cancel", command.RequestKey, "")
	}
	if current.Request.Revision != command.ExpectedRevision {
		return permission.Snapshot{}, false, permissionFailf(permission.ErrRevisionConflict, "cancel", command.RequestKey, "")
	}
	if current.Request.Check.AttemptRef != command.AttemptRef || current.Request.Check.FenceToken != command.FenceToken {
		return permission.Snapshot{}, false, permissionFailf(permission.ErrStaleFence, "cancel", command.RequestKey, "")
	}
	current.Request.State, current.Request.ResolvedAt = permission.ApprovalCanceled, now
	current.Request.Revision++
	if err = saveRequest(ctx, tx, current); err != nil {
		return permission.Snapshot{}, false, err
	}
	return current, true, nil
}

// GetRequest returns one approval, applying lazy expiry first.
func (s *SQLitePermissionStore) GetRequest(ctx context.Context, query permission.GetRequestQuery) (snapshot permission.Snapshot, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return permission.Snapshot{}, err
	}
	defer finishTx(tx, &err)
	current, err := loadRequest(ctx, tx, query.TenantKey, query.RequestKey)
	if err != nil {
		return permission.Snapshot{}, err
	}
	if err = s.expireRequest(ctx, tx, &current, s.clock().UTC()); err != nil {
		return permission.Snapshot{}, err
	}
	return current, nil
}

// ListPending returns the approvals a human still has to answer.
func (s *SQLitePermissionStore) ListPending(ctx context.Context, query permission.ListPendingQuery) (snapshots []permission.Snapshot, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finishTx(tx, &err)
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM permission_requests WHERE tenant_key = ? AND state = ? ORDER BY request_key`,
		string(query.TenantKey), string(permission.ApprovalPending))
	if err != nil {
		return nil, err
	}
	var candidates []permission.Snapshot
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var snapshot permission.Snapshot
		if err = json.Unmarshal(payload, &snapshot); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, snapshot)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	now := s.clock().UTC()
	snapshots = make([]permission.Snapshot, 0, len(candidates))
	for i := range candidates {
		if err = s.expireRequest(ctx, tx, &candidates[i], now); err != nil {
			return nil, err
		}
		if candidates[i].Request.State != permission.ApprovalPending {
			continue
		}
		if query.SessionRef != "" && candidates[i].Request.Check.SessionRef != query.SessionRef {
			continue
		}
		snapshots = append(snapshots, candidates[i])
	}
	return snapshots, nil
}

// FindGrants returns the active grants that already authorize this exact check.
// The SQL narrows by the identity columns; the applicability rule is then applied
// in full so scope and digest binding are never approximated.
func (s *SQLitePermissionStore) FindGrants(ctx context.Context, query permission.GrantQuery) (grants []permission.Grant, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer finishTx(tx, &err)
	check := query.Check
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM permission_grants WHERE tenant_key = ? AND principal_key = ? AND tool_name = ? AND action = ? AND resource_kind = ? AND resource_key = ? ORDER BY grant_key`,
		string(query.TenantKey), check.Subject.PrincipalKey, check.ToolName, check.Action, check.Resource.Kind, check.Resource.Key)
	if err != nil {
		return nil, err
	}
	var candidates []permission.Grant
	for rows.Next() {
		var payload []byte
		if err = rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var grant permission.Grant
		if err = json.Unmarshal(payload, &grant); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, grant)
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	now := s.clock().UTC()
	grants = []permission.Grant{}
	for i := range candidates {
		if err = s.expireGrant(ctx, tx, &candidates[i], now); err != nil {
			return nil, err
		}
		if candidates[i].State == permission.GrantActive && candidates[i].TenantKey == check.Subject.TenantKey && grantMatchesCheck(candidates[i].Spec, check) {
			grants = append(grants, candidates[i])
		}
	}
	return grants, nil
}

// ConsumeGrant spends one use of a limited grant.
func (s *SQLitePermissionStore) ConsumeGrant(ctx context.Context, command permission.ConsumeGrantCommand) (grant permission.Grant, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return permission.Grant{}, err
	}
	defer finishTx(tx, &err)
	current, err := loadGrant(ctx, tx, command.TenantKey, command.GrantKey)
	if err != nil {
		return permission.Grant{}, err
	}
	if err = s.expireGrant(ctx, tx, &current, s.clock().UTC()); err != nil {
		return permission.Grant{}, err
	}
	if current.Revision != command.ExpectedRevision {
		return permission.Grant{}, permission.ErrRevisionConflict
	}
	switch current.State {
	case permission.GrantExpired:
		return permission.Grant{}, permission.ErrGrantExpired
	case permission.GrantRevoked:
		return permission.Grant{}, permission.ErrGrantRevoked
	case permission.GrantExhausted:
		return permission.Grant{}, permission.ErrGrantExhausted
	}
	if !grantMatchesCheck(current.Spec, command.Check) {
		return permission.Grant{}, permission.ErrGrantNotApplicable
	}
	current.Used++
	current.Revision++
	if current.RemainingUses != nil {
		if *current.RemainingUses == 0 {
			return permission.Grant{}, permission.ErrGrantExhausted
		}
		remaining := *current.RemainingUses - 1
		current.RemainingUses = &remaining
		if remaining == 0 {
			current.State = permission.GrantExhausted
		}
	}
	if err = saveGrant(ctx, tx, current); err != nil {
		return permission.Grant{}, err
	}
	return current, nil
}

// RevokeGrant withdraws an approval before it expires.
func (s *SQLitePermissionStore) RevokeGrant(ctx context.Context, command permission.RevokeGrantCommand) (grant permission.Grant, err error) {
	tx, err := s.begin(ctx)
	if err != nil {
		return permission.Grant{}, err
	}
	defer finishTx(tx, &err)
	current, err := loadGrant(ctx, tx, command.TenantKey, command.GrantKey)
	if err != nil {
		return permission.Grant{}, err
	}
	if current.Revision != command.ExpectedRevision {
		return permission.Grant{}, permission.ErrRevisionConflict
	}
	if current.State == permission.GrantRevoked {
		return current, nil
	}
	if current.State != permission.GrantActive {
		return permission.Grant{}, permission.ErrGrantNotApplicable
	}
	current.State, current.RevokedAt = permission.GrantRevoked, s.clock().UTC()
	current.Revision++
	if err = saveGrant(ctx, tx, current); err != nil {
		return permission.Grant{}, err
	}
	return current, nil
}

// ExpireDue sweeps timed-out approvals and grants. Lazy expiry already protects
// every read; this exists so an operator can see what lapsed.
func (s *SQLitePermissionStore) ExpireDue(ctx context.Context, command permission.ExpireCommand) (result permission.ExpireResult, err error) {
	if command.Limit < 0 {
		return permission.ExpireResult{}, permission.ErrInvalidRequest
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return permission.ExpireResult{}, err
	}
	defer finishTx(tx, &err)
	now := s.clock().UTC()
	requests, err := dueRequests(ctx, tx, command.TenantKey, now)
	if err != nil {
		return permission.ExpireResult{}, err
	}
	for i := range requests {
		if command.Limit > 0 && len(result.Approvals) >= command.Limit {
			break
		}
		if err = s.expireRequest(ctx, tx, &requests[i], now); err != nil {
			return permission.ExpireResult{}, err
		}
		result.Approvals = append(result.Approvals, requests[i])
	}
	grants, err := dueGrants(ctx, tx, command.TenantKey, now)
	if err != nil {
		return permission.ExpireResult{}, err
	}
	for i := range grants {
		if command.Limit > 0 && len(result.Approvals)+len(result.Grants) >= command.Limit {
			break
		}
		if err = s.expireGrant(ctx, tx, &grants[i], now); err != nil {
			return permission.ExpireResult{}, err
		}
		result.Grants = append(result.Grants, grants[i])
	}
	return result, nil
}

func dueRequests(ctx context.Context, tx *sql.Tx, tenant permission.TenantKey, now time.Time) ([]permission.Snapshot, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM permission_requests WHERE tenant_key = ? AND state = ? AND expires_at > 0 AND expires_at <= ? ORDER BY request_key`,
		string(tenant), string(permission.ApprovalPending), nanos(now))
	if err != nil {
		return nil, err
	}
	var out []permission.Snapshot
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var snapshot permission.Snapshot
		if err := json.Unmarshal(payload, &snapshot); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func dueGrants(ctx context.Context, tx *sql.Tx, tenant permission.TenantKey, now time.Time) ([]permission.Grant, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM permission_grants WHERE tenant_key = ? AND state = ? AND expires_at > 0 AND expires_at <= ? ORDER BY grant_key`,
		string(tenant), string(permission.GrantActive), nanos(now))
	if err != nil {
		return nil, err
	}
	var out []permission.Grant
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var grant permission.Grant
		if err := json.Unmarshal(payload, &grant); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, grant)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func loadGrant(ctx context.Context, tx *sql.Tx, tenant permission.TenantKey, key permission.GrantKey) (permission.Grant, error) {
	var payload []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM permission_grants WHERE tenant_key = ? AND grant_key = ?`, string(tenant), string(key)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return permission.Grant{}, permission.ErrGrantNotFound
	}
	if err != nil {
		return permission.Grant{}, err
	}
	var grant permission.Grant
	if err := json.Unmarshal(payload, &grant); err != nil {
		return permission.Grant{}, err
	}
	return grant, nil
}

// grantMatchesCheck mirrors the permission package's applicability rule. It is
// duplicated rather than exported because every Store implementation has to
// enforce it independently; approximating it would widen authorization.
func grantMatchesCheck(spec permission.GrantSpec, check permission.CheckRequest) bool {
	validScope := spec.Scope == permission.ScopeInvocation || spec.Scope == permission.ScopeSession || spec.Scope == permission.ScopePrincipal
	return validScope &&
		spec.PrincipalKey == check.Subject.PrincipalKey && spec.ToolName == check.ToolName && spec.Action == check.Action &&
		spec.ResourceKind == check.Resource.Kind && spec.ResourceKey == check.Resource.Key &&
		spec.PolicyVersion == check.PolicyVersion && spec.ToolGeneration == check.ToolGeneration &&
		(spec.Scope != permission.ScopeInvocation || spec.InputDigest == check.InputDigest) &&
		(spec.Scope != permission.ScopeSession || spec.SessionRef == check.SessionRef)
}

// sameImmutableApproval compares everything a replayed create must not change.
func sameImmutableApproval(left, right permission.Snapshot) bool {
	a, b := left.Request, right.Request
	a.State, b.State = "", ""
	a.Revision, b.Revision = 0, 0
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	a.ResolvedAt, b.ResolvedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func sameApprovalResolution(resolution permission.Resolution, command permission.ResolveCommand) bool {
	return resolution.DecisionKey == command.DecisionKey && resolution.CommandKey == command.CommandKey &&
		resolution.ApproverKey == command.ApproverKey && resolution.Kind == command.Kind &&
		resolution.ReasonCode == command.ReasonCode && resolution.Comment == command.Comment
}
