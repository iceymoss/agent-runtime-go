package skills_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/skills"
)

type mutableSource struct {
	mu       sync.RWMutex
	name     string
	kind     skills.SourceKind
	snapshot skills.SourceSnapshot
	err      error
	loads    int
}

func (s *mutableSource) Name() string            { return s.name }
func (s *mutableSource) Kind() skills.SourceKind { return s.kind }
func (s *mutableSource) Load(_ context.Context, _ skills.Scope) (skills.SourceSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.err != nil {
		return skills.SourceSnapshot{}, s.err
	}
	return cloneSourceSnapshot(s.snapshot), nil
}
func (s *mutableSource) replace(snapshot skills.SourceSnapshot, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapshot, s.err = snapshot, err
}
func (s *mutableSource) loadCount() int { s.mu.RLock(); defer s.mu.RUnlock(); return s.loads }

func TestCatalogPrecedenceRequiresExplicitReplacementAndTombstoneWins(t *testing.T) {
	platform := &mutableSource{name: "platform", kind: skills.SourcePlatformFilesystem, snapshot: skills.SourceSnapshot{Generation: "p1", Skills: []skills.SourceSkill{testSkill("coach", "1.0.0", skills.TrustPlatform, "platform")}}}
	tenant := &mutableSource{name: "tenant-db", kind: skills.SourceTenantDB, snapshot: skills.SourceSnapshot{Generation: "d1", Skills: []skills.SourceSkill{testSkill("coach", "2.0.0", skills.TrustTenantReviewed, "tenant")}}}
	catalog := newCatalog(t, platform, tenant)
	scope := skills.Scope{TenantKey: agent.TenantKey("tenant-a")}
	if err := catalog.StartScope(context.Background(), scope); err != nil {
		t.Fatalf("StartScope: %v", err)
	}
	snapshot, ok := catalog.Current(scope)
	if !ok {
		t.Fatal("catalog not ready")
	}
	if _, found := snapshot.Descriptor("coach"); found {
		t.Fatal("undeclared override must make the skill unavailable")
	}
	if !hasDiagnostic(snapshot, skills.CodePrecedenceConflict) {
		t.Fatalf("diagnostics = %#v", snapshot.Diagnostics)
	}

	override := testSkill("coach", "2.0.0", skills.TrustTenantReviewed, "tenant")
	override.Descriptor.Replaces = &skills.Replacement{Key: "coach", VersionRange: "=1.0.0"}
	tenant.replace(skills.SourceSnapshot{Generation: "d2", Skills: []skills.SourceSkill{override}}, nil)
	snapshot, err := catalog.Refresh(context.Background(), scope)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	descriptor, found := snapshot.Descriptor("coach")
	if !found || descriptor.Version != "2.0.0" {
		t.Fatalf("descriptor = %#v, found=%v", descriptor, found)
	}

	tenant.replace(skills.SourceSnapshot{Generation: "d3", Tombstones: []skills.Tombstone{{Key: "coach", Reason: "disabled"}}}, nil)
	snapshot, err = catalog.Refresh(context.Background(), scope)
	if err != nil {
		t.Fatalf("Refresh tombstone: %v", err)
	}
	if _, found = snapshot.Descriptor("coach"); found {
		t.Fatal("tombstone must mask lower-precedence skill")
	}
}

