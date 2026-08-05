package provider_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/provider"
)

func TestCatalogSnapshotCanonicalImmutableAndExactGenerations(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	maxTokens := 256
	pricing := &provider.Pricing{InputPerMillion: "1.25", Currency: "USD", Version: "prices-1"}
	input := []provider.ProviderDescriptor{
		{ID: "zeta", Version: "p2", Models: []provider.ModelDescriptor{descriptor("zeta", "small", "m2")}},
		{ID: "alpha", Version: "p1", Models: []provider.ModelDescriptor{
			descriptor("alpha", "z-model", "m2"),
			{Ref: provider.ModelRef{Provider: "alpha", Model: "a-model"}, Version: "m1", ContextWindow: 4096, DefaultMaxTokens: 512, DefaultOptions: agent.GenerationOptions{MaxTokens: &maxTokens}, Pricing: pricing},
		}},
	}
	snapshot, err := provider.NewCatalogSnapshot("generation-1", now, input)
	if err != nil {
		t.Fatal(err)
	}
	input[0].Models[0].Version = "mutated"
	maxTokens = 999
	pricing.Currency = "MUTATED"

	providers := snapshot.ProvidersList()
	if providers[0].ID != "alpha" || providers[0].Models[0].Ref.Model != "a-model" {
		t.Fatalf("catalog is not canonical: %#v", providers)
	}
	if *providers[0].Models[0].DefaultOptions.MaxTokens != 256 || providers[0].Models[0].Pricing.Currency != "USD" {
		t.Fatalf("constructor retained caller-owned values: %#v", providers[0].Models[0])
	}
	providers[0].Models[0].DefaultOptions.MaxTokens = &maxTokens
	providers[0].Models[0].Pricing.Currency = "changed"
	model, ok := snapshot.Model(provider.ModelRef{Provider: "alpha", Model: "a-model"})
	if !ok || *model.DefaultOptions.MaxTokens != 256 || model.Pricing.Currency != "USD" {
		t.Fatalf("read escaped mutable catalog state: %#v, %v", model, ok)
	}

	store := provider.NewMemoryCatalog()
	scope := provider.Scope{TenantKey: "tenant-a"}
	if err := store.Publish(context.Background(), scope, snapshot); err != nil {
		t.Fatal(err)
	}
	second, err := provider.NewCatalogSnapshot("generation-2", now.Add(time.Minute), []provider.ProviderDescriptor{{ID: "alpha", Version: "p2", Models: []provider.ModelDescriptor{descriptor("alpha", "new-model", "m3")}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(context.Background(), scope, second); err != nil {
		t.Fatal(err)
	}
	current, err := store.Current(context.Background(), scope)
	if err != nil || current.Generation != "generation-2" {
		t.Fatalf("current = %#v, %v", current, err)
	}
	old, err := store.Get(context.Background(), scope, "generation-1")
	if err != nil || old.Generation != "generation-1" {
		t.Fatalf("old generation = %#v, %v", old, err)
	}
	if _, ok := old.Model(provider.ModelRef{Provider: "alpha", Model: "a-model"}); !ok {
		t.Fatal("old generation changed after publishing current")
	}
	if _, err := store.Get(context.Background(), provider.Scope{TenantKey: "tenant-b"}, "generation-1"); !errors.Is(err, provider.ErrCatalogUnavailable) {
		t.Fatalf("cross-tenant get error = %v", err)
	}
}

func TestCatalogValidationAndCreateOrVerifyConflict(t *testing.T) {
	now := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		providers []provider.ProviderDescriptor
	}{
		{name: "empty catalog"},
		{name: "duplicate provider", providers: []provider.ProviderDescriptor{
			{ID: "p", Version: "1", Models: []provider.ModelDescriptor{descriptor("p", "m", "1")}},
			{ID: "p", Version: "2", Models: []provider.ModelDescriptor{descriptor("p", "n", "1")}},
		}},
		{name: "duplicate model", providers: []provider.ProviderDescriptor{
			{ID: "p", Version: "1", Models: []provider.ModelDescriptor{descriptor("p", "m", "1"), descriptor("p", "m", "2")}},
		}},
		{name: "invalid context", providers: []provider.ProviderDescriptor{
			{ID: "p", Version: "1", Models: []provider.ModelDescriptor{{Ref: provider.ModelRef{Provider: "p", Model: "m"}, Version: "1", ContextWindow: 0, DefaultMaxTokens: 1}}},
		}},
		{name: "capability contradiction", providers: []provider.ProviderDescriptor{
			{ID: "p", Version: "1", Models: []provider.ModelDescriptor{{Ref: provider.ModelRef{Provider: "p", Model: "m"}, Version: "1", ContextWindow: 10, DefaultMaxTokens: 1, Capabilities: agent.Capabilities{ToolChoiceNamed: true}}}},
		}},
		{name: "invalid default", providers: []provider.ProviderDescriptor{
			{ID: "p", Version: "1", Models: []provider.ModelDescriptor{{Ref: provider.ModelRef{Provider: "p", Model: "m"}, Version: "1", ContextWindow: 10, DefaultMaxTokens: 11}}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := provider.NewCatalogSnapshot("generation", now, test.providers)
			if !errors.Is(err, provider.ErrCatalogInvalid) {
				t.Fatalf("error = %v, want ErrCatalogInvalid", err)
			}
		})
	}

	store := provider.NewMemoryCatalog()
	scope := provider.Scope{TenantKey: "tenant"}
	first := mustSnapshot(t, "same", now, descriptor("p", "m", "1"))
	if err := store.Publish(context.Background(), scope, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish(context.Background(), scope, first); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
	conflict := mustSnapshot(t, "same", now, descriptor("p", "m", "2"))
	if err := store.Publish(context.Background(), scope, conflict); !errors.Is(err, provider.ErrCatalogConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func descriptor(providerID provider.ProviderID, modelID provider.ModelID, version string) provider.ModelDescriptor {
	return provider.ModelDescriptor{Ref: provider.ModelRef{Provider: providerID, Model: modelID}, Version: version, ContextWindow: 4096, DefaultMaxTokens: 512}
}

func mustSnapshot(t *testing.T, generation string, now time.Time, model provider.ModelDescriptor) provider.CatalogSnapshot {
	t.Helper()
	snapshot, err := provider.NewCatalogSnapshot(generation, now, []provider.ProviderDescriptor{{ID: model.Ref.Provider, Version: "provider-1", Models: []provider.ModelDescriptor{model}}})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
