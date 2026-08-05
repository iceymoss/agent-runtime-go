package tool

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/iceymoss/agent-runtime-go"
	jsoncodec "github.com/iceymoss/agent-runtime-go/internal/jsoncodec"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Registry struct {
	mu    sync.Mutex
	tools map[string]registration
}

type registration struct {
	tool             agent.Tool
	definition       agent.ToolDefinition
	definitionDigest string
	schema           *jsonschema.Schema
	metadata         Metadata
}

func NewRegistry() *Registry { return &Registry{tools: make(map[string]registration)} }

func (r *Registry) Register(tool agent.Tool, metadata Metadata) error {
	entry, err := validateRegistration(tool, metadata)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[entry.definition.Name]; exists {
		return lifecycleError(ErrInvalidConfiguration, nil, "register", "", "duplicate tool name")
	}
	r.tools[entry.definition.Name] = entry
	return nil
}

type Generation struct {
	digest       string
	tools        map[string]registration
	interceptors []Interceptor
}

func (r *Registry) Freeze(interceptors ...Interceptor) (*Generation, error) {
	seen := make(map[string]struct{}, len(interceptors))
	chain := make([]Interceptor, len(interceptors))
	versions := make([]string, len(interceptors))
	for index, interceptor := range interceptors {
		if interceptor == nil || strings.TrimSpace(interceptor.Name()) == "" || strings.TrimSpace(interceptor.Version()) == "" {
			return nil, lifecycleError(ErrInvalidConfiguration, nil, "freeze", "", "interceptor name and version are required")
		}
		if _, exists := seen[interceptor.Name()]; exists {
			return nil, lifecycleError(ErrInvalidConfiguration, nil, "freeze", "", "duplicate interceptor name")
		}
		seen[interceptor.Name()] = struct{}{}
		chain[index] = interceptor
		versions[index] = interceptor.Name() + "@" + interceptor.Version()
	}
	r.mu.Lock()
	tools := make(map[string]registration, len(r.tools))
	for name, entry := range r.tools {
		entry.definition = cloneDefinition(entry.definition)
		entry.tool = frozenTool{implementation: entry.tool, definition: entry.definition, replay: entry.metadata.ReplayPolicy, executableVersion: entry.metadata.Version}
		tools[name] = entry
	}
	r.mu.Unlock()
	names := make([]string, 0, len(tools))
	for name := range tools {
		names = append(names, name)
	}
	sort.Strings(names)
	type digestEntry struct {
		Name       string   `json:"name"`
		Definition string   `json:"definition"`
		Metadata   Metadata `json:"metadata"`
	}
	entries := make([]digestEntry, 0, len(names))
	for _, name := range names {
		entry := tools[name]
		entries = append(entries, digestEntry{Name: name, Definition: entry.definitionDigest, Metadata: entry.metadata})
	}
	digest, err := agent.CanonicalDigest(struct {
		Tools        []digestEntry `json:"tools"`
		Interceptors []string      `json:"interceptors"`
	}{entries, versions})
	if err != nil {
		return nil, lifecycleError(ErrInvalidConfiguration, err, "freeze", "", "generation digest failed")
	}
	return &Generation{digest: digest, tools: tools, interceptors: chain}, nil
}

func (g *Generation) Digest() string { return g.digest }

func (g *Generation) Definitions() []agent.ToolDefinition {
	names := make([]string, 0, len(g.tools))
	for name := range g.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	definitions := make([]agent.ToolDefinition, 0, len(names))
	for _, name := range names {
		definitions = append(definitions, cloneDefinition(g.tools[name].definition))
	}
	return definitions
}

func (g *Generation) Tool(name string) (agent.Tool, bool) {
	entry, ok := g.tools[name]
	return entry.tool, ok
}

type frozenTool struct {
	implementation    agent.Tool
	definition        agent.ToolDefinition
	replay            agent.ReplayPolicy
	executableVersion string
}

func (t frozenTool) Definition() agent.ToolDefinition { return cloneDefinition(t.definition) }
func (t frozenTool) ReplayPolicy() agent.ReplayPolicy { return t.replay }
func (t frozenTool) ExecutableVersion() string        { return t.executableVersion }
func (t frozenTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	return t.implementation.Execute(ctx, invocation)
}

