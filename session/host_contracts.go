package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type MergeStrategy string

const (
	MergeFastForward MergeStrategy = "fast_forward"
	MergeNone        MergeStrategy = "none"
)

type AdmissionState string

const (
	AdmissionQueued   AdmissionState = "queued"
	AdmissionClaimed  AdmissionState = "claimed"
	AdmissionReleased AdmissionState = "released"
)

type ExecutionState string

const (
	ExecutionQueued       ExecutionState = "queued"
	ExecutionRunning      ExecutionState = "running"
	ExecutionSuspended    ExecutionState = "suspended"
	ExecutionMergePending ExecutionState = "merge_pending"
	ExecutionReleased     ExecutionState = "released"
	ExecutionFailed       ExecutionState = "failed"
)

type CancelMode string

const (
	CancelAttempt CancelMode = "attempt"
	CancelSuspend CancelMode = "suspend"
	CancelAbandon CancelMode = "abandon"
)

var (
	ErrAdmissionClosed       = errors.New("agent session: admission closed")
	ErrQueueFull             = errors.New("agent session: queue full")
	ErrRunNotFound           = errors.New("agent session: run not found")
	ErrRunConflict           = errors.New("agent session: run conflict")
	ErrInvalidTransition     = errors.New("agent session: invalid transition")
	ErrStaleClaim            = errors.New("agent session: stale claim")
	ErrGenerationUnavailable = errors.New("agent session: generation unavailable")
	ErrDrainDeadline         = errors.New("agent session: drain deadline exceeded")
	ErrCanceled              = errors.New("agent session: canceled")
	ErrResumeUnsupported     = errors.New("agent session: suspended snapshot cannot be resumed")
)

type TerminalStateError struct {
	RunKey RunKey
	State  ExecutionState
	Reason string
}

func (e *TerminalStateError) Error() string {
	return fmt.Sprintf("%v: run %q in %s: %s", ErrInvalidTransition, e.RunKey, e.State, e.Reason)
}

func (e *TerminalStateError) Unwrap() error { return ErrInvalidTransition }

type SelectorValue struct {
	Key   string
	Value string
}

type RunRequest struct {
	TenantKey    agent.TenantKey
	SessionKey   SessionKey
	RequestID    string
	AgentKey     string
	Selectors    []SelectorValue
	Messages     []agent.Message
	BaseRevision *uint64
	Merge        MergeStrategy
	Priority     int
	StepPolicy   StepPolicyArtifact
}

type RunReceipt struct {
	RunKey            RunKey
	BranchKey         BranchKey
	SessionKey        SessionKey
	BaseRevision      uint64
	AdmissionState    AdmissionState
	ExecutionState    ExecutionState
	DefinitionDigest  string
	ContextPlanKey    string
	ContextPlanDigest string
	StepPolicyDigest  string
	CreatedAt         time.Time
}

type Failure struct {
	Code      string
	Message   string
	Retryable bool
	Cause     error
}

func (f *Failure) Error() string {
	if f == nil {
		return ""
	}
	if f.Message != "" {
		return f.Message
	}
	return f.Code
}

func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

type RunResult struct {
	Receipt         RunReceipt
	SessionRevision uint64
	CoreResult      *agent.RunResult
	Failure         *Failure
}

type CancelRequest struct {
	TenantKey agent.TenantKey
	RunKey    RunKey
	Mode      CancelMode
	Reason    string
}

type CancelResult struct {
	RunKey       RunKey
	Requested    bool
	AlreadyFinal bool
	State        ExecutionState
}

type ResumeRequest struct {
	TenantKey agent.TenantKey
	RunKey    RunKey
}

type ResumeResult struct {
	RunKey       RunKey
	Resumed      bool
	AlreadyReady bool
	State        ExecutionState
}

type MergeRequest struct {
	TenantKey    agent.TenantKey
	RunKey       RunKey
	Strategy     MergeStrategy
	ExpectedBase uint64
}

type Limits struct {
	MaxActiveGlobal     int
	MaxActivePerTenant  int
	MaxActivePerSession int
	MaxQueuedPerTenant  int
	MaxQueuedPerSession int
}

type DrainResult struct {
	Completed int
	Suspended int
	Failed    int
	Remaining int
}

type Options struct {
	Store          Store
	Definitions    DefinitionResolver
	Attempts       AttemptRunner
	Events         EventSink
	Limits         Limits
	WorkerID       string
	LeaseDuration  time.Duration
	CleanupTimeout time.Duration
	Clock          func() time.Time
}

type DefinitionResolver interface {
	Resolve(context.Context, DefinitionRequest) (ResolvedExecution, error)
	ResolveGeneration(context.Context, agent.TenantKey, string) (ResolvedExecution, error)
}

