package icoder

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/coordinator"
	"github.com/iceymoss/agent-runtime-go/provider"
)

// recipeKey and recipeVersion name the composition rule iCoder applies. They are
// part of the stored manifest, so a change to how the runtime is assembled is
// visible in an audit even when every individual artifact looks the same.
const (
	recipeKey      = "icoder.coding-agent"
	recipeVersion  = "v1"
	optionsVersion = "icoder-options-v1"
	catalogID      = "icoder-catalog-v1"
	providerID     = provider.ProviderID("openai-compatible")
)

// runtimeIngredients are the already-resolved parts a definition is assembled
// from. They are gathered once at startup: the model client, the frozen tool
// generation, and the rendered system prompt.
type runtimeIngredients struct {
	model          agent.Model
	modelName      string
	contextWindow  int
	maxTokens      int
	maxSteps       int
	tools          *agent.ToolSet
	toolNames      []string
	catalog        *ToolCatalog
	systemMessages []agent.Message
	promptVersion  string
	skillsID       string
}

// RuntimeBuilder assembles one immutable runtime generation.
//
// Building is deliberately application-owned: only iCoder knows which prompt,
// which tools, and which policy belong together. The coordinator's job is to
// record exactly what was built and to refuse anything that does not match later.
type RuntimeBuilder struct{ ingredients runtimeIngredients }

// RuntimeReconstructor rebuilds a definition from a stored manifest.
//
// It rebuilds from the same ingredients rather than from the manifest's contents,
// and the coordinator then checks that the result has the manifest's exact
// definition digest, prompt, and artifact identities. A drifted binary therefore
// fails to reconstruct instead of quietly running something else.
type RuntimeReconstructor struct{ builder *RuntimeBuilder }

// NewRuntimeBuilder validates the ingredients a definition needs.
func NewRuntimeBuilder(ingredients runtimeIngredients) (*RuntimeBuilder, error) {
	if ingredients.model == nil || ingredients.tools == nil || ingredients.catalog == nil {
		return nil, fmt.Errorf("runtime builder requires a model, a tool set, and a tool catalog")
	}
	if ingredients.modelName == "" || ingredients.promptVersion == "" {
		return nil, fmt.Errorf("runtime builder requires a model name and a prompt version")
	}
	return &RuntimeBuilder{ingredients: ingredients}, nil
}

// Build produces the executable definition and the durable manifest that
// identifies it.
func (b *RuntimeBuilder) Build(_ context.Context, request coordinator.BuildRequest) (coordinator.BuildResult, error) {
	ingredients := b.ingredients
	maxTokens := ingredients.maxTokens
	execution := agent.ExecutionSettings{MaxSteps: ingredients.maxSteps, MaxTokens: &maxTokens}
	definition, err := agent.NewRuntimeDefinition(agent.RuntimeDefinitionSpec{
		Key: recipeKey,
		Model: agent.ModelMetadata{
			Name: ingredients.modelName, Version: ingredients.modelName,
			ContextWindow: ingredients.contextWindow, Capabilities: ingredients.model.Capabilities(),
		},
		Execution:     execution,
		PromptVersion: ingredients.promptVersion,
		PolicyVersion: string(policyVersion),
	}, ingredients.model, ingredients.tools)
	if err != nil {
		return coordinator.BuildResult{}, err
	}
	role := request.ModelRole
	if role == "" {
		role = provider.RolePrimary
	}
	names := append([]string(nil), ingredients.toolNames...)
	sort.Strings(names)
	wire := coordinator.ManifestWire{
		SchemaVersion:    coordinator.CurrentSchemaVersion,
		DefinitionDigest: definition.ArtifactVersions().Definition,
		RecipeKey:        recipeKey, RecipeVersion: recipeVersion, Role: role,
		Provider: providerID, Model: provider.ModelID(ingredients.modelName), ModelVersion: ingredients.modelName,
		CatalogGeneration: catalogID,
		PromptMessages:    ingredients.systemMessages,
		// StopConditions are executable functions and cannot be serialized, so a
		// wire manifest must never carry them.
		Execution:      agent.ExecutionSettings{MaxSteps: execution.MaxSteps, MaxTokens: execution.MaxTokens},
		Options:        agent.GenerationOptions{MaxTokens: execution.MaxTokens},
		OptionsVersion: optionsVersion,
		// Declaring the root tool names makes the manifest self-describing: an
		// auditor can see which tools a past run could reach without the binary.
		RootToolsDeclared: true, RootToolNames: names,
		Artifacts: ingredients.artifactRefs(definition),
	}
	return coordinator.BuildResult{Definition: definition, SystemMessages: ingredients.systemMessages, Wire: wire}, nil
}

// Reconstruct rebuilds the definition an earlier manifest describes.
func (r *RuntimeReconstructor) Reconstruct(ctx context.Context, _ provider.Scope, manifest coordinator.ArtifactManifest) (coordinator.Reconstruction, error) {
	built, err := r.builder.Build(ctx, coordinator.BuildRequest{
		Selector:  coordinator.Selector{AgentKey: recipeKey, TenantKey: manifest.TenantKey},
		ModelRole: manifest.Wire.Role,
	})
	if err != nil {
		return coordinator.Reconstruction{}, err
	}
	return coordinator.Reconstruction{
		Definition: built.Definition, SystemMessages: built.SystemMessages, Artifacts: built.Wire.Artifacts,
	}, nil
}

