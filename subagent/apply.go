package subagent

import (
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// This file is the sub-agent state machine that a Store must enforce, expressed
// as pure functions and value methods.
//
// It is exported for a specific reason: depth, fanout, cycle, and budget are
// safety limits, not bookkeeping. An adapter that restated them slightly
// differently would not fail loudly - it would quietly allow a deeper tree, a
// wider fan-out, or more spend than the caller authorized. Storage adapters are
// therefore expected to call these rather than reimplement them, and to own only
// what storage genuinely owns: looking up the parent, counting siblings, and
// writing the result atomically.

// DeriveRunKey returns the child run key one spawn request always produces.
// Derivation is deterministic so a retried spawn addresses the same child.
func DeriveRunKey(tenant agent.TenantKey, request RequestKey) RunKey {
	return RunKey(derivedKey("run", string(tenant), request))
}

// DeriveRelationshipKey returns the parent-child relationship identity.
func DeriveRelationshipKey(tenant agent.TenantKey, request RequestKey) RelationshipKey {
	return RelationshipKey(derivedKey("relationship", string(tenant), request))
}

// DeriveSessionKey returns the child's own session identity.
func DeriveSessionKey(tenant agent.TenantKey, request RequestKey) SessionKey {
	return SessionKey(derivedKey("session", string(tenant), request))
}

// SpecDigest fingerprints the immutable spawn input. A replayed request with the
// same digest is the same child; a different digest under the same request key
// is an idempotency conflict.
func SpecDigest(request SpawnRequest) (string, error) {
	value, err := digest(request)
	if err != nil {
		return "", fmt.Errorf("%w: digest spawn request: %v", ErrInvalidRequest, err)
	}
	return value, nil
}

// ValidateSpawn reports whether a spawn request is well formed and still within
// its deadline. It checks the request alone; depth, fanout, cycle, and budget
// need the surrounding tree and have their own functions.
func ValidateSpawn(request SpawnRequest, now time.Time) error {
	return validateSpawn(request, now)
}

// NewChildRef assembles the child identity a spawn produces at a known depth.
func NewChildRef(tenant agent.TenantKey, request RequestKey, depth uint16) ChildRef {
	return ChildRef{
		TenantKey: tenant, RelationshipKey: DeriveRelationshipKey(tenant, request),
		SessionKey: DeriveSessionKey(tenant, request), RunKey: DeriveRunKey(tenant, request), Depth: depth,
	}
}

// EffectiveLimits narrows a tree's limits to what this child actually reserved,
// and to the earliest applicable deadline. A child may never spend more than the
// slice of the tree budget it was given.
func EffectiveLimits(limits Limits, reserve Reservation, requested, now time.Time) Limits {
	return effectiveLimits(limits, reserve, requested, now)
}

// ValidateDepth reports whether a child at this depth is within the tree's limit.
func ValidateDepth(depth uint16, limits Limits) error {
	if depth > limits.MaxDepth {
		return ErrDepthExceeded
	}
	return nil
}

// ValidateFanout reports whether one more direct child is within the limit.
// directChildren counts the parent's existing non-canceled children.
func ValidateFanout(directChildren int, limits Limits) error {
	if directChildren >= int(limits.MaxFanout) {
		return ErrFanoutExceeded
	}
	return nil
}

// ParentLookup resolves a run key to its parent run key. It reports false for a
// run the store does not know, which ends the walk.
type ParentLookup func(RunKey) (RunKey, bool)

// ValidateNoCycle reports whether adding this child would close a loop in the
// delegation graph. A cycle would let a tree delegate to itself forever, so the
// walk is bounded by the runs the store already knows.
func ValidateNoCycle(parentRun, childRun RunKey, lookup ParentLookup) error {
	if parentRun == childRun {
		return ErrCycle
	}
	if lookup == nil {
		return nil
	}
	seen := map[RunKey]struct{}{childRun: {}}
	current := parentRun
	for current != "" {
		if _, exists := seen[current]; exists {
			return ErrCycle
		}
		seen[current] = struct{}{}
		next, ok := lookup(current)
		if !ok {
			return nil
		}
		current = next
	}
	return nil
}

// ValidateUsage reports whether a settlement's numbers are usable.
func ValidateUsage(usage Usage) error { return validateUsage(usage) }

// ExceedsReservation reports whether settled usage is larger than what the child
// reserved. A child that overspends its reservation has escaped the tree budget.
func ExceedsReservation(usage Usage, reservation Reservation) bool {
	return exceedsReservation(usage, reservation)
}

// Reserve claims a slice of the tree budget for one child.
//
// Reservation happens before the child runs, which is what makes the limit
// binding: by the time a child could overspend, the budget was already committed
// and any sibling that would have pushed the tree over was refused.
func (b BudgetSnapshot) Reserve(reserve Reservation) (BudgetSnapshot, error) {
	if b.Reserved.InputTokens+b.Settled.InputTokens+reserve.InputTokens > b.Limits.MaxInputTokens ||
		b.Reserved.OutputTokens+b.Settled.OutputTokens+reserve.OutputTokens > b.Limits.MaxOutputTokens {
		return b, ErrTokenBudgetExceeded
	}
	if b.Reserved.CostMicros+b.Settled.CostMicros+reserve.CostMicros > b.Limits.MaxCostMicros {
		return b, ErrCostBudgetExceeded
	}
	if b.Reserved.ToolCalls+b.Settled.ToolCalls+reserve.ToolCalls > b.Limits.MaxToolCalls {
		return b, ErrToolBudgetExceeded
	}
	if b.Reserved.Runtime+b.Settled.Runtime+reserve.Runtime > b.Limits.MaxRuntime {
		return b, ErrRuntimeBudgetExceeded
	}
	b.Reserved = addReservation(b.Reserved, reserve)
	return b, nil
}

// Settle converts one child's reservation into recorded usage and returns the
// unused remainder to the tree, so a cheap child does not permanently hold
// budget its siblings could have used.
func (b BudgetSnapshot) Settle(reservation Reservation, usage Usage) (BudgetSnapshot, error) {
	if err := ValidateUsage(usage); err != nil {
		return b, err
	}
	if ExceedsReservation(usage, reservation) {
		return b, ErrUsageExceedsReserve
	}
	b.Reserved = subtractReservation(b.Reserved, reservation)
	b.Settled = addUsage(b.Settled, usage)
	b.Released = addReservation(b.Released, unusedReservation(reservation, usage))
	return b, nil
}
