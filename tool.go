package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

type ToolSuspensionKind string

const ToolSuspensionApproval ToolSuspensionKind = "approval"

type ToolSuspension struct {
	Kind         ToolSuspensionKind `json:"kind"`
	ExecutionKey string             `json:"execution_key,omitempty"`
	RequestRef   string             `json:"request_ref,omitempty"`
	ResumeToken  string             `json:"resume_token,omitempty"`
	Revision     uint64             `json:"revision,omitempty"`
}

type ToolSuspensionError struct {
	Suspension ToolSuspension
	Cause      error
}

func (e *ToolSuspensionError) Error() string { return "agent: tool execution suspended" }
func (e *ToolSuspensionError) Unwrap() error { return e.Cause }

func AsToolSuspension(err error) (ToolSuspension, bool) {
	var suspended *ToolSuspensionError
	if !errors.As(err, &suspended) || suspended == nil {
		return ToolSuspension{}, false
	}
	return suspended.Suspension, true
}

// ToolDefinition is the tool declaration sent to the model.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters is a JSON Schema in object form; adapters convert it to the
	// structure the upstream provider requires.
	Parameters map[string]any `json:"parameters"`
	// Strict rejects properties not declared by an object schema unless the
	// schema already specifies its own additionalProperties behavior.
	Strict bool `json:"strict,omitempty"`
}

// Tool is a tool the model can call.
// Implementations belong to the consuming application; the runtime only uses this interface.
type Tool interface {
	// Definition returns the model-visible declaration.
	Definition() ToolDefinition
	// ReplayPolicy declares recovery behavior after an ambiguous side effect.
	ReplayPolicy() ReplayPolicy
	// Execute returns IsError for model-correctable failures. A non-nil Go error
	// is fatal to the attempt and retains its cause.
	Execute(ctx context.Context, invocation ToolInvocation) (ToolResult, error)
}

// ExecutableVersioner identifies behavior that is not represented by a tool's
// model-visible definition or replay policy. Durable artifact builders require
// this capability so exact restoration cannot substitute a different binary
// implementation under a historical tool reference.
type ExecutableVersioner interface {
	ExecutableVersion() string
}

// LegacyExecutableVersion explicitly opts a tool into the ToolSet digest used
// before executable identities were introduced. It is only appropriate while
// restoring artifacts created by that implementation under the legacy format.
const LegacyExecutableVersion = "legacy"

type ToolInvocation struct {
	CallID       string `json:"call_id"`
	Name         string `json:"name"`
	RawInput     string `json:"raw_input"`
	ExecutionKey string `json:"execution_key,omitempty"`
}

type ReplayPolicy string

const (
	ReplayPolicyNever      ReplayPolicy = "never"
	ReplayPolicyIdempotent ReplayPolicy = "idempotent"
	ReplayPolicyResolve    ReplayPolicy = "resolve"
)

// Registry is a concurrency-safe tool registry. One Registry may be shared
// across sessions.
type Registry struct {
	mu    sync.RWMutex
	tools map[string]registeredTool
}

type registeredTool struct {
	tool                      Tool
	definition                ToolDefinition
	schema                    *jsonschema.Schema
	replayPolicy              ReplayPolicy
	executableVersion         string
	executableVersionDeclared bool
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]registeredTool)}
}

// Register registers a new tool and rejects duplicate names.
func (r *Registry) Register(t Tool) error {
	entry, err := prepareRegisteredTool(t)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name := entry.definition.Name
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("%w: duplicate tool %s", ErrAgentConfigInvalid, name)
	}
	r.tools[name] = entry
	return nil
}

// Replace explicitly replaces an existing tool. Missing names are rejected.
func (r *Registry) Replace(t Tool) error {
	entry, err := prepareRegisteredTool(t)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name := entry.definition.Name
	if _, exists := r.tools[name]; !exists {
		return fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}
	r.tools[name] = entry
	return nil
}

