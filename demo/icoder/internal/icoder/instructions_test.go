package icoder

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectInstructionsFollowWorkingDirectoryScope(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{
		"AGENTS.md":           "root rule",
		"pkg/AGENTS.md":       "package rule",
		"pkg/api/AGENTS.md":   "api rule",
		"pkg/other/AGENTS.md": "other rule",
	} {
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
	if _, err := workspace.ChangeDirectory("pkg/api"); err != nil {
		t.Fatal(err)
	}
	messages, err := workspace.ProjectInstructions(context.Background())
	if err != nil || len(messages) != 3 {
		t.Fatalf("ProjectInstructions() = %#v, %v", messages, err)
	}
	joined := messages[0].Text() + messages[1].Text() + messages[2].Text()
	for _, expected := range []string{"root rule", "package rule", "api rule", `scope="pkg/api/**"`} {
		if !strings.Contains(joined, expected) {
			t.Errorf("instructions omitted %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "other rule") {
		t.Fatalf("instructions included a sibling scope: %s", joined)
	}
}

func TestProjectInstructionsRejectSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.ProjectInstructions(context.Background()); err == nil {
		t.Fatal("ProjectInstructions() accepted a symbolic link")
	}
}
