package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ToolDefinition 是发给模型的工具声明。
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Parameters JSON Schema（对象形式），由适配器转成上游要求的结构。
	Parameters map[string]any `json:"parameters"`
	// Strict rejects properties not declared by an object schema unless the
	// schema already specifies its own additionalProperties behavior.
	Strict bool `json:"strict,omitempty"`
}

// Tool 是一个可被模型调用的工具。
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

// Registry 是工具注册表，支持并发读写（内核可能在多会话间共享一个 Registry）。
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

// NewRegistry 创建空注册表。
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
		return registeredTool{}, fmt.Errorf("%w: 工具为 nil", ErrAgentConfigInvalid)
	}
	definition, err := normalizeToolDefinition(t.Definition())
	if err != nil {
		return registeredTool{}, fmt.Errorf("%w: tool schema: %w", ErrAgentConfigInvalid, err)
	}
	name := definition.Name
	if name == "" {
		return registeredTool{}, fmt.Errorf("%w: 工具名为空", ErrAgentConfigInvalid)
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

// Get 按名字取工具。
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
		return fmt.Errorf("参数不是合法 JSON 对象: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("参数不是合法 JSON 对象")
	}
	if err := entry.schema.Validate(value); err != nil {
		return fmt.Errorf("参数不符合 JSON Schema: %w", err)
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

// Names 返回已注册的工具名（排序，便于测试与日志稳定）。
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

// NewToolSet 按白名单构造工具集。
// allowed 为 nil 时不限制；为空切片时表示不允许任何工具。
// 白名单里列了但注册表没有的工具直接返回 ErrToolNotFound —— 装配期暴露配置错误，
// 比运行期让模型撞上「工具不可用」要好。
func NewToolSet(registry *Registry, allowed []string) (*ToolSet, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: registry 为 nil", ErrAgentConfigInvalid)
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

// Allowed 判断工具是否在白名单内。
func (s *ToolSet) Allowed(name string) bool {
	_, ok := s.tools[name]
	return ok
}

// Get 取白名单内的工具。工具存在但不在白名单时返回 ErrToolNotAllowed，
// 调用方据此回灌「工具不可用」而不是中断整轮。
func (s *ToolSet) Get(name string) (Tool, error) {
	if !s.Allowed(name) {
		return nil, fmt.Errorf("%w: %s", ErrToolNotAllowed, name)
	}
	return s.tools[name].tool, nil
}

// Definitions 返回发给模型的工具声明列表（按名字排序，保证请求可复现）。
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
