package durable

import (
	"context"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

const SnapshotSchemaVersion = 1

type RunKey string
type AttemptKey string
type ExecutionKey string
type UsageKey string
type TenantKey string

type Status string

const (
	StatusClaimed   Status = "claimed"
	StatusRunning   Status = "running"
	StatusSuspended Status = "suspended"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusAbandoned Status = "abandoned"
)

type Phase string

const (
	PhaseModelReady    Phase = "model_ready"
	PhaseModelInflight Phase = "model_inflight"
	PhaseToolsReady    Phase = "tools_ready"
	PhaseToolInflight  Phase = "tool_inflight"
	PhaseFinalizing    Phase = "finalizing"
	PhaseTerminal      Phase = "terminal"
)

type Identity struct {
	RunKey    string `json:"run_key"`
	AgentKey  string `json:"agent_key"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
}

type Checkpoint = agent.Checkpoint
type Failure = agent.RunFailure
type ImmutableInput = agent.ImmutableRunInput
type ImmutableConfig = agent.ImmutableRunConfig

// Snapshot is a complete point-in-time value. Store and ledger boundaries
// always return deep copies, so previously returned snapshots never change.
// Field order and tags are frozen by snapshot v1.
type Snapshot struct {
	SchemaVersion int        `json:"schema_version"`
	Identity      Identity   `json:"identity"`
	InputDigest   string     `json:"input_digest"`
	ConfigDigest  string     `json:"config_digest"`
	Status        Status     `json:"status"`
	Phase         Phase      `json:"phase"`
	Revision      uint64     `json:"revision"`
	FenceToken    uint64     `json:"fence_token"`
	LeaseOwner    string     `json:"lease_owner,omitempty"`
	LeaseUntil    time.Time  `json:"lease_until,omitempty"`
	Checkpoint    Checkpoint `json:"checkpoint"`
	Failure       *Failure   `json:"failure,omitempty"`
}

type Guard struct {
	RunKey     RunKey
	LeaseOwner string
	Revision   uint64
	FenceToken uint64
}

func (s Snapshot) Guard() Guard {
	return Guard{RunKey: RunKey(s.Identity.RunKey), LeaseOwner: s.LeaseOwner, Revision: s.Revision, FenceToken: s.FenceToken}
}

func (s Snapshot) Terminal() bool { return terminalStatus(s.Status) }

type BeginRequest struct {
	Identity     Identity
	InputDigest  string
	ConfigDigest string
	Checkpoint   Checkpoint
}

type AcquireRequest struct {
	RunKey         RunKey
	Owner          string
	Now            time.Time
	LeaseUntil     time.Time
	AllowSuspended bool
}

type SaveRequest struct {
	Guard      Guard
	Status     Status
	Phase      Phase
	Checkpoint Checkpoint
	Failure    *Failure
}

type ScanRequest struct {
	Cursor        RunKey
	Limit         int
	Statuses      []Status
	ExpiredBefore time.Time
}

type ScanPage struct {
	Snapshots []Snapshot
	Next      RunKey
}

type RevokeLeaseRequest struct {
	RunKey           RunKey
	ExpectedRevision uint64
	ExpectedFence    uint64
	Now              time.Time
	RequireExpired   bool
}

// Store is the durable run authority. Save compares owner, fence, and expected
// revision in the same atomic operation. RevokeLease advances the fence so an
// old worker cannot write even if it retained its previous guard. It must also
// atomically mark running effects at the revoked fence unknown before exposing
// the suspended snapshot; this is how reconciliation avoids unsafe replay.
type Store interface {
	Begin(context.Context, BeginRequest) (Snapshot, bool, error)
	Load(context.Context, RunKey) (Snapshot, error)
	Acquire(context.Context, AcquireRequest) (Snapshot, error)
	Renew(context.Context, Guard, time.Time, time.Time) (Snapshot, error)
	Release(context.Context, Guard, Phase) (Snapshot, error)
	Save(context.Context, SaveRequest) (Snapshot, error)
	RevokeLease(context.Context, RevokeLeaseRequest) (Snapshot, error)
	Scan(context.Context, ScanRequest) (ScanPage, error)
}
