package skills_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iceymoss/agent-runtime-go/skills"
)

func TestFilesystemLoadsDeclaredContentAndRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "safe-skill")
	if err := os.Mkdir(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	artifact := []byte("artifact")
	digest := sha256.Sum256(artifact)
	manifest := "---\nname: safe-skill\ndescription: safe\nversion: 1.0.0\nschema_version: 1\nresources:\n  - key: guide\n    path: guide.txt\n    mime_type: text/plain\n    size_bytes: 8\n    digest: " + hex.EncodeToString(digest[:]) + "\n---\nuntrusted instructions"
	writeFile(t, filepath.Join(skillDir, "SKILL.md"), []byte(manifest))
	writeFile(t, filepath.Join(skillDir, "guide.txt"), artifact)
	source := skills.NewFilesystemSource(skills.FilesystemOptions{Name: "platform", Kind: skills.SourcePlatformFilesystem, Root: root, Generation: "fs1"})
	snapshot, err := source.Load(context.Background(), skills.Scope{TenantKey: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Skills) != 1 {
		t.Fatalf("skills = %d, diagnostics=%#v", len(snapshot.Skills), snapshot.Diagnostics)
	}
	if snapshot.Skills[0].Descriptor.ContentDigest == "" || snapshot.Skills[0].Descriptor.InstructionsDigest == "" {
		t.Fatal("content digests missing")
	}

	badDir := filepath.Join(root, "bad-skill")
	if err := os.Mkdir(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(manifest, "name: safe-skill", "name: bad-skill", 1)
	bad = strings.Replace(bad, "path: guide.txt", "path: ../safe-skill/guide.txt", 1)
	writeFile(t, filepath.Join(badDir, "SKILL.md"), []byte(bad))
	snapshot, err = source.Load(context.Background(), skills.Scope{TenantKey: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Skills) != 1 || !containsDiagnostic(snapshot.Diagnostics, skills.CodeResourceEscape) {
		t.Fatalf("traversal was not isolated: %#v", snapshot)
	}
}

func TestFilesystemReadDetectsSourceReplacement(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "stable")
	if err := os.Mkdir(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(skillDir, "SKILL.md")
	writeFile(t, manifestPath, []byte("---\nname: stable\ndescription: stable\nversion: 1.0.0\nschema_version: 1\n---\noriginal"))
	source := skills.NewFilesystemSource(skills.FilesystemOptions{Name: "platform", Kind: skills.SourcePlatformFilesystem, Root: root, Generation: "fs1"})
	catalog, err := skills.NewCatalog(skills.Options{Sources: []skills.SourceRegistration{{Source: source, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	scope := skills.Scope{TenantKey: "tenant"}
	if err := catalog.StartScope(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := catalog.Current(scope)
	writeFile(t, manifestPath, []byte("---\nname: stable\ndescription: stable\nversion: 1.0.0\nschema_version: 1\n---\nchanged"))
	_, err = catalog.Read(context.Background(), skills.ReadRequest{Scope: scope, Generation: snapshot.Generation, Skill: "stable", Version: "1.0.0"})
	if !errors.Is(err, skills.ErrResourceChanged) {
		t.Fatalf("changed source read error = %v", err)
	}
}

func TestFilesystemRejectsSymlinkEscapeMalformedAndOversized(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	writeFile(t, outside, []byte("outside"))
	symlinkDir := filepath.Join(root, "linked-skill")
	if err := os.Mkdir(symlinkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: linked-skill\ndescription: linked\nversion: 1.0.0\nschema_version: 1\nresources:\n  - key: secret\n    path: secret.txt\n    mime_type: text/plain\n    size_bytes: 7\n    digest: 31207a206db033e32c7a67bb33f6e47004c3f2bd97f386b9f3f9e3f6f1742d32\n---\ntext"
	writeFile(t, filepath.Join(symlinkDir, "SKILL.md"), []byte(manifest))
	if err := os.Symlink(outside, filepath.Join(symlinkDir, "secret.txt")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	malformedDir := filepath.Join(root, "malformed")
	if err := os.Mkdir(malformedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(malformedDir, "SKILL.md"), []byte("not frontmatter"))
	largeDir := filepath.Join(root, "large")
	if err := os.Mkdir(largeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(largeDir, "SKILL.md"), []byte("---\nname: large\ndescription: large\nversion: 1.0.0\nschema_version: 1\n---\n"+strings.Repeat("x", 128)))
	source := skills.NewFilesystemSource(skills.FilesystemOptions{Name: "platform", Kind: skills.SourcePlatformFilesystem, Root: root, Limits: skills.Limits{MaxManifestBytes: 512, MaxInstructionsBytes: 32}})
	snapshot, err := source.Load(context.Background(), skills.Scope{TenantKey: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Skills) != 0 {
		t.Fatalf("unsafe skills loaded: %#v", snapshot.Skills)
	}
	if !containsDiagnostic(snapshot.Diagnostics, skills.CodeResourceEscape) || !containsDiagnostic(snapshot.Diagnostics, skills.CodeDescriptorInvalid) || !containsDiagnostic(snapshot.Diagnostics, skills.CodeResourceTooLarge) {
		t.Fatalf("diagnostics = %#v", snapshot.Diagnostics)
	}
}

func TestFilesystemCanonicalizesConfiguredSymlinkRootAndRejectsInvalidUTF8(t *testing.T) {
	realRoot := t.TempDir()
	parent := t.TempDir()
	linkedRoot := filepath.Join(parent, "skills")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	source := skills.NewFilesystemSource(skills.FilesystemOptions{Name: "platform", Kind: skills.SourcePlatformFilesystem, Root: linkedRoot})
	if _, err := source.Load(context.Background(), skills.Scope{TenantKey: "tenant"}); err != nil {
		t.Fatalf("canonical root load: %v", err)
	}
	dir := filepath.Join(realRoot, "invalid")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "SKILL.md"), append([]byte("---\nname: invalid\ndescription: invalid\nversion: 1.0.0\nschema_version: 1\n---\n"), 0xff))
	direct := skills.NewFilesystemSource(skills.FilesystemOptions{Name: "platform", Kind: skills.SourcePlatformFilesystem, Root: realRoot})
	snapshot, err := direct.Load(context.Background(), skills.Scope{TenantKey: "tenant"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsDiagnostic(snapshot.Diagnostics, skills.CodeDescriptorInvalid) {
		t.Fatalf("diagnostics = %#v", snapshot.Diagnostics)
	}
}

func writeFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
func containsDiagnostic(values []skills.Diagnostic, code skills.Code) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}