type DefinitionRequest struct {
	TenantKey agent.TenantKey
	AgentKey  string
	Selectors []SelectorValue
}

type ArtifactRef struct {
	Kind          string
	Key           string
	Generation    string
	Digest        string
	SchemaVersion uint16
}

type ResolvedExecution struct {
	Definition       *agent.RuntimeDefinition
	DefinitionDigest string
	SchemaVersion    uint16
	Artifacts        []ArtifactRef
}

type AttemptRequest struct {
	TenantKey         agent.TenantKey
	RunKey            RunKey
	BranchKey         BranchKey
	ContextPlanKey    string
	ContextPlanDigest string
	Execution         ResolvedExecution
	Input             []agent.Message
	StepPolicy        StepPolicyArtifact
	StepPolicyDigest  string
}

type AttemptResult struct {
	Fence   uint64
	Outcome agent.Outcome
	Result  *agent.RunResult
	Failure *Failure
}

type AttemptRunner interface {
	Resume(context.Context, AttemptRequest) (AttemptResult, error)
	Interrupt(context.Context, RunKey, CancelMode) error
}

type FinalizationResumer interface {
	ResumeFinalization(context.Context, AttemptRequest, agent.RunResult) (agent.RunResult, error)
}

// EventSink is deliberately post-commit only. Transactional event writes belong
// to the Store composition implementation.
type EventSink interface {
	PublishAfterCommit(context.Context, RunKey) error
}

type Admission struct {
	TenantKey         agent.TenantKey
	SessionKey        SessionKey
	BranchKey         BranchKey
	RunKey            RunKey
	RequestID         string
	AgentKey          string
	BaseRevision      *uint64
	DefinitionDigest  string
	ContextPlanKey    string
	ContextPlanDigest string
	InputDigest       string
	ConfigDigest      string
	StepPolicy        StepPolicyArtifact
	StepPolicyDigest  string
	Messages          []agent.Message
	Merge             MergeStrategy
	Priority          int
	CreatedAt         time.Time
}

type Claim struct {
	TenantKey  agent.TenantKey
	RunKey     RunKey
	BranchKey  BranchKey
	Merge      MergeStrategy
	WorkerID   string
	ClaimToken uint64
	LeaseUntil time.Time
}

type AdmitAndBeginCommand struct {
	Admission Admission
	Execution ResolvedExecution
	Limits    Limits
}

type MergeAndFinalizeCommand struct {
	Request       MergeRequest
	Claim         Claim
	DurableFence  uint64
	DurableResult agent.RunResult
}

type FinalizeFailureCommand struct {
	Claim         Claim
	DurableFence  uint64
	DurableResult agent.RunResult
	Failure       Failure
}

type Store interface {
	AdmitAndBegin(context.Context, AdmitAndBeginCommand) (RunReceipt, bool, error)
	ClaimNext(context.Context, string, time.Time, Limits) (Claim, bool, error)
	LoadBranchInput(context.Context, RunKey) ([]agent.Message, error)
	LoadStepPolicy(context.Context, RunKey) (StepPolicyArtifact, string, error)
	MarkRunning(context.Context, Claim, uint64) error
	MarkSuspended(context.Context, Claim, Failure) error
	MarkMergePending(context.Context, Claim, agent.RunResult) error
	MarkFailed(context.Context, Claim, Failure) error
	Resume(context.Context, ResumeRequest, Limits) (ResumeResult, error)
	FinalizeFailure(context.Context, FinalizeFailureCommand) error
	MergeAndFinalize(context.Context, MergeAndFinalizeCommand) (MergeResult, error)
	RequestCancel(context.Context, CancelRequest) (CancelResult, error)
	Abandon(context.Context, RunKey, string) error
	Release(context.Context, Claim) error
	Get(context.Context, RunKey) (RunResult, error)
	Reconcile(context.Context, time.Time) error
}

func validateHostOptions(options Options) error {
	if options.Store == nil || options.Definitions == nil || options.Attempts == nil {
		return fmt.Errorf("%w: store, definitions, and attempts are required", ErrInvalidCommand)
	}
	if options.WorkerID == "" || options.LeaseDuration <= 0 {
		return fmt.Errorf("%w: worker ID and positive lease duration are required", ErrInvalidCommand)
	}
	if options.CleanupTimeout < 0 {
		return fmt.Errorf("%w: cleanup timeout must not be negative", ErrInvalidCommand)
	}
	return validateLimits(options.Limits)
}

func validateLimits(limits Limits) error {
	if limits.MaxActiveGlobal <= 0 || limits.MaxActivePerTenant <= 0 || limits.MaxActivePerSession <= 0 || limits.MaxQueuedPerTenant <= 0 || limits.MaxQueuedPerSession <= 0 {
		return fmt.Errorf("%w: all admission limits must be positive", ErrInvalidCommand)
	}
	return nil
}
