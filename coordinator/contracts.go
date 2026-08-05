// Package coordinator resolves selectors into immutable executable agent
// definitions while persisting only canonical data manifests.
package coordinator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/provider"
)

const CurrentSchemaVersion uint16 = 1

type SelectorValue struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type Selector struct {
	AgentKey  string          `json:"agent_key"`
	TenantKey agent.TenantKey `json:"tenant_key"`
	Values    []SelectorValue `json:"values,omitempty"`
}

type BuildRequest struct {
	Selector   Selector                `json:"selector"`
	ModelRole  provider.Role           `json:"model_role"`
	RunOptions agent.GenerationOptions `json:"run_options"`
}

type ResolveRequest = BuildRequest

// ArtifactRef identifies one immutable tenant-scoped artifact generation.
type ArtifactRef struct {
	Kind          string `json:"kind"`
	Key           string `json:"key"`
	Generation    string `json:"generation"`
	Digest        string `json:"digest"`
	SchemaVersion uint16 `json:"schema_version"`
}

// ManifestWire contains only durable values. Executable interfaces and root
// RuntimeDefinition values deliberately have no representation here.
type ManifestWire struct {
	SchemaVersion     uint16                  `json:"schema_version"`
	DefinitionDigest  string                  `json:"definition_digest"`
	RecipeKey         string                  `json:"recipe_key"`
	RecipeVersion     string                  `json:"recipe_version"`
	Role              provider.Role           `json:"role"`
	Provider          provider.ProviderID     `json:"provider"`
	Model             provider.ModelID        `json:"model"`
	ModelVersion      string                  `json:"model_version"`
	CatalogGeneration string                  `json:"catalog_generation"`
	PromptMessages    []agent.Message         `json:"prompt_messages,omitempty"`
	Execution         agent.ExecutionSettings `json:"execution"`
	Options           agent.GenerationOptions `json:"options"`
	OptionsVersion    string                  `json:"options_version"`
	RootToolsDeclared bool                    `json:"root_tools_declared,omitempty"`
	RootToolNames     []string                `json:"root_tool_names,omitempty"`
	Artifacts         []ArtifactRef           `json:"artifacts"`
}

// WireManifest is retained as the descriptive name used by the S19 spec.
type WireManifest = ManifestWire

// ArtifactManifest is the only coordinator persistence payload.
type ArtifactManifest struct {
	TenantKey      agent.TenantKey `json:"tenant_key"`
	Wire           ManifestWire    `json:"wire"`
	ManifestDigest string          `json:"manifest_digest"`
	CreatedAt      time.Time       `json:"created_at"`
}

type RetentionPin struct {
	PinKey      string    `json:"pin_key"`
	OwnerKey    string    `json:"owner_key"`
	RetainUntil time.Time `json:"retain_until,omitempty"`
}

type StoredGeneration struct {
	Manifest ArtifactManifest `json:"manifest"`
	Pins     []RetentionPin   `json:"pins"`
}

// BuildResult contains the application-built executable and its wire identity.
// Definition is never serialized, even if a caller accidentally marshals it.
type BuildResult struct {
	Definition     *agent.RuntimeDefinition `json:"-"`
	SystemMessages []agent.Message          `json:"-"`
	Wire           ManifestWire             `json:"wire"`
}

// Reconstruction reports the exact artifacts used to reconstruct a definition.
// Coordinator verifies these identities against the stored manifest.
type Reconstruction struct {
	Definition     *agent.RuntimeDefinition `json:"-"`
	SystemMessages []agent.Message          `json:"-"`
	Artifacts      []ArtifactRef            `json:"-"`
}

// Builder is application-owned because recipes, credentials, prompts, tools,
// Skills, and MCP registries are application concerns.
type Builder interface {
	Build(context.Context, BuildRequest) (BuildResult, error)
}

// Reconstructor resolves every artifact by the exact tenant, generation, and
// digest in a verified manifest and returns a newly executable definition.
type Reconstructor interface {
	Reconstruct(context.Context, provider.Scope, ArtifactManifest) (Reconstruction, error)
}

type ManifestStore interface {
	SaveGeneration(context.Context, StoredGeneration, RetentionPin) error
	ResolveGeneration(context.Context, provider.Scope, string) (StoredGeneration, error)
	PinGeneration(context.Context, provider.Scope, string, RetentionPin) error
	ReleasePin(context.Context, provider.Scope, string, string) error
}

// ArtifactStore is the S19 name for the exact-generation manifest port.
type ArtifactStore = ManifestStore

// ResolvedRuntime is executable in memory only. Its Clone method provides
// defensive slice copies while sharing the immutable RuntimeDefinition.
type ResolvedRuntime struct {
	Definition        *agent.RuntimeDefinition `json:"-"`
	SystemMessages    []agent.Message          `json:"-"`
	SchemaVersion     uint16                   `json:"schema_version"`
	DefinitionDigest  string                   `json:"definition_digest"`
	ManifestDigest    string                   `json:"manifest_digest"`
	RecipeVersion     string                   `json:"recipe_version"`
	CatalogGeneration string                   `json:"catalog_generation"`
	OptionsVersion    string                   `json:"options_version"`
	Artifacts         []ArtifactRef            `json:"artifacts"`
}

// RuntimeSnapshot is the S19 name for a resolved executable generation.
type RuntimeSnapshot = ResolvedRuntime

func (r ResolvedRuntime) Clone() ResolvedRuntime {
	r.SystemMessages = cloneMessages(r.SystemMessages)
	r.Artifacts = append([]ArtifactRef(nil), r.Artifacts...)
	return r
}

type Options struct {
	Builder       Builder
	Reconstructor Reconstructor
	Artifacts     ManifestStore
	Clock         func() time.Time
}

type Code string

const (
	CodeNotReady              Code = "not_ready"
	CodeClosed                Code = "closed"
	CodeInvalidRequest        Code = "invalid_request"
	CodeGenerationUnavailable Code = "generation_unavailable"
	CodeDefinitionInvalid     Code = "definition_invalid"
	CodeManifestInvalid       Code = "manifest_invalid"
	CodeInvariantConflict     Code = "invariant_conflict"
	CodeRetentionPinned       Code = "retention_pinned"
	CodeCapabilityUnavailable Code = "capability_unavailable"
)

var (
	ErrNotReady              = errors.New("agent coordinator: not ready")
	ErrClosed                = errors.New("agent coordinator: closed")
	ErrInvalidRequest        = errors.New("agent coordinator: invalid request")
	ErrRecipeNotFound        = errors.New("agent coordinator: recipe not found")
	ErrProviderNotFound      = errors.New("agent coordinator: provider not found")
	ErrModelNotFound         = errors.New("agent coordinator: model not found")
	ErrCapabilityUnavailable = errors.New("agent coordinator: capability unavailable")
	ErrGenerationUnavailable = errors.New("agent coordinator: generation unavailable")
	ErrDefinitionInvalid     = errors.New("agent coordinator: definition invalid")
	ErrManifestInvalid       = errors.New("agent coordinator: manifest invalid")
	ErrInvariantConflict     = errors.New("agent coordinator: invariant conflict")
	ErrRetentionPinned       = errors.New("agent coordinator: generation is pinned")
)

type Error struct {
	Code       Code
	Operation  string
	Source     string
	Generation string
	Retryable  bool
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	message := "agent coordinator"
	if e.Operation != "" {
		message += ": " + e.Operation
	}
	if e.Source != "" {
		message += fmt.Sprintf(" source=%q", e.Source)
	}
	if e.Generation != "" {
		message += fmt.Sprintf(" generation=%q", e.Generation)
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
