package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/provider"
)

type cacheEntry struct {
	ready chan struct{}
	value ResolvedRuntime
	err   error
}

type Coordinator struct {
	mu            sync.Mutex
	builder       Builder
	reconstructor Reconstructor
	artifacts     ManifestStore
	clock         func() time.Time
	cache         map[string]*cacheEntry
	closed        bool
}

func New(options Options) (*Coordinator, error) {
	if options.Builder == nil || options.Reconstructor == nil || options.Artifacts == nil {
		return nil, &Error{Code: CodeInvalidRequest, Operation: "new", Cause: fmt.Errorf("%w: builder, reconstructor, and artifact store are required", ErrInvalidRequest)}
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Coordinator{
		builder: options.Builder, reconstructor: options.Reconstructor,
		artifacts: options.Artifacts, clock: clock, cache: make(map[string]*cacheEntry),
	}, nil
}

func (c *Coordinator) Resolve(ctx context.Context, request ResolveRequest) (ResolvedRuntime, error) {
	if err := c.checkOpen(); err != nil {
		return ResolvedRuntime{}, err
	}
	normalized, err := normalizeRequest(request)
	if err != nil {
		return ResolvedRuntime{}, err
	}
	built, err := c.builder.Build(ctx, normalized)
	if err != nil {
		return ResolvedRuntime{}, &Error{Code: CodeCapabilityUnavailable, Operation: "build", Source: "builder", Retryable: true, Cause: errors.Join(ErrCapabilityUnavailable, err)}
	}
	built.Wire = canonicalWire(built.Wire)
	if normalized.ModelRole != "" && built.Wire.Role != normalized.ModelRole {
		return ResolvedRuntime{}, definitionError("resolve", built.Wire.DefinitionDigest, "builder returned a different model role", nil)
	}
	manifest, runtime, err := c.runtimeFromBuild(normalized.Selector.TenantKey, built)
	if err != nil {
		return ResolvedRuntime{}, err
	}
	if err := c.artifacts.SaveGeneration(ctx, StoredGeneration{Manifest: manifest}, RetentionPin{}); err != nil {
		return ResolvedRuntime{}, err
	}
	c.putCache(manifest, runtime)
	return runtime.Clone(), nil
}

func (c *Coordinator) ResolveGeneration(ctx context.Context, scope provider.Scope, definitionDigest string) (ResolvedRuntime, error) {
	if err := c.checkOpen(); err != nil {
		return ResolvedRuntime{}, err
	}
	if !scope.TenantKey.Valid() || strings.TrimSpace(definitionDigest) == "" {
		return ResolvedRuntime{}, generationUnavailable("resolve_generation", definitionDigest)
	}
	stored, err := c.artifacts.ResolveGeneration(ctx, scope, definitionDigest)
	if err != nil {
		return ResolvedRuntime{}, generationUnavailableWithCause("resolve_generation", definitionDigest, err)
	}
	manifest := cloneManifest(stored.Manifest)
	if manifest.TenantKey != scope.TenantKey || manifest.Wire.DefinitionDigest != definitionDigest {
		return ResolvedRuntime{}, generationUnavailable("resolve_generation", definitionDigest)
	}
	if err := ValidateArtifactManifest(manifest); err != nil {
		return ResolvedRuntime{}, generationUnavailableWithCause("resolve_generation", definitionDigest, err)
	}
	return c.resolveCached(ctx, scope, manifest)
}

func (c *Coordinator) resolveCached(ctx context.Context, scope provider.Scope, manifest ArtifactManifest) (ResolvedRuntime, error) {
	key := cacheKey(scope.TenantKey, manifest.ManifestDigest)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ResolvedRuntime{}, &Error{Code: CodeClosed, Operation: "resolve_generation", Cause: ErrClosed}
	}
	if existing := c.cache[key]; existing != nil {
		ready := existing.ready
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return ResolvedRuntime{}, ctx.Err()
		case <-ready:
		}
		if existing.err != nil {
			return ResolvedRuntime{}, existing.err
		}
		if !cacheMatches(existing.value, manifest) {
			return ResolvedRuntime{}, generationUnavailable("resolve_generation", manifest.Wire.DefinitionDigest)
		}
		return existing.value.Clone(), nil
	}
	entry := &cacheEntry{ready: make(chan struct{})}
	c.cache[key] = entry
	c.mu.Unlock()

	value, err := c.reconstruct(ctx, scope, manifest)
	if err != nil {
		err = generationUnavailableWithCause("reconstruct", manifest.Wire.DefinitionDigest, err)
	}
	c.mu.Lock()
	entry.value = value.Clone()
	entry.err = err
	close(entry.ready)
	if err != nil {
		delete(c.cache, key)
	}
	c.mu.Unlock()
	if err != nil {
		return ResolvedRuntime{}, err
	}
	return value.Clone(), nil
}