func validateRegistration(tool agent.Tool, metadata Metadata) (registration, error) {
	if tool == nil {
		return registration{}, lifecycleError(ErrInvalidConfiguration, nil, "register", "", "nil tool")
	}
	definition := cloneDefinition(tool.Definition())
	if strings.TrimSpace(definition.Name) == "" || strings.TrimSpace(metadata.Version) == "" || strings.TrimSpace(metadata.SchemaVersion) == "" {
		return registration{}, lifecycleError(ErrInvalidConfiguration, nil, "register", "", "tool name, version, and schema version are required")
	}
	if definition.Parameters == nil {
		definition.Parameters = map[string]any{"type": "object"}
	}
	if err := normalizeSchema(definition.Parameters, definition.Strict); err != nil {
		return registration{}, lifecycleError(ErrInvalidConfiguration, err, "register", "", "invalid tool schema")
	}
	if metadata.ReplayPolicy == "" {
		metadata.ReplayPolicy = tool.ReplayPolicy()
	}
	if metadata.Concurrency == "" {
		metadata.Concurrency = ConcurrencySequential
	}
	if metadata.EffectClass == "" {
		metadata.EffectClass = EffectExternal
	}
	if metadata.Idempotency == "" {
		metadata.Idempotency = IdempotencyNone
	}
	if !validMetadata(metadata) {
		return registration{}, lifecycleError(ErrInvalidConfiguration, nil, "register", "", "invalid effect, replay, or concurrency declaration")
	}
	schemaMap := cloneDefinition(definition).Parameters
	if _, exists := schemaMap["$schema"]; !exists {
		schemaMap["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("schema.json", schemaMap); err != nil {
		return registration{}, lifecycleError(ErrInvalidConfiguration, err, "register", "", "invalid tool schema")
	}
	schema, err := compiler.Compile("schema.json")
	if err != nil {
		return registration{}, lifecycleError(ErrInvalidConfiguration, err, "register", "", "invalid tool schema")
	}
	digest, err := agent.CanonicalDigest(definition)
	if err != nil {
		return registration{}, lifecycleError(ErrInvalidConfiguration, err, "register", "", "definition digest failed")
	}
	return registration{tool: tool, definition: definition, definitionDigest: digest, schema: schema, metadata: metadata}, nil
}

func normalizeSchema(value any, strict bool) error {
	switch typed := value.(type) {
	case map[string]any:
		if reference, ok := typed["$ref"].(string); ok && reference != "" && !strings.HasPrefix(reference, "#") {
			return fmt.Errorf("external schema reference %q is not supported", reference)
		}
		if strict && containsObjectType(typed["type"]) {
			if _, exists := typed["additionalProperties"]; !exists {
				typed["additionalProperties"] = false
			}
		}
		for _, child := range typed {
			if err := normalizeSchema(child, strict); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := normalizeSchema(child, strict); err != nil {
				return err
			}
		}
	}
	return nil
}

func containsObjectType(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed == "object"
	case []any:
		for _, item := range typed {
			if item == "object" {
				return true
			}
		}
	case []string:
		for _, item := range typed {
			if item == "object" {
				return true
			}
		}
	}
	return false
}

func validMetadata(metadata Metadata) bool {
	validConcurrency := metadata.Concurrency == ConcurrencySequential || metadata.Concurrency == ConcurrencyParallel || metadata.Concurrency == ConcurrencyExclusive
	validEffect := metadata.EffectClass == EffectNone || metadata.EffectClass == EffectRead || metadata.EffectClass == EffectWrite || metadata.EffectClass == EffectExternal
	validReplay := metadata.ReplayPolicy == agent.ReplayPolicyNever || metadata.ReplayPolicy == agent.ReplayPolicyIdempotent || metadata.ReplayPolicy == agent.ReplayPolicyResolve
	validIdempotency := metadata.Idempotency == IdempotencyNone || metadata.Idempotency == IdempotencyExecutionKey
	if metadata.EffectClass != EffectNone && strings.TrimSpace(metadata.Action) == "" {
		return false
	}
	if metadata.ReplayPolicy == agent.ReplayPolicyIdempotent && metadata.Idempotency != IdempotencyExecutionKey {
		return false
	}
	return validConcurrency && validEffect && validReplay && validIdempotency
}

func canonicalInput(entry registration, input string) (string, string, error) {
	if input == "" {
		input = "{}"
	}
	var value any
	if err := jsoncodec.Unmarshal([]byte(input), &value); err != nil {
		return "", "", lifecycleError(ErrToolInputInvalid, err, "validate", "", "input is not a JSON object")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "", "", lifecycleError(ErrToolInputInvalid, nil, "validate", "", "input is not a JSON object")
	}
	if err := entry.schema.Validate(object); err != nil {
		return "", "", lifecycleError(ErrToolInputInvalid, err, "validate", "", "input does not match schema")
	}
	canonical, err := jsoncodec.MarshalString(object)
	if err != nil {
		return "", "", lifecycleError(ErrToolInputInvalid, err, "canonicalize", "", "input cannot be canonicalized")
	}
	digest, err := agent.CanonicalDigest(object)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrToolInputInvalid, err)
	}
	return canonical, digest, nil
}
