package durable

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/iceymoss/agent-runtime-go"
	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"
)

const digestPrefix = "sha256:"

func MarshalSnapshot(snapshot Snapshot) ([]byte, error) {
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		return nil, durableError(ErrSnapshotSchema, "marshal", RunKey(snapshot.Identity.RunKey), fmt.Sprintf("version %d", snapshot.SchemaVersion))
	}
	return jsoncodec.Marshal(snapshot)
}

func UnmarshalSnapshot(data []byte) (Snapshot, error) {
	var snapshot Snapshot
	if err := jsoncodec.UnmarshalStrict(data, &snapshot); err != nil {
		return Snapshot{}, durableError(ErrSnapshotSchema, "decode", "", err.Error())
	}
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		return Snapshot{}, durableError(ErrSnapshotSchema, "decode", RunKey(snapshot.Identity.RunKey), fmt.Sprintf("version %d", snapshot.SchemaVersion))
	}
	return snapshot, nil
}

func CanonicalDigest(value any) (string, error) {
	data, err := jsoncodec.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal canonical digest input: %w", err)
	}
	return digestBytes(data), nil
}

func DigestInput(input ImmutableInput) (string, error)    { return CanonicalDigest(input) }
func DigestConfig(config ImmutableConfig) (string, error) { return CanonicalDigest(config) }

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

func ToolExecutionKey(identity Identity, step, ordinal int, call agent.ToolCall) ExecutionKey {
	inputDigest, err := DigestToolInput(call.Input)
	if err != nil {
		inputDigest = digestBytes([]byte(call.Input))
	}
	payload := struct {
		RunKey     string `json:"run_key"`
		AgentKey   string `json:"agent_key"`
		StepNumber int    `json:"step_number"`
		Ordinal    int    `json:"ordinal"`
		CallID     string `json:"call_id"`
		ToolName   string `json:"tool_name"`
		InputHash  string `json:"input_hash"`
	}{identity.RunKey, identity.AgentKey, step, ordinal, call.ID, call.Name, inputDigest}
	digest, err := CanonicalDigest(payload)
	if err != nil {
		panic("durable: fixed tool execution key payload is not serializable")
	}
	return ExecutionKey("tool:" + strings.TrimPrefix(digest, digestPrefix))
}

func UsageFactKey(tenant TenantKey, attempt AttemptKey, execution ExecutionKey, kind string) UsageKey {
	payload := struct {
		Tenant    TenantKey    `json:"tenant_key"`
		Attempt   AttemptKey   `json:"attempt_key"`
		Execution ExecutionKey `json:"execution_key,omitempty"`
		Kind      string       `json:"kind"`
	}{tenant, attempt, execution, kind}
	digest, err := CanonicalDigest(payload)
	if err != nil {
		panic("durable: fixed usage key payload is not serializable")
	}
	return UsageKey("usage:" + strings.TrimPrefix(digest, digestPrefix))
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%s%x", digestPrefix, sum)
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	data, err := jsoncodec.Marshal(snapshot)
	if err != nil {
		panic("durable: snapshot clone marshal failed: " + err.Error())
	}
	var cloned Snapshot
	if err := jsoncodec.Unmarshal(data, &cloned); err != nil {
		panic("durable: snapshot clone unmarshal failed: " + err.Error())
	}
	return cloned
}

func ValidateSnapshot(snapshot Snapshot) error {
	key := RunKey(snapshot.Identity.RunKey)
	if snapshot.SchemaVersion != SnapshotSchemaVersion {
		return durableError(ErrSnapshotSchema, "validate", key, fmt.Sprintf("version %d", snapshot.SchemaVersion))
	}
	if snapshot.Identity.RunKey == "" || snapshot.Identity.AgentKey == "" || snapshot.Identity.SessionID == "" || snapshot.Identity.RequestID == "" || snapshot.InputDigest == "" || snapshot.ConfigDigest == "" {
		return durableError(ErrSnapshotIncoherent, "validate", key, "missing immutable identity or digest")
	}
	if !validStatusPhase(snapshot.Status, snapshot.Phase) {
		return durableError(ErrSnapshotIncoherent, "validate", key, "illegal status and phase")
	}
	if snapshot.Status == StatusRunning {
		if snapshot.LeaseOwner == "" || snapshot.LeaseUntil.IsZero() || snapshot.FenceToken == 0 {
			return durableError(ErrSnapshotIncoherent, "validate", key, "running state lacks lease authority")
		}
	} else if snapshot.LeaseOwner != "" || !snapshot.LeaseUntil.IsZero() {
		return durableError(ErrSnapshotIncoherent, "validate", key, "unleased state retains lease authority")
	}
	return nil
}

func terminalStatus(status Status) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusAbandoned
}

func validStatusPhase(status Status, phase Phase) bool {
	switch status {
	case StatusClaimed:
		return phase == PhaseModelReady
	case StatusRunning:
		return phase == PhaseModelReady || phase == PhaseModelInflight || phase == PhaseToolsReady || phase == PhaseToolInflight || phase == PhaseFinalizing
	case StatusSuspended:
		return phase == PhaseModelReady || phase == PhaseToolsReady
	case StatusCompleted, StatusFailed, StatusAbandoned:
		return phase == PhaseTerminal
	default:
		return false
	}
}

func validPhaseEdge(from, to Phase) bool {
	if to == PhaseTerminal {
		return true
	}
	switch from {
	case PhaseModelReady:
		return to == PhaseModelReady || to == PhaseModelInflight
	case PhaseModelInflight:
		return to == PhaseModelInflight || to == PhaseModelReady || to == PhaseToolsReady || to == PhaseFinalizing
	case PhaseToolsReady:
		return to == PhaseToolsReady || to == PhaseToolInflight || to == PhaseModelReady || to == PhaseFinalizing
	case PhaseToolInflight:
		return to == PhaseToolInflight || to == PhaseToolsReady || to == PhaseModelReady || to == PhaseFinalizing
	case PhaseFinalizing:
		return to == PhaseFinalizing
	default:
		return false
	}
}
