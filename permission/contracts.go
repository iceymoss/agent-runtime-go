// Package permission provides portable effect authorization and durable
// approval contracts. References to runs, attempts, and executions are opaque;
// their owning packages are integrated by Store implementations.
package permission

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type TenantKey = agent.TenantKey

type PolicyVersion string
type InputDigest string
type RequestKey string
type DecisionKey string
type GrantKey string
type ResumeToken string
type RunRef string
type AttemptRef string
type ExecutionRef string

type CheckDecision string

const (
	DecisionAllow CheckDecision = "allow"
	DecisionDeny  CheckDecision = "deny"
	DecisionAsk   CheckDecision = "ask"
)

type ApprovalState string

const (
	ApprovalPending  ApprovalState = "pending"
	ApprovalApproved ApprovalState = "approved"
	ApprovalDenied   ApprovalState = "denied"
	ApprovalExpired  ApprovalState = "expired"
	ApprovalCanceled ApprovalState = "canceled"
)

type ResolutionKind string

const (
	ResolutionApprove ResolutionKind = "approve"
	ResolutionDeny    ResolutionKind = "deny"
)

type Scope string

const (
	ScopeInvocation Scope = "invocation"
	ScopeSession    Scope = "session"
	ScopePrincipal  Scope = "principal"
)

type Subject struct {
	TenantKey    TenantKey
	PrincipalKey string
	ActorType    string
}

type Resource struct {
	Kind      string
	Key       string
	ParentKey string
}

// CheckRequest identifies the exact post-rewrite effect being authorized.
// RequestKey must be stable across retries of the same permission boundary.
type CheckRequest struct {
	RequestKey        RequestKey
	Subject           Subject
	Resource          Resource
	SessionRef        string
	RunRef            RunRef
	AttemptRef        AttemptRef
	ExecutionRef      ExecutionRef
	FenceToken        uint64
	StepNumber        uint32
	Ordinal           uint32
	ToolCallID        string
	ToolName          string
	Action            string
	InputDigest       InputDigest
	ToolGeneration    string
	DefinitionDigest  string
	PolicyVersion     PolicyVersion
	ApprovalExpiresAt time.Time
}

type GrantConstraint struct {
	Scope     Scope
	ExpiresAt time.Time
	MaxUses   *uint64
}

type CheckResult struct {
	Decision      CheckDecision
	PolicyVersion PolicyVersion
	InputDigest   InputDigest
	RuleKey       string
	ReasonCode    string
	Constraint    GrantConstraint
	Approval      *Snapshot
	Blocker       *SuspensionBlocker
}

type Policy interface {
	Version() PolicyVersion
	Evaluate(context.Context, CheckRequest, []Grant) (CheckResult, error)
}

type PolicyFunc struct {
	PolicyVersion PolicyVersion
	EvaluateFunc  func(context.Context, CheckRequest, []Grant) (CheckResult, error)
}

func (p PolicyFunc) Version() PolicyVersion { return p.PolicyVersion }
func (p PolicyFunc) Evaluate(ctx context.Context, request CheckRequest, grants []Grant) (CheckResult, error) {
	return p.EvaluateFunc(ctx, request, grants)
}

type ApprovalRequest struct {
	RequestKey           RequestKey
	Check                CheckRequest
	Description          string
	RedactedInput        string
	SupersedesRequestKey RequestKey
	State                ApprovalState
	Revision             uint64
	ExpiresAt            time.Time
	CreatedAt            time.Time
	ResolvedAt           time.Time
}

type Resolution struct {
	DecisionKey DecisionKey
	RequestKey  RequestKey
	CommandKey  string
	ApproverKey string
	Kind        ResolutionKind
	ReasonCode  string
	Comment     string
	DecidedAt   time.Time
}

// Snapshot is the detached authoritative view of an approval.
type Snapshot struct {
	Request     ApprovalRequest
	Resolution  *Resolution
	Grant       *Grant
	ResumeToken ResumeToken
}

// SuspensionBlocker is safe to persist in a durable snapshot projection. Its
// token is random opaque correlation data and contains no request input.
type SuspensionBlocker struct {
	RequestRef  RequestKey
	ResumeToken ResumeToken
	Revision    uint64
}

