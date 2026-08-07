package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessRunsDeterministicCodingSuite(t *testing.T) {
	suite := CodingSuite(".")
	runner := RunnerFunc(func(_ context.Context, workspace, prompt string) error {
		switch {
		case strings.Contains(prompt, "Add"):
			return os.WriteFile(filepath.Join(workspace, "math.go"), []byte("package single\n\nfunc Add(left, right int) int { return left + right }\n"), 0o644)
		case strings.Contains(prompt, "Greeter"):
			if err := os.WriteFile(filepath.Join(workspace, "greeter.go"), []byte("package multi\n\nfunc Greet(name string) string { return \"Hello, \" + name }\n"), 0o644); err != nil {
				return err
			}
			return os.Remove(filepath.Join(workspace, "obsolete.txt"))
		case strings.Contains(prompt, "编译错误"):
			return os.WriteFile(filepath.Join(workspace, "broken.go"), []byte("package compilefix\n\nfunc Value() int { return 1 }\n"), 0o644)
		case strings.Contains(prompt, "OldName"):
			return os.WriteFile(filepath.Join(workspace, "name.go"), []byte("package rename\n\nfunc CurrentName() string { return \"current\" }\n"), 0o644)
		case strings.Contains(prompt, "Normalize"):
			return os.WriteFile(filepath.Join(workspace, "normalize_test.go"), []byte("package addtest\n\nimport \"testing\"\n\nfunc TestNormalizeWhitespace(t *testing.T) { if Normalize(\"   \") != \"\" { t.Fatal(\"unexpected value\") } }\n"), 0o644)
		case strings.Contains(prompt, "Timeout"):
			return os.WriteFile(filepath.Join(workspace, "config.go"), []byte("package config\n\ntype Config struct { Timeout int }\n\nfunc Default() Config { return Config{Timeout: 30} }\n"), 0o644)
		case strings.Contains(prompt, "REVIEW.md"):
			return os.WriteFile(filepath.Join(workspace, "REVIEW.md"), []byte("divide.go:3 can panic on division by zero.\n"), 0o644)
		case strings.Contains(prompt, "ARCHITECTURE.md"):
			return os.WriteFile(filepath.Join(workspace, "ARCHITECTURE.md"), []byte("Service delegates persistence to Store.\n"), 0o644)
		case strings.Contains(prompt, "AGENTS.md"):
			return os.WriteFile(filepath.Join(workspace, "pkg", "value.go"), []byte("package pkg\n\nfunc Value() int { return 7 }\n"), 0o644)
		default:
			return os.WriteFile(filepath.Join(workspace, "value.go"), []byte("package preserve\n\nfunc Value() int { return 42 }\n"), 0o644)
		}
	})
	report, err := (Harness{Runner: runner}).Run(context.Background(), suite)
	if err != nil || report.Passed != 10 || report.Failed != 0 {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
}

func TestHarnessReportsAssertionFailure(t *testing.T) {
	task := Task{ID: "failure", Prompt: "do nothing", Fixture: filepath.Join("testdata", "single-file"), Assertions: []Assertion{{Kind: "command", Program: "go", Args: []string{"test", "./..."}}}}
	report, err := (Harness{Runner: RunnerFunc(func(context.Context, string, string) error { return nil })}).Run(context.Background(), []Task{task})
	if err != nil || report.Passed != 0 || report.Failed != 1 || report.Results[0].Error == "" {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
}

func TestHarnessRejectsEscapingAssertions(t *testing.T) {
	task := Task{ID: "escape", Prompt: "none", Fixture: filepath.Join("testdata", "single-file"), Assertions: []Assertion{{Kind: "file_contains", Path: "../outside", Contains: "x"}}}
	report, err := (Harness{Runner: RunnerFunc(func(context.Context, string, string) error { return nil })}).Run(context.Background(), []Task{task})
	if err != nil || report.Failed != 1 || !strings.Contains(report.Results[0].Error, "escapes workspace") {
		t.Fatalf("Run() = %#v, %v", report, err)
	}
}