// artifactRefs names every external input the definition depends on. The list is
// sorted canonically because the manifest digest covers it.
func (i runtimeIngredients) artifactRefs(definition *agent.RuntimeDefinition) []coordinator.ArtifactRef {
	versions := definition.ArtifactVersions()
	refs := []coordinator.ArtifactRef{
		{Kind: "prompt", Key: "icoder-prompt", Generation: i.promptVersion, Digest: promptDigest(i.systemMessages), SchemaVersion: 1},
		{Kind: "policy", Key: "icoder-policy", Generation: string(policyVersion), Digest: digest([]byte(policyVersion)), SchemaVersion: 1},
		{Kind: "tools", Key: "icoder-tools", Generation: toolGenerationVersion, Digest: i.catalog.GenerationDigest(), SchemaVersion: 1},
		{Kind: "toolset", Key: "icoder-toolset", Generation: toolSchemaVersion, Digest: versions.Tools, SchemaVersion: 1},
	}
	if i.skillsID != "" {
		refs = append(refs, coordinator.ArtifactRef{Kind: "skills", Key: "icoder-skills", Generation: i.skillsID, Digest: digest([]byte(i.skillsID)), SchemaVersion: 1})
	}
	sort.Slice(refs, func(a, b int) bool {
		if refs[a].Kind == refs[b].Kind {
			return refs[a].Key < refs[b].Key
		}
		return refs[a].Kind < refs[b].Kind
	})
	return refs
}

func promptDigest(messages []agent.Message) string {
	value, err := agent.CanonicalDigest(messages)
	if err != nil {
		return digest([]byte(fmt.Sprint(messages)))
	}
	return value
}

// ResolvedRuntimeInfo is the human-readable identity of the generation a process
// is running, for `icoder runtime show`.
type ResolvedRuntimeInfo struct {
	DefinitionDigest  string                    `json:"definition_digest"`
	ManifestDigest    string                    `json:"manifest_digest"`
	RecipeVersion     string                    `json:"recipe_version"`
	CatalogGeneration string                    `json:"catalog_generation"`
	OptionsVersion    string                    `json:"options_version"`
	Model             string                    `json:"model"`
	Tools             []string                  `json:"tools"`
	Artifacts         []coordinator.ArtifactRef `json:"artifacts"`
}

// resolveRuntime builds the generation, persists its manifest, and returns both
// the executable definition and the identity a caller can print or store.
func resolveRuntime(ctx context.Context, builder *RuntimeBuilder, manifests coordinator.ManifestStore) (*coordinator.Coordinator, coordinator.ResolvedRuntime, error) {
	resolver, err := coordinator.New(coordinator.Options{
		Builder: builder, Reconstructor: &RuntimeReconstructor{builder: builder},
		Artifacts: manifests, Clock: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, coordinator.ResolvedRuntime{}, err
	}
	resolved, err := resolver.Resolve(ctx, coordinator.ResolveRequest{
		Selector:  coordinator.Selector{AgentKey: recipeKey, TenantKey: tenantKey},
		ModelRole: provider.RolePrimary,
	})
	if err != nil {
		_ = resolver.Close()
		return nil, coordinator.ResolvedRuntime{}, err
	}
	return resolver, resolved, nil
}

// RuntimeInfo reports the exact generation this process is running.
//
// The definition digest is what makes a past run auditable: it identifies the
// model, prompt, tool set, execution settings, and policy that produced it, and
// the same digest can be resolved back into an executable definition.
func (a *App) RuntimeInfo() ResolvedRuntimeInfo {
	return ResolvedRuntimeInfo{
		DefinitionDigest: a.resolved.DefinitionDigest, ManifestDigest: a.resolved.ManifestDigest,
		RecipeVersion: a.resolved.RecipeVersion, CatalogGeneration: a.resolved.CatalogGeneration,
		OptionsVersion: a.resolved.OptionsVersion, Model: a.config.Model,
		Tools: a.Tools(), Artifacts: append([]coordinator.ArtifactRef(nil), a.resolved.Artifacts...),
	}
}

// RuntimeGenerations lists the compositions this workspace has recorded.
func (a *App) RuntimeGenerations(ctx context.Context, limit int) ([]coordinator.ArtifactManifest, error) {
	return a.store.ManifestStore().ListGenerations(ctx, provider.Scope{TenantKey: tenantKey}, limit)
}

// ReconstructRuntime rebuilds a stored generation and reports whether it still
// matches. It is the check an operator runs before trusting that a recorded run
// can be reproduced by the current binary.
func (a *App) ReconstructRuntime(ctx context.Context, definitionDigest string) (ResolvedRuntimeInfo, error) {
	resolved, err := a.resolver.ResolveGeneration(ctx, provider.Scope{TenantKey: tenantKey}, definitionDigest)
	if err != nil {
		return ResolvedRuntimeInfo{}, err
	}
	names := make([]string, 0)
	for _, definition := range resolved.Definition.ToolDefinitions() {
		names = append(names, definition.Name)
	}
	return ResolvedRuntimeInfo{
		DefinitionDigest: resolved.DefinitionDigest, ManifestDigest: resolved.ManifestDigest,
		RecipeVersion: resolved.RecipeVersion, CatalogGeneration: resolved.CatalogGeneration,
		OptionsVersion: resolved.OptionsVersion, Model: resolved.Definition.ModelMetadata().Name,
		Tools: names, Artifacts: append([]coordinator.ArtifactRef(nil), resolved.Artifacts...),
	}, nil
}
