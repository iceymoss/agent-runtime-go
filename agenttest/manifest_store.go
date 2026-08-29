package agenttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/coordinator"
	"github.com/iceymoss/agent-runtime-go/provider"
)

// ManifestStoreFactory returns a fresh, empty generation store for one subtest.
type ManifestStoreFactory func(t *testing.T) coordinator.ManifestStore

// TestManifestStore runs the runtime generation conformance suite.
//
// A manifest is what makes a past run reproducible: it names the model, prompt,
// tools, and policy that run actually executed under. That only holds if one
// definition digest can never describe two compositions, and if a generation
// something still depends on cannot be resolved from the wrong tenant or
// quietly replaced. The suite drives exactly those.
func TestManifestStore(t *testing.T, factory ManifestStoreFactory) {
	t.Helper()
	t.Run("a definition digest describes one composition", func(t *testing.T) {
		testManifestImmutability(t, factory)
	})
	t.Run("resolution is exact and tenant-scoped", func(t *testing.T) {
		testManifestResolution(t, factory)
	})
	t.Run("retention pins are explicit and verified", func(t *testing.T) {
		testManifestPins(t, factory)
	})
}

const manifestTenant = "tenant-1"

func manifestWire(definitionDigest, recipeVersion string) coordinator.ManifestWire {
	return coordinator.ManifestWire{
		SchemaVersion: coordinator.CurrentSchemaVersion, DefinitionDigest: definitionDigest,
		RecipeKey: "conformance.agent", RecipeVersion: recipeVersion, Role: provider.RolePrimary,
		Provider: "conformance-provider", Model: "conformance-model", ModelVersion: "v1",
		CatalogGeneration: "catalog-v1", OptionsVersion: "options-v1",
		Artifacts: []coordinator.ArtifactRef{
			{Kind: "prompt", Key: "prompt-1", Generation: "v1", Digest: "sha256:prompt", SchemaVersion: 1},
		},
	}
}

func manifestFixture(t *testing.T, definitionDigest, recipeVersion string) coordinator.ArtifactManifest {
	t.Helper()
	manifest, err := coordinator.NewArtifactManifest(manifestTenant, manifestWire(definitionDigest, recipeVersion), manifestCreatedAt())
	if err != nil {
		t.Fatalf("NewArtifactManifest() error = %v", err)
	}
	return manifest
}

func manifestCreatedAt() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }

func testManifestImmutability(t *testing.T, factory ManifestStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	manifest := manifestFixture(t, "sha256:definition-1", "v1")

	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, coordinator.RetentionPin{}); err != nil {
		t.Fatalf("SaveGeneration() error = %v", err)
	}
	// Re-publishing the identical generation is what a restart does, so it must
	// be accepted rather than treated as a collision.
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, coordinator.RetentionPin{}); err != nil {
		t.Fatalf("replayed SaveGeneration() error = %v", err)
	}

	// A definition digest is the identity a resumed run is pinned to. Letting it
	// describe a second composition would make "reproduce this run" meaningless.
	forged := manifestFixture(t, "sha256:definition-1", "v2")
	forged.Wire.DefinitionDigest = manifest.Wire.DefinitionDigest
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: forged}, coordinator.RetentionPin{}); err == nil {
		t.Fatal("SaveGeneration() accepted two compositions under one definition digest")
	}

	invalid := manifest
	invalid.ManifestDigest = "sha256:tampered"
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: invalid}, coordinator.RetentionPin{}); !errors.Is(err, coordinator.ErrManifestInvalid) {
		t.Fatalf("SaveGeneration() with a tampered digest error = %v, want coordinator.ErrManifestInvalid", err)
	}
}

func testManifestResolution(t *testing.T, factory ManifestStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	manifest := manifestFixture(t, "sha256:definition-1", "v1")
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, coordinator.RetentionPin{}); err != nil {
		t.Fatalf("SaveGeneration() error = %v", err)
	}
	second := manifestFixture(t, "sha256:definition-2", "v2")
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: second}, coordinator.RetentionPin{}); err != nil {
		t.Fatalf("SaveGeneration() error = %v", err)
	}

	stored, err := store.ResolveGeneration(ctx, provider.Scope{TenantKey: manifestTenant}, manifest.Wire.DefinitionDigest)
	if err != nil || stored.Manifest.ManifestDigest != manifest.ManifestDigest || stored.Manifest.Wire.RecipeVersion != "v1" {
		t.Fatalf("ResolveGeneration() = %+v, error %v", stored.Manifest.Wire, err)
	}

	// Falling back to the newest generation, or to another tenant's, is how a
	// resumed run silently executes under a composition it never started with.
	tests := []struct {
		name   string
		scope  provider.Scope
		digest string
	}{
		{name: "an unknown digest", scope: provider.Scope{TenantKey: manifestTenant}, digest: "sha256:absent"},
		{name: "another tenant", scope: provider.Scope{TenantKey: "tenant-2"}, digest: manifest.Wire.DefinitionDigest},
		{name: "an empty digest", scope: provider.Scope{TenantKey: manifestTenant}, digest: ""},
		{name: "an empty tenant", scope: provider.Scope{}, digest: manifest.Wire.DefinitionDigest},
	}
	for _, test := range tests {
		t.Run(test.name+" resolves nothing", func(t *testing.T) {
			if _, err := store.ResolveGeneration(ctx, test.scope, test.digest); !errors.Is(err, coordinator.ErrGenerationUnavailable) {
				t.Fatalf("ResolveGeneration() error = %v, want coordinator.ErrGenerationUnavailable", err)
			}
		})
	}
}