func prepareRegisteredTool(t Tool) (registeredTool, error) {
	if t == nil {
		return registeredTool{}, fmt.Errorf("%w: tool is nil", ErrAgentConfigInvalid)
	}
	definition, err := normalizeToolDefinition(t.Definition())
	if err != nil {
		return registeredTool{}, fmt.Errorf("%w: tool schema: %w", ErrAgentConfigInvalid, err)
	}
	name := definition.Name
	if name == "" {
		return registeredTool{}, fmt.Errorf("%w: tool name is empty", ErrAgentConfigInvalid)
	}
	schema, err := compileToolSchema(definition)
	if err != nil {
		return registeredTool{}, fmt.Errorf("%w: tool %s schema: %w", ErrAgentConfigInvalid, name, err)
	}
	executableVersion := ""
	versioned, declared := t.(ExecutableVersioner)
	if declared {
		executableVersion = strings.TrimSpace(versioned.ExecutableVersion())
	}
	return registeredTool{tool: t, definition: definition, schema: schema, replayPolicy: normalizeReplayPolicy(t.ReplayPolicy()), executableVersion: executableVersion, executableVersionDeclared: declared}, nil
}

func normalizeReplayPolicy(policy ReplayPolicy) ReplayPolicy {
	switch policy {
	case ReplayPolicyIdempotent, ReplayPolicyResolve:
		return policy
	default:
		return ReplayPolicyNever
	}
}

// Get returns a tool by name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.tools[name]
	return entry.tool, ok
}

func (r *Registry) validate(name, input string) error {
	r.mu.RLock()
	entry, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrToolNotFound, name)
	}
	return validateToolInput(entry, input)
}

func validateToolInput(entry registeredTool, input string) error {
	var value any
	if input == "" {
		input = "{}"
	}
	if err := json.Unmarshal([]byte(input), &value); err != nil {
		return fmt.Errorf("input is not a valid JSON object: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("input is not a valid JSON object")
	}
	if err := entry.schema.Validate(value); err != nil {
		return fmt.Errorf("input does not conform to the JSON Schema: %w", err)
	}
	return nil
}

func compileToolSchema(definition ToolDefinition) (*jsonschema.Schema, error) {
	schemaMap := cloneJSONMap(definition.Parameters)
	if _, ok := schemaMap["$schema"]; !ok {
		schemaMap["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	if err := compiler.AddResource("tool-schema.json", schemaMap); err != nil {
		return nil, err
	}
	return compiler.Compile("tool-schema.json")
}

func normalizeToolDefinition(definition ToolDefinition) (ToolDefinition, error) {
	definition.Parameters = cloneJSONMap(definition.Parameters)
	if definition.Parameters == nil {
		definition.Parameters = map[string]any{"type": "object"}
	}
	if err := normalizeSchemaNode(definition.Parameters, definition.Strict); err != nil {
		return ToolDefinition{}, err
	}
	return definition, nil
}

func normalizeSchemaNode(node any, strict bool) error {
	switch value := node.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok && ref != "" && !strings.HasPrefix(ref, "#") {
			return fmt.Errorf("external schema reference %q is not supported", ref)
		}
		if strict && schemaTypeContainsObject(value["type"]) {
			if _, exists := value["additionalProperties"]; !exists {
				value["additionalProperties"] = false
			}
		}
		for _, child := range value {
			if err := normalizeSchemaNode(child, strict); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := normalizeSchemaNode(child, strict); err != nil {
				return err
			}
		}
	}
	return nil
}

func schemaTypeContainsObject(value any) bool {
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

func cloneToolDefinition(definition ToolDefinition) ToolDefinition {
	definition.Parameters = cloneJSONMap(definition.Parameters)
	return definition
}

func cloneJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = cloneJSONValue(item)
	}
	return cloned
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			cloned[i] = cloneJSONValue(item)
		}
		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return value
	}
}

// Names returns the registered tool names, sorted for stable tests and logs.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolSet is an immutable snapshot of the tools available to an agent.
type ToolSet struct {
	tools   map[string]registeredTool
	version string
}

// Subset returns a tool set restricted to names already available in s.
func (s *ToolSet) Subset(names []string) (*ToolSet, error) {
	if names == nil {
		return s, nil
	}
	tools := make(map[string]registeredTool, len(names))
	for _, name := range names {
		entry, ok := s.tools[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrToolNotAllowed, name)
		}
		tools[name] = entry
	}
	return newToolSetSnapshot(tools)
}

func (s *ToolSet) validate(name, input string) error {
	if !s.Allowed(name) {
		return fmt.Errorf("%w: %s", ErrToolNotAllowed, name)
	}
	entry := s.tools[name]
	return validateToolInput(entry, input)
}

