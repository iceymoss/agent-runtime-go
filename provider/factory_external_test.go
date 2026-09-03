package provider_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/provider"
)

// buildFromCatalog is the consumer flow the package documents but deliberately
// does not ship: resolve a reference against a CatalogSource snapshot, then
// hand the frozen descriptor to a Factory. It lives in the test because the
// classification codes below are reserved for exactly this code - an
// application's composition root - and the test pins the contract that code
// relies on: every failure is a *provider.Error that unwraps to its sentinel.
func buildFromCatalog(ctx context.Context, source provider.CatalogSource, factory provider.Factory, scope provider.Scope, ref provider.ModelRef, role provider.Role, options agent.GenerationOptions) (agent.Model, error) {
	snapshot, err := source.Snapshot(ctx, scope)
	if err != nil {
		return nil, err
	}
	if _, ok := snapshot.Provider(ref.Provider); !ok {
		return nil, &provider.Error{Code: provider.CodeProviderNotFound, Operation: "resolve", Provider: ref.Provider, Generation: snapshot.Generation, Cause: provider.ErrProviderNotFound}
	}
	model, ok := snapshot.Model(ref)
	if !ok {
		return nil, &provider.Error{Code: provider.CodeModelNotFound, Operation: "resolve", Provider: ref.Provider, Model: ref.Model, Generation: snapshot.Generation, Cause: provider.ErrModelNotFound}
	}
	return factory.Build(ctx, provider.BuildRequest{
		Scope: scope, Ref: ref, Role: role, Descriptor: model,
		// The snapshot generation rides along as the config version so the
		// factory's output can be traced back to the exact catalog it came from.
		ConfigVersion:   snapshot.Generation,
		SelectedOptions: options,
	})
}

// staticModel is the least model a Factory can produce; the flow under test
// ends at construction, so generation is never exercised.
type staticModel struct{ name string }