func testManifestPins(t *testing.T, factory ManifestStoreFactory) {
	store := factory(t)
	ctx := context.Background()
	manifest := manifestFixture(t, "sha256:definition-1", "v1")
	scope := provider.Scope{TenantKey: manifestTenant}

	// Pins are supplied through the pin argument. Accepting them inside the
	// stored value too would give one save two places to disagree about what is
	// retained.
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{
		Manifest: manifest, Pins: []coordinator.RetentionPin{{PinKey: "pin-1", OwnerKey: "owner-1"}},
	}, coordinator.RetentionPin{}); err == nil {
		t.Fatal("SaveGeneration() accepted pins inside the stored generation")
	}
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, coordinator.RetentionPin{OwnerKey: "owner-1"}); err == nil {
		t.Fatal("SaveGeneration() accepted a pin with no key")
	}
	if err := store.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, coordinator.RetentionPin{PinKey: "pin-1", OwnerKey: "owner-1"}); err != nil {
		t.Fatalf("SaveGeneration() with a pin error = %v", err)
	}

	stored, err := store.ResolveGeneration(ctx, scope, manifest.Wire.DefinitionDigest)
	if err != nil || len(stored.Pins) != 1 || stored.Pins[0].PinKey != "pin-1" || stored.Pins[0].OwnerKey != "owner-1" {
		t.Fatalf("ResolveGeneration() pins = %+v, error %v", stored.Pins, err)
	}

	// A pin names who is retaining the generation, so an owner is required and a
	// second owner may not silently take over an existing pin key.
	if err := store.PinGeneration(ctx, scope, manifest.Wire.DefinitionDigest, coordinator.RetentionPin{PinKey: "pin-2"}); err == nil {
		t.Fatal("PinGeneration() accepted a pin with no owner")
	}
	if err := store.PinGeneration(ctx, scope, manifest.Wire.DefinitionDigest, coordinator.RetentionPin{PinKey: "pin-1", OwnerKey: "owner-2"}); err == nil {
		t.Fatal("PinGeneration() let a second owner take over an existing pin key")
	}
	if err := store.PinGeneration(ctx, scope, manifest.Wire.DefinitionDigest, coordinator.RetentionPin{PinKey: "pin-1", OwnerKey: "owner-1"}); err != nil {
		t.Fatalf("repeated PinGeneration() error = %v", err)
	}
	if err := store.PinGeneration(ctx, scope, "sha256:absent", coordinator.RetentionPin{PinKey: "pin-3", OwnerKey: "owner-1"}); !errors.Is(err, coordinator.ErrGenerationUnavailable) {
		t.Fatalf("PinGeneration() on an unknown generation error = %v, want coordinator.ErrGenerationUnavailable", err)
	}

	if err := store.ReleasePin(ctx, scope, manifest.Wire.DefinitionDigest, "pin-1"); err != nil {
		t.Fatalf("ReleasePin() error = %v", err)
	}
	released, err := store.ResolveGeneration(ctx, scope, manifest.Wire.DefinitionDigest)
	if err != nil || len(released.Pins) != 0 {
		t.Fatalf("ResolveGeneration() after release = %+v, error %v", released.Pins, err)
	}
	// Releasing a pin that is not there is the same end state, so a retried
	// release must not fail.
	if err := store.ReleasePin(ctx, scope, manifest.Wire.DefinitionDigest, "pin-1"); err != nil {
		t.Fatalf("repeated ReleasePin() error = %v", err)
	}
	if err := store.ReleasePin(ctx, scope, "sha256:absent", "pin-1"); !errors.Is(err, coordinator.ErrGenerationUnavailable) {
		t.Fatalf("ReleasePin() on an unknown generation error = %v, want coordinator.ErrGenerationUnavailable", err)
	}
}
