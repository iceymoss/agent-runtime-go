// Package message owns the portable persisted message aggregate contract.
package message

import (
	"context"
	"errors"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// MessageKey is a stable, tenant-scoped message aggregate identity.
type MessageKey string

// Valid reports whether the key can identify a persisted message aggregate.
func (k MessageKey) Valid() bool {
	return k != ""
}

// State is the lifecycle state of a persisted cumulative message snapshot.
type State string

const (
	StateBuilding   State = "building"
	StateComplete   State = "complete"
	StateCanceled   State = "canceled"
	StateFailed     State = "failed"
	StateTombstoned State = "tombstoned"
	StateTerminal   State = "terminal"
)

var (
	ErrInvalidCommand           = errors.New("agent/message: invalid command")
	ErrMessageNotFound          = errors.New("agent/message: message not found")
	ErrIdempotencyConflict      = errors.New("agent/message: idempotency conflict")
	ErrRevisionConflict         = errors.New("agent/message: revision conflict")
	ErrStaleFence               = errors.New("agent/message: stale fence")
	ErrInvalidMessageTransition = errors.New("agent/message: invalid message transition")
	ErrOrdinalConflict          = errors.New("agent/message: ordinal conflict")
	ErrSnapshotInvariant        = errors.New("agent/message: snapshot invariant violation")
)

// MutationMeta records event-neutral provenance supplied with a mutation.
type MutationMeta struct {
	CorrelationID string
	CausationID   string
	OccurredAt    time.Time
}

// MutationFact describes a committed mutation without depending on an event package.
type MutationFact struct {
	FactType          string
	FactVersion       uint16
	TenantKey         agent.TenantKey
	AggregateType     string
	AggregateKey      string
	AggregateRevision uint64
	RunKey            string
	AttemptKey        string
	FenceToken        uint64
	CorrelationID     string
	CausationID       string
	OccurredAt        time.Time
	PayloadType       string
	PayloadVersion    uint16
	Payload           []byte
}

// Snapshot is the complete authoritative value at one message revision.
// SessionKey, BranchKey, and RunKey intentionally remain raw storage-boundary
// strings so this leaf package does not depend on their owning packages.
type Snapshot struct {
	TenantKey         agent.TenantKey
	MessageKey        MessageKey
	SessionKey        string
	BranchKey         string
	BranchOrdinal     uint64
	Role              agent.Role
	Parts             []agent.ContentPart
	State             State
	FinishReason      agent.FinishReason
	ModelKey          string
	ProviderKey       string
	AdapterState      []byte
	RunKey            string
	AttemptKey        string
	FenceToken        uint64
	StepIndex         uint32
	Revision          uint64
	VisibleAtRevision uint64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CreateCommand creates a stable aggregate. BranchOrdinal zero requests atomic
// deterministic allocation in the branch.
type CreateCommand struct {
	TenantKey         agent.TenantKey
	MessageKey        MessageKey
	SessionKey        string
	BranchKey         string
	BranchOrdinal     uint64
	Role              agent.Role
	Parts             []agent.ContentPart
	State             State
	FinishReason      agent.FinishReason
	ModelKey          string
	ProviderKey       string
	AdapterState      []byte
	RunKey            string
	AttemptKey        string
	FenceToken        uint64
	StepIndex         uint32
	VisibleAtRevision uint64
	MutationMeta      MutationMeta
}

// SaveCommand replaces all mutable cumulative snapshot fields using revision CAS.
type SaveCommand struct {
	TenantKey        agent.TenantKey
	MessageKey       MessageKey
	ExpectedRevision uint64
	AttemptKey       string
	FenceToken       uint64
	State            State
	FinishReason     agent.FinishReason
	Parts            []agent.ContentPart
	AdapterState     []byte
	MutationMeta     MutationMeta
}

type GetQuery struct {
	TenantKey  agent.TenantKey
	MessageKey MessageKey
}

type ListBranchQuery struct {
	TenantKey  agent.TenantKey
	SessionKey string
	BranchKey  string
}

type ListVisibleQuery struct {
	TenantKey  agent.TenantKey
	SessionKey string
	Revision   uint64
}

// TombstoneCommand redacts content through the same revision and fence guards.
type TombstoneCommand struct {
	TenantKey        agent.TenantKey
	MessageKey       MessageKey
	ExpectedRevision uint64
	AttemptKey       string
	FenceToken       uint64
	MutationMeta     MutationMeta
}

type Service interface {
	Create(context.Context, CreateCommand) (Snapshot, error)
	SaveSnapshot(context.Context, SaveCommand) (Snapshot, error)
	Get(context.Context, GetQuery) (Snapshot, error)
	ListBranch(context.Context, ListBranchQuery) ([]Snapshot, error)
	ListVisible(context.Context, ListVisibleQuery) ([]Snapshot, error)
	Tombstone(context.Context, TombstoneCommand) (Snapshot, error)
}
