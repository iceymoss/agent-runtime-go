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
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

type FileContent struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line"`
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated"`
	Digest     string `json:"digest"`
}

type PatchOperation struct {
	Operation      string `json:"operation"`
	Path           string `json:"path"`
	Content        string `json:"content,omitempty"`
	OldText        string `json:"old_text,omitempty"`
	NewText        string `json:"new_text,omitempty"`
	ExpectedDigest string `json:"expected_digest,omitempty"`
	ReplaceAll     bool   `json:"replace_all,omitempty"`
}

type PatchResult struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Digest    string `json:"digest,omitempty"`
}

type preparedPatch struct {
	operation PatchOperation
	path      string
	original  []byte
	updated   []byte
	mode      os.FileMode
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
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return "", fmt.Errorf("file appears to be binary or is not valid UTF-8")
	}
	if len(data) > maxToolOutput {
		data = data[:maxToolOutput]
		for !utf8.Valid(data) {
			data = data[:len(data)-1]
		}
	}
	return string(data), nil
}

func (w *Workspace) ReadFileLines(ctx context.Context, name string, offset, limit int) (FileContent, error) {
	path, err := w.resolveExisting(name)
	if err != nil {
		return FileContent{}, err
	}
	if err := ctx.Err(); err != nil {
		return FileContent{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return FileContent{}, err
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return FileContent{}, fmt.Errorf("file appears to be binary or is not valid UTF-8")
	}
	if offset <= 0 {
		offset = 1
	}
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	lines := strings.Split(string(data), "\n")
	start := min(offset-1, len(lines))
	end := min(start+limit, len(lines))
	selected := lines[start:end]
	for i := range selected {
		selected[i] = fmt.Sprintf("%d: %s", start+i+1, selected[i])
	}
	content := strings.Join(selected, "\n")
	truncated := end < len(lines)
	if len(content) > maxToolOutput {
		content = content[:maxToolOutput]
		for !utf8.ValidString(content) {
			content = content[:len(content)-1]
		}
		truncated = true
	}
	relative, _ := filepath.Rel(w.WorkingDirectory(), path)
	return FileContent{Path: filepath.ToSlash(relative), Content: content, StartLine: start + 1, EndLine: end, TotalLines: len(lines), Truncated: truncated, Digest: digest(data)}, nil
}

func (w *Workspace) Glob(ctx context.Context, pattern string, limit int) ([]string, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	base := w.WorkingDirectory()
	var files []string
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
		matched, err := globMatch(pattern, filepath.ToSlash(relative))
		if err != nil {
			return fmt.Errorf("invalid glob: %w", err)
		}
		if matched {
			files = append(files, filepath.ToSlash(relative))
		}
		if len(files) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (w *Workspace) SearchPattern(ctx context.Context, pattern, include string, regex bool, limit int) ([]SearchMatch, error) {
	if pattern == "" {
		return nil, fmt.Errorf("pattern is required")
	}
	var expression *regexp.Regexp
	if regex {
		var err error
		expression, err = regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regular expression: %w", err)
		}
	}
	if limit <= 0 || limit > 500 {
		limit = 100
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
			if path != base && (entry.Name() == ".git" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		if include != "" {
			matched, err := globMatch(include, filepath.ToSlash(relative))
			if err != nil {
				return fmt.Errorf("invalid include glob: %w", err)
			}
			if !matched {
				return nil
			}
		}
		fileMatches, err := searchFilePattern(path, base, pattern, expression, limit-len(matches))
		if err != nil {
			return err
		}
		matches = append(matches, fileMatches...)
		if len(matches) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	return matches, err
}

func (w *Workspace) Search(ctx context.Context, pattern string, limit int) ([]SearchMatch, error) {
	return w.SearchPattern(ctx, pattern, "", false, limit)
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
	return searchFilePattern(path, root, pattern, nil, limit)
}

func searchFilePattern(path, root, pattern string, expression *regexp.Regexp, limit int) (matches []SearchMatch, resultErr error) {
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
		lineText := scanner.Text()
		matched := strings.Contains(lineText, pattern)
		if expression != nil {
			matched = expression.MatchString(lineText)
		}
		if matched {
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

func (w *Workspace) EditFile(ctx context.Context, name, oldText, newText, expectedDigest string, replaceAll bool) (string, error) {
	if oldText == "" {
		return "", fmt.Errorf("old_text is required")
	}
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
	if expectedDigest != "" && digest(data) != expectedDigest {
		return "", fmt.Errorf("file changed: expected %s, got %s", expectedDigest, digest(data))
	}
	count := strings.Count(string(data), oldText)
	if count == 0 {
		return "", fmt.Errorf("old_text was not found")
	}
	if count > 1 && !replaceAll {
		return "", fmt.Errorf("old_text matched %d times; provide more context or set replace_all", count)
	}
	limit := 1
	if replaceAll {
		limit = -1
	}
	updated := strings.Replace(string(data), oldText, newText, limit)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return digest([]byte(updated)), nil
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

func (w *Workspace) MoveFile(ctx context.Context, source, destination, expectedDigest string) error {
	sourcePath, err := w.resolveForWrite(source)
	if err != nil {
		return err
	}
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("source must be a regular file")
	}
	destinationPath, err := w.resolveForWrite(destination)
	if err != nil {
		return err
	}
	if sourcePath == destinationPath {
		return fmt.Errorf("source and destination must be different")
	}
	if _, err := os.Lstat(destinationPath); err == nil {
		return fmt.Errorf("destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	if expectedDigest != "" && digest(data) != expectedDigest {
		return fmt.Errorf("file changed: expected %s, got %s", expectedDigest, digest(data))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(sourcePath, destinationPath)
}

func (w *Workspace) CreateDirectory(ctx context.Context, name string, parents bool) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", fmt.Errorf("a relative directory path is required")
	}
	path := filepath.Clean(filepath.Join(w.WorkingDirectory(), filepath.Clean(name)))
	if !within(w.root, path) || path == w.root {
		return "", fmt.Errorf("directory must be inside the workspace")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if parents {
		if err := w.createDirectories(path); err != nil {
			return "", err
		}
	} else if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("path is not a regular directory")
	}
	relative, err := filepath.Rel(w.root, path)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(relative), nil
}

func (w *Workspace) createDirectories(path string) error {
	relative, err := filepath.Rel(w.root, path)
	if err != nil {
		return err
	}
	current := w.root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path component %q is not a regular directory", component)
		}
	}
	return nil
}

func (w *Workspace) ApplyPatch(ctx context.Context, operations []PatchOperation) ([]PatchResult, error) {
	if len(operations) == 0 {
		return nil, fmt.Errorf("at least one patch operation is required")
	}
	if len(operations) > 100 {
		return nil, fmt.Errorf("at most 100 patch operations are allowed")
	}
	prepared := make([]preparedPatch, 0, len(operations))
	seen := make(map[string]struct{}, len(operations))
	for index, operation := range operations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item, err := w.preparePatch(operation)
		if err != nil {
			return nil, fmt.Errorf("operation %d (%s %q): %w", index+1, operation.Operation, operation.Path, err)
		}
		if _, exists := seen[item.path]; exists {
			return nil, fmt.Errorf("operation %d (%s %q): path is modified more than once", index+1, operation.Operation, operation.Path)
		}
		seen[item.path] = struct{}{}
		prepared = append(prepared, item)
	}

	applied := 0
	for index, item := range prepared {
		if err := ctx.Err(); err != nil {
			return nil, errors.Join(err, rollbackPatch(prepared[:applied]))
		}
		if err := validatePreparedPatch(item); err != nil {
			return nil, errors.Join(fmt.Errorf("operation %d (%s %q): %w", index+1, item.operation.Operation, item.operation.Path, err), rollbackPatch(prepared[:applied]))
		}
		var err error
		if item.operation.Operation == "delete" {
			err = os.Remove(item.path)
		} else {
			err = atomicWriteFile(item.path, item.updated, item.mode)
		}
		if err != nil {
			return nil, errors.Join(fmt.Errorf("operation %d (%s %q): %w", index+1, item.operation.Operation, item.operation.Path, err), rollbackPatch(prepared[:applied]))
		}
		applied++
	}

	results := make([]PatchResult, 0, len(prepared))
	for _, item := range prepared {
		result := PatchResult{Operation: item.operation.Operation, Path: item.operation.Path}
		if item.operation.Operation != "delete" {
			result.Digest = digest(item.updated)
		}
		results = append(results, result)
	}
	return results, nil
}

func validatePreparedPatch(item preparedPatch) error {
	current, err := os.ReadFile(item.path)
	if item.operation.Operation == "create" {
		if err == nil {
			return fmt.Errorf("file appeared after patch validation")
		}
		if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("file changed after patch validation: %w", err)
	}
	if digest(current) != digest(item.original) {
		return fmt.Errorf("file changed after patch validation")
	}
	return nil
}

func (w *Workspace) preparePatch(operation PatchOperation) (preparedPatch, error) {
	if operation.Path == "" {
		return preparedPatch{}, fmt.Errorf("path is required")
	}
	path, err := w.resolveForWrite(operation.Path)
	if err != nil {
		return preparedPatch{}, err
	}
	info, statErr := os.Lstat(path)
	if statErr != nil && !os.IsNotExist(statErr) {
		return preparedPatch{}, statErr
	}
	if statErr == nil && !info.Mode().IsRegular() {
		return preparedPatch{}, fmt.Errorf("path must be a regular file")
	}
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return preparedPatch{}, fmt.Errorf("symbolic links are not supported")
	}

	item := preparedPatch{operation: operation, path: path, mode: 0o644}
	switch operation.Operation {
	case "create":
		if statErr == nil {
			return preparedPatch{}, fmt.Errorf("file already exists")
		}
		item.updated = []byte(operation.Content)
	case "update":
		if os.IsNotExist(statErr) {
			return preparedPatch{}, fmt.Errorf("file does not exist")
		}
		item.original, err = os.ReadFile(path)
		if err != nil {
			return preparedPatch{}, err
		}
		item.mode = info.Mode().Perm()
		if operation.ExpectedDigest != "" && digest(item.original) != operation.ExpectedDigest {
			return preparedPatch{}, fmt.Errorf("file changed: expected %s, got %s", operation.ExpectedDigest, digest(item.original))
		}
		if operation.OldText == "" {
			return preparedPatch{}, fmt.Errorf("old_text is required for update")
		}
		count := strings.Count(string(item.original), operation.OldText)
		if count == 0 {
			return preparedPatch{}, fmt.Errorf("old_text was not found")
		}
		if count > 1 && !operation.ReplaceAll {
			return preparedPatch{}, fmt.Errorf("old_text matched %d times; provide more context or set replace_all", count)
		}
		limit := 1
		if operation.ReplaceAll {
			limit = -1
		}
		item.updated = []byte(strings.Replace(string(item.original), operation.OldText, operation.NewText, limit))
	case "delete":
		if os.IsNotExist(statErr) {
			return preparedPatch{}, fmt.Errorf("file does not exist")
		}
		item.original, err = os.ReadFile(path)
		if err != nil {
			return preparedPatch{}, err
		}
		item.mode = info.Mode().Perm()
		if operation.ExpectedDigest != "" && digest(item.original) != operation.ExpectedDigest {
			return preparedPatch{}, fmt.Errorf("file changed: expected %s, got %s", operation.ExpectedDigest, digest(item.original))
		}
	default:
		return preparedPatch{}, fmt.Errorf("operation must be create, update, or delete")
	}
	return item, nil
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) (resultErr error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".icoder-patch-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		if temporary != nil {
			resultErr = errors.Join(resultErr, temporary.Close())
		}
		if removeErr := os.Remove(temporaryPath); removeErr != nil && !os.IsNotExist(removeErr) {
			resultErr = errors.Join(resultErr, removeErr)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	temporary = nil
	return os.Rename(temporaryPath, path)
}

func rollbackPatch(applied []preparedPatch) error {
	var result error
	for index := len(applied) - 1; index >= 0; index-- {
		item := applied[index]
		if item.operation.Operation == "create" {
			if err := os.Remove(item.path); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, fmt.Errorf("rollback create %q: %w", item.operation.Path, err))
			}
			continue
		}
		if err := atomicWriteFile(item.path, item.original, item.mode); err != nil {
			result = errors.Join(result, fmt.Errorf("rollback %s %q: %w", item.operation.Operation, item.operation.Path, err))
		}
	}
	return result
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

func globMatch(pattern, name string) (bool, error) {
	patternParts := strings.Split(filepath.ToSlash(pattern), "/")
	nameParts := strings.Split(filepath.ToSlash(name), "/")
	var match func(int, int) (bool, error)
	match = func(patternIndex, nameIndex int) (bool, error) {
		if patternIndex == len(patternParts) {
			return nameIndex == len(nameParts), nil
		}
		if patternParts[patternIndex] == "**" {
			for next := nameIndex; next <= len(nameParts); next++ {
				matched, err := match(patternIndex+1, next)
				if err != nil || matched {
					return matched, err
				}
			}
			return false, nil
		}
		if nameIndex == len(nameParts) {
			return false, nil
		}
		matched, err := filepath.Match(patternParts[patternIndex], nameParts[nameIndex])
		if err != nil || !matched {
			return false, err
		}
		return match(patternIndex+1, nameIndex+1)
	}
	return match(0, 0)
}

func digest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}