type GrantSpec struct {
	Scope          Scope
	PrincipalKey   string
	SessionRef     string
	ToolName       string
	Action         string
	ResourceKind   string
	ResourceKey    string
	InputDigest    InputDigest
	PolicyVersion  PolicyVersion
	ToolGeneration string
	ExpiresAt      time.Time
	// nil means unlimited. A non-nil zero is a valid, immediately exhausted limit.
	MaxUses *uint64
}

type GrantState string

const (
	GrantActive    GrantState = "active"
	GrantExhausted GrantState = "exhausted"
	GrantExpired   GrantState = "expired"
	GrantRevoked   GrantState = "revoked"
)

type Grant struct {
	TenantKey TenantKey
	GrantKey  GrantKey
	Spec      GrantSpec
	State     GrantState
	Revision  uint64
	Used      uint64
	// RemainingUses is nil for unlimited grants. A non-nil zero is preserved.
	RemainingUses        *uint64
	CreatedByDecisionKey DecisionKey
	CreatedAt            time.Time
	RevokedAt            time.Time
}

type CreateApprovalCommand struct {
	Request     ApprovalRequest
	ResumeToken ResumeToken
}

type ResolveCommand struct {
	TenantKey        TenantKey
	RequestKey       RequestKey
	CommandKey       string
	DecisionKey      DecisionKey
	GrantKey         GrantKey
	ApproverKey      string
	ExpectedRevision uint64
	Kind             ResolutionKind
	ReasonCode       string
	Comment          string
	Grant            *GrantSpec
}

type CancelCommand struct {
	TenantKey        TenantKey
	RequestKey       RequestKey
	ExpectedRevision uint64
	AttemptRef       AttemptRef
	FenceToken       uint64
}

type GetRequestQuery struct {
	TenantKey  TenantKey
	RequestKey RequestKey
}
type ListPendingQuery struct {
	TenantKey  TenantKey
	SessionRef string
}
type GrantQuery struct {
	TenantKey TenantKey
	Check     CheckRequest
}
type ConsumeGrantCommand struct {
	TenantKey        TenantKey
	GrantKey         GrantKey
	Check            CheckRequest
	ExpectedRevision uint64
}
type RevokeGrantCommand struct {
	TenantKey            TenantKey
	GrantKey             GrantKey
	ExpectedRevision     uint64
	ActorKey, ReasonCode string
}
type ExpireCommand struct {
	TenantKey TenantKey
	Limit     int
}
type ExpireResult struct {
	Approvals []Snapshot
	Grants    []Grant
}

type RevalidateCommand struct {
	TenantKey      TenantKey
	RequestKey     RequestKey
	ResumeToken    ResumeToken
	AttemptRef     AttemptRef
	FenceToken     uint64
	InputDigest    InputDigest
	PolicyVersion  PolicyVersion
	ToolGeneration string
}

type Clock interface{ Now() time.Time }

// FenceValidator lets the durable owner reject stale attempts without an
// import edge from permission to the durable package.
type FenceValidator interface {
	ValidateFence(context.Context, TenantKey, RunRef, AttemptRef, uint64) error
}

type Store interface {
	CreateAndSuspend(context.Context, CreateApprovalCommand) (Snapshot, bool, error)
	Resolve(context.Context, ResolveCommand) (Snapshot, bool, error)
	Cancel(context.Context, CancelCommand) (Snapshot, bool, error)
	GetRequest(context.Context, GetRequestQuery) (Snapshot, error)
	ListPending(context.Context, ListPendingQuery) ([]Snapshot, error)
	FindGrants(context.Context, GrantQuery) ([]Grant, error)
	ConsumeGrant(context.Context, ConsumeGrantCommand) (Grant, error)
	RevokeGrant(context.Context, RevokeGrantCommand) (Grant, error)
	ExpireDue(context.Context, ExpireCommand) (ExpireResult, error)
}

type Service interface {
	Check(context.Context, CheckRequest) (CheckResult, error)
	Resolve(context.Context, ResolveCommand) (Snapshot, bool, error)
	Cancel(context.Context, CancelCommand) (Snapshot, bool, error)
	Revalidate(context.Context, RevalidateCommand) (CheckResult, error)
	GetRequest(context.Context, GetRequestQuery) (Snapshot, error)
	ListPending(context.Context, ListPendingQuery) ([]Snapshot, error)
	ConsumeGrant(context.Context, ConsumeGrantCommand) (Grant, error)
	RevokeGrant(context.Context, RevokeGrantCommand) (Grant, error)
	ExpireDue(context.Context, ExpireCommand) (ExpireResult, error)
}
