package session

import (
	"github.com/iceymoss/agent-runtime-go"
)

// This file exports the session aggregate's legality rules so a storage adapter
// can enforce exactly the same ones the reference implementation does.
//
// A session's status and a branch's status are small state machines, but they
// are the machines that decide whether a finished conversation can be reopened
// and whether a branch that already merged can merge again. An adapter that
// restated them from memory would be one missing case away from double-merging
// a branch, so the tables live here and adapters call them.

// ValidStatusTransition reports whether a session may move between two statuses.
//
// Completed and abandoned are terminal on purpose: a session that has been
// finalized must not silently accept more turns, because its transcript and its
// usage totals have already been reported as final.
func ValidStatusTransition(from, to Status) bool { return validStatusTransition(from, to) }

// KnownStatus reports whether a status value is one this package defines. A
// stored value that is not recognized means the row was written by something
// that does not share this contract.
func KnownStatus(status Status) bool { return knownStatus(status) }

// ValidBranchTransition reports whether a branch may move between two statuses.
//
// The one-way steps matter: a merged branch is final, so a duplicate merge
// command is rejected rather than applied twice, and a conflicted branch must be
// made ready again before it can merge.
func ValidBranchTransition(from, to BranchStatus) bool { return validBranchTransition(from, to) }

// ValidContextPivot reports whether a stored context pivot is internally
// coherent for a session at the given revision.
//
// An empty pivot is valid: it means no compaction has happened. A partial pivot
// is not, because a resumed session would then reconstruct its history from an
// artifact it cannot verify.
func ValidContextPivot(pivot ContextPivotSnapshot, revision uint64) bool {
	return validPivot(pivot, revision)
}

// AddNonNegative accumulates a usage total, reporting false when the delta is
// negative or the sum would overflow.
//
// Usage totals only ever grow. A negative delta means a caller is trying to undo
// recorded spend, which is a correction to make explicitly rather than by
// subtracting from a running total.
func AddNonNegative(current, delta int64) (int64, bool) { return addNonnegative(current, delta) }

// CloneSnapshot returns a deep copy of a session snapshot.
func CloneSnapshot(snapshot Snapshot) Snapshot { return cloneSnapshot(snapshot) }

// CloneMessages returns a deep copy of run input or output messages, so an
// adapter can hand callers values they may safely mutate.
func CloneMessages(messages []agent.Message) []agent.Message { return cloneMessages(messages) }

// SameCreate reports whether two create commands describe the same session.
//
// Creation is idempotent by session key, so a replay must be recognized as the
// same command rather than rejected or applied twice. Mutation metadata is
// deliberately ignored: it records who issued the command and when, which
// legitimately differs between a first attempt and its retry.
func SameCreate(got, want CreateCommand) bool { return sameCreate(got, want) }

// DeriveBranchKey returns the branch identity one run always produces.
//
// It is derived rather than generated so a retried CreateBranch addresses the
// branch that already exists. An adapter that generated its own key would create
// a second branch for the same run, and both would then claim the right to merge.
func DeriveBranchKey(tenant agent.TenantKey, run RunKey) BranchKey {
	return makeBranchKey(tenant, run)
}

// SameAdmission reports whether two admissions describe the same queued run.
//
// Admission is idempotent by request id, so a retried submit must be recognized
// rather than queued twice. The creation timestamp is excluded because it
// records when the request arrived, which legitimately differs between the first
// attempt and its retry, while everything the run will actually execute must
// match exactly.
func SameAdmission(left, right Admission) bool { return sameAdmission(left, right) }

// CancelSupersedes reports whether a requested cancellation is stronger than the
// one already recorded.
//
// Cancellation only escalates: attempt, then suspend, then abandon. A weaker
// request arriving late must not walk an abandoned run back to a merely
// suspended one, which would let work resume after someone decided it should not.
func CancelSupersedes(current, requested CancelMode) bool {
	return cancelRank(requested) > cancelRank(current)
}

// KnownCancelMode reports whether a cancellation mode is one this package
// defines. An unrecognized mode is a caller error, not a weaker cancellation.
func KnownCancelMode(mode CancelMode) bool { return cancelRank(mode) != 0 }

// TerminalExecutionState reports whether a run has reached a state it can never
// leave. A terminal run accepts no outcome, no cancellation, and no resume, so
// this is the guard every one of those paths checks first.
func TerminalExecutionState(state ExecutionState) bool { return terminalExecution(state) }

// ClaimBefore reports whether left should be claimed before right.
//
// The order is priority, then arrival, then keys. It is a total order on
// purpose: two workers scanning the same queue must agree on what "next" means,
// and ties broken arbitrarily would let a low-priority run overtake a
// high-priority one depending on which worker looked first.
func ClaimBefore(left, right Admission) bool {
	if left.Priority != right.Priority {
		return left.Priority > right.Priority
	}
	if !left.CreatedAt.Equal(right.CreatedAt) {
		return left.CreatedAt.Before(right.CreatedAt)
	}
	if left.BranchKey != right.BranchKey {
		return left.BranchKey < right.BranchKey
	}
	return left.RunKey < right.RunKey
}
