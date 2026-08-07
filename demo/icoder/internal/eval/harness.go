package eval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Runner interface {
	Run(context.Context, string, string) error
}

type RunnerFunc func(context.Context, string, string) error

func (f RunnerFunc) Run(ctx context.Context, workspace, prompt string) error {
	return f(ctx, workspace, prompt)
}

type Task struct {
	ID         string
	Prompt     string
	Fixture    string
	Assertions []Assertion
}

type Assertion struct {
	Kind     string
	Path     string
	Contains string
	Program  string
	Args     []string
}

type TaskResult struct {
	ID       string        `json:"id"`
	Passed   bool          `json:"passed"`
	Duration time.Duration `json:"duration"`
	Error    string        `json:"error,omitempty"`
}

type Report struct {
	Passed  int          `json:"passed"`
	Failed  int          `json:"failed"`
	Results []TaskResult `json:"results"`
}

type Harness struct {
	Runner   Runner
	TempRoot string
}

func (h Harness) Run(ctx context.Context, tasks []Task) (Report, error) {
	if h.Runner == nil {
		return Report{}, fmt.Errorf("eval runner is required")
	}
	if len(tasks) == 0 {
		return Report{}, fmt.Errorf("at least one eval task is required")
	}
	root, err := os.MkdirTemp(h.TempRoot, "icoder-eval-*")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(root)
	report := Report{Results: make([]TaskResult, 0, len(tasks))}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		result := TaskResult{ID: task.ID}
		started := time.Now()
		workspace := filepath.Join(root, safeTaskID(task.ID))
		err := copyFixture(task.Fixture, workspace)
		if err == nil {
			err = h.Runner.Run(ctx, workspace, task.Prompt)
		}
		if err == nil {
			err = evaluateAssertions(ctx, workspace, task.Assertions)
		}
		result.Duration = time.Since(started)
		result.Passed = err == nil
		if err != nil {
			result.Error = err.Error()
			report.Failed++
		} else {
			report.Passed++
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func copyFixture(source, destination string) error {
	if source == "" {
		return fmt.Errorf("fixture path is required")
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("fixture is not a directory")
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture contains symbolic link %q", path)
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func evaluateAssertions(ctx context.Context, workspace string, assertions []Assertion) error {
	var failures []error
	for index, assertion := range assertions {
		if err := evaluateAssertion(ctx, workspace, assertion); err != nil {
			failures = append(failures, fmt.Errorf("assertion %d: %w", index+1, err))
		}
	}
	return errors.Join(failures...)
}

func evaluateAssertion(ctx context.Context, workspace string, assertion Assertion) error {
	switch assertion.Kind {
	case "file_contains":
		path, err := fixturePath(workspace, assertion.Path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), assertion.Contains) {
			return fmt.Errorf("%s does not contain %q", assertion.Path, assertion.Contains)
		}
		return nil
	case "file_not_exists":
		path, err := fixturePath(workspace, assertion.Path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return fmt.Errorf("%s still exists", assertion.Path)
		}
		return nil
	case "file_not_contains":
		path, err := fixturePath(workspace, assertion.Path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), assertion.Contains) {
			return fmt.Errorf("%s still contains %q", assertion.Path, assertion.Contains)
		}
		return nil
	case "command":
		if assertion.Program == "" {
			return fmt.Errorf("assertion command program is required")
		}
		command := exec.CommandContext(ctx, assertion.Program, assertion.Args...)
		command.Dir = workspace
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s failed: %w: %s", assertion.Program, strings.Join(assertion.Args, " "), err, output)
		}
		return nil
	default:
		return fmt.Errorf("unknown assertion kind %q", assertion.Kind)
	}
}

func fixturePath(workspace, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("assertion path must be relative")
	}
	path := filepath.Join(workspace, filepath.Clean(name))
	relative, err := filepath.Rel(workspace, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("assertion path escapes workspace")
	}
	return path, nil
}

func safeTaskID(id string) string {
	var builder strings.Builder
	for _, value := range id {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '-' || value == '_' {
			builder.WriteRune(value)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "task"
	}
	return builder.String()
}
