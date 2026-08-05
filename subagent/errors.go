package subagent

import "errors"

var (
	ErrInvalidRequest        = errors.New("agent/subagent: invalid request")
	ErrNotFound              = errors.New("agent/subagent: child not found")
	ErrIdempotencyConflict   = errors.New("agent/subagent: idempotency conflict")
	ErrStaleVersion          = errors.New("agent/subagent: stale version")
	ErrCycle                 = errors.New("agent/subagent: cycle detected")
	ErrDepthExceeded         = errors.New("agent/subagent: maximum depth exceeded")
	ErrFanoutExceeded        = errors.New("agent/subagent: maximum fanout exceeded")
	ErrTokenBudgetExceeded   = errors.New("agent/subagent: token budget exceeded")
	ErrCostBudgetExceeded    = errors.New("agent/subagent: cost budget exceeded")
	ErrToolBudgetExceeded    = errors.New("agent/subagent: tool budget exceeded")
	ErrRuntimeBudgetExceeded = errors.New("agent/subagent: runtime budget exceeded")
	ErrDeadlineExceeded      = errors.New("agent/subagent: deadline exceeded")
	ErrInvalidTransition     = errors.New("agent/subagent: invalid transition")
	ErrUsageConflict         = errors.New("agent/subagent: usage fact conflict")
	ErrUsageExceedsReserve   = errors.New("agent/subagent: usage exceeds reservation")
	ErrTraversalLimit        = errors.New("agent/subagent: cancellation traversal limit exceeded")
	ErrWakeConflict          = errors.New("agent/subagent: wake conflict")
	ErrStoreInvariant        = errors.New("agent/subagent: store invariant violation")
)
