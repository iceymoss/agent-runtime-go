package icoder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	if err != nil || result.ExitCode != 0 || result.Stdout == "" {
		t.Fatalf("RunCommand() = %#v, %v", result, err)
	}
}

func TestWorkspaceRunCommandReturnsStructuredOutputAndCWD(t *testing.T) {
	root := t.TempDir()
	subdirectory := filepath.Join(root, "module")
	if err := os.MkdirAll(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subdirectory, "go.mod"), []byte("module example.com/nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := workspace.RunCommandIn(context.Background(), "go", []string{"env", "GOMOD"}, "module", 30*time.Second)
	if err != nil || result.ExitCode != 0 || result.Stderr != "" || !strings.Contains(result.Stdout, "module/go.mod") || result.TimedOut || result.StdoutTruncated || result.StderrTruncated {
		t.Fatalf("RunCommandIn() = %#v, %v", result, err)
	}
	failure, err := workspace.RunCommand(context.Background(), "go", []string{"env", "-invalid-flag"}, 30*time.Second)
	if err != nil || failure.ExitCode == 0 || failure.Stderr == "" {
		t.Fatalf("RunCommand() failure = %#v, %v", failure, err)
	}
	if _, err := workspace.RunCommandIn(context.Background(), "go", []string{"env", "GOMOD"}, "../outside", 30*time.Second); err == nil {
		t.Fatal("RunCommandIn() allowed cwd outside the workspace")
	}
}

func TestLimitedBufferReportsTruncation(t *testing.T) {
	buffer := &limitedBuffer{limit: 5}
	written, err := buffer.Write([]byte("123456789"))
	if err != nil || written != 9 || buffer.buffer.String() != "12345" || !buffer.truncated {
		t.Fatalf("limitedBuffer.Write() = %d, %q, %t, %v", written, buffer.buffer.String(), buffer.truncated, err)
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
	if err != nil || content.StartLine != 3 || content.EndLine != 4 || content.TotalLines != 6 || !content.Truncated || !strings.Contains(content.Content, "3: func Handle") {
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

func TestWorkspaceReadFileLinesReportsCompleteMetadata(t *testing.T) {
	root := t.TempDir()
	data := []byte("first\nsecond\nthird")
	if err := os.WriteFile(filepath.Join(root, "text.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	content, err := workspace.ReadFileLines(context.Background(), "text.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if content.Path != "text.txt" || content.StartLine != 1 || content.EndLine != 3 || content.TotalLines != 3 || content.Truncated || content.Digest != digest(data) || content.Content != "1: first\n2: second\n3: third" {
		t.Fatalf("ReadFileLines() = %#v", content)
	}
}

func TestWorkspaceReadFileLinesRejectsBinaryAndInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"binary.dat":  {0, 1, 2},
		"invalid.txt": {0xff, 0xfe},
	} {
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := workspace.ReadFileLines(context.Background(), name, 0, 0); err == nil {
			t.Fatalf("ReadFileLines(%q) accepted non-text content", name)
		}
	}
}

func TestWorkspaceReadFileLinesReportsByteTruncation(t *testing.T) {
	root := t.TempDir()
	data := []byte(strings.Repeat("界", maxToolOutput))
	if err := os.WriteFile(filepath.Join(root, "large.txt"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	content, err := workspace.ReadFileLines(context.Background(), "large.txt", 0, 0)
	if err != nil || !content.Truncated || !utf8.ValidString(content.Content) || len(content.Content) > maxToolOutput {
		t.Fatalf("ReadFileLines() length = %d, truncated = %t, error = %v", len(content.Content), content.Truncated, err)
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

func TestWorkspaceMoveFileAndCreateDirectory(t *testing.T) {
	root := t.TempDir()
	content := []byte("package source\n")
	if err := os.WriteFile(filepath.Join(root, "source.go"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := workspace.CreateDirectory(context.Background(), "pkg/nested", true)
	if err != nil || directory != "pkg/nested" {
		t.Fatalf("CreateDirectory() = %q, %v", directory, err)
	}
	if err := workspace.MoveFile(context.Background(), "source.go", "pkg/nested/destination.go", digest(content)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "source.go")); !os.IsNotExist(err) {
		t.Fatalf("source stat error = %v", err)
	}
	moved, err := os.ReadFile(filepath.Join(root, "pkg", "nested", "destination.go"))
	if err != nil || string(moved) != string(content) {
		t.Fatalf("moved file = %q, %v", moved, err)
	}
	info, err := os.Stat(filepath.Join(root, "pkg", "nested", "destination.go"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("moved mode = %v, %v", info.Mode().Perm(), err)
	}
}

func TestWorkspaceMoveFileRejectsUnsafeChanges(t *testing.T) {
	root := t.TempDir()
	content := []byte("source\n")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "destination.txt"), []byte("destination\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		source      string
		destination string
		digest      string
	}{
		{name: "stale digest", source: "source.txt", destination: "renamed.txt", digest: digest([]byte("stale"))},
		{name: "existing destination", source: "source.txt", destination: "destination.txt", digest: digest(content)},
		{name: "workspace escape", source: "source.txt", destination: "../outside.txt", digest: digest(content)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := workspace.MoveFile(context.Background(), test.source, test.destination, test.digest); err == nil {
				t.Fatal("MoveFile() accepted an unsafe change")
			}
		})
	}
	current, err := os.ReadFile(filepath.Join(root, "source.txt"))
	if err != nil || string(current) != string(content) {
		t.Fatalf("source file = %q, %v", current, err)
	}
}

func TestWorkspaceCreateDirectoryRejectsSymlinkAndEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.CreateDirectory(context.Background(), "linked/nested", true); err == nil {
		t.Fatal("CreateDirectory() traversed a symbolic link")
	}
	if _, err := workspace.CreateDirectory(context.Background(), "../outside", true); err == nil {
		t.Fatal("CreateDirectory() escaped the workspace")
	}
}

func TestWorkspaceDiscoveryHonorsIgnoreFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		".gitignore":        "ignored/\n*.log\n!important.log\n/root.txt\n",
		".ignore":           "generated.txt\n",
		"keep.go":           "package keep\n",
		"ignored/hidden.go": "package hidden\n",
		"debug.log":         "hidden token\n",
		"important.log":     "visible token\n",
		"root.txt":          "hidden token\n",
		"generated.txt":     "hidden token\n",
		"nested/root.txt":   "visible token\n",
		".icoder.db":        "hidden token\n",
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	discovered, err := workspace.ListFiles(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	discoveredSet := make(map[string]bool, len(discovered))
	for _, name := range discovered {
		discoveredSet[name] = true
	}
	for _, expected := range []string{".gitignore", ".ignore", "important.log", "keep.go", "nested/root.txt"} {
		if !discoveredSet[expected] {
			t.Errorf("ListFiles() omitted %q: %v", expected, discovered)
		}
	}
	for _, excluded := range []string{"ignored/hidden.go", "debug.log", "root.txt", "generated.txt", ".icoder.db"} {
		if discoveredSet[excluded] {
			t.Errorf("ListFiles() included %q: %v", excluded, discovered)
		}
	}
	matches, err := workspace.Search(context.Background(), "token", 100)
	if err != nil || len(matches) != 2 || matches[0].Path != "important.log" || matches[1].Path != "nested/root.txt" {
		t.Fatalf("Search() = %#v, %v", matches, err)
	}
}

func TestWorkspaceSearchSkipsBinaryLargeAndSymlinkFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), []byte{'m', 'a', 't', 'c', 'h', 0}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(strings.Repeat("match", maxSearchFile)), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("match\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked.txt")); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := workspace.Search(context.Background(), "match", 100)
	if err != nil || len(matches) != 0 {
		t.Fatalf("Search() = %#v, %v", matches, err)
	}
}
