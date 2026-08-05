package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

const (
	// RunSnapshotSchemaVersion is the only checkpoint representation understood
	// by this runtime. Readers reject every other version instead of guessing.
	RunSnapshotSchemaVersion = 1
	digestPrefix             = "sha256:"
)

var (
	// ErrUnsupportedRunSnapshotSchema means persisted state requires an explicit
	// migration or a runtime that understands its schema.
	ErrUnsupportedRunSnapshotSchema = errors.New("agent: unsupported run snapshot schema")
	// ErrInvalidRunTransition means a store mutation attempted an illegal
	// lifecycle transition.
	ErrInvalidRunTransition = errors.New("agent: invalid run transition")
	// ErrCheckpointConflict means revision, fence token, or lease ownership no
	// longer authorizes a mutation. Callers must reload rather than retry a stale write.
	ErrCheckpointConflict = errors.New("agent: checkpoint conflict")
	// ErrToolExecutionUnknown means an old worker may have executed a tool whose
	// result was never committed and the tool did not opt into safe replay.
	ErrToolExecutionUnknown = errors.New("agent: tool execution status unknown")
)

// RunStatus is the durable ownership and terminal lifecycle of one run.
type RunStatus string

const (
	RunStatusClaimed   RunStatus = "claimed"
	RunStatusRunning   RunStatus = "running"
	RunStatusSuspended RunStatus = "suspended"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
	RunStatusAbandoned RunStatus = "abandoned"
)

// RunPhase identifies the next durable boundary. Inflight phases do not prove
// that an external operation did or did not happen; they only record intent.
type RunPhase string

const (
	RunPhaseModelReady    RunPhase = "model_ready"
	RunPhaseModelInflight RunPhase = "model_inflight"
	RunPhaseToolsReady    RunPhase = "tools_ready"
	RunPhaseToolInflight  RunPhase = "tool_inflight"
	RunPhaseFinalizing    RunPhase = "finalizing"
	RunPhaseTerminal      RunPhase = "terminal"
)

