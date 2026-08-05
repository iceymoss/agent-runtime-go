package session

import (
	"context"
	"fmt"
	"math"
)

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidCommand)
	}
	return ctx.Err()
}

func validateSessionScope(tenant interface{ Valid() bool }, key SessionKey) error {
	if !tenant.Valid() || !key.Valid() {
		return fmt.Errorf("%w: tenant and session key are required", ErrInvalidCommand)
	}
	return nil
}

func validStatusTransition(from, to Status) bool {
	if from == StatusActive {
		return to == StatusSuspended || to == StatusCompleted || to == StatusAbandoned
	}
	if from == StatusSuspended {
		return to == StatusActive || to == StatusCompleted || to == StatusAbandoned
	}
	return false
}

func knownStatus(status Status) bool {
	return status == StatusActive || status == StatusSuspended || status == StatusCompleted || status == StatusAbandoned
}

func validBranchTransition(from, to BranchStatus) bool {
	switch from {
	case BranchStatusOpen:
		return to == BranchStatusReadyToMerge || to == BranchStatusAbandoned
	case BranchStatusReadyToMerge:
		return to == BranchStatusMerged || to == BranchStatusConflicted || to == BranchStatusAbandoned
	case BranchStatusConflicted:
		return to == BranchStatusReadyToMerge || to == BranchStatusAbandoned
	default:
		return false
	}
}

func addNonnegative(current, delta int64) (int64, bool) {
	if delta < 0 || current > math.MaxInt64-delta {
		return 0, false
	}
	return current + delta, true
}

func validPivot(pivot ContextPivotSnapshot, revision uint64) bool {
	empty := pivot.ArtifactKey == "" && pivot.ArtifactDigest == "" && pivot.CoveredThrough == 0 && pivot.SourceDigest == "" && pivot.ProtectedFactSetDigest == ""
	if empty {
		return true
	}
	return pivot.ArtifactKey != "" && pivot.ArtifactDigest != "" && pivot.SourceDigest != "" && pivot.ProtectedFactSetDigest != "" && pivot.CoveredThrough <= revision
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	snapshot.Metadata = cloneBytes(snapshot.Metadata)
	return snapshot
}
