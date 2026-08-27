// Package tool provides the production lifecycle for local agent tools. It
// owns orchestration only; durable storage and cross-owner transactions are
// supplied through narrow ports by application adapters.
package tool

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
)

type Phase string

const (
	PhasePrepare   Phase = "prepare"
	PhasePreflight Phase = "preflight"
	PhaseAuthorize Phase = "authorize"
	PhaseExecute   Phase = "execute"
	PhaseRecord    Phase = "record"
	PhaseComplete  Phase = "complete"
)

type SideEffectClass string

const (
	EffectNone     SideEffectClass = "none"
	EffectRead     SideEffectClass = "read"
	EffectWrite    SideEffectClass = "write"
	EffectExternal SideEffectClass = "external"
)

type IdempotencyClass string

const (
	IdempotencyNone         IdempotencyClass = "none"
	IdempotencyExecutionKey IdempotencyClass = "execution_key"
)

type ConcurrencyMode string

const (
	ConcurrencySequential ConcurrencyMode = "sequential"
	ConcurrencyParallel   ConcurrencyMode = "parallel"
	ConcurrencyExclusive  ConcurrencyMode = "exclusive"
)

type Metadata struct {
	Version       string
	SchemaVersion string
	Action        string
	EffectGroup   string
	EffectClass   SideEffectClass
	Idempotency   IdempotencyClass
	Concurrency   ConcurrencyMode
	ReplayPolicy  agent.ReplayPolicy
}

type InvocationIdentity struct {
	TenantKey    agent.TenantKey
	RunKey       string
	AttemptKey   string
	FenceToken   uint64
	StepNumber   uint32
	Ordinal      uint32
	CallID       string
	ToolName     string
	RawInput     string
	PrincipalKey string
	SessionRef   string
	Resource     permission.Resource
}

// PreparedExecution is the immutable post-preflight identity. CanonicalInput
// includes explicit JSON zero values; digests are computed from that value.
type PreparedExecution struct {
	TenantKey        agent.TenantKey
	RunKey           string
	AttemptKey       string
	FenceToken       uint64
	StepNumber       uint32
	Ordinal          uint32
	CallID           string
	ToolName         string
	RawInput         string
	CanonicalInput   string
	InputDigest      string
	ExecutionKey     string
	EffectDigest     string
	ToolGeneration   string
	DefinitionDigest string
	SchemaVersion    string
	ToolVersion      string
	Action           string
	EffectGroup      string
	EffectClass      SideEffectClass
	Idempotency      IdempotencyClass
	ReplayPolicy     agent.ReplayPolicy
	PrincipalKey     string
	SessionRef       string
	Resource         permission.Resource
}

type PreflightDecision string

const (
	PreflightContinue PreflightDecision = "continue"
	PreflightRewrite  PreflightDecision = "rewrite"
	PreflightReturn   PreflightDecision = "return"
)

type PreflightResult struct {
	Decision PreflightDecision
	Name     string
	Input    string
	Result   *agent.ToolResult
}

// Interceptor order is the order supplied to Freeze. Before runs forwards;
// After and OnError run in reverse for interceptors whose Before succeeded.
type Interceptor interface {
	Name() string
	Version() string
	Before(context.Context, InvocationIdentity) (PreflightResult, error)
	After(context.Context, PreparedExecution, agent.ToolResult) (agent.ToolResult, error)
	OnError(context.Context, PreparedExecution, error) error
}

type ExecutionStatus string

const (
	StatusPrepared ExecutionStatus = "prepared"
	StatusRunning  ExecutionStatus = "running"
	// StatusSuspended means the tool started work it cannot finish now and
	// handed back a durable handle. It is distinct from unknown: the outcome is
	// not ambiguous, it simply has not happened yet.
	StatusSuspended ExecutionStatus = "suspended"
	StatusSucceeded ExecutionStatus = "succeeded"
	StatusFailed    ExecutionStatus = "failed"
	StatusUnknown   ExecutionStatus = "unknown"
)

