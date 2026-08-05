// Package event defines portable Agent event, outbox, delivery, and replay
// contracts. Database transactions and transport adapters deliberately live
// outside this package.
package event

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// Reliability identifies the guarantees attached to an event.
type Reliability string

const (
	ReliabilityObservation     Reliability = "observation"
	ReliabilitySessionSnapshot Reliability = "session_snapshot"
	ReliabilityTerminal        Reliability = "terminal"
	ReliabilityDomain          Reliability = "domain"
)

// Envelope is the canonical immutable event representation.
type Envelope struct {
	TenantKey         agent.TenantKey
	EventID           string
	StreamKey         string
	Sequence          uint64
	Type              string
	SchemaVersion     uint16
	Reliability       Reliability
	AggregateType     string
	AggregateKey      string
	AggregateRevision uint64
	CorrelationID     string
	CausationID       string
	OccurredAt        time.Time
	PersistedAt       time.Time
	Payload           []byte
}

// Clone returns a detached copy of the envelope.
func (e Envelope) Clone() Envelope {
	e.Payload = cloneBytes(e.Payload)
	return e
}

// AppendCommand appends one stable event. Sequence and PersistedAt are
// allocated by the store and must be zero on input.
type AppendCommand struct {
	Envelope Envelope
}

// AppendBatchCommand appends all events atomically and in command order.
type AppendBatchCommand struct {
	Events []AppendCommand
}

type OutboxState string

const (
	OutboxPending   OutboxState = "pending"
	OutboxLeased    OutboxState = "leased"
	OutboxDelivered OutboxState = "delivered"
	OutboxDead      OutboxState = "dead"
)

// OutboxRecord is a detached view of delivery state.
type OutboxRecord struct {
	Envelope     Envelope
	State        OutboxState
	Attempts     uint32
	NextAttempt  time.Time
	LeaseOwner   string
	LeaseToken   string
	LeaseFence   uint64
	LeaseExpires time.Time
	DeliveredAt  time.Time
	DeadAt       time.Time
	LastError    string
}

type ClaimCommand struct {
	TenantKey     agent.TenantKey
	Owner         string
	Limit         int
	LeaseDuration time.Duration
}

type ClaimedEvent struct {
	Envelope   Envelope
	Attempts   uint32
	LeaseOwner string
	LeaseToken string
	LeaseFence uint64
	ExpiresAt  time.Time
}

type AckCommand struct {
	TenantKey  agent.TenantKey
	EventID    string
	LeaseOwner string
	LeaseToken string
	LeaseFence uint64
}

// NackCommand either schedules another attempt or moves the record to dead.
// MaxAttempts zero means no attempt limit.
type NackCommand struct {
	AckCommand
	RetryAfter  time.Duration
	MaxAttempts uint32
	Dead        bool
	Reason      string
}

type Cursor struct {
	TenantKey agent.TenantKey
	StreamKey string
	Sequence  uint64
	EventID   string
}

type GapReason string

const (
	GapRetentionExpired  GapReason = "retention_expired"
	GapSequenceInvariant GapReason = "sequence_invariant"
)

type ResetReason string

const (
	ResetCursorUnknown  ResetReason = "cursor_unknown"
	ResetCursorExpired  ResetReason = "cursor_expired"
	ResetReconciliation ResetReason = "server_reconciliation"
)

type Gap struct {
	ExpectedSequence uint64
	ReceivedSequence uint64
	Reason           GapReason
}

type Reset struct {
	Reason           ResetReason
	SnapshotRequired bool
	MinimumCursor    Cursor
}

type ReplayQuery struct {
	Cursor Cursor
	Limit  int
}

// ReplayResult never silently skips missing retained history. Reconcile is set
// when the caller must obtain an authoritative cumulative snapshot.
type ReplayResult struct {
	Events    []Envelope
	Next      Cursor
	Gap       *Gap
	Reset     *Reset
	Reconcile bool
}

// Store is the portable persisted-event contract. A production adapter can
// implement AppendBatch inside an owning aggregate transaction.
type Store interface {
	Append(context.Context, AppendCommand) (Envelope, error)
	AppendBatch(context.Context, AppendBatchCommand) ([]Envelope, error)
	Claim(context.Context, ClaimCommand) ([]ClaimedEvent, error)
	Ack(context.Context, AckCommand) error
	Nack(context.Context, NackCommand) error
	Replay(context.Context, ReplayQuery) (ReplayResult, error)
}

type Publisher interface {
	Publish(context.Context, Envelope) error
}

// Consumer handles at-least-once event deliveries. Inbox can wrap it to make
// repeated Event IDs harmless within one process.
type Consumer interface {
	Name() string
	Handle(context.Context, Envelope) error
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