// NewToolSet builds a tool set from an allowlist.
// A nil allowed slice means no restriction; an empty slice allows no tools.
// A name that is allowlisted but not registered returns ErrToolNotFound:
// exposing the configuration error at assembly time beats letting the model
// hit an unavailable tool at runtime.
func NewToolSet(registry *Registry, allowed []string) (*ToolSet, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: registry is nil", ErrAgentConfigInvalid)
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if allowed == nil {
		tools := make(map[string]registeredTool, len(registry.tools))
		for name, entry := range registry.tools {
			tools[name] = entry
		}
		return newToolSetSnapshot(tools)
	}
	tools := make(map[string]registeredTool, len(allowed))
	for _, name := range allowed {
		entry, ok := registry.tools[name]
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrToolNotFound, name)
		}
		tools[name] = entry
	}
	return newToolSetSnapshot(tools)
}

func newToolSetSnapshot(tools map[string]registeredTool) (*ToolSet, error) {
	set := &ToolSet{tools: tools}
	type versionEntry struct {
		Definition   ToolDefinition `json:"definition"`
		ReplayPolicy ReplayPolicy   `json:"replay_policy"`
	}
	definitions := set.Definitions()
	entries := make([]versionEntry, len(definitions))
	usesExecutableVersions := false
	for i, definition := range definitions {
		entries[i] = versionEntry{Definition: definition, ReplayPolicy: tools[definition.Name].replayPolicy}
		if tools[definition.Name].executableVersion != "" && tools[definition.Name].executableVersion != LegacyExecutableVersion {
			usesExecutableVersions = true
		}
	}
	var versionValue any = entries
	if usesExecutableVersions {
		type executableVersionEntry struct {
			Definition        ToolDefinition `json:"definition"`
			ReplayPolicy      ReplayPolicy   `json:"replay_policy"`
			ExecutableVersion string         `json:"executable_version"`
		}
		executableEntries := make([]executableVersionEntry, len(definitions))
		for i, definition := range definitions {
			executableEntries[i] = executableVersionEntry{Definition: definition, ReplayPolicy: tools[definition.Name].replayPolicy, ExecutableVersion: tools[definition.Name].executableVersion}
		}
		versionValue = executableEntries
	}
	version, err := CanonicalDigest(versionValue)
	if err != nil {
		return nil, fmt.Errorf("%w: digest tool snapshot: %w", ErrAgentConfigInvalid, err)
	}
	set.version = version
	return set, nil
}

// Version returns the deterministic digest of definitions, replay policies,
// and declared executable implementation versions.
func (s *ToolSet) Version() string { return s.version }

// ValidateExecutableVersions rejects snapshots that cannot safely participate
// in durable exact restoration. Ordinary non-durable ToolSets may omit this
// optional capability.
func (s *ToolSet) ValidateExecutableVersions() error {
	if s == nil {
		return fmt.Errorf("%w: tool set is nil", ErrAgentConfigInvalid)
	}
	for name, entry := range s.tools {
		if !entry.executableVersionDeclared || entry.executableVersion == "" {
			return fmt.Errorf("%w: tool %s executable version is required for durable artifacts", ErrAgentConfigInvalid, name)
		}
	}
	return nil
}

func (s *ToolSet) replayPolicy(name string) ReplayPolicy {
	entry, ok := s.tools[name]
	if !ok {
		return ReplayPolicyNever
	}
	return entry.replayPolicy
}

// Allowed reports whether the tool is on the allowlist.
func (s *ToolSet) Allowed(name string) bool {
	_, ok := s.tools[name]
	return ok
}

// Get returns an allowlisted tool. If the tool exists but is not allowlisted,
// it returns ErrToolNotAllowed; callers feed a "tool unavailable" result back
// to the model instead of aborting the run.
func (s *ToolSet) Get(name string) (Tool, error) {
	if !s.Allowed(name) {
		return nil, fmt.Errorf("%w: %s", ErrToolNotAllowed, name)
	}
	return s.tools[name].tool, nil
}

// Definitions returns the tool declarations sent to the model, sorted by name
// so requests stay reproducible.
func (s *ToolSet) Definitions() []ToolDefinition {
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]ToolDefinition, 0, len(names))
	for _, name := range names {
		defs = append(defs, cloneToolDefinition(s.tools[name].definition))
	}
	return defs
}
