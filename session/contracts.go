// Package session owns the portable session aggregate and branch contracts.
package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/message"
)

type SessionKey string

func (k SessionKey) Valid() bool { return k != "" }

type BranchKey string

func (k BranchKey) Valid() bool { return k != "" }

type RunKey string

func (k RunKey) Valid() bool { return k != "" }

// UsageFactKey is an opaque reference to an immutable usage fact owned by the
// durable runtime. It is not billing authority inside this package.
type UsageFactKey string

func (k UsageFactKey) Valid() bool { return k != "" }

type Status string

const (
	StatusActive    Status = "active"
	StatusSuspended Status = "suspended"
	StatusCompleted Status = "completed"
	StatusFinished  Status = "finished"
	StatusAbandoned Status = "abandoned"
)

type BranchStatus string

const (
	BranchStatusOpen         BranchStatus = "open"
	BranchStatusReadyToMerge BranchStatus = "ready_to_merge"
	BranchStatusConflicted   BranchStatus = "conflicted"
	BranchStatusMerged       BranchStatus = "merged"
	BranchStatusAbandoned    BranchStatus = "abandoned"
)

type MergeKind string

const (
	MergeKindNone        MergeKind = ""
	MergeKindFastForward MergeKind = "fast_forward"
)

var (
	ErrInvalidCommand           = errors.New("agent/session: invalid command")
	ErrSessionNotFound          = errors.New("agent/session: session not found")
	ErrRevisionNotFound         = errors.New("agent/session: revision not found")
	ErrBranchNotFound           = errors.New("agent/session: branch not found")
	ErrIdempotencyConflict      = errors.New("agent/session: idempotency conflict")
	ErrRevisionConflict         = errors.New("agent/session: revision conflict")
	ErrBranchVersionConflict    = errors.New("agent/session: branch version conflict")
	ErrBranchClosed             = errors.New("agent/session: branch closed")
	ErrMergeConflict            = errors.New("agent/session: merge conflict")
	ErrInvalidSessionTransition = errors.New("agent/session: invalid session transition")
	ErrInvalidBranchTransition  = errors.New("agent/session: invalid branch transition")
	ErrSnapshotInvariant        = errors.New("agent/session: snapshot invariant violation")
	ErrSessionFinished          = errors.New("agent/session: session finished")
)

// MergeConflictError reports the current values that rejected a merge.
type MergeConflictError struct {
	RunKey          RunKey
	SessionKey      SessionKey
	BranchKey       BranchKey
	BaseRevision    uint64
	CurrentRevision uint64
	BranchVersion   uint64
	Reason          string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("%v: session %q branch %q: %s (base=%d current=%d branch_version=%d)", ErrMergeConflict, e.SessionKey, e.BranchKey, e.Reason, e.BaseRevision, e.CurrentRevision, e.BranchVersion)
}

func (e *MergeConflictError) Unwrap() error { return ErrMergeConflict }

type ContextPivotSnapshot struct {
	ArtifactKey            string
	ArtifactDigest         string
	CoveredThrough         uint64
	SourceDigest           string
	ProtectedFactSetDigest string
}

