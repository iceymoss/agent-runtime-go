package coordinator_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/coordinator"
	"github.com/iceymoss/agent-runtime-go/provider"
	"github.com/iceymoss/agent-runtime-go/session"
)

type model struct{}

func (model) Name() string                     { return "test" }
func (model) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (model) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	channel := make(chan agent.StreamChunk)
	close(channel)
	return channel, nil
}

func TestArtifactManifestWireRoundTripAndExecutableNeverSerialized(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	definition := newDefinition(t, "recipe", "model-v1")
	wire := testWire(definition, "catalog-1", "recipe-v1")
	wire.Artifacts = []coordinator.ArtifactRef{
		{Kind: "tool", Key: "z", Generation: "1", Digest: "z-digest", SchemaVersion: 1},
		{Kind: "prompt", Key: "a", Generation: "1", Digest: "a-digest", SchemaVersion: 1},
	}
	manifest, err := coordinator.NewArtifactManifest("tenant", wire, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "RuntimeDefinition") || strings.Contains(string(encoded), "Definition") || strings.Contains(string(encoded), "Stream") {
		t.Fatalf("manifest serialized executable data: %s", encoded)
	}
	var decoded coordinator.ArtifactManifest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.ValidateArtifactManifest(decoded); err != nil {
		t.Fatalf("round-trip manifest invalid: %v", err)
	}
	if decoded.ManifestDigest != manifest.ManifestDigest || decoded.Wire.Artifacts[0].Kind != "prompt" {
		t.Fatalf("round-trip/canonical manifest mismatch: %#v", decoded)
	}

	built := coordinator.BuildResult{Definition: definition, SystemMessages: wire.PromptMessages, Wire: wire}
	builtJSON, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(builtJSON), "model-v1") && !strings.Contains(string(builtJSON), "model_version") {
		t.Fatalf("unexpected executable serialization: %s", builtJSON)
	}
}

