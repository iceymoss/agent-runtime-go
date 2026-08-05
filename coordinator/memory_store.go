package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/provider"
)

type memoryGeneration struct {
	manifest ArtifactManifest
	pins     map[string]RetentionPin
}

// MemoryManifestStore is a tenant-scoped exact-generation reference store.
// It never falls back to another tenant or to the current generation.
type MemoryManifestStore struct {
	mu      sync.RWMutex
	values  map[agent.TenantKey]map[string]*memoryGeneration
	current map[agent.TenantKey]string
}

func NewMemoryManifestStore() *MemoryManifestStore {
	return &MemoryManifestStore{
		values:  make(map[agent.TenantKey]map[string]*memoryGeneration),
		current: make(map[agent.TenantKey]string),
	}
}

func (s *MemoryManifestStore) SaveGeneration(_ context.Context, generation StoredGeneration, pin RetentionPin) error {
	if s == nil {
		return generationUnavailable("save_generation", "")
	}
	manifest := cloneManifest(generation.Manifest)
	if err := ValidateArtifactManifest(manifest); err != nil {
		return err
	}
	if len(generation.Pins) != 0 {
		return conflictError("save_generation", manifest.Wire.DefinitionDigest, "pins must be supplied through the pin argument")
	}
	if pin.PinKey != "" {
		if err := validatePin(pin); err != nil {
			return err
		}
	} else if pin.OwnerKey != "" || !pin.RetainUntil.IsZero() {
		return conflictError("save_generation", manifest.Wire.DefinitionDigest, "partial retention pin")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	byDigest := s.values[manifest.TenantKey]
	if byDigest == nil {
		byDigest = make(map[string]*memoryGeneration)
		s.values[manifest.TenantKey] = byDigest
	}
	digest := manifest.Wire.DefinitionDigest
	stored, exists := byDigest[digest]
	if exists {
		if !sameManifest(stored.manifest, manifest) {
			return conflictError("save_generation", digest, "same tenant and definition digest has different manifest data")
		}
	} else {
		stored = &memoryGeneration{manifest: manifest, pins: make(map[string]RetentionPin)}
		byDigest[digest] = stored
	}
	if pin.PinKey != "" {
		if err := createOrVerifyPin(stored.pins, pin); err != nil {
			if !exists && len(stored.pins) == 0 {
				delete(byDigest, digest)
			}
			return err
		}
	}
	s.current[manifest.TenantKey] = digest
	return nil
}

func (s *MemoryManifestStore) ResolveGeneration(_ context.Context, scope provider.Scope, definitionDigest string) (StoredGeneration, error) {
	if s == nil || !scope.TenantKey.Valid() || definitionDigest == "" {
		return StoredGeneration{}, generationUnavailable("resolve_generation", definitionDigest)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored, ok := s.values[scope.TenantKey][definitionDigest]
	if !ok {
		return StoredGeneration{}, generationUnavailable("resolve_generation", definitionDigest)
	}
	return cloneStored(stored), nil
}

// Current returns the tenant's most recently published manifest. Exact resume
// must use ResolveGeneration instead.
func (s *MemoryManifestStore) Current(_ context.Context, scope provider.Scope) (StoredGeneration, error) {
	if s == nil || !scope.TenantKey.Valid() {
		return StoredGeneration{}, generationUnavailable("current", "")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	digest := s.current[scope.TenantKey]
	stored, ok := s.values[scope.TenantKey][digest]
	if !ok {
		return StoredGeneration{}, generationUnavailable("current", "")
	}
	return cloneStored(stored), nil
}

func (s *MemoryManifestStore) PinGeneration(_ context.Context, scope provider.Scope, definitionDigest string, pin RetentionPin) error {
	if s == nil || !scope.TenantKey.Valid() || definitionDigest == "" {
		return generationUnavailable("pin_generation", definitionDigest)
	}
	if err := validatePin(pin); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.values[scope.TenantKey][definitionDigest]
	if !ok {
		return generationUnavailable("pin_generation", definitionDigest)
	}
	return createOrVerifyPin(stored.pins, pin)
}

func (s *MemoryManifestStore) ReleasePin(_ context.Context, scope provider.Scope, definitionDigest, pinKey string) error {
	if s == nil || !scope.TenantKey.Valid() || definitionDigest == "" || pinKey == "" {
		return generationUnavailable("release_pin", definitionDigest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.values[scope.TenantKey][definitionDigest]
	if !ok {
		return generationUnavailable("release_pin", definitionDigest)
	}
	delete(stored.pins, pinKey)
	return nil
}

// RetireGeneration removes an unpinned generation only after it is older than
// the application-supplied recovery floor. A zero floor never authorizes GC.
func (s *MemoryManifestStore) RetireGeneration(_ context.Context, scope provider.Scope, definitionDigest string, recoveryFloor time.Time) error {
	if s == nil || !scope.TenantKey.Valid() || definitionDigest == "" {
		return generationUnavailable("retire_generation", definitionDigest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.values[scope.TenantKey][definitionDigest]
	if !ok {
		return generationUnavailable("retire_generation", definitionDigest)
	}
	if len(stored.pins) != 0 {
		return &Error{Code: CodeRetentionPinned, Operation: "retire_generation", Generation: definitionDigest, Cause: ErrRetentionPinned}
	}
	if recoveryFloor.IsZero() || !stored.manifest.CreatedAt.Before(recoveryFloor) {
		return &Error{Code: CodeRetentionPinned, Operation: "retire_generation", Generation: definitionDigest, Cause: errors.Join(ErrRetentionPinned, errors.New("generation has not crossed recovery floor"))}
	}
	delete(s.values[scope.TenantKey], definitionDigest)
	if s.current[scope.TenantKey] == definitionDigest {
		delete(s.current, scope.TenantKey)
	}
	return nil
}

func validatePin(pin RetentionPin) error {
	if pin.PinKey == "" || pin.OwnerKey == "" {
		return conflictError("validate_pin", "", "pin key and owner key are required")
	}
	return nil
}

func createOrVerifyPin(pins map[string]RetentionPin, pin RetentionPin) error {
	if existing, ok := pins[pin.PinKey]; ok && existing != pin {
		return conflictError("pin_generation", "", "pin key has different owner or retention horizon")
	}
	pins[pin.PinKey] = pin
	return nil
}

func sameManifest(left, right ArtifactManifest) bool {
	if left.TenantKey != right.TenantKey || left.ManifestDigest != right.ManifestDigest {
		return false
	}
	leftDigest, leftErr := agent.CanonicalDigest(left.Wire)
	rightDigest, rightErr := agent.CanonicalDigest(right.Wire)
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}

func cloneStored(stored *memoryGeneration) StoredGeneration {
	result := StoredGeneration{Manifest: cloneManifest(stored.manifest), Pins: make([]RetentionPin, 0, len(stored.pins))}
	for _, pin := range stored.pins {
		result.Pins = append(result.Pins, pin)
	}
	sort.Slice(result.Pins, func(i, j int) bool { return result.Pins[i].PinKey < result.Pins[j].PinKey })
	return result
}

func conflictError(operation, generation, detail string) error {
	return &Error{Code: CodeInvariantConflict, Operation: operation, Generation: generation, Cause: fmt.Errorf("%w: %s", ErrInvariantConflict, detail)}
}

func generationUnavailable(operation, generation string) error {
	return &Error{Code: CodeGenerationUnavailable, Operation: operation, Generation: generation, Cause: ErrGenerationUnavailable}
}
