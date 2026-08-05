// Package provider defines provider-neutral model catalogs and factories.
package provider

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type ProviderID string
type ModelID string
type Role string

const (
	RolePrimary Role = "primary"
	RoleUtility Role = "utility"
	RoleSummary Role = "summary"
	RoleTitle   Role = "title"
)

type Scope struct {
	TenantKey agent.TenantKey `json:"tenant_key"`
}

type ModelRef struct {
	Provider ProviderID `json:"provider"`
	Model    ModelID    `json:"model"`
}

type Pricing struct {
	InputPerMillion      string `json:"input_per_million,omitempty"`
	OutputPerMillion     string `json:"output_per_million,omitempty"`
	CacheWritePerMillion string `json:"cache_write_per_million,omitempty"`
	CacheReadPerMillion  string `json:"cache_read_per_million,omitempty"`
	Currency             string `json:"currency,omitempty"`
	Version              string `json:"version,omitempty"`
}

type ModelDescriptor struct {
	Ref              ModelRef                `json:"ref"`
	DisplayName      string                  `json:"display_name,omitempty"`
	Version          string                  `json:"version"`
	ContextWindow    int                     `json:"context_window"`
	DefaultMaxTokens int                     `json:"default_max_tokens"`
	Capabilities     agent.Capabilities      `json:"capabilities"`
	DefaultOptions   agent.GenerationOptions `json:"default_options"`
	Pricing          *Pricing                `json:"pricing,omitempty"`
}

type ProviderDescriptor struct {
	ID      ProviderID        `json:"id"`
	Version string            `json:"version"`
	Models  []ModelDescriptor `json:"models"`
}

// CatalogSnapshot is immutable through its API. Construction and all reads
// cross deep-copy boundaries.
type CatalogSnapshot struct {
	Generation string    `json:"generation"`
	CreatedAt  time.Time `json:"created_at"`
	providers  []ProviderDescriptor
}

func NewCatalogSnapshot(generation string, createdAt time.Time, providers []ProviderDescriptor) (CatalogSnapshot, error) {
	if strings.TrimSpace(generation) == "" || createdAt.IsZero() {
		return CatalogSnapshot{}, catalogError("new_snapshot", generation, "generation and creation time are required", nil)
	}
	cloned := cloneProviders(providers)
	if len(cloned) == 0 {
		return CatalogSnapshot{}, catalogError("new_snapshot", generation, "at least one provider is required", nil)
	}
	sort.Slice(cloned, func(i, j int) bool { return cloned[i].ID < cloned[j].ID })
	seenProviders := make(map[ProviderID]struct{}, len(cloned))
	for i := range cloned {
		provider := &cloned[i]
		if provider.ID == "" || strings.TrimSpace(provider.Version) == "" {
			return CatalogSnapshot{}, catalogError("new_snapshot", generation, "provider ID and version are required", nil)
		}
		if _, exists := seenProviders[provider.ID]; exists {
			return CatalogSnapshot{}, catalogError("new_snapshot", generation, fmt.Sprintf("duplicate provider %q", provider.ID), nil)
		}
		seenProviders[provider.ID] = struct{}{}
		if len(provider.Models) == 0 {
			return CatalogSnapshot{}, catalogError("new_snapshot", generation, fmt.Sprintf("provider %q has no models", provider.ID), nil)
		}
		sort.Slice(provider.Models, func(i, j int) bool { return provider.Models[i].Ref.Model < provider.Models[j].Ref.Model })
		seenModels := make(map[ModelID]struct{}, len(provider.Models))
		for j := range provider.Models {
			model := &provider.Models[j]
			if model.Ref.Provider == "" {
				model.Ref.Provider = provider.ID
			}
			if model.Ref.Provider != provider.ID || model.Ref.Model == "" || strings.TrimSpace(model.Version) == "" {
				return CatalogSnapshot{}, catalogError("new_snapshot", generation, "model reference and version are invalid", nil)
			}
			if _, exists := seenModels[model.Ref.Model]; exists {
				return CatalogSnapshot{}, catalogError("new_snapshot", generation, fmt.Sprintf("duplicate model %q/%q", provider.ID, model.Ref.Model), nil)
			}
			seenModels[model.Ref.Model] = struct{}{}
			if err := validateModel(*model); err != nil {
				return CatalogSnapshot{}, catalogError("new_snapshot", generation, fmt.Sprintf("model %q/%q", provider.ID, model.Ref.Model), err)
			}
		}
	}
	return CatalogSnapshot{Generation: generation, CreatedAt: createdAt, providers: cloned}, nil
}

