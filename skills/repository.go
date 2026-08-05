package skills

import (
	"context"
	"sort"
	"sync"

	"github.com/iceymoss/agent-runtime-go"
)

type RepositorySnapshot struct {
	Generation string
	Skills     []SourceSkill
	Tombstones []Tombstone
}

// Repository is a portable tenant-scoped persistence port. Implementations
// must include tenant scope in every query and return only published records.
type Repository interface {
	LoadPublished(context.Context, agent.TenantKey) (RepositorySnapshot, error)
}

type RepositorySource struct {
	name       string
	repository Repository
	limits     Limits
}

func NewRepositorySource(name string, repository Repository, limits Limits) *RepositorySource {
	if name == "" {
		name = string(SourceTenantDB)
	}
	return &RepositorySource{name: name, repository: repository, limits: normalizeLimits(limits)}
}
func (s *RepositorySource) Name() string {
	if s == nil {
		return ""
	}
	return s.name
}
func (s *RepositorySource) Kind() SourceKind { return SourceTenantDB }
func (s *RepositorySource) Load(ctx context.Context, scope Scope) (SourceSnapshot, error) {
	if s == nil || s.repository == nil || !scope.TenantKey.Valid() {
		return SourceSnapshot{}, &Error{Code: CodeInvalidSource, Operation: "repository_load", Source: s.Name(), Cause: ErrInvalidSource}
	}
	value, err := s.repository.LoadPublished(ctx, scope.TenantKey)
	if err != nil {
		return SourceSnapshot{}, &Error{Code: CodeInvalidSource, Operation: "repository_load", Source: s.name, Cause: err}
	}
	if value.Generation == "" {
		return SourceSnapshot{}, &Error{Code: CodeInvalidSource, Operation: "repository_load", Source: s.name, Cause: ErrInvalidSource}
	}
	result := SourceSnapshot{Generation: value.Generation, Tombstones: append([]Tombstone(nil), value.Tombstones...)}
	for _, candidate := range value.Skills {
		if candidate.Disabled {
			// Disabled records do not publish and, unlike tombstones, do not mask
			// a lower-precedence skill with the same key.
			continue
		}
		validated, validateErr := validateAndDigest(candidate, SourceRef{Name: s.name, Kind: SourceTenantDB, Generation: value.Generation}, s.limits)
		if validateErr != nil {
			result.Diagnostics = append(result.Diagnostics, diagnosticForError(s.name, candidate.Descriptor.Key, validateErr))
			continue
		}
		result.Skills = append(result.Skills, validated)
	}
	sort.Slice(result.Skills, func(i, j int) bool {
		if result.Skills[i].Descriptor.Key == result.Skills[j].Descriptor.Key {
			return result.Skills[i].Descriptor.Version < result.Skills[j].Descriptor.Version
		}
		return result.Skills[i].Descriptor.Key < result.Skills[j].Descriptor.Key
	})
	sort.Slice(result.Tombstones, func(i, j int) bool { return result.Tombstones[i].Key < result.Tombstones[j].Key })
	return result, nil
}

type MemoryRepository struct {
	mu       sync.RWMutex
	tenants  map[agent.TenantKey]RepositorySnapshot
	failures map[agent.TenantKey]error
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{tenants: make(map[agent.TenantKey]RepositorySnapshot), failures: make(map[agent.TenantKey]error)}
}
func (r *MemoryRepository) Replace(tenant agent.TenantKey, snapshot RepositorySnapshot) error {
	if r == nil || !tenant.Valid() {
		return ErrInvalidSource
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tenants[tenant] = cloneRepositorySnapshot(snapshot)
	delete(r.failures, tenant)
	return nil
}
func (r *MemoryRepository) SetFailure(tenant agent.TenantKey, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.failures, tenant)
	} else {
		r.failures[tenant] = err
	}
}
func (r *MemoryRepository) LoadPublished(ctx context.Context, tenant agent.TenantKey) (RepositorySnapshot, error) {
	if r == nil || !tenant.Valid() {
		return RepositorySnapshot{}, ErrInvalidSource
	}
	if err := ctx.Err(); err != nil {
		return RepositorySnapshot{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if err := r.failures[tenant]; err != nil {
		return RepositorySnapshot{}, err
	}
	return cloneRepositorySnapshot(r.tenants[tenant]), nil
}
func cloneRepositorySnapshot(value RepositorySnapshot) RepositorySnapshot {
	result := RepositorySnapshot{Generation: value.Generation, Tombstones: append([]Tombstone(nil), value.Tombstones...)}
	result.Skills = make([]SourceSkill, len(value.Skills))
	for i := range value.Skills {
		result.Skills[i] = cloneSkill(value.Skills[i])
	}
	return result
}