// Suspension is the durable handle a parked execution is resumed from. It
// mirrors agent.ToolSuspension without the runtime's step coordinates, which the
// executor fills in.
type Suspension struct {
	Kind        agent.ToolSuspensionKind
	RequestRef  string
	ResumeToken string
	Revision    uint64
}

// SuspendExecution parks a running execution under the caller's fence.
type SuspendExecution struct {
	ExecutionKey string
	FenceToken   uint64
	Suspension   Suspension
}

type Failure struct {
	Code      string
	Message   string
	Retryable bool
}

type ExecutionRecord struct {
	Prepared   PreparedExecution
	Status     ExecutionStatus
	FenceToken uint64
	Revision   uint64
	Result     *agent.ToolResult
	Failure    *Failure
	// Suspension is set while the execution is parked, so a later attempt can
	// resume the exact call rather than deriving a new one.
	Suspension *Suspension
}

type CompleteExecution struct {
	ExecutionKey string
	FenceToken   uint64
	Result       *agent.ToolResult
	Failure      *Failure
}

// ExecutionLedger is a lifecycle consumer port, not a second persistence
// owner. Implementations adapt these commands to the durable owner's records.
//
// Suspend and Resume exist because a tool that is waiting is not a tool that
// failed. Recording a parked execution as failed would lose the handle a later
// attempt needs; recording it as unknown would forbid the replay that is in fact
// safe and expected.
type ExecutionLedger interface {
	Prepare(context.Context, PreparedExecution) (ExecutionRecord, bool, error)
	Reject(context.Context, string, uint64, Failure) (ExecutionRecord, error)
	Begin(context.Context, string, uint64) (ExecutionRecord, error)
	Suspend(context.Context, SuspendExecution) (ExecutionRecord, error)
	Resume(context.Context, string, uint64) (ExecutionRecord, error)
	Complete(context.Context, CompleteExecution) (ExecutionRecord, error)
	MarkUnknown(context.Context, string, uint64, Failure) (ExecutionRecord, error)
	Load(context.Context, string) (ExecutionRecord, error)
}

type Observer interface {
	TryObserve(Observation) bool
}

type Observation struct {
	Phase        Phase
	ExecutionKey string
	ToolName     string
	StepNumber   uint32
	Ordinal      uint32
	At           time.Time
}

type ExecuteRequest struct {
	Invocation        InvocationIdentity
	PolicyVersion     permission.PolicyVersion
	ApprovalExpiresAt time.Time
}

type ApprovalResume struct {
	RequestRef  permission.RequestKey
	ResumeToken permission.ResumeToken
	Revision    uint64
}

type ResumeApprovalRequest struct {
	Execute      ExecuteRequest
	ExecutionKey string
	Approval     ApprovalResume
}

type ExecuteResult struct {
	Prepared PreparedExecution
	Status   ExecutionStatus
	Result   *agent.ToolResult
	// Blocker is the approval-specific view of a suspension, kept for callers
	// that only handle approvals.
	Blocker *permission.SuspensionBlocker
	// Suspension is set for every parked execution, whatever the reason. Prefer
	// it over Blocker: it is what a general resume path needs.
	Suspension *Suspension
}

// ResumeRequest continues one parked execution. The suspension must be the
// handle the executor previously returned, which is how a resume is bound to the
// exact call it belongs to rather than to a similar one.
type ResumeRequest struct {
	Execute      ExecuteRequest
	ExecutionKey string
	Suspension   Suspension
}

type ErrorDisposition string

const (
	DispositionFailed    ErrorDisposition = "failed"
	DispositionRetryable ErrorDisposition = "retryable"
	DispositionUnknown   ErrorDisposition = "unknown"
)

// ClassifiedError lets a tool explicitly state whether a returned error proves
// no effect, is retryable under a new policy decision, or is ambiguous.
type ClassifiedError interface {
	error
	ToolErrorDisposition() ErrorDisposition
}