func validateModel(model ModelDescriptor) error {
	if model.ContextWindow <= 0 || model.DefaultMaxTokens <= 0 || model.DefaultMaxTokens > model.ContextWindow {
		return fmt.Errorf("context window and default max tokens are inconsistent")
	}
	if err := model.Capabilities.Validate(); err != nil {
		return err
	}
	if value := model.DefaultOptions.MaxTokens; value != nil && (*value <= 0 || *value > model.ContextWindow) {
		return fmt.Errorf("default max tokens are out of range")
	}
	if value := model.DefaultOptions.Temperature; value != nil && (*value < 0 || *value > 2) {
		return fmt.Errorf("default temperature is out of range")
	}
	if value := model.DefaultOptions.TopP; value != nil && (*value < 0 || *value > 1) {
		return fmt.Errorf("default top_p is out of range")
	}
	if model.Pricing != nil {
		if strings.TrimSpace(model.Pricing.Currency) == "" || strings.TrimSpace(model.Pricing.Version) == "" {
			return fmt.Errorf("pricing currency and version are required")
		}
		for _, value := range []string{model.Pricing.InputPerMillion, model.Pricing.OutputPerMillion, model.Pricing.CacheWritePerMillion, model.Pricing.CacheReadPerMillion} {
			if value == "" {
				continue
			}
			decimal, ok := new(big.Rat).SetString(value)
			if !ok || decimal.Sign() < 0 {
				return fmt.Errorf("pricing values must be non-negative decimal strings")
			}
		}
	}
	return nil
}

func (s CatalogSnapshot) Provider(id ProviderID) (ProviderDescriptor, bool) {
	index := sort.Search(len(s.providers), func(i int) bool { return s.providers[i].ID >= id })
	if index == len(s.providers) || s.providers[index].ID != id {
		return ProviderDescriptor{}, false
	}
	return cloneProvider(s.providers[index]), true
}

func (s CatalogSnapshot) Model(ref ModelRef) (ModelDescriptor, bool) {
	provider, ok := s.Provider(ref.Provider)
	if !ok {
		return ModelDescriptor{}, false
	}
	index := sort.Search(len(provider.Models), func(i int) bool { return provider.Models[i].Ref.Model >= ref.Model })
	if index == len(provider.Models) || provider.Models[index].Ref.Model != ref.Model {
		return ModelDescriptor{}, false
	}
	return cloneModel(provider.Models[index]), true
}

func (s CatalogSnapshot) ProvidersList() []ProviderDescriptor { return cloneProviders(s.providers) }

func (s CatalogSnapshot) Clone() CatalogSnapshot {
	return CatalogSnapshot{Generation: s.Generation, CreatedAt: s.CreatedAt, providers: cloneProviders(s.providers)}
}

type CatalogSource interface {
	Snapshot(context.Context, Scope) (CatalogSnapshot, error)
}

type BuildRequest struct {
	Scope           Scope                   `json:"scope"`
	Ref             ModelRef                `json:"ref"`
	Role            Role                    `json:"role"`
	Descriptor      ModelDescriptor         `json:"descriptor"`
	ConfigVersion   string                  `json:"config_version"`
	SelectedOptions agent.GenerationOptions `json:"selected_options"`
}

type Factory interface {
	Build(context.Context, BuildRequest) (agent.Model, error)
}

func cloneProviders(value []ProviderDescriptor) []ProviderDescriptor {
	if value == nil {
		return nil
	}
	result := make([]ProviderDescriptor, len(value))
	for i := range value {
		result[i] = cloneProvider(value[i])
	}
	return result
}

func cloneProvider(value ProviderDescriptor) ProviderDescriptor {
	models := value.Models
	value.Models = make([]ModelDescriptor, len(models))
	for i := range models {
		value.Models[i] = cloneModel(models[i])
	}
	return value
}

func cloneModel(value ModelDescriptor) ModelDescriptor {
	value.DefaultOptions = cloneOptions(value.DefaultOptions)
	if value.Pricing != nil {
		pricing := *value.Pricing
		value.Pricing = &pricing
	}
	return value
}

func cloneOptions(value agent.GenerationOptions) agent.GenerationOptions {
	result := value
	if value.Temperature != nil {
		v := *value.Temperature
		result.Temperature = &v
	}
	if value.MaxTokens != nil {
		v := *value.MaxTokens
		result.MaxTokens = &v
	}
	if value.TopP != nil {
		v := *value.TopP
		result.TopP = &v
	}
	return result
}

// MemoryCatalog atomically publishes immutable exact catalog generations.
type MemoryCatalog struct {
	mu      sync.RWMutex
	current map[agent.TenantKey]string
	values  map[agent.TenantKey]map[string]CatalogSnapshot
}

func NewMemoryCatalog() *MemoryCatalog {
	return &MemoryCatalog{current: make(map[agent.TenantKey]string), values: make(map[agent.TenantKey]map[string]CatalogSnapshot)}
}