type Snapshot struct {
	TenantKey            agent.TenantKey
	SessionKey           SessionKey
	UserKey              string
	AgentKey             string
	Identity             string
	Status               Status
	Revision             uint64
	Title                string
	PromptTokens         int64
	CompletionTokens     int64
	CostMicros           int64
	SummaryMessageKey    message.MessageKey
	SummaryAtRevision    uint64
	ContextPivot         ContextPivotSnapshot
	RuntimeDefinitionKey string
	MetadataVersion      uint16
	Metadata             []byte
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type Branch struct {
	TenantKey      agent.TenantKey
	BranchKey      BranchKey
	SessionKey     SessionKey
	RunKey         RunKey
	BaseRevision   uint64
	HeadRevision   uint64
	Status         BranchStatus
	MergeKind      MergeKind
	MergedRevision uint64
	Version        uint64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type CreateCommand struct {
	TenantKey            agent.TenantKey
	SessionKey           SessionKey
	UserKey              string
	AgentKey             string
	Identity             string
	Title                string
	ContextPivot         ContextPivotSnapshot
	RuntimeDefinitionKey string
	MetadataVersion      uint16
	Metadata             []byte
	MutationMeta         message.MutationMeta
}

type GetQuery struct {
	TenantKey  agent.TenantKey
	SessionKey SessionKey
}

type RevisionQuery struct {
	TenantKey  agent.TenantKey
	SessionKey SessionKey
	Revision   uint64
}

type CreateBranchCommand struct {
	TenantKey    agent.TenantKey
	SessionKey   SessionKey
	RunKey       RunKey
	BaseRevision uint64
	MutationMeta message.MutationMeta
}

type GetBranchQuery struct {
	TenantKey  agent.TenantKey
	SessionKey SessionKey
	BranchKey  BranchKey
}

// BranchCommand performs a branch version CAS. HeadRevision is used when a
// branch becomes ready and must never precede its fixed base revision.
type BranchCommand struct {
	TenantKey       agent.TenantKey
	SessionKey      SessionKey
	BranchKey       BranchKey
	ExpectedVersion uint64
	HeadRevision    uint64
	MutationMeta    message.MutationMeta
}

type ConflictCommand struct {
	TenantKey       agent.TenantKey
	SessionKey      SessionKey
	BranchKey       BranchKey
	ExpectedVersion uint64
	MergeKind       MergeKind
	MutationMeta    message.MutationMeta
}

type MergeCommit struct {
	TenantKey       agent.TenantKey
	SessionKey      SessionKey
	BranchKey       BranchKey
	ExpectedVersion uint64
	MergeKind       MergeKind
	MutationMeta    message.MutationMeta
}

type MergeResult struct {
	RunKey           RunKey
	BranchKey        BranchKey
	PreviousRevision uint64
	SessionRevision  uint64
	BranchState      string
	ExecutionState   ExecutionState
	Session          Snapshot
	Branch           Branch
}

type UsageCommand struct {
	TenantKey        agent.TenantKey
	SessionKey       SessionKey
	ExpectedRevision uint64
	UsageFactKey     UsageFactKey
	PromptTokens     int64
	CompletionTokens int64
	CostMicros       int64
	MutationMeta     message.MutationMeta
}

type SummaryCommand struct {
	TenantKey                agent.TenantKey
	SessionKey               SessionKey
	ExpectedRevision         uint64
	SummaryMessageKey        message.MessageKey
	SummaryAtRevision        uint64
	SummaryVisibleAtRevision uint64
	MutationMeta             message.MutationMeta
}

type TransitionCommand struct {
	TenantKey        agent.TenantKey
	SessionKey       SessionKey
	ExpectedRevision uint64
	Status           Status
	ContextPivot     *ContextPivotSnapshot
	MutationMeta     message.MutationMeta
}

type Service interface {
	Create(context.Context, CreateCommand) (Snapshot, error)
	Get(context.Context, GetQuery) (Snapshot, error)
	GetRevision(context.Context, RevisionQuery) (Snapshot, error)
	CreateBranch(context.Context, CreateBranchCommand) (Branch, error)
	GetBranch(context.Context, GetBranchQuery) (Branch, error)
	MarkBranchReady(context.Context, BranchCommand) (Branch, error)
	MarkBranchConflict(context.Context, ConflictCommand) (Branch, error)
	AbandonBranch(context.Context, BranchCommand) (Branch, error)
	CommitMerge(context.Context, MergeCommit) (MergeResult, error)
	AddUsage(context.Context, UsageCommand) (Snapshot, error)
	SetSummary(context.Context, SummaryCommand) (Snapshot, error)
	Transition(context.Context, TransitionCommand) (Snapshot, error)
}