// RunIdentity contains caller-assigned immutable identifiers. RunKey must be
// globally unique; SessionID plus RequestID should also uniquely identify a run.
type RunIdentity struct {
	RunKey    string `json:"run_key"`
	AgentKey  string `json:"agent_key"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
}

// ImmutableRunInput is the input envelope whose digest is fixed by Begin.
// Event handlers and other process-local behavior are deliberately excluded.
type ImmutableRunInput struct {
	Messages   []Message   `json:"messages"`
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
}

// ImmutableRunConfig contains execution choices that must not drift on resume.
// Version fields let callers represent prompt and policy implementations whose
// executable functions cannot themselves be serialized.
type ImmutableRunConfig struct {
	AgentKey            string            `json:"agent_key"`
	ModelName           string            `json:"model_name"`
	MaxSteps            int               `json:"max_steps"`
	ContextWindow       int               `json:"context_window,omitempty"`
	LoopDetectWindow    int               `json:"loop_detect_window,omitempty"`
	LoopDetectThreshold int               `json:"loop_detect_threshold,omitempty"`
	ToolRepairLimit     int               `json:"tool_repair_limit,omitempty"`
	Generation          GenerationOptions `json:"generation"`
	ToolChoice          *ToolChoice       `json:"tool_choice,omitempty"`
	Tools               []ToolDefinition  `json:"tools,omitempty"`
	PromptVersion       string            `json:"prompt_version,omitempty"`
	PolicyVersion       string            `json:"policy_version,omitempty"`
}

// Checkpoint is the complete resumable runtime state. History is the immutable
// input plus all committed messages; NewMessages is the run-local output subset.
// NextStep is the model step to execute after pending work has been resolved.
type Checkpoint struct {
	History          []Message    `json:"history"`
	NewMessages      []Message    `json:"new_messages"`
	CompletedSteps   []StepResult `json:"completed_steps"`
	Usage            Usage        `json:"usage"`
	RepairCount      int          `json:"repair_count"`
	NextStep         int          `json:"next_step"`
	PendingToolCalls []ToolCall   `json:"pending_tool_calls"`
	Outcome          RunResult    `json:"outcome"`
}

// RunSnapshot is the persistence-neutral serialized form of a durable run.
// Revision is incremented by every successful mutation. FenceToken is
// incremented by every successful Acquire and never decreases. LeaseUntil is
// advisory for acquisition; authorization of writes always requires the exact
// LeaseOwner, FenceToken, and Revision tuple.
type RunSnapshot struct {
	SchemaVersion int         `json:"schema_version"`
	Identity      RunIdentity `json:"identity"`
	InputDigest   string      `json:"input_digest"`
	ConfigDigest  string      `json:"config_digest"`
	Status        RunStatus   `json:"status"`
	Phase         RunPhase    `json:"phase"`
	Revision      uint64      `json:"revision"`
	FenceToken    uint64      `json:"fence_token"`
	LeaseOwner    string      `json:"lease_owner,omitempty"`
	LeaseUntil    time.Time   `json:"lease_until,omitempty"`
	Checkpoint    Checkpoint  `json:"checkpoint"`
	Failure       *RunFailure `json:"failure,omitempty"`
}

// RunFailure is a serializable terminal failure. Error values are process-local
// and therefore do not belong in snapshots.
type RunFailure struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// ToolExecutionStatus records the durable side-effect ledger lifecycle.
type ToolExecutionStatus string

const (
	ToolExecutionPrepared  ToolExecutionStatus = "prepared"
	ToolExecutionExecuting ToolExecutionStatus = "executing"
	ToolExecutionCompleted ToolExecutionStatus = "completed"
	// Unknown means execution may have produced an effect but no result was
	// durably committed. It must not be automatically replayed unless the tool's
	// system deduplicates or resolves the same idempotency key.
	ToolExecutionUnknown ToolExecutionStatus = "unknown"
)

// ToolExecution is one durable tool-call ledger entry. IdempotencyKey and
// InputHash are fixed at preparation and must never be regenerated from mutable
// process state.
type ToolExecution struct {
	RunKey         string              `json:"run_key"`
	StepNumber     int                 `json:"step_number"`
	Ordinal        int                 `json:"ordinal"`
	ToolCall       ToolCall            `json:"tool_call"`
	IdempotencyKey string              `json:"idempotency_key"`
	InputHash      string              `json:"input_hash"`
	Status         ToolExecutionStatus `json:"status"`
	Result         *ToolResult         `json:"result,omitempty"`
}

// MutationGuard is the complete write authority returned by Acquire or a prior
// mutation. A store must compare all fields in the same transaction as its write.
type MutationGuard struct {
	RunKey     string `json:"run_key"`
	LeaseOwner string `json:"lease_owner"`
	Revision   uint64 `json:"revision"`
	FenceToken uint64 `json:"fence_token"`
}

// CheckpointStore is the durable recovery boundary. Implementations must make
// each method atomic in one local persistence transaction and return deep copies.
// They must not hold a transaction open while model or tool code executes.
//
// Begin is create-if-absent by RunKey and by the caller's session/request pair.
// A duplicate with identical identity and digests returns the existing snapshot;
// conflicting immutable data returns ErrCheckpointConflict.
//
// Acquire grants or takes over an expired/suspended lease, atomically increments
// FenceToken and Revision, and returns running state. Lease expiry permits a new
// acquisition but never authorizes writes by itself. Every remaining mutating
// operation atomically compares MutationGuard, applies one valid transition, and
// increments Revision; stale guards return ErrCheckpointConflict.
//
// CommitModelResponse atomically records a complete provider response, usage,
// and history before tools can run. A crash during model_inflight cannot reveal
// whether the provider processed the request, so recovery repeats the model call:
// model generation is at-least-once.
//
// PrepareTools atomically creates every ToolExecution in prepared state with its
// stable key before any Tool.Execute call. BeginTool changes exactly one entry to
// executing before the external call. CommitTool atomically stores its complete
// result and advances the checkpoint. A crash after an external effect but before
// CommitTool leaves executing ambiguity. Recovery must mark it unknown unless the
// tool can safely retry or query using the same key. Thus tool effects are also
// at-least-once by default and become effectively-once only when the tool's own
// database/provider deduplicates that key; this interface cannot promise exactly-once.
//
// Complete, Suspend, and Fail atomically persist their final checkpoint and state.
// Complete must be invoked through a transaction-scoped implementation helper
// when an owning domain record must become final in the same database transaction.
// Load is read-only and does not acquire a lease or authorize a subsequent write.
type CheckpointStore interface {
	Begin(ctx context.Context, identity RunIdentity, inputDigest, configDigest string, checkpoint Checkpoint) (RunSnapshot, error)
	Acquire(ctx context.Context, runKey, leaseOwner string, leaseUntil time.Time) (RunSnapshot, error)
	ModelInflight(ctx context.Context, guard MutationGuard, checkpoint Checkpoint) (RunSnapshot, error)
	CommitModelResponse(ctx context.Context, guard MutationGuard, response Response, checkpoint Checkpoint) (RunSnapshot, error)
	PrepareTools(ctx context.Context, guard MutationGuard, tools []ToolExecution, checkpoint Checkpoint) (RunSnapshot, error)
	BeginTool(ctx context.Context, guard MutationGuard, idempotencyKey string, safeReplay bool) (RunSnapshot, ToolExecution, error)
	CommitTool(ctx context.Context, guard MutationGuard, execution ToolExecution, checkpoint Checkpoint) (RunSnapshot, error)
	Complete(ctx context.Context, guard MutationGuard, checkpoint Checkpoint) (RunSnapshot, error)
	Suspend(ctx context.Context, guard MutationGuard, checkpoint Checkpoint) (RunSnapshot, error)
	Fail(ctx context.Context, guard MutationGuard, checkpoint Checkpoint, failure RunFailure) (RunSnapshot, error)
	Load(ctx context.Context, runKey string) (RunSnapshot, error)
}

// CanonicalDigest returns the SHA-256 digest of the project's deterministic JSON
// representation. JSON object keys are ordered by the codec; array order remains
// significant because it can change message and tool semantics.
func CanonicalDigest(value any) (string, error) {
	data, err := jsoncodec.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal canonical digest input: %w", err)
	}
	return digestBytes(data), nil
}

// DigestRunInput fingerprints immutable invocation data.
func DigestRunInput(input ImmutableRunInput) (string, error) {
	return CanonicalDigest(input)
}

// DigestRunConfig fingerprints immutable execution configuration.
func DigestRunConfig(config ImmutableRunConfig) (string, error) {
	return CanonicalDigest(config)
}

// DigestToolInput fingerprints tool input deterministically. Valid JSON uses
// its semantic representation; malformed JSON uses its exact raw bytes so it
// can be durably recorded before model-visible validation.
func DigestToolInput(input string) (string, error) {
	normalized := input
	if normalized == "" {
		normalized = "{}"
	}
	var value any
	if err := jsoncodec.Unmarshal([]byte(normalized), &value); err != nil {
		return digestBytes([]byte(input)), nil
	}
	return CanonicalDigest(value)
}

// ToolExecutionKey derives a stable opaque key from immutable run and call
// coordinates. Valid JSON inputs are canonicalized; malformed inputs still get
// a deterministic key from their exact bytes so preparation can be audited.
func ToolExecutionKey(identity RunIdentity, stepNumber, ordinal int, call ToolCall) string {
	inputHash, err := DigestToolInput(call.Input)
	if err != nil {
		inputHash = digestBytes([]byte(call.Input))
	}
	payload := struct {
		RunKey     string `json:"run_key"`
		AgentKey   string `json:"agent_key"`
		StepNumber int    `json:"step_number"`
		Ordinal    int    `json:"ordinal"`
		CallID     string `json:"call_id"`
		ToolName   string `json:"tool_name"`
		InputHash  string `json:"input_hash"`
	}{identity.RunKey, identity.AgentKey, stepNumber, ordinal, call.ID, call.Name, inputHash}
	digest, err := CanonicalDigest(payload)
	if err != nil {
		panic("agent: fixed tool execution key payload is not JSON serializable")
	}
	return "tool:" + digest[len(digestPrefix):]
}

// NewToolExecution creates a prepared ledger record for raw tool input.
// Runtime schema validation remains in the shared execution path.
func NewToolExecution(identity RunIdentity, stepNumber, ordinal int, call ToolCall) (ToolExecution, error) {
	inputHash, err := DigestToolInput(call.Input)
	if err != nil {
		return ToolExecution{}, err
	}
	return ToolExecution{
		RunKey: identity.RunKey, StepNumber: stepNumber, Ordinal: ordinal,
		ToolCall: call, IdempotencyKey: ToolExecutionKey(identity, stepNumber, ordinal, call),
		InputHash: inputHash, Status: ToolExecutionPrepared,
	}, nil
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s%x", digestPrefix, sum)
}

type toolExecutionContextKey struct{}

// WithToolExecutionKey exposes the stable key to Tool.Execute without using a
// collision-prone string context key. An empty key leaves ctx unchanged.
func WithToolExecutionKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, toolExecutionContextKey{}, key)
}

// ToolExecutionKeyFromContext returns the stable key supplied by the durable
// runner. Tools must treat absence as meaning no idempotency guarantee.
func ToolExecutionKeyFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	key, ok := ctx.Value(toolExecutionContextKey{}).(string)
	return key, ok && key != ""
}

// MarshalRunSnapshot serializes only supported, internally coherent state.
func MarshalRunSnapshot(snapshot RunSnapshot) ([]byte, error) {
	if snapshot.SchemaVersion != RunSnapshotSchemaVersion {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedRunSnapshotSchema, snapshot.SchemaVersion)
	}
	return jsoncodec.Marshal(snapshot)
}

// UnmarshalRunSnapshot rejects old and future schemas before state is resumed.
func UnmarshalRunSnapshot(data []byte) (RunSnapshot, error) {
	var snapshot RunSnapshot
	if err := jsoncodec.UnmarshalStrict(data, &snapshot); err != nil {
		return RunSnapshot{}, fmt.Errorf("decode run snapshot: %w", err)
	}
	if snapshot.SchemaVersion != RunSnapshotSchemaVersion {
		return RunSnapshot{}, fmt.Errorf("%w: %d", ErrUnsupportedRunSnapshotSchema, snapshot.SchemaVersion)
	}
	return snapshot, nil
}

// Clone returns a deep copy suitable for crossing a store or observer boundary.
func (snapshot RunSnapshot) Clone() RunSnapshot {
	cloned := snapshot
	cloned.Checkpoint = snapshot.Checkpoint.Clone()
	if snapshot.Failure != nil {
		failure := *snapshot.Failure
		cloned.Failure = &failure
	}
	return cloned
}

// Clone returns a deep copy of all mutable checkpoint slices and pointers.
func (checkpoint Checkpoint) Clone() Checkpoint {
	cloned := checkpoint
	cloned.History = cloneMessages(checkpoint.History)
	cloned.NewMessages = cloneMessages(checkpoint.NewMessages)
	cloned.CompletedSteps = cloneSteps(checkpoint.CompletedSteps)
	cloned.PendingToolCalls = append([]ToolCall(nil), checkpoint.PendingToolCalls...)
	cloned.Outcome.Messages = cloneMessages(checkpoint.Outcome.Messages)
	cloned.Outcome.Steps = cloneSteps(checkpoint.Outcome.Steps)
	return cloned
}

// Guard returns the write authority represented by a leased snapshot.
func (snapshot RunSnapshot) Guard() MutationGuard {
	return MutationGuard{RunKey: snapshot.Identity.RunKey, LeaseOwner: snapshot.LeaseOwner, Revision: snapshot.Revision, FenceToken: snapshot.FenceToken}
}

// ValidateRunTransition validates adjacent lifecycle states independently of a
// persistence implementation. It also enforces immutable identity and digests.
func ValidateRunTransition(from, to RunSnapshot) error {
	if from.Identity != to.Identity || from.InputDigest != to.InputDigest || from.ConfigDigest != to.ConfigDigest || from.SchemaVersion != to.SchemaVersion {
		return fmt.Errorf("%w: immutable run fields changed", ErrInvalidRunTransition)
	}
	if to.Revision != from.Revision+1 {
		return fmt.Errorf("%w: revision must increment by one", ErrInvalidRunTransition)
	}
	if isTerminalRunStatus(from.Status) {
		return fmt.Errorf("%w: terminal run is immutable", ErrInvalidRunTransition)
	}
	if to.Status == RunStatusClaimed {
		return fmt.Errorf("%w: claimed is an initial status only", ErrInvalidRunTransition)
	}
	if !validStatusPhase(to.Status, to.Phase) {
		return fmt.Errorf("%w: status %q cannot use phase %q", ErrInvalidRunTransition, to.Status, to.Phase)
	}
	if to.Status == RunStatusCompleted && from.Phase != RunPhaseFinalizing {
		return fmt.Errorf("%w: completed run must be finalized", ErrInvalidRunTransition)
	}
	if !validPhaseEdge(from.Phase, to.Phase) {
		return fmt.Errorf("%w: phase %q cannot advance to %q", ErrInvalidRunTransition, from.Phase, to.Phase)
	}
	return nil
}

func isTerminalRunStatus(status RunStatus) bool {
	return status == RunStatusCompleted || status == RunStatusFailed || status == RunStatusAbandoned
}

func validStatusPhase(status RunStatus, phase RunPhase) bool {
	switch status {
	case RunStatusClaimed:
		return phase == RunPhaseModelReady
	case RunStatusRunning:
		return phase != RunPhaseTerminal
	case RunStatusSuspended:
		return phase == RunPhaseModelReady || phase == RunPhaseToolsReady
	case RunStatusCompleted, RunStatusFailed, RunStatusAbandoned:
		return phase == RunPhaseTerminal
	default:
		return false
	}
}

func validPhaseEdge(from, to RunPhase) bool {
	if to == RunPhaseTerminal {
		return true
	}
	switch from {
	case RunPhaseModelReady:
		return to == RunPhaseModelReady || to == RunPhaseModelInflight
	case RunPhaseModelInflight:
		return to == RunPhaseModelInflight || to == RunPhaseModelReady || to == RunPhaseToolsReady || to == RunPhaseFinalizing
	case RunPhaseToolsReady:
		return to == RunPhaseToolsReady || to == RunPhaseToolInflight || to == RunPhaseModelReady || to == RunPhaseFinalizing
	case RunPhaseToolInflight:
		return to == RunPhaseToolInflight || to == RunPhaseToolsReady || to == RunPhaseModelReady || to == RunPhaseFinalizing
	case RunPhaseFinalizing:
		return to == RunPhaseFinalizing
	default:
		return false
	}
}
