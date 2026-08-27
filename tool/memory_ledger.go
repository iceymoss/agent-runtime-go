package tool

import (
	"context"
	"sync"

	"github.com/iceymoss/agent-runtime-go"
)

// MemoryLedger is a thread-safe in-memory reference implementation of
// ExecutionLedger.
//
// It exists so an executor can be assembled and tested without a database, and
// so there is one authoritative statement of what the lifecycle transitions
// mean. A production adapter maps the same commands onto the durable owner's
// records; this type is not a second persistence owner and keeps nothing after
// the process exits.
type MemoryLedger struct {
	mu      sync.Mutex
	records map[string]ExecutionRecord
	digests map[string]string
}

// NewMemoryLedger returns an empty in-memory execution ledger.
func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{records: make(map[string]ExecutionRecord), digests: make(map[string]string)}
}

var _ ExecutionLedger = (*MemoryLedger)(nil)

// Prepare records the immutable identity of one authorized invocation.
//
// It is idempotent by execution key so a retried preparation adopts the existing
// record. A different prepared execution under the same key is a conflict: the
// key is derived from the call, so two different calls sharing one means
// something upstream computed it wrong.
func (l *MemoryLedger) Prepare(_ context.Context, prepared PreparedExecution) (ExecutionRecord, bool, error) {
	digest, err := agent.CanonicalDigest(prepared)
	if err != nil {
		return ExecutionRecord{}, false, lifecycleError(ErrExecutionConflict, err, "prepare", prepared.ExecutionKey, "prepared execution is not serializable")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if existing, ok := l.records[prepared.ExecutionKey]; ok {
		if l.digests[prepared.ExecutionKey] != digest {
			return ExecutionRecord{}, false, lifecycleError(ErrExecutionConflict, nil, "prepare", prepared.ExecutionKey, "prepared execution differs")
		}
		return cloneRecord(existing), false, nil
	}
	record := ExecutionRecord{Prepared: prepared, Status: StatusPrepared, Revision: 1}
	l.records[prepared.ExecutionKey] = cloneRecord(record)
	l.digests[prepared.ExecutionKey] = digest
	return cloneRecord(record), true, nil
}

// Begin moves a prepared execution to running under the caller's fence. It is
// the last durable write before the tool may change anything outside the process.
func (l *MemoryLedger) Begin(_ context.Context, key string, fence uint64) (ExecutionRecord, error) {
	return l.transition(key, func(current ExecutionRecord) (ExecutionRecord, error) {
		if current.Status != StatusPrepared {
			return ExecutionRecord{}, lifecycleError(ErrExecutionConflict, nil, "begin", key, "execution is not prepared")
		}
		current.Status, current.FenceToken, current.Revision = StatusRunning, fence, current.Revision+1
		return current, nil
	})
}

// Reject records that authorization refused an execution that never ran, which
// is why it compares the prepared fence rather than a running one.
func (l *MemoryLedger) Reject(_ context.Context, key string, fence uint64, failure Failure) (ExecutionRecord, error) {
	return l.transition(key, func(current ExecutionRecord) (ExecutionRecord, error) {
		if current.Status != StatusPrepared || current.Prepared.FenceToken != fence {
			return ExecutionRecord{}, lifecycleError(ErrStaleFence, nil, "reject", key, "execution is not prepared at this fence")
		}
		stored := failure
		current.Status, current.Failure, current.FenceToken = StatusFailed, &stored, fence
		current.Revision++
		return current, nil
	})
}

// Suspend parks a running execution and stores the handle a later attempt
// resumes from.
//
// It is a distinct state from failed and from unknown on purpose: the tool did
// not fail, and its outcome is not ambiguous - it simply has not happened yet,
// and the handle is what makes finishing it possible.
func (l *MemoryLedger) Suspend(_ context.Context, command SuspendExecution) (ExecutionRecord, error) {
	if command.Suspension.Kind == "" {
		return ExecutionRecord{}, lifecycleError(ErrInvalidConfiguration, nil, "suspend", command.ExecutionKey, "a suspension kind is required")
	}
	return l.transition(command.ExecutionKey, func(current ExecutionRecord) (ExecutionRecord, error) {
		if current.Status != StatusRunning || current.FenceToken != command.FenceToken {
			return ExecutionRecord{}, lifecycleError(ErrStaleFence, nil, "suspend", command.ExecutionKey, "execution is not running at this fence")
		}
		stored := command.Suspension
		current.Status, current.Suspension = StatusSuspended, &stored
		current.Revision++
		return current, nil
	})
}

// Resume re-arms a suspended execution so the tool can be invoked again with its
// handle. It never resurrects an execution that reached a terminal state.
func (l *MemoryLedger) Resume(_ context.Context, key string, fence uint64) (ExecutionRecord, error) {
	return l.transition(key, func(current ExecutionRecord) (ExecutionRecord, error) {
		if current.Status != StatusSuspended || current.FenceToken != fence {
			return ExecutionRecord{}, lifecycleError(ErrStaleFence, nil, "resume", key, "execution is not suspended at this fence")
		}
		current.Status = StatusRunning
		current.Revision++
		return current, nil
	})
}

// Complete stores the one durable outcome of a running execution.
func (l *MemoryLedger) Complete(_ context.Context, command CompleteExecution) (ExecutionRecord, error) {
	return l.transition(command.ExecutionKey, func(current ExecutionRecord) (ExecutionRecord, error) {
		if current.Status != StatusRunning || current.FenceToken != command.FenceToken {
			return ExecutionRecord{}, lifecycleError(ErrStaleFence, nil, "complete", command.ExecutionKey, "execution is not running at this fence")
		}
		current.Result, current.Failure = command.Result, command.Failure
		current.Status, current.Suspension = StatusFailed, nil
		if command.Result != nil {
			current.Status = StatusSucceeded
		}
		current.Revision++
		return current, nil
	})
}

// MarkUnknown records that an effect may have happened but its result could not
// be stored. Such an execution is never replayed automatically.
func (l *MemoryLedger) MarkUnknown(_ context.Context, key string, fence uint64, failure Failure) (ExecutionRecord, error) {
	return l.transition(key, func(current ExecutionRecord) (ExecutionRecord, error) {
		if current.Status != StatusRunning || current.FenceToken != fence {
			return ExecutionRecord{}, lifecycleError(ErrStaleFence, nil, "mark unknown", key, "execution is not running at this fence")
		}
		stored := failure
		current.Status, current.Failure = StatusUnknown, &stored
		current.Revision++
		return current, nil
	})
}

// Load returns one execution record, which is how a resumed approval finds the
// exact prepared execution it must be revalidated against.
func (l *MemoryLedger) Load(_ context.Context, key string) (ExecutionRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.records[key]
	if !ok {
		return ExecutionRecord{}, lifecycleError(ErrToolNotFound, nil, "load", key, "execution is not prepared")
	}
	return cloneRecord(record), nil
}

// transition applies one guarded state change. The apply function owns the
// legality rules; this helper owns atomicity.
func (l *MemoryLedger) transition(key string, apply func(ExecutionRecord) (ExecutionRecord, error)) (ExecutionRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, ok := l.records[key]
	if !ok {
		return ExecutionRecord{}, lifecycleError(ErrToolNotFound, nil, "transition", key, "execution is not prepared")
	}
	next, err := apply(current)
	if err != nil {
		return ExecutionRecord{}, err
	}
	l.records[key] = cloneRecord(next)
	return cloneRecord(next), nil
}
