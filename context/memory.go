package context

import (
	stdcontext "context"
	"fmt"
	"sync"

	"github.com/iceymoss/agent-runtime-go"
)

// MemoryStore is an exact, tenant-scoped immutable PlanStore. It intentionally
// has no current/latest lookup.
type MemoryStore struct {
	mu    sync.RWMutex
	plans map[agent.TenantKey]map[string]Plan
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{plans: make(map[agent.TenantKey]map[string]Plan)}
}

func (s *MemoryStore) CreateOrVerify(ctx stdcontext.Context, plan Plan) (Plan, error) {
	if err := validateStoreContext(ctx); err != nil {
		return Plan{}, err
	}
	if s == nil {
		return Plan{}, contextError(CodeInvalidRequest, "store_plan", "", ErrInvalidRequest, fmt.Errorf("nil store"))
	}
	if err := validatePlan(plan); err != nil {
		return Plan{}, err
	}
	plan = clonePlan(plan)
	s.mu.Lock()
	defer s.mu.Unlock()
	byKey := s.plans[plan.ref.TenantKey]
	if byKey == nil {
		byKey = make(map[string]Plan)
		s.plans[plan.ref.TenantKey] = byKey
	}
	if existing, exists := byKey[plan.ref.PlanKey]; exists {
		if existing.ref != plan.ref || !samePlanWire(existing.wire, plan.wire) {
			return Plan{}, contextError(CodeArtifactConflict, "store_plan", plan.ref.PlanKey, ErrArtifactConflict, nil)
		}
		return clonePlan(existing), nil
	}
	byKey[plan.ref.PlanKey] = plan
	return clonePlan(plan), nil
}

func (s *MemoryStore) Get(ctx stdcontext.Context, ref PlanRef) (Plan, error) {
	if err := validateStoreContext(ctx); err != nil {
		return Plan{}, err
	}
	if s == nil || !ref.TenantKey.Valid() || ref.PlanKey == "" || ref.PlanDigest == "" {
		return Plan{}, contextError(CodePlanNotFound, "get_plan", ref.PlanKey, ErrPlanNotFound, nil)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	plan, exists := s.plans[ref.TenantKey][ref.PlanKey]
	if !exists {
		return Plan{}, contextError(CodePlanNotFound, "get_plan", ref.PlanKey, ErrPlanNotFound, nil)
	}
	if plan.ref != ref {
		return Plan{}, contextError(CodePlanDrift, "get_plan", ref.PlanKey, ErrPlanDrift, nil)
	}
	if err := validatePlan(plan); err != nil {
		return Plan{}, err
	}
	return clonePlan(plan), nil
}

func samePlanWire(left, right planWire) bool {
	leftDigest, leftErr := agent.CanonicalDigest(left)
	rightDigest, rightErr := agent.CanonicalDigest(right)
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}

func validateStoreContext(ctx stdcontext.Context) error {
	if ctx == nil {
		return contextError(CodeInvalidRequest, "store", "", ErrInvalidRequest, fmt.Errorf("nil context"))
	}
	return ctx.Err()
}

type MemoryArtifactStore struct {
	mu        sync.RWMutex
	artifacts map[agent.TenantKey]map[string]SummaryArtifact
}

func NewMemoryArtifactStore() *MemoryArtifactStore {
	return &MemoryArtifactStore{artifacts: make(map[agent.TenantKey]map[string]SummaryArtifact)}
}

func (s *MemoryArtifactStore) CreateOrVerify(ctx stdcontext.Context, artifact SummaryArtifact) (SummaryArtifact, error) {
	if err := validateStoreContext(ctx); err != nil {
		return SummaryArtifact{}, err
	}
	if s == nil {
		return SummaryArtifact{}, contextError(CodeInvalidRequest, "store_artifact", "", ErrInvalidRequest, fmt.Errorf("nil store"))
	}
	if err := validateSummaryArtifact(artifact); err != nil {
		return SummaryArtifact{}, err
	}
	artifact = cloneSummaryArtifact(artifact)
	s.mu.Lock()
	defer s.mu.Unlock()
	byKey := s.artifacts[artifact.source.TenantKey]
	if byKey == nil {
		byKey = make(map[string]SummaryArtifact)
		s.artifacts[artifact.source.TenantKey] = byKey
	}
	if existing, exists := byKey[artifact.ref.Key]; exists {
		if existing.ref != artifact.ref || !sameArtifact(existing, artifact) {
			return SummaryArtifact{}, contextError(CodeArtifactConflict, "store_artifact", artifact.ref.Key, ErrArtifactConflict, nil)
		}
		return cloneSummaryArtifact(existing), nil
	}
	byKey[artifact.ref.Key] = artifact
	return cloneSummaryArtifact(artifact), nil
}

func (s *MemoryArtifactStore) Get(ctx stdcontext.Context, tenant agent.TenantKey, ref ArtifactRef) (SummaryArtifact, error) {
	if err := validateStoreContext(ctx); err != nil {
		return SummaryArtifact{}, err
	}
	if s == nil || !tenant.Valid() || ref.Key == "" || ref.Digest == "" {
		return SummaryArtifact{}, contextError(CodeArtifactNotFound, "get_artifact", ref.Key, ErrArtifactNotFound, nil)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	artifact, exists := s.artifacts[tenant][ref.Key]
	if !exists {
		return SummaryArtifact{}, contextError(CodeArtifactNotFound, "get_artifact", ref.Key, ErrArtifactNotFound, nil)
	}
	if artifact.ref != ref {
		return SummaryArtifact{}, contextError(CodeArtifactConflict, "get_artifact", ref.Key, ErrArtifactConflict, nil)
	}
	if err := validateSummaryArtifact(artifact); err != nil {
		return SummaryArtifact{}, err
	}
	return cloneSummaryArtifact(artifact), nil
}

func sameArtifact(left, right SummaryArtifact) bool {
	leftDigest, leftErr := agent.CanonicalDigest(left.wire())
	rightDigest, rightErr := agent.CanonicalDigest(right.wire())
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}
