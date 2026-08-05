// Package subagent orchestrates independent durable child runs.
package subagent

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type RequestKey string
type RelationshipKey string
type SessionKey string
type RunKey string
type AttemptKey string
type TreeKey string
type UsageFactKey string
type ResultRef string
type FailureRef string
type WakeKey string
type FactKey string

type ParentRef struct {
	TenantKey       agent.TenantKey
	SessionKey      SessionKey
	RunKey          RunKey
	AttemptKey      AttemptKey
	Fence           uint64
	RelationshipKey RelationshipKey
	TreeKey         TreeKey
}

type ChildRef struct {
	TenantKey       agent.TenantKey
	RelationshipKey RelationshipKey
	SessionKey      SessionKey
	RunKey          RunKey
	Depth           uint16
}

type ChildState string

const (
	ChildQueued    ChildState = "queued"
	ChildRunning   ChildState = "running"
	ChildSuspended ChildState = "suspended"
	ChildCompleted ChildState = "completed"
	ChildFailed    ChildState = "failed"
	ChildCanceled  ChildState = "canceled"
)

func (s ChildState) Terminal() bool {
	return s == ChildCompleted || s == ChildFailed || s == ChildCanceled
}

type Limits struct {
	MaxDepth        uint16
	MaxFanout       uint16
	MaxInputTokens  int64
	MaxOutputTokens int64
	MaxCostMicros   int64
	MaxToolCalls    int64
	MaxRuntime      time.Duration
	Deadline        time.Time
}

type Reservation struct {
	InputTokens  int64
	OutputTokens int64
	CostMicros   int64
	ToolCalls    int64
	Runtime      time.Duration
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CostMicros   int64
	ToolCalls    int64
	Runtime      time.Duration
}

type BudgetSnapshot struct {
	Limits   Limits
	Reserved Reservation
	Settled  Usage
	Released Reservation
}

type ContextRef struct {
	Kind   string
	Key    string
	Digest string
}

type SpawnRequest struct {
	RequestKey  RequestKey
	Parent      ParentRef
	AgentKey    string
	Input       []byte
	ContextRefs []ContextRef
	Limits      Limits
	Reserve     Reservation
	Deadline    time.Time
}

type SpawnReceipt struct {
	RequestKey     RequestKey
	Child          ChildRef
	State          ChildState
	SpecDigest     string
	ParentBlocked  bool
	Reservation    Reservation
	EffectiveLimit Limits
	CreatedAt      time.Time
}

type Failure struct {
	Code      string
	Message   string
	Retryable bool
}

type Snapshot struct {
	Receipt       SpawnReceipt
	Parent        ParentRef
	AgentKey      string
	Input         []byte
	ContextRefs   []ContextRef
	State         ChildState
	ResultRef     ResultRef
	FailureRef    FailureRef
	Failure       *Failure
	CancelMode    CancelMode
	UsageFactKey  UsageFactKey
	Usage         Usage
	Version       uint64
	ClaimOwner    string
	ClaimUntil    time.Time
	TerminalAt    time.Time
	WakePending   bool
	WakeDelivered bool
}

type RunRequest struct {
	Child       ChildRef
	AgentKey    string
	Input       []byte
	ContextRefs []ContextRef
	Limits      Limits
	Deadline    time.Time
}

type RunResult struct {
	State        ChildState
	ResultRef    ResultRef
	FailureRef   FailureRef
	Failure      *Failure
	UsageFactKey UsageFactKey
	Usage        Usage
}

type Runner interface {
	Run(context.Context, RunRequest) (RunResult, error)
}

type WakeRequest struct {
	WakeKey WakeKey
	Parent  ParentRef
	Child   ChildRef
	State   ChildState
}

type ParentWaker interface {
	// Wake must be idempotent by WakeKey because committed wake intents can be
	// delivered concurrently or retried after a caller crash.
	Wake(context.Context, WakeRequest) error
}

type CancelMode string

const (
	CancelSuspend CancelMode = "suspend"
	CancelAbandon CancelMode = "abandon"
)

type CancelRequest struct {
	TenantKey    agent.TenantKey
	RequestKey   string
	RunKey       RunKey
	Mode         CancelMode
	Reason       string
	MaxTraversal int
}

type CancelResult struct {
	Affected int
	Terminal int
	Pending  int
}

type FactKind string

const (
	FactChildAccepted        FactKind = "child.accepted"
	FactChildRunning         FactKind = "child.running"
	FactChildTerminal        FactKind = "child.terminal"
	FactChildCancelRequested FactKind = "child.cancel_requested"
	FactChildConsumed        FactKind = "child.consumed"
	FactChildReconcileFailed FactKind = "child.reconcile_failed"
)

type CommittedFact struct {
	FactKey         FactKey
	TenantKey       agent.TenantKey
	Kind            FactKind
	RelationshipKey RelationshipKey
	ParentRunKey    RunKey
	ChildRunKey     RunKey
	Revision        uint64
	OccurredAt      time.Time
	Payload         []byte
}

type ClaimRequest struct {
	TenantKey agent.TenantKey
	Owner     string
	Now       time.Time
	Lease     time.Duration
}

type TerminalCommand struct {
	TenantKey       agent.TenantKey
	RelationshipKey RelationshipKey
	ExpectedVersion uint64
	Owner           string
	Result          RunResult
	CompletedAt     time.Time
}

type SuspendCommand struct {
	TenantKey       agent.TenantKey
	RelationshipKey RelationshipKey
	ExpectedVersion uint64
	Owner           string
	Failure         *Failure
	SuspendedAt     time.Time
}

type SettleCommand struct {
	TenantKey       agent.TenantKey
	RelationshipKey RelationshipKey
	UsageFactKey    UsageFactKey
	Usage           Usage
	OccurredAt      time.Time
}

type WakeClaim struct {
	Request WakeRequest
	Version uint64
}

type ReconcileRequest struct {
	TenantKey agent.TenantKey
	Now       time.Time
	Limit     int
}

type ReconcileReport struct {
	LeasesRecovered int
	WakesDelivered  int
	WakeFailures    int
}

// Store owns the atomic relationship, blocker, tree-budget, cancellation, and
// wake-intent transaction. Implementations must scope every operation by tenant.
type Store interface {
	Spawn(context.Context, SpawnRequest, time.Time) (SpawnReceipt, bool, error)
	Get(context.Context, agent.TenantKey, RelationshipKey) (Snapshot, error)
	ClaimNext(context.Context, ClaimRequest) (Snapshot, bool, error)
	CommitSuspended(context.Context, SuspendCommand) (Snapshot, error)
	CommitTerminal(context.Context, TerminalCommand) (Snapshot, error)
	SettleUsage(context.Context, SettleCommand) (BudgetSnapshot, bool, error)
	RequestCancel(context.Context, CancelRequest, time.Time) (CancelResult, error)
	ClaimWake(context.Context, agent.TenantKey) (WakeClaim, bool, error)
	CompleteWake(context.Context, agent.TenantKey, WakeKey, uint64, time.Time) error
	Recover(context.Context, ReconcileRequest) (int, error)
	Facts(context.Context, agent.TenantKey, uint64, int) ([]CommittedFact, error)
}

type Options struct {
	Store         Store
	Runner        Runner
	ParentWaker   ParentWaker
	WorkerID      string
	LeaseDuration time.Duration
	Clock         func() time.Time
}
