package icoder

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/coordinator"
	"github.com/iceymoss/agent-runtime-go/provider"
)

func newTestRuntime(t *testing.T) (*Store, *RuntimeBuilder) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	catalog := newTestCatalog(t, newTestGate(t, false), workspaceToolEntries(workspace, nil))
	toolSet, err := agent.NewToolSet(catalog.Registry(), catalog.Names())
	if err != nil {
		t.Fatal(err)
	}
	builder, err := NewRuntimeBuilder(runtimeIngredients{
		model: reviewModel{}, modelName: "fixture", contextWindow: 128000, maxTokens: 4096, maxSteps: 20,
		tools: toolSet, toolNames: catalog.Names(), catalog: catalog,
		systemMessages: []agent.Message{agent.NewSystemMessage("system prompt")},
		promptVersion:  "icoder/v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, builder
}

func TestRuntimeGenerationIsStableAndReconstructible(t *testing.T) {
	store, builder := newTestRuntime(t)
	ctx := context.Background()
	resolver, resolved, err := resolveRuntime(ctx, builder, store.ManifestStore())
	if err != nil {
		t.Fatalf("resolveRuntime() error = %v", err)
	}
	defer func() {
		if err := resolver.Close(); err != nil {
			t.Error(err)
		}
	}()
	if resolved.DefinitionDigest == "" || resolved.ManifestDigest == "" || resolved.Definition == nil {
		t.Fatalf("resolved runtime = %+v", resolved)
	}

	// Resolving the same ingredients again must produce the same identity, or a
	// restart would silently become a different runtime generation.
	second, secondResolved, err := resolveRuntime(ctx, builder, store.ManifestStore())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	}()
	if secondResolved.DefinitionDigest != resolved.DefinitionDigest || secondResolved.ManifestDigest != resolved.ManifestDigest {
		t.Fatalf("second resolve = %s/%s, first = %s/%s",
			secondResolved.DefinitionDigest, secondResolved.ManifestDigest, resolved.DefinitionDigest, resolved.ManifestDigest)
	}

	// The recorded generation must rebuild into an executable definition whose
	// digest, prompt, and artifacts all still match what was stored.
	rebuilt, err := resolver.ResolveGeneration(ctx, provider.Scope{TenantKey: tenantKey}, resolved.DefinitionDigest)
	if err != nil {
		t.Fatalf("ResolveGeneration() error = %v", err)
	}
	if rebuilt.DefinitionDigest != resolved.DefinitionDigest || rebuilt.Definition == nil {
		t.Fatalf("reconstructed runtime = %+v", rebuilt)
	}
	if _, err := rebuilt.Definition.NewAgent(); err != nil {
		t.Fatalf("reconstructed definition is not executable: %v", err)
	}
}

func TestRuntimeGenerationChangesWithTheTools(t *testing.T) {
	store, builder := newTestRuntime(t)
	ctx := context.Background()
	resolver, first, err := resolveRuntime(ctx, builder, store.ManifestStore())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resolver.Close(); err != nil {
			t.Error(err)
		}
	}()

	// Removing a tool must change the identity: an approval or a resumed run tied
	// to the old digest must not silently apply to a different tool set.
	narrowed := builder.ingredients
	narrowed.toolNames = narrowed.toolNames[:len(narrowed.toolNames)-1]
	toolSet, err := agent.NewToolSet(narrowed.catalog.Registry(), narrowed.toolNames)
	if err != nil {
		t.Fatal(err)
	}
	narrowed.tools = toolSet
	changed, err := NewRuntimeBuilder(narrowed)
	if err != nil {
		t.Fatal(err)
	}
	secondResolver, second, err := resolveRuntime(ctx, changed, store.ManifestStore())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := secondResolver.Close(); err != nil {
			t.Error(err)
		}
	}()
	if second.DefinitionDigest == first.DefinitionDigest {
		t.Fatal("a different tool set produced the same definition digest")
	}
	manifests, err := store.ManifestStore().ListGenerations(ctx, provider.Scope{TenantKey: tenantKey}, 10)
	if err != nil || len(manifests) != 2 {
		t.Fatalf("ListGenerations() = %d, error %v", len(manifests), err)
	}
}

func TestManifestStoreRefusesADifferentManifestForTheSameDefinition(t *testing.T) {
	store, builder := newTestRuntime(t)
	ctx := context.Background()
	built, err := builder.Build(ctx, coordinator.BuildRequest{
		Selector: coordinator.Selector{AgentKey: recipeKey, TenantKey: tenantKey}, ModelRole: provider.RolePrimary,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifests := store.ManifestStore()
	manifest, err := coordinator.NewArtifactManifest(tenantKey, built.Wire, timeFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := manifests.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, coordinator.RetentionPin{}); err != nil {
		t.Fatal(err)
	}
	if err := manifests.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: manifest}, coordinator.RetentionPin{}); err != nil {
		t.Fatalf("replayed SaveGeneration() error = %v", err)
	}

	// Same definition digest, different contents: the store must refuse rather
	// than let one digest describe two compositions.
	forged := built.Wire
	forged.RecipeVersion = "v2"
	conflicting, err := coordinator.NewArtifactManifest(tenantKey, forged, timeFixture())
	if err != nil {
		t.Fatal(err)
	}
	conflicting.Wire.DefinitionDigest = manifest.Wire.DefinitionDigest
	if err := manifests.SaveGeneration(ctx, coordinator.StoredGeneration{Manifest: conflicting}, coordinator.RetentionPin{}); err == nil {
		t.Fatal("SaveGeneration() accepted two manifests for one definition digest")
	}
	if _, err := manifests.ResolveGeneration(ctx, provider.Scope{TenantKey: "other"}, manifest.Wire.DefinitionDigest); err == nil {
		t.Fatal("ResolveGeneration() leaked a generation across tenants")
	}
}

// timeFixture is a fixed creation time so manifest digests stay comparable
// across runs of this test.
func timeFixture() time.Time { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC) }
