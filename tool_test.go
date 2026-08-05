package agent

import (
	"context"
	"errors"
	"testing"
)

// fakeTool 是测试用工具：按 name 返回固定结果，可选返回错误。
type fakeTool struct {
	name           string
	desc           string
	executable     string
	result         ToolResult
	execErr        error
	calls          int
	lastArgs       string
	lastInvocation ToolInvocation
	replayPolicy   ReplayPolicy
}

func (f *fakeTool) ExecutableVersion() string { return f.executable }

func (f *fakeTool) Definition() ToolDefinition {
	return ToolDefinition{
		Name:        f.name,
		Description: f.desc,
		Parameters:  map[string]any{"type": "object"},
	}
}

func (f *fakeTool) ReplayPolicy() ReplayPolicy { return f.replayPolicy }

func (f *fakeTool) Execute(_ context.Context, invocation ToolInvocation) (ToolResult, error) {
	f.calls++
	f.lastArgs = invocation.RawInput
	f.lastInvocation = invocation
	if f.execErr != nil {
		return ToolResult{}, f.execErr
	}
	res := f.result
	res.Name = f.name
	return res, nil
}

func TestRegistryRegister(t *testing.T) {
	tests := []struct {
		name    string
		tool    Tool
		wantErr error
	}{
		{name: "正常注册", tool: &fakeTool{name: "score_round"}},
		{name: "工具名为空", tool: &fakeTool{name: ""}, wantErr: ErrAgentConfigInvalid},
		{name: "工具为 nil", tool: nil, wantErr: ErrAgentConfigInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry()
			err := r.Register(tt.tool)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Register() error = %v, 期望 %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Register() 意外报错: %v", err)
			}
			if _, ok := r.Get("score_round"); !ok {
				t.Fatal("注册后 Get() 取不到工具")
			}
		})
	}
}

func TestRegistryRejectsDuplicateAndReplacesExplicitly(t *testing.T) {
	registry := NewRegistry()
	original := &fakeTool{name: "score_round", desc: "original"}
	replacement := &fakeTool{name: "score_round", desc: "replacement"}
	if err := registry.Register(original); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(replacement); !errors.Is(err, ErrAgentConfigInvalid) {
		t.Fatalf("duplicate Register() error = %v", err)
	}
	got, ok := registry.Get("score_round")
	if !ok || got != original {
		t.Fatalf("Get() = %#v, want original", got)
	}
	if err := registry.Replace(&fakeTool{name: "missing"}); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("missing Replace() error = %v", err)
	}
	if err := registry.Replace(replacement); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if got, ok = registry.Get("score_round"); !ok || got != replacement {
		t.Fatalf("Get() = %#v, want replacement", got)
	}
	if names := registry.Names(); len(names) != 1 || names[0] != "score_round" {
		t.Fatalf("Names() = %v", names)
	}
}

type sliceSchemaTool struct{ parameters map[string]any }

func (t *sliceSchemaTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "slice_schema", Parameters: t.parameters}
}
func (*sliceSchemaTool) ReplayPolicy() ReplayPolicy { return ReplayPolicyNever }
func (*sliceSchemaTool) Execute(context.Context, ToolInvocation) (ToolResult, error) {
	return ToolResult{}, nil
}

func TestToolSetDefinitionsDeepCopySchemaSlices(t *testing.T) {
	required := []any{"value"}
	examples := []any{"one"}
	registry := NewRegistry()
	if err := registry.Register(&sliceSchemaTool{parameters: map[string]any{
		"type": "object", "required": required, "examples": examples,
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
	}}); err != nil {
		t.Fatal(err)
	}
	set, err := NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	required[0], examples[0] = "mutated", "mutated"
	definition := set.Definitions()[0]
	if definition.Parameters["required"].([]any)[0] != "value" || definition.Parameters["examples"].([]any)[0] != "one" {
		t.Fatalf("caller slices mutated definition: %#v", definition.Parameters)
	}
	definition.Parameters["required"].([]any)[0] = "returned"
	definition.Parameters["examples"].([]any)[0] = "returned"
	again := set.Definitions()[0]
	if again.Parameters["required"].([]any)[0] != "value" || again.Parameters["examples"].([]any)[0] != "one" {
		t.Fatalf("Definitions() leaked slices: %#v", again.Parameters)
	}
}