func (m staticModel) Name() string                     { return m.name }
func (m staticModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (m staticModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	return nil, errors.New("static model does not generate")
}

// recordingFactory classifies its failures with the codes the package reserves
// for Factory implementers, and keeps the last request so the test can prove
// what actually crossed the port.
type recordingFactory struct {
	apiKey string
	last   *provider.BuildRequest
}

func (f *recordingFactory) Build(_ context.Context, request provider.BuildRequest) (agent.Model, error) {
	f.last = &request
	if f.apiKey == "" {
		return nil, &provider.Error{Code: provider.CodeCredentialUnavailable, Operation: "build", Provider: request.Ref.Provider, Model: request.Ref.Model, Cause: provider.ErrCredentialUnavailable}
	}
	if request.Descriptor.Capabilities.ImageInput {
		return nil, &provider.Error{Code: provider.CodeCapabilityMismatch, Operation: "build", Provider: request.Ref.Provider, Model: request.Ref.Model, Cause: provider.ErrCapabilityMismatch}
	}
	if request.Ref.Model == "flaky" {
		return nil, &provider.Error{Code: provider.CodeModelBuildFailed, Operation: "build", Provider: request.Ref.Provider, Model: request.Ref.Model, Retryable: true, Cause: errors.Join(provider.ErrModelBuildFailed, errors.New("upstream handshake failed"))}
	}
	return staticModel{name: string(request.Ref.Provider) + "/" + string(request.Ref.Model)}, nil
}

func TestFactoryBuildFlowClassifiesFailures(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	chat := descriptor("alpha", "chat", "m1")
	chat.DisplayName = "Alpha Chat"
	vision := descriptor("alpha", "vision", "m1")
	vision.Capabilities = agent.Capabilities{ImageInput: true}
	flaky := descriptor("alpha", "flaky", "m1")
	snapshot, err := provider.NewCatalogSnapshot("generation-7", now, []provider.ProviderDescriptor{
		{ID: "alpha", Version: "p1", Models: []provider.ModelDescriptor{chat, vision, flaky}},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := provider.NewMemoryCatalog()
	scope := provider.Scope{TenantKey: "tenant-a"}
	if err := catalog.Publish(context.Background(), scope, snapshot); err != nil {
		t.Fatal(err)
	}
	// The flow depends on the port, not the memory implementation, which is the
	// point of CatalogSource existing at all.
	var source provider.CatalogSource = catalog

	tests := []struct {
		name      string
		ref       provider.ModelRef
		apiKey    string
		wantIs    error
		wantCode  provider.Code
		retryable bool
	}{
		{name: "unknown provider is classified before any factory call", ref: provider.ModelRef{Provider: "missing", Model: "chat"}, apiKey: "key", wantIs: provider.ErrProviderNotFound, wantCode: provider.CodeProviderNotFound},
		{name: "unknown model is classified before any factory call", ref: provider.ModelRef{Provider: "alpha", Model: "missing"}, apiKey: "key", wantIs: provider.ErrModelNotFound, wantCode: provider.CodeModelNotFound},
		{name: "missing credential maps to credential_unavailable", ref: provider.ModelRef{Provider: "alpha", Model: "chat"}, apiKey: "", wantIs: provider.ErrCredentialUnavailable, wantCode: provider.CodeCredentialUnavailable},
		{name: "descriptor beyond adapter maps to capability_mismatch", ref: provider.ModelRef{Provider: "alpha", Model: "vision"}, apiKey: "key", wantIs: provider.ErrCapabilityMismatch, wantCode: provider.CodeCapabilityMismatch},
		{name: "constructor failure maps to model_build_failed with cause kept", ref: provider.ModelRef{Provider: "alpha", Model: "flaky"}, apiKey: "key", wantIs: provider.ErrModelBuildFailed, wantCode: provider.CodeModelBuildFailed, retryable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factory := &recordingFactory{apiKey: tt.apiKey}
			_, err := buildFromCatalog(context.Background(), source, factory, scope, tt.ref, provider.RolePrimary, agent.GenerationOptions{})
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("error = %v, want errors.Is %v", err, tt.wantIs)
			}
			var classified *provider.Error
			if !errors.As(err, &classified) || classified.Code != tt.wantCode || classified.Retryable != tt.retryable {
				t.Fatalf("classified error = %#v, want code %q retryable %v", classified, tt.wantCode, tt.retryable)
			}
			if !strings.Contains(err.Error(), string(tt.ref.Provider)) {
				t.Fatalf("error message %q does not name the provider", err.Error())
			}
		})
	}
}

func TestFactoryBuildReceivesFrozenDescriptorAndSelection(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)
	chat := descriptor("alpha", "chat", "m1")
	chat.DisplayName = "Alpha Chat"
	snapshot := mustSnapshot(t, "generation-9", now, chat)
	catalog := provider.NewMemoryCatalog()
	scope := provider.Scope{TenantKey: "tenant-a"}
	if err := catalog.Publish(context.Background(), scope, snapshot); err != nil {
		t.Fatal(err)
	}
	temperature := 0.2
	factory := &recordingFactory{apiKey: "key"}
	model, err := buildFromCatalog(context.Background(), catalog, factory, scope, chat.Ref, provider.RoleSummary, agent.GenerationOptions{Temperature: &temperature})
	if err != nil {
		t.Fatal(err)
	}
	if model.Name() != "alpha/chat" {
		t.Fatalf("model name = %q", model.Name())
	}
	request := factory.last
	if request == nil || request.Role != provider.RoleSummary || request.Scope != scope {
		t.Fatalf("factory request = %#v", request)
	}
	// The generation the descriptor was resolved from must reach the factory,
	// or nothing downstream can say which catalog produced a running model.
	if request.ConfigVersion != "generation-9" {
		t.Fatalf("ConfigVersion = %q", request.ConfigVersion)
	}
	if request.SelectedOptions.Temperature == nil || *request.SelectedOptions.Temperature != 0.2 {
		t.Fatalf("SelectedOptions = %#v", request.SelectedOptions)
	}
	// DisplayName is presentation metadata; it must survive the catalog's
	// canonicalization and deep copies to be worth declaring.
	if request.Descriptor.DisplayName != "Alpha Chat" {
		t.Fatalf("Descriptor = %#v", request.Descriptor)
	}
}