func (c *MemoryCatalog) Publish(_ context.Context, scope Scope, snapshot CatalogSnapshot) error {
	if c == nil || !scope.TenantKey.Valid() || snapshot.Generation == "" {
		return catalogError("publish", snapshot.Generation, "scope and snapshot are required", nil)
	}
	validated, err := NewCatalogSnapshot(snapshot.Generation, snapshot.CreatedAt, snapshot.ProvidersList())
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byGeneration := c.values[scope.TenantKey]
	if byGeneration == nil {
		byGeneration = make(map[string]CatalogSnapshot)
		c.values[scope.TenantKey] = byGeneration
	}
	if existing, ok := byGeneration[validated.Generation]; ok {
		left, leftErr := agent.CanonicalDigest(existing.ProvidersList())
		right, rightErr := agent.CanonicalDigest(validated.ProvidersList())
		if leftErr != nil || rightErr != nil || left != right || !existing.CreatedAt.Equal(validated.CreatedAt) {
			return &Error{Code: CodeCatalogConflict, Operation: "publish", Generation: validated.Generation, Cause: ErrCatalogConflict}
		}
	} else {
		byGeneration[validated.Generation] = validated.Clone()
	}
	c.current[scope.TenantKey] = validated.Generation
	return nil
}

func (c *MemoryCatalog) Snapshot(ctx context.Context, scope Scope) (CatalogSnapshot, error) {
	return c.Current(ctx, scope)
}

func (c *MemoryCatalog) Current(_ context.Context, scope Scope) (CatalogSnapshot, error) {
	if c == nil || !scope.TenantKey.Valid() {
		return CatalogSnapshot{}, &Error{Code: CodeCatalogUnavailable, Operation: "current", Cause: ErrCatalogUnavailable}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	generation := c.current[scope.TenantKey]
	value, ok := c.values[scope.TenantKey][generation]
	if !ok {
		return CatalogSnapshot{}, &Error{Code: CodeCatalogUnavailable, Operation: "current", Cause: ErrCatalogUnavailable}
	}
	return value.Clone(), nil
}

func (c *MemoryCatalog) Get(_ context.Context, scope Scope, generation string) (CatalogSnapshot, error) {
	if c == nil || !scope.TenantKey.Valid() || generation == "" {
		return CatalogSnapshot{}, &Error{Code: CodeCatalogUnavailable, Operation: "get", Generation: generation, Cause: ErrCatalogUnavailable}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	value, ok := c.values[scope.TenantKey][generation]
	if !ok {
		return CatalogSnapshot{}, &Error{Code: CodeCatalogUnavailable, Operation: "get", Generation: generation, Cause: ErrCatalogUnavailable}
	}
	return value.Clone(), nil
}

type Code string

const (
	CodeCatalogInvalid        Code = "catalog_invalid"
	CodeCatalogConflict       Code = "catalog_conflict"
	CodeCatalogUnavailable    Code = "catalog_unavailable"
	CodeProviderNotFound      Code = "provider_not_found"
	CodeModelNotFound         Code = "model_not_found"
	CodeModelBuildFailed      Code = "model_build_failed"
	CodeCapabilityMismatch    Code = "capability_mismatch"
	CodeCredentialUnavailable Code = "credential_unavailable"
)

var (
	ErrCatalogInvalid        = errors.New("agent provider: invalid catalog")
	ErrCatalogConflict       = errors.New("agent provider: catalog conflict")
	ErrCatalogUnavailable    = errors.New("agent provider: catalog unavailable")
	ErrProviderNotFound      = errors.New("agent provider: provider not found")
	ErrModelNotFound         = errors.New("agent provider: model not found")
	ErrModelBuildFailed      = errors.New("agent provider: model build failed")
	ErrCapabilityMismatch    = errors.New("agent provider: capability mismatch")
	ErrCredentialUnavailable = errors.New("agent provider: credential unavailable")
)

type Error struct {
	Code       Code
	Operation  string
	Provider   ProviderID
	Model      ModelID
	Generation string
	Retryable  bool
	Cause      error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	message := "agent provider"
	if e.Operation != "" {
		message += ": " + e.Operation
	}
	if e.Provider != "" {
		message += fmt.Sprintf(" provider=%q", e.Provider)
	}
	if e.Model != "" {
		message += fmt.Sprintf(" model=%q", e.Model)
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

func catalogError(operation, generation, detail string, cause error) error {
	if cause == nil {
		cause = ErrCatalogInvalid
	} else {
		cause = errors.Join(ErrCatalogInvalid, cause)
	}
	if detail != "" {
		cause = fmt.Errorf("%w: %s", cause, detail)
	}
	return &Error{Code: CodeCatalogInvalid, Operation: operation, Generation: generation, Cause: cause}
}