func TestToolSetVersionIsDeterministicAndSnapshotScoped(t *testing.T) {
	registry := NewRegistry()
	for _, name := range []string{"second", "first"} {
		if err := registry.Register(&fakeTool{name: name, desc: name}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewToolSet(registry, []string{"second", "first"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Version() == "" || first.Version() != second.Version() {
		t.Fatalf("versions = %q, %q", first.Version(), second.Version())
	}
	if err := registry.Replace(&fakeTool{name: "first", desc: "changed"}); err != nil {
		t.Fatal(err)
	}
	changed, err := NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Version() == first.Version() || first.Definitions()[0].Description != "first" {
		t.Fatalf("snapshot versions = old %q, changed %q", first.Version(), changed.Version())
	}
	if err := registry.Replace(&fakeTool{name: "first", desc: "changed", replayPolicy: ReplayPolicyIdempotent}); err != nil {
		t.Fatal(err)
	}
	policyChanged, err := NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if policyChanged.Version() == changed.Version() {
		t.Fatal("replay policy did not change snapshot version")
	}
}

func TestToolSetVersionIncludesExecutableImplementation(t *testing.T) {
	versions := make([]string, 0, 2)
	for _, executable := range []string{"v1", "v2"} {
		registry := NewRegistry()
		if err := registry.Register(&fakeTool{name: "lookup", desc: "same", executable: executable}); err != nil {
			t.Fatal(err)
		}
		tools, err := NewToolSet(registry, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tools.ValidateExecutableVersions(); err != nil {
			t.Fatalf("ValidateExecutableVersions() error = %v", err)
		}
		versions = append(versions, tools.Version())
	}
	if versions[0] == versions[1] {
		t.Fatalf("executable versions share ToolSet version %q", versions[0])
	}
}

func TestToolSetValidateExecutableVersionsRejectsMissingOrEmpty(t *testing.T) {
	tests := []struct {
		name string
		tool Tool
	}{
		{name: "missing capability", tool: unversionedTool{}},
		{name: "empty version", tool: &fakeTool{name: "lookup"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := NewRegistry()
			if err := registry.Register(tt.tool); err != nil {
				t.Fatal(err)
			}
			tools, err := NewToolSet(registry, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := tools.ValidateExecutableVersions(); !errors.Is(err, ErrAgentConfigInvalid) {
				t.Fatalf("ValidateExecutableVersions() error = %v, want ErrAgentConfigInvalid", err)
			}
		})
	}
}

type unversionedTool struct{}

func (unversionedTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "lookup", Parameters: map[string]any{"type": "object"}}
}
func (unversionedTool) ReplayPolicy() ReplayPolicy { return ReplayPolicyNever }
func (unversionedTool) Execute(context.Context, ToolInvocation) (ToolResult, error) {
	return ToolResult{}, nil
}

func TestToolSetLegacyExecutableVersionRetainsLegacyDigest(t *testing.T) {
	registry := NewRegistry()
	tool := &fakeTool{name: "lookup", desc: "same", executable: LegacyExecutableVersion}
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	tools, err := NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest, err := CanonicalDigest([]struct {
		Definition   ToolDefinition `json:"definition"`
		ReplayPolicy ReplayPolicy   `json:"replay_policy"`
	}{{Definition: tool.Definition(), ReplayPolicy: ReplayPolicyNever}})
	if err != nil {
		t.Fatal(err)
	}
	if tools.Version() != legacyDigest {
		t.Fatalf("legacy ToolSet version = %q, want historical %q", tools.Version(), legacyDigest)
	}
}

func TestRegistryNamesSorted(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"gen_options", "score_round", "aaa"} {
		if err := r.Register(&fakeTool{name: name}); err != nil {
			t.Fatalf("Register(%s) 报错: %v", name, err)
		}
	}
	got := r.Names()
	want := []string{"aaa", "gen_options", "score_round"}
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, 期望 %v", got, want)
		}
	}
}

func TestNewToolSetWhitelist(t *testing.T) {
	newRegistry := func(t *testing.T) *Registry {
		t.Helper()
		r := NewRegistry()
		for _, name := range []string{"score_round", "gen_options", "danger"} {
			if err := r.Register(&fakeTool{name: name}); err != nil {
				t.Fatalf("Register(%s) 报错: %v", name, err)
			}
		}
		return r
	}

	tests := []struct {
		name        string
		allowed     []string
		wantErr     error
		allowedName string
		wantAllowed bool
		wantDefs    int
	}{
		{
			name:        "白名单内的工具可用",
			allowed:     []string{"score_round", "gen_options"},
			allowedName: "score_round",
			wantAllowed: true,
			wantDefs:    2,
		},
		{
			name:        "白名单外的工具不可用",
			allowed:     []string{"score_round"},
			allowedName: "danger",
			wantAllowed: false,
			wantDefs:    1,
		},
		{
			name:        "nil 白名单不限制",
			allowed:     nil,
			allowedName: "danger",
			wantAllowed: true,
			wantDefs:    3,
		},
		{
			name:        "空白名单禁用全部工具",
			allowed:     []string{},
			allowedName: "score_round",
			wantAllowed: false,
			wantDefs:    0,
		},
		{
			name:    "白名单引用未注册的工具即装配失败",
			allowed: []string{"not_registered"},
			wantErr: ErrToolNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, err := NewToolSet(newRegistry(t), tt.allowed)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("NewToolSet() error = %v, 期望 %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewToolSet() 意外报错: %v", err)
			}
			if got := set.Allowed(tt.allowedName); got != tt.wantAllowed {
				t.Errorf("Allowed(%s) = %v, 期望 %v", tt.allowedName, got, tt.wantAllowed)
			}
			if got := len(set.Definitions()); got != tt.wantDefs {
				t.Errorf("len(Definitions()) = %d, 期望 %d", got, tt.wantDefs)
			}
		})
	}
}