func (c *Coordinator) reconstruct(ctx context.Context, scope provider.Scope, manifest ArtifactManifest) (ResolvedRuntime, error) {
	rebuilt, err := c.reconstructor.Reconstruct(ctx, scope, cloneManifest(manifest))
	if err != nil {
		return ResolvedRuntime{}, err
	}
	if rebuilt.Definition == nil || rebuilt.Definition.ArtifactVersions().Definition != manifest.Wire.DefinitionDigest {
		return ResolvedRuntime{}, ErrDefinitionInvalid
	}
	if !sameArtifacts(rebuilt.Artifacts, manifest.Wire.Artifacts) {
		return ResolvedRuntime{}, fmt.Errorf("%w: reconstructed artifact identities differ", ErrDefinitionInvalid)
	}
	if !sameMessages(rebuilt.SystemMessages, manifest.Wire.PromptMessages) {
		return ResolvedRuntime{}, fmt.Errorf("%w: reconstructed prompt differs", ErrDefinitionInvalid)
	}
	return runtimeFromManifest(manifest, rebuilt.Definition, rebuilt.SystemMessages), nil
}

func (c *Coordinator) runtimeFromBuild(tenant agent.TenantKey, built BuildResult) (ArtifactManifest, ResolvedRuntime, error) {
	if built.Definition == nil || !tenant.Valid() {
		return ArtifactManifest{}, ResolvedRuntime{}, definitionError("resolve", built.Wire.DefinitionDigest, "definition and tenant are required", nil)
	}
	digest := built.Definition.ArtifactVersions().Definition
	if digest == "" || built.Wire.DefinitionDigest != digest {
		return ArtifactManifest{}, ResolvedRuntime{}, definitionError("resolve", built.Wire.DefinitionDigest, "root definition digest mismatch", nil)
	}
	if !sameMessages(built.SystemMessages, built.Wire.PromptMessages) {
		return ArtifactManifest{}, ResolvedRuntime{}, definitionError("resolve", digest, "system messages differ from wire prompt", nil)
	}
	manifest, err := NewArtifactManifest(tenant, built.Wire, c.clock())
	if err != nil {
		return ArtifactManifest{}, ResolvedRuntime{}, err
	}
	return manifest, runtimeFromManifest(manifest, built.Definition, built.SystemMessages), nil
}

func runtimeFromManifest(manifest ArtifactManifest, definition *agent.RuntimeDefinition, messages []agent.Message) ResolvedRuntime {
	return ResolvedRuntime{
		Definition: definition, SystemMessages: cloneMessages(messages),
		SchemaVersion: manifest.Wire.SchemaVersion, DefinitionDigest: manifest.Wire.DefinitionDigest,
		ManifestDigest: manifest.ManifestDigest, RecipeVersion: manifest.Wire.RecipeVersion,
		CatalogGeneration: manifest.Wire.CatalogGeneration, OptionsVersion: manifest.Wire.OptionsVersion,
		Artifacts: append([]ArtifactRef(nil), manifest.Wire.Artifacts...),
	}
}

