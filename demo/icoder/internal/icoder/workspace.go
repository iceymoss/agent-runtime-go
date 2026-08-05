package icoder

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const maxToolOutput = 64 << 10

type Workspace struct {
	root string
	mu   sync.RWMutex
	cwd  string
}

type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type CommandResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

func NewWorkspace(root string) (*Workspace, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("workspace must be a directory")
	}
	clean := filepath.Clean(real)
	return &Workspace{root: clean, cwd: clean}, nil
}

func (w *Workspace) Root() string { return w.root }

func (w *Workspace) WorkingDirectory() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cwd
}

func (w *Workspace) ChangeDirectory(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("directory is required")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	base := w.cwd
	if name == "/" {
		base, name = w.root, "."
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute paths are not allowed; use / to return to the workspace root")
	}
	path, err := filepath.EvalSymlinks(filepath.Join(base, filepath.Clean(name)))
	if err != nil {
		return "", err
	}
	if !within(w.root, path) {
		return "", fmt.Errorf("directory escapes workspace")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	w.cwd = path
	return path, nil
}

func (w *Workspace) ReadFile(ctx context.Context, name string) (string, error) {
	path, err := w.resolveExisting(name)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) > maxToolOutput {
		data = data[:maxToolOutput]
	}
	return string(data), nil
}

func (w *Workspace) Search(ctx context.Context, pattern string, limit int) ([]SearchMatch, error) {
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var matches []SearchMatch
	base := w.WorkingDirectory()
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != base && (strings.HasPrefix(entry.Name(), ".git") || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		fileMatches, scanErr := searchFile(path, base, pattern, limit-len(matches))
		matches = append(matches, fileMatches...)
		if scanErr != nil {
			return scanErr
		}
		if len(matches) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return matches, err
}

func (w *Workspace) ListFiles(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	files := make([]string, 0, limit)
	base := w.WorkingDirectory()
	err := filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != base && (entry.Name() == ".git" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(relative))
		if len(files) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (w *Workspace) RunCommand(ctx context.Context, program string, args []string, timeout time.Duration) (CommandResult, error) {
	if timeout <= 0 || timeout > 2*time.Minute {
		timeout = time.Minute
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, program, args...)
	command.Dir = w.WorkingDirectory()
	command.Env = append([]string(nil), os.Environ()...)
	output, err := command.CombinedOutput()
	if len(output) > maxToolOutput {
		output = output[:maxToolOutput]
	}
	result := CommandResult{Output: string(output)}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return CommandResult{}, err
}

func searchFile(path, root, pattern string, limit int) (matches []SearchMatch, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		if strings.Contains(scanner.Text(), pattern) {
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return nil, err
			}
			matches = append(matches, SearchMatch{Path: filepath.ToSlash(relative), Line: line, Text: scanner.Text()})
			if len(matches) >= limit {
				break
			}
		}
	}
	return matches, scanner.Err()
}

func (w *Workspace) WriteFile(ctx context.Context, name, content, expectedDigest string) (string, error) {
	path, err := w.resolveForWrite(name)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if expectedDigest != "" && digest(current) != expectedDigest {
		return "", fmt.Errorf("file changed: expected %s, got %s", expectedDigest, digest(current))
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return digest([]byte(content)), nil
}

func (w *Workspace) resolveExisting(name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	path, err := filepath.EvalSymlinks(filepath.Join(w.WorkingDirectory(), filepath.Clean(name)))
	if err != nil {
		return "", err
	}
	if !within(w.root, path) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return path, nil
}

func (w *Workspace) resolveForWrite(name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	joined := filepath.Join(w.WorkingDirectory(), filepath.Clean(name))
	parent, err := filepath.EvalSymlinks(filepath.Dir(joined))
	if err != nil {
		return "", err
	}
	if !within(w.root, parent) {
		return "", fmt.Errorf("path escapes workspace")
	}
	return filepath.Join(parent, filepath.Base(joined)), nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func digest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}