func TestToolSetGetNotAllowed(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&fakeTool{name: "danger"}); err != nil {
		t.Fatalf("Register 报错: %v", err)
	}
	if err := r.Register(&fakeTool{name: "score_round"}); err != nil {
		t.Fatalf("Register 报错: %v", err)
	}
	set, err := NewToolSet(r, []string{"score_round"})
	if err != nil {
		t.Fatalf("NewToolSet 报错: %v", err)
	}

	if _, err := set.Get("danger"); !errors.Is(err, ErrToolNotAllowed) {
		t.Fatalf("Get(danger) error = %v, 期望 ErrToolNotAllowed", err)
	}
	if _, err := set.Get("score_round"); err != nil {
		t.Fatalf("Get(score_round) 意外报错: %v", err)
	}
}

func TestMessageAccessors(t *testing.T) {
	msg := Message{
		Role: RoleAssistant,
		Parts: []ContentPart{
			{Type: PartText, Text: "先"},
			{Type: PartToolCall, ToolCall: &ToolCall{ID: "c1", Name: "score_round", Input: `{"a":1}`}},
			{Type: PartText, Text: "后"},
			{Type: PartToolCall, ToolCall: &ToolCall{ID: "c2", Name: "gen_options"}},
		},
	}
	if got := msg.Text(); got != "先后" {
		t.Errorf("Text() = %q, 期望 %q", got, "先后")
	}
	calls := msg.ToolCalls()
	if len(calls) != 2 || calls[0].ID != "c1" || calls[1].Name != "gen_options" {
		t.Errorf("ToolCalls() = %+v, 期望两个调用 c1/gen_options", calls)
	}

	toolMsg := NewToolMessage(
		ToolResult{ToolCallID: "c1", Name: "score_round", Content: "ok"},
		ToolResult{ToolCallID: "c2", Name: "gen_options", IsError: true},
	)
	if toolMsg.Role != RoleTool {
		t.Errorf("NewToolMessage().Role = %q, 期望 %q", toolMsg.Role, RoleTool)
	}
	results := toolMsg.ToolResults()
	if len(results) != 2 || results[0].ToolCallID != "c1" || !results[1].IsError {
		t.Errorf("ToolResults() = %+v, 期望保留两条结果与 IsError", results)
	}
}

func TestUsageAdd(t *testing.T) {
	u := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	u.Add(Usage{PromptTokens: 3, CompletionTokens: 7, TotalTokens: 10})
	want := Usage{PromptTokens: 13, CompletionTokens: 12, TotalTokens: 25}
	if u != want {
		t.Errorf("Add() = %+v, 期望 %+v", u, want)
	}
}