func TestCatalogIsLazyAndTenantScoped(t *testing.T) {
	repository := skills.NewMemoryRepository()
	if err := repository.Replace("tenant-a", skills.RepositorySnapshot{Generation: "a1", Skills: []skills.SourceSkill{testSkill("alpha", "1.0.0", skills.TrustTenantReviewed, "A")}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.Replace("tenant-b", skills.RepositorySnapshot{Generation: "b1", Skills: []skills.SourceSkill{testSkill("beta", "1.0.0", skills.TrustTenantReviewed, "B")}}); err != nil {
		t.Fatal(err)
	}
	source := skills.NewRepositorySource("db", repository, skills.DefaultLimits())
	catalog, err := skills.NewCatalog(skills.Options{Sources: []skills.SourceRegistration{{Source: source, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Current(skills.Scope{TenantKey: "tenant-a"}); ok {
		t.Fatal("construction must not enumerate or load tenants")
	}
	if err := catalog.StartScope(context.Background(), skills.Scope{TenantKey: "tenant-a"}); err != nil {
		t.Fatal(err)
	}
	a, _ := catalog.Current(skills.Scope{TenantKey: "tenant-a"})
	if _, ok := a.Descriptor("alpha"); !ok {
		t.Fatal("tenant A skill missing")
	}
	if _, ok := a.Descriptor("beta"); ok {
		t.Fatal("tenant B skill leaked")
	}
	if _, ok := catalog.Current(skills.Scope{TenantKey: "tenant-b"}); ok {
		t.Fatal("tenant B was implicitly started")
	}
	if _, err := catalog.Resolve(skills.ResolveRequest{Scope: skills.Scope{TenantKey: "tenant-a"}, Generation: a.Generation, Selectors: []skills.Selector{{Key: "beta"}}}); !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("wrong-tenant resolve error = %v", err)
	}
}

func TestDisabledDBRecordDoesNotMaskPlatformSkill(t *testing.T) {
	platform := &mutableSource{name: "platform", kind: skills.SourcePlatformFilesystem, snapshot: skills.SourceSnapshot{Generation: "p1", Skills: []skills.SourceSkill{testSkill("coach", "1.0.0", skills.TrustPlatform, "platform")}}}
	disabled := testSkill("coach", "2.0.0", skills.TrustTenantReviewed, "disabled")
	disabled.Disabled = true
	tenant := &mutableSource{name: "tenant-db", kind: skills.SourceTenantDB, snapshot: skills.SourceSnapshot{Generation: "d1", Skills: []skills.SourceSkill{disabled}}}
	catalog := newCatalog(t, platform, tenant)
	scope := skills.Scope{TenantKey: "tenant-a"}
	if err := catalog.StartScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := catalog.Current(scope)
	descriptor, found := snapshot.Descriptor("coach")
	if !found || descriptor.Version != "1.0.0" {
		t.Fatalf("disabled record masked platform descriptor: %#v", descriptor)
	}
}

func TestRefreshRetainsLeasedGenerationAndFailureRetainsCurrent(t *testing.T) {
	source := &mutableSource{name: "platform", kind: skills.SourcePlatformFilesystem, snapshot: skills.SourceSnapshot{Generation: "one", Skills: []skills.SourceSkill{testSkill("coach", "1.0.0", skills.TrustPlatform, "one")}}}
	catalog := newCatalog(t, source)
	scope := skills.Scope{TenantKey: "tenant-a"}
	if err := catalog.StartScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	first, _ := catalog.Current(scope)
	lease, err := catalog.Acquire(scope, first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	source.replace(skills.SourceSnapshot{Generation: "two", Skills: []skills.SourceSkill{testSkill("coach", "2.0.0", skills.TrustPlatform, "two")}}, nil)
	second, err := catalog.Refresh(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if second.Generation == first.Generation {
		t.Fatal("generation did not change")
	}
	old, err := catalog.Read(context.Background(), skills.ReadRequest{Scope: scope, Generation: first.Generation, Skill: "coach", Version: "1.0.0"})
	if err != nil || string(old.Content) != "one" {
		t.Fatalf("old read = %q, %v", old.Content, err)
	}
	if _, err = catalog.Acquire(scope, first.Generation); !errors.Is(err, skills.ErrGenerationUnavailable) {
		t.Fatalf("retired generation adoption error = %v", err)
	}
	lease.Release()
	if _, err = catalog.Read(context.Background(), skills.ReadRequest{Scope: scope, Generation: first.Generation, Skill: "coach", Version: "1.0.0"}); !errors.Is(err, skills.ErrGenerationUnavailable) {
		t.Fatalf("released generation read error = %v", err)
	}

	source.replace(skills.SourceSnapshot{}, errors.New("database unavailable"))
	if _, err = catalog.Refresh(context.Background(), scope); !errors.Is(err, skills.ErrInvalidSource) {
		t.Fatalf("refresh failure = %v", err)
	}
	current, ok := catalog.Current(scope)
	if !ok || current.Generation != second.Generation {
		t.Fatalf("current changed after failure: %#v", current)
	}
	if !catalog.Status(scope).Degraded {
		t.Fatal("failed refresh must mark scope degraded")
	}
}

func TestCatalogDigestAndCopiesAreDeterministic(t *testing.T) {
	clock := func() time.Time { return time.Unix(123, 0) }
	one := testSkill("one", "1.0.0", skills.TrustPlatform, "one")
	two := testSkill("two", "1.0.0", skills.TrustPlatform, "two")
	source := &mutableSource{name: "platform", kind: skills.SourcePlatformFilesystem, snapshot: skills.SourceSnapshot{Generation: "same", Skills: []skills.SourceSkill{two, one}}}
	catalog, err := skills.NewCatalog(skills.Options{Clock: clock, Sources: []skills.SourceRegistration{{Source: source, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := skills.Scope{TenantKey: "tenant-a"}
	if err := catalog.StartScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	first, _ := catalog.Current(scope)
	source.replace(skills.SourceSnapshot{Generation: "same", Skills: []skills.SourceSkill{one, two}}, nil)
	second, err := catalog.Refresh(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if first.Generation != second.Generation || first.Digest != second.Digest {
		t.Fatalf("digest changed with source order: %s != %s", first.Digest, second.Digest)
	}
	first.Descriptors[0].Metadata["mutated"] = "yes"
	again, _ := catalog.Current(scope)
	if _, exists := again.Descriptors[0].Metadata["mutated"]; exists {
		t.Fatal("snapshot mutation crossed deep-copy boundary")
	}
}

func TestConcurrentAcquireResolveReadAndRefresh(t *testing.T) {
	source := &mutableSource{name: "platform", kind: skills.SourcePlatformFilesystem, snapshot: skills.SourceSnapshot{Generation: "0", Skills: []skills.SourceSkill{testSkill("coach", "1.0.0", skills.TrustPlatform, "0")}}}
	catalog := newCatalog(t, source)
	scope := skills.Scope{TenantKey: "tenant-a"}
	if err := catalog.StartScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 100; iteration++ {
				snapshot, ok := catalog.Current(scope)
				if !ok {
					continue
				}
				lease, err := catalog.Acquire(scope, snapshot.Generation)
				if err != nil {
					continue
				}
				_, _ = catalog.Resolve(skills.ResolveRequest{Scope: scope, Generation: lease.Generation(), Selectors: []skills.Selector{{Key: "coach"}}})
				_, _ = catalog.Read(context.Background(), skills.ReadRequest{Scope: scope, Generation: lease.Generation(), Skill: "coach", Version: "1.0.0"})
				lease.Release()
			}
		}()
	}
	for generation := 1; generation <= 20; generation++ {
		source.replace(skills.SourceSnapshot{Generation: fmt.Sprint(generation), Skills: []skills.SourceSkill{testSkill("coach", "1.0.0", skills.TrustPlatform, fmt.Sprint(generation))}}, nil)
		if _, err := catalog.Refresh(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
	}
	workers.Wait()
}

func testSkill(key string, version skills.Version, trust skills.TrustLevel, instructions string) skills.SourceSkill {
	return skills.SourceSkill{Descriptor: skills.Descriptor{Name: key, Version: version, SchemaVersion: skills.CurrentSchemaVersion, Description: "description", Trust: trust, Metadata: map[string]string{"stable": "true"}}, Instructions: []byte(instructions)}
}
func newCatalog(t *testing.T, sources ...skills.Source) *skills.Manager {
	t.Helper()
	registrations := make([]skills.SourceRegistration, len(sources))
	for i, source := range sources {
		registrations[i] = skills.SourceRegistration{Source: source, Required: true}
	}
	catalog, err := skills.NewCatalog(skills.Options{Sources: registrations})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}
func cloneSourceSnapshot(value skills.SourceSnapshot) skills.SourceSnapshot {
	result := skills.SourceSnapshot{Generation: value.Generation, Tombstones: append([]skills.Tombstone(nil), value.Tombstones...), Diagnostics: append([]skills.Diagnostic(nil), value.Diagnostics...)}
	result.Skills = make([]skills.SourceSkill, len(value.Skills))
	for i, skill := range value.Skills {
		result.Skills[i] = skills.SourceSkill{Descriptor: skill.Descriptor, Instructions: append([]byte(nil), skill.Instructions...), Artifacts: make(map[string][]byte, len(skill.Artifacts)), Disabled: skill.Disabled}
		result.Skills[i].Descriptor.Metadata = map[string]string{}
		for key, item := range skill.Descriptor.Metadata {
			result.Skills[i].Descriptor.Metadata[key] = item
		}
		if skill.Descriptor.Replaces != nil {
			replacement := *skill.Descriptor.Replaces
			result.Skills[i].Descriptor.Replaces = &replacement
		}
		for key, body := range skill.Artifacts {
			result.Skills[i].Artifacts[key] = append([]byte(nil), body...)
		}
	}
	return result
}
func hasDiagnostic(snapshot skills.Snapshot, code skills.Code) bool {
	for _, item := range snapshot.Diagnostics {
		if item.Code == code {
			return true
		}
	}
	return false
}
