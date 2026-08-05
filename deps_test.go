package agent

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentPackagesDoNotImportInternalPackages(t *testing.T) {
	for packagePath, imports := range agentPackageImports(t) {
		for _, dependency := range imports {
			if strings.HasPrefix(dependency, "github.com/iceymoss/agent-runtime-go/internal/") &&
				dependency != "github.com/iceymoss/agent-runtime-go/internal/jsoncodec" {
				t.Fatalf("%s imports internal package %s", packagePath, dependency)
			}
		}
	}
}

func TestRootAgentDoesNotImportChildPackages(t *testing.T) {
	for _, dependency := range agentPackageImports(t)["github.com/iceymoss/agent-runtime-go"] {
		if strings.HasPrefix(dependency, "github.com/iceymoss/agent-runtime-go/") &&
			dependency != "github.com/iceymoss/agent-runtime-go/internal/jsoncodec" {
			t.Fatalf("root agent imports child package %s", dependency)
		}
	}
}

func TestProductionAgentPackagesDoNotImportAgenttest(t *testing.T) {
	for packagePath, imports := range agentPackageImports(t) {
		if packagePath == "github.com/iceymoss/agent-runtime-go/agenttest" {
			continue
		}
		for _, dependency := range imports {
			if dependency == "github.com/iceymoss/agent-runtime-go/agenttest" {
				t.Fatalf("production package %s imports agenttest", packagePath)
			}
		}
	}
}

func agentPackageImports(t *testing.T) map[string][]string {
	t.Helper()
	cmd := exec.Command("go", "list", "-f", `{{.ImportPath}}|{{join .Imports ","}}`, "./...")
	cmd.Dir = moduleRoot(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list error: %v\n%s", err, output)
	}
	packages := make(map[string][]string)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		packagePath, rawImports, ok := strings.Cut(line, "|")
		if !ok {
			t.Fatalf("unexpected go list output %q", line)
		}
		if rawImports == "" {
			packages[packagePath] = nil
			continue
		}
		packages[packagePath] = strings.Split(rawImports, ",")
	}
	return packages
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() 报错: %v", err)
	}
	for {
		data, readErr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if readErr == nil && bytes.Contains(data, []byte("module github.com/iceymoss/agent-runtime-go")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("agent-runtime-go go.mod not found")
		}
		dir = parent
	}
}