func normalizeRequest(request ResolveRequest) (ResolveRequest, error) {
	if !request.Selector.TenantKey.Valid() || strings.TrimSpace(request.Selector.AgentKey) == "" {
		return ResolveRequest{}, &Error{Code: CodeInvalidRequest, Operation: "resolve", Cause: ErrInvalidRequest}
	}
	if request.ModelRole == "" {
		request.ModelRole = provider.RolePrimary
	}
	if request.ModelRole != provider.RolePrimary && request.ModelRole != provider.RoleUtility && request.ModelRole != provider.RoleSummary && request.ModelRole != provider.RoleTitle {
		return ResolveRequest{}, &Error{Code: CodeInvalidRequest, Operation: "resolve", Cause: fmt.Errorf("%w: invalid role", ErrInvalidRequest)}
	}
	request.Selector.Values = append([]SelectorValue(nil), request.Selector.Values...)
	sort.Slice(request.Selector.Values, func(i, j int) bool {
		if request.Selector.Values[i].Key == request.Selector.Values[j].Key {
			return request.Selector.Values[i].Value < request.Selector.Values[j].Value
		}
		return request.Selector.Values[i].Key < request.Selector.Values[j].Key
	})
	for i, value := range request.Selector.Values {
		if strings.TrimSpace(value.Key) == "" || strings.TrimSpace(value.Value) == "" || (i > 0 && request.Selector.Values[i-1].Key == value.Key) {
			return ResolveRequest{}, &Error{Code: CodeInvalidRequest, Operation: "resolve", Cause: fmt.Errorf("%w: selector keys and values must be non-empty and unique", ErrInvalidRequest)}
		}
	}
	if err := validateOptions(request.RunOptions); err != nil {
		return ResolveRequest{}, &Error{Code: CodeInvalidRequest, Operation: "resolve", Cause: errors.Join(ErrInvalidRequest, err)}
	}
	request.RunOptions = cloneOptions(request.RunOptions)
	return request, nil
}

func (c *Coordinator) checkOpen() error {
	if c == nil {
		return &Error{Code: CodeClosed, Operation: "call", Cause: ErrClosed}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return &Error{Code: CodeClosed, Operation: "call", Cause: ErrClosed}
	}
	return nil
}

func (c *Coordinator) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.closed = true
	c.cache = make(map[string]*cacheEntry)
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) putCache(manifest ArtifactManifest, runtime ResolvedRuntime) {
	key := cacheKey(manifest.TenantKey, manifest.ManifestDigest)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	ready := make(chan struct{})
	close(ready)
	c.cache[key] = &cacheEntry{ready: ready, value: runtime.Clone()}
}

func cacheKey(tenant agent.TenantKey, digest string) string { return string(tenant) + "\x00" + digest }

func cacheMatches(value ResolvedRuntime, manifest ArtifactManifest) bool {
	return value.Definition != nil && value.DefinitionDigest == manifest.Wire.DefinitionDigest &&
		value.ManifestDigest == manifest.ManifestDigest && value.SchemaVersion == manifest.Wire.SchemaVersion &&
		sameArtifacts(value.Artifacts, manifest.Wire.Artifacts)
}

func sameMessages(left, right []agent.Message) bool {
	leftDigest, leftErr := agent.CanonicalDigest(left)
	rightDigest, rightErr := agent.CanonicalDigest(right)
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}

func definitionError(operation, generation, detail string, cause error) error {
	base := ErrDefinitionInvalid
	if cause != nil {
		base = errors.Join(base, cause)
	}
	if detail != "" {
		base = fmt.Errorf("%w: %s", base, detail)
	}
	return &Error{Code: CodeDefinitionInvalid, Operation: operation, Generation: generation, Cause: base}
}

func generationUnavailableWithCause(operation, generation string, cause error) error {
	return &Error{Code: CodeGenerationUnavailable, Operation: operation, Generation: generation, Retryable: true, Cause: errors.Join(ErrGenerationUnavailable, cause)}
}