func TestArtifactManifestDeepCopiesImagePrompt(t *testing.T) {
	definition := newDefinition(t, "image-recipe", "model-v1")
	wire := testWire(definition, "catalog-1", "recipe-v1")
	data := []byte{1, 2, 3}
	wire.PromptMessages = []agent.Message{{Role: agent.RoleUser, Parts: []agent.ContentPart{{Type: agent.PartImage, Image: &agent.ImageContent{MediaType: "image/png", Data: data}}}}}
	manifest, err := coordinator.NewArtifactManifest("tenant", wire, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	data[0] = 9
	if got := manifest.Wire.PromptMessages[0].Parts[0].Image.Data; !reflect.DeepEqual(got, []byte{1, 2, 3}) {
		t.Fatalf("manifest construction aliased prompt: %v", got)
	}
}

func TestMemoryManifestStoreExactTenantIdempotencyPinsAndRecoveryFloor(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	definition := newDefinition(t, "recipe", "model-v1")
	manifest, err := coordinator.NewArtifactManifest("tenant-a", testWire(definition, "catalog-1", "recipe-v1"), now)
	if err != nil {
		t.Fatal(err)
	}
	store := coordinator.NewMemoryManifestStore()
	pin := coordinator.RetentionPin{PinKey: "run-1", OwnerKey: "run-1", RetainUntil: now.Add(time.Hour)}
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, pin); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, pin); err != nil {
		t.Fatalf("idempotent save failed: %v", err)
	}
	if _, err := store.ResolveGeneration(ctx, provider.Scope{TenantKey: "tenant-b"}, manifest.Wire.DefinitionDigest); !errors.Is(err, coordinator.ErrGenerationUnavailable) {
		t.Fatalf("cross-tenant error = %v", err)
	}
	resolved, err := store.ResolveGeneration(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest)
	if err != nil {
		t.Fatal(err)
	}
	resolved.Manifest.Wire.RecipeVersion = "mutated"
	again, err := store.ResolveGeneration(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest)
	if err != nil || again.Manifest.Wire.RecipeVersion != "recipe-v1" {
		t.Fatalf("store leaked mutable data: %#v, %v", again, err)
	}
	conflict := manifest
	conflict.ManifestDigest = "different"
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: conflict}, coordinator.RetentionPin{}); !errors.Is(err, coordinator.ErrManifestInvalid) {
		t.Fatalf("tampered manifest error = %v", err)
	}
	if err := store.PinGeneration(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest, coordinator.RetentionPin{PinKey: "run-1", OwnerKey: "other"}); !errors.Is(err, coordinator.ErrInvariantConflict) {
		t.Fatalf("pin conflict error = %v", err)
	}
	if err := store.RetireGeneration(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest, now.Add(time.Minute)); !errors.Is(err, coordinator.ErrRetentionPinned) {
		t.Fatalf("pinned retire error = %v", err)
	}
	if err := store.ReleasePin(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest, pin.PinKey); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleasePin(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest, pin.PinKey); err != nil {
		t.Fatalf("release is not idempotent: %v", err)
	}
	if err := store.RetireGeneration(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest, time.Time{}); !errors.Is(err, coordinator.ErrRetentionPinned) {
		t.Fatalf("zero-floor retire error = %v", err)
	}
	if err := store.RetireGeneration(ctx, provider.Scope{TenantKey: "tenant-a"}, manifest.Wire.DefinitionDigest, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorResolveOldGenerationTamperAndConcurrentCache(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	store := coordinator.NewMemoryManifestStore()
	definition1 := newDefinition(t, "recipe", "model-v1")
	definition2 := newDefinition(t, "recipe", "model-v2")
	builds := []coordinator.BuildResult{
		{Definition: definition1, SystemMessages: []agent.Message{agent.NewSystemMessage("one")}, Wire: testWireWithPrompt(definition1, "catalog-1", "recipe-v1", "one")},
		{Definition: definition2, SystemMessages: []agent.Message{agent.NewSystemMessage("two")}, Wire: testWireWithPrompt(definition2, "catalog-2", "recipe-v2", "two")},
	}
	builder := &sequenceBuilder{values: builds}
	reconstructor := &countingReconstructor{definitions: map[string]*agent.RuntimeDefinition{definition1.ArtifactVersions().Definition: definition1, definition2.ArtifactVersions().Definition: definition2}}
	resolver, err := coordinator.New(coordinator.Options{Builder: builder, Reconstructor: reconstructor, Artifacts: store, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	request := coordinator.ResolveRequest{Selector: coordinator.Selector{TenantKey: "tenant", AgentKey: "agent"}}
	first, err := resolver.Resolve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := resolver.Resolve(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.DefinitionDigest == second.DefinitionDigest {
		t.Fatal("test definitions unexpectedly share a digest")
	}
	old, err := resolver.ResolveGeneration(ctx, provider.Scope{TenantKey: "tenant"}, first.DefinitionDigest)
	if err != nil || old.DefinitionDigest != first.DefinitionDigest || old.CatalogGeneration != "catalog-1" {
		t.Fatalf("old exact generation = %#v, %v", old, err)
	}

	// A fresh coordinator exercises reconstruction singleflight rather than the
	// cache populated during Build.
	freshReconstructor := &countingReconstructor{definitions: reconstructor.definitions, delay: 10 * time.Millisecond}
	fresh, err := coordinator.New(coordinator.Options{Builder: builder, Reconstructor: freshReconstructor, Artifacts: store})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var wait sync.WaitGroup
	errorsSeen := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			resolved, err := fresh.ResolveGeneration(ctx, provider.Scope{TenantKey: "tenant"}, first.DefinitionDigest)
			if err != nil {
				errorsSeen <- err
				return
			}
			resolved.Artifacts = append(resolved.Artifacts, coordinator.ArtifactRef{Kind: "caller"})
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
	if freshReconstructor.calls.Load() != 1 {
		t.Fatalf("reconstruction calls = %d, want 1", freshReconstructor.calls.Load())
	}

	tamperedStore := tamperStore{ManifestStore: store}
	tampered, err := coordinator.New(coordinator.Options{Builder: builder, Reconstructor: reconstructor, Artifacts: tamperedStore})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tampered.ResolveGeneration(ctx, provider.Scope{TenantKey: "tenant"}, first.DefinitionDigest); !errors.Is(err, coordinator.ErrGenerationUnavailable) {
		t.Fatalf("tamper error = %v", err)
	}
}

func TestSessionDefinitionResolverAdapterConformance(t *testing.T) {
	var _ session.DefinitionResolver = sessionAdapter{}
}

type sessionAdapter struct{ resolver *coordinator.Coordinator }

func (a sessionAdapter) Resolve(ctx context.Context, request session.DefinitionRequest) (session.ResolvedExecution, error) {
	values := make([]coordinator.SelectorValue, len(request.Selectors))
	for i, value := range request.Selectors {
		values[i] = coordinator.SelectorValue{Key: value.Key, Value: value.Value}
	}
	resolved, err := a.resolver.Resolve(ctx, coordinator.ResolveRequest{Selector: coordinator.Selector{TenantKey: request.TenantKey, AgentKey: request.AgentKey, Values: values}})
	return sessionExecution(resolved), err
}

func (a sessionAdapter) ResolveGeneration(ctx context.Context, tenant agent.TenantKey, digest string) (session.ResolvedExecution, error) {
	resolved, err := a.resolver.ResolveGeneration(ctx, provider.Scope{TenantKey: tenant}, digest)
	return sessionExecution(resolved), err
}

func sessionExecution(resolved coordinator.ResolvedRuntime) session.ResolvedExecution {
	refs := make([]session.ArtifactRef, len(resolved.Artifacts))
	for i, ref := range resolved.Artifacts {
		refs[i] = session.ArtifactRef{Kind: ref.Kind, Key: ref.Key, Generation: ref.Generation, Digest: ref.Digest, SchemaVersion: ref.SchemaVersion}
	}
	return session.ResolvedExecution{Definition: resolved.Definition, DefinitionDigest: resolved.DefinitionDigest, SchemaVersion: resolved.SchemaVersion, Artifacts: refs}
}

type sequenceBuilder struct {
	mu     sync.Mutex
	values []coordinator.BuildResult
	next   int
}

func (b *sequenceBuilder) Build(_ context.Context, _ coordinator.BuildRequest) (coordinator.BuildResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.values[b.next]
	if b.next+1 < len(b.values) {
		b.next++
	}
	return value, nil
}

type countingReconstructor struct {
	calls       atomic.Int32
	delay       time.Duration
	definitions map[string]*agent.RuntimeDefinition
}

func (r *countingReconstructor) Reconstruct(_ context.Context, scope provider.Scope, manifest coordinator.ArtifactManifest) (coordinator.Reconstruction, error) {
	r.calls.Add(1)
	if scope.TenantKey != manifest.TenantKey {
		return coordinator.Reconstruction{}, errors.New("tenant mismatch")
	}
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	definition := r.definitions[manifest.Wire.DefinitionDigest]
	if definition == nil {
		return coordinator.Reconstruction{}, errors.New("definition missing")
	}
	return coordinator.Reconstruction{Definition: definition, SystemMessages: manifest.Wire.PromptMessages, Artifacts: manifest.Wire.Artifacts}, nil
}

type tamperStore struct{ coordinator.ManifestStore }

func (s tamperStore) ResolveGeneration(ctx context.Context, scope provider.Scope, digest string) (coordinator.StoredGeneration, error) {
	stored, err := s.ManifestStore.ResolveGeneration(ctx, scope, digest)
	if err == nil {
		stored.Manifest.Wire.RecipeVersion = "tampered"
	}
	return stored, err
}

func newDefinition(t *testing.T, key, modelVersion string) *agent.RuntimeDefinition {
	t.Helper()
	registry := agent.NewRegistry()
	tools, err := agent.NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := agent.NewRuntimeDefinition(agent.RuntimeDefinitionSpec{
		Key: key, Model: agent.ModelMetadata{Name: "test", Version: modelVersion, ContextWindow: 4096},
		Execution: agent.ExecutionSettings{MaxSteps: 5}, PromptVersion: "prompt-v1", PolicyVersion: "policy-v1",
	}, model{}, tools)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}

func testWire(definition *agent.RuntimeDefinition, catalog, recipe string) coordinator.ManifestWire {
	return testWireWithPrompt(definition, catalog, recipe, "system")
}

func testWireWithPrompt(definition *agent.RuntimeDefinition, catalog, recipe, prompt string) coordinator.ManifestWire {
	return coordinator.ManifestWire{
		SchemaVersion: coordinator.CurrentSchemaVersion, DefinitionDigest: definition.ArtifactVersions().Definition,
		RecipeKey: definition.Key(), RecipeVersion: recipe, Role: provider.RolePrimary,
		Provider: "provider", Model: "model", ModelVersion: definition.ModelMetadata().Version,
		CatalogGeneration: catalog, PromptMessages: []agent.Message{agent.NewSystemMessage(prompt)},
		Execution: definition.ExecutionSettings(), OptionsVersion: "options-v1",
		Artifacts: []coordinator.ArtifactRef{{Kind: "prompt", Key: "system", Generation: recipe, Digest: "prompt-digest-" + recipe, SchemaVersion: 1}},
	}
}
