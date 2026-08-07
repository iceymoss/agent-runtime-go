package icoder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceConfinementAndOperations(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	content, err := workspace.ReadFile(context.Background(), "main.go")
	if err != nil || content == "" {
		t.Fatalf("ReadFile() = %q, %v", content, err)
	}
	matches, err := workspace.Search(context.Background(), "func main", 10)
	if err != nil || len(matches) != 1 || matches[0].Path != "main.go" {
		t.Fatalf("Search() = %#v, %v", matches, err)
	}
	if _, err := workspace.ReadFile(context.Background(), "../outside"); err == nil {
		t.Fatal("ReadFile() allowed workspace escape")
	}
	before := digest([]byte(content))
	after, err := workspace.WriteFile(context.Background(), "main.go", "package main\n", before)
	if err != nil || after == before {
		t.Fatalf("WriteFile() = %q, %v", after, err)
	}
	if _, err := workspace.WriteFile(context.Background(), "main.go", "stale", before); err == nil {
		t.Fatal("WriteFile() accepted stale digest")
	}
}

func TestSearchHonorsCancellation(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = workspace.Search(ctx, "x", 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %v", err)
	}
}

func TestWorkspaceListAndCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	files, err := workspace.ListFiles(context.Background(), 10)
	if err != nil || len(files) != 1 || files[0] != "go.mod" {
		t.Fatalf("ListFiles() = %#v, %v", files, err)
	}
	result, err := workspace.RunCommand(context.Background(), "go", []string{"env", "GOMOD"}, 0)
	if err != nil || result.ExitCode != 0 || result.Output == "" {
		t.Fatalf("RunCommand() = %#v, %v", result, err)
	}
}

func TestWorkspaceChangeDirectoryAffectsRelativeTools(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "pkg", "agent")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdir, "agent.go"), []byte("package agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := workspace.ChangeDirectory("pkg/agent")
	if err != nil || changed != subdir || workspace.WorkingDirectory() != subdir {
		t.Fatalf("ChangeDirectory() = %q, %v", changed, err)
	}
	content, err := workspace.ReadFile(context.Background(), "agent.go")
	if err != nil || content != "package agent\n" {
		t.Fatalf("ReadFile() = %q, %v", content, err)
	}
	files, err := workspace.ListFiles(context.Background(), 10)
	if err != nil || len(files) != 1 || files[0] != "agent.go" {
		t.Fatalf("ListFiles() = %#v, %v", files, err)
	}
	if _, err := workspace.ChangeDirectory("../../.."); err == nil {
		t.Fatal("ChangeDirectory() allowed workspace escape")
	}
	changed, err = workspace.ChangeDirectory("/")
	if err != nil || changed != root {
		t.Fatalf("ChangeDirectory(/) = %q, %v", changed, err)
	}
}

func TestWorkspaceCodeAgentOperations(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pkg", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "pkg", "api", "handler.go")
	original := "package api\n\nfunc Handle() string {\n\treturn \"old\"\n}\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	files, err := workspace.Glob(context.Background(), "**/*.go", 10)
	if err != nil || len(files) != 1 || files[0] != "pkg/api/handler.go" {
		t.Fatalf("Glob() = %#v, %v", files, err)
	}
	matches, err := workspace.SearchPattern(context.Background(), `func\s+Handle`, "**/*.go", true, 10)
	if err != nil || len(matches) != 1 || matches[0].Line != 3 {
		t.Fatalf("SearchPattern() = %#v, %v", matches, err)
	}
	content, err := workspace.ReadFileLines(context.Background(), "pkg/api/handler.go", 3, 2)
	if err != nil || content.StartLine != 3 || content.EndLine != 4 || !content.Truncated || !strings.Contains(content.Content, "3: func Handle") {
		t.Fatalf("ReadFileLines() = %#v, %v", content, err)
	}
	updatedDigest, err := workspace.EditFile(context.Background(), "pkg/api/handler.go", `return "old"`, `return "new"`, content.Digest, false)
	if err != nil || updatedDigest == content.Digest {
		t.Fatalf("EditFile() = %q, %v", updatedDigest, err)
	}
	if _, err := workspace.EditFile(context.Background(), "pkg/api/handler.go", `return "new"`, `return "stale"`, content.Digest, false); err == nil {
		t.Fatal("EditFile() accepted stale digest")
	}
}

func TestWorkspaceApplyPatch(t *testing.T) {
	root := t.TempDir()
	original := "package sample\n\nconst value = \"old\"\n"
	deleteContent := "obsolete\n"
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(original), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "obsolete.txt"), []byte(deleteContent), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	results, err := workspace.ApplyPatch(context.Background(), []PatchOperation{
		{Operation: "update", Path: "sample.go", OldText: `"old"`, NewText: `"new"`, ExpectedDigest: digest([]byte(original))},
		{Operation: "create", Path: "created.txt", Content: "created\n"},
		{Operation: "delete", Path: "obsolete.txt", ExpectedDigest: digest([]byte(deleteContent))},
	})
	if err != nil || len(results) != 3 || results[0].Digest == "" || results[2].Digest != "" {
		t.Fatalf("ApplyPatch() = %#v, %v", results, err)
	}
	updated, err := os.ReadFile(filepath.Join(root, "sample.go"))
	if err != nil || !strings.Contains(string(updated), `"new"`) {
		t.Fatalf("updated file = %q, %v", updated, err)
	}
	info, err := os.Stat(filepath.Join(root, "sample.go"))
	if err != nil || info.Mode().Perm() != 0o750 {
		t.Fatalf("updated mode = %v, %v", info.Mode().Perm(), err)
	}
	created, err := os.ReadFile(filepath.Join(root, "created.txt"))
	if err != nil || string(created) != "created\n" {
		t.Fatalf("created file = %q, %v", created, err)
	}
	if _, err := os.Stat(filepath.Join(root, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted file stat error = %v", err)
	}
}

func TestWorkspaceApplyPatchPreflightIsAtomic(t *testing.T) {
	root := t.TempDir()
	original := "original\n"
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	_, err = workspace.ApplyPatch(context.Background(), []PatchOperation{
		{Operation: "create", Path: "new.txt", Content: "new\n"},
		{Operation: "update", Path: "existing.txt", OldText: "missing", NewText: "changed"},
	})
	if err == nil || !strings.Contains(err.Error(), "old_text was not found") {
		t.Fatalf("ApplyPatch() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("preflight created a partial file: %v", err)
	}
	current, err := os.ReadFile(filepath.Join(root, "existing.txt"))
	if err != nil || string(current) != original {
		t.Fatalf("existing file = %q, %v", current, err)
	}
}

func TestWorkspaceApplyPatchRejectsUnsafeOperations(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "existing.txt"), []byte("value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		operations []PatchOperation
	}{
		{name: "workspace escape", operations: []PatchOperation{{Operation: "create", Path: "../outside.txt", Content: "outside"}}},
		{name: "stale digest", operations: []PatchOperation{{Operation: "delete", Path: "existing.txt", ExpectedDigest: digest([]byte("stale"))}}},
		{name: "duplicate path", operations: []PatchOperation{{Operation: "update", Path: "existing.txt", OldText: "value", NewText: "one"}, {Operation: "delete", Path: "existing.txt"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := workspace.ApplyPatch(context.Background(), test.operations); err == nil {
				t.Fatal("ApplyPatch() accepted unsafe operations")
			}
		})
	}
	current, err := os.ReadFile(filepath.Join(root, "existing.txt"))
	if err != nil || string(current) != "value\n" {
		t.Fatalf("existing file = %q, %v", current, err)
	}
}
