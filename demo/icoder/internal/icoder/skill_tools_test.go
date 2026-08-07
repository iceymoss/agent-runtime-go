package icoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/skills"
)

func TestSkillToolsLoadInstructionsOnDemand(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "review")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: review\ndescription: Review changes\nversion: 1.0.0\nschema_version: 1\n---\nRead callers before reviewing."
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	catalog, snapshot, messages, err := loadSkills(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close(context.Background()) }()
	if len(messages) != 1 || strings.Contains(messages[0].Text(), "Read callers") || !strings.Contains(messages[0].Text(), "Review changes") {
		t.Fatalf("skill metadata messages = %#v", messages)
	}
	service, err := NewPermissionService(false)
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewRegistry()
	if err := registerSkillTools(registry, catalog, snapshot, service, func() string { return "session" }); err != nil {
		t.Fatal(err)
	}
	tool, ok := registry.Get("load_skill")
	if !ok {
		t.Fatal("load_skill was not registered")
	}
	result, err := tool.Execute(context.Background(), agent.ToolInvocation{CallID: "load", Name: "load_skill", RawInput: `{"key":"review"}`})
	if err != nil || result.IsError || !strings.Contains(result.Content, "Read callers") || !strings.Contains(result.Content, "untrusted-skill") {
		t.Fatalf("load_skill result = %#v, %v", result, err)
	}
}

func TestSkillToolsAreAbsentWithoutCatalog(t *testing.T) {
	registry := agent.NewRegistry()
	service, err := NewPermissionService(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := registerSkillTools(registry, nil, skills.Snapshot{}, service, func() string { return "session" }); err != nil {
		t.Fatal(err)
	}
	if len(registry.Names()) != 0 {
		t.Fatalf("registered tools = %v", registry.Names())
	}
}
