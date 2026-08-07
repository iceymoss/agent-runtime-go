package icoder

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
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

const (
	maxToolOutput = 64 << 10
	maxSearchFile = 2 << 20
)

var defaultIgnoredDirectories = map[string]struct{}{
	".git": {}, ".idea": {}, ".venv": {}, "node_modules": {}, "vendor": {},
}

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
	ExitCode        int    `json:"exit_code"`
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	TimedOut        bool   `json:"timed_out"`
	StdoutTruncated bool   `json:"stdout_truncated"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

type GitCommitResult struct {
	Commit  string   `json:"commit"`
	Message string   `json:"message"`
	Paths   []string `json:"paths"`
	Output  string   `json:"output,omitempty"`
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (w *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := w.limit - w.buffer.Len()
	if remaining <= 0 {
		w.truncated = w.truncated || originalLength > 0
		return originalLength, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		w.truncated = true
	}
	_, _ = w.buffer.Write(data)
	return originalLength, nil
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

type ignorePattern struct {
	pattern   string
	negated   bool
	directory bool
	rooted    bool
}

type ignoreMatcher struct{ patterns []ignorePattern }

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
	ignored, err := w.loadIgnoreMatcher()
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != base && isDefaultIgnoredDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			if path != base {
				workspaceRelative, err := filepath.Rel(w.root, path)
				if err != nil {
					return err
				}
				if ignored.Match(filepath.ToSlash(workspaceRelative), true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		workspaceRelative, err := filepath.Rel(w.root, path)
		if err != nil {
			return err
		}
		if ignored.Match(filepath.ToSlash(workspaceRelative), false) {
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
	ignored, err := w.loadIgnoreMatcher()
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != base && isDefaultIgnoredDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			if path != base {
				workspaceRelative, err := filepath.Rel(w.root, path)
				if err != nil {
					return err
				}
				if ignored.Match(filepath.ToSlash(workspaceRelative), true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		workspaceRelative, err := filepath.Rel(w.root, path)
		if err != nil {
			return err
		}
		if ignored.Match(filepath.ToSlash(workspaceRelative), false) {
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
	ignored, err := w.loadIgnoreMatcher()
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(base, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != base && isDefaultIgnoredDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			if path != base {
				workspaceRelative, err := filepath.Rel(w.root, path)
				if err != nil {
					return err
				}
				if ignored.Match(filepath.ToSlash(workspaceRelative), true) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		workspaceRelative, err := filepath.Rel(w.root, path)
		if err != nil {
			return err
		}
		if ignored.Match(filepath.ToSlash(workspaceRelative), false) {
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
	return w.RunCommandIn(ctx, program, args, "", timeout)
}

func (w *Workspace) RunCommandIn(ctx context.Context, program string, args []string, directory string, timeout time.Duration) (CommandResult, error) {
	if timeout <= 0 || timeout > 2*time.Minute {
		timeout = time.Minute
	}
	workingDirectory := w.WorkingDirectory()
	if directory != "" && directory != "." {
		resolved, err := w.resolveExisting(directory)
		if err != nil {
			return CommandResult{}, err
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return CommandResult{}, err
		}
		if !info.IsDir() {
			return CommandResult{}, fmt.Errorf("command cwd is not a directory")
		}
		workingDirectory = resolved
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, program, args...)
	command.Dir = workingDirectory
	command.Env = append([]string(nil), os.Environ()...)
	command.Env = append(command.Env, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "GIT_PAGER=cat", "PAGER=cat")
	stdout := &limitedBuffer{limit: maxToolOutput}
	stderr := &limitedBuffer{limit: maxToolOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	result := CommandResult{ExitCode: 0, Stdout: stdout.buffer.String(), Stderr: stderr.buffer.String(), StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated}
	if err == nil {
		return result, nil
	}
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		result.ExitCode = -1
		result.TimedOut = true
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return CommandResult{}, err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return CommandResult{}, err
}

func (w *Workspace) GitCommit(ctx context.Context, message string, paths []string) (GitCommitResult, error) {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 200 || strings.ContainsAny(message, "\r\n") {
		return GitCommitResult{}, fmt.Errorf("commit message must be one non-empty line of at most 200 bytes")
	}
	for _, r := range message {
		if r < 32 || r == 127 {
			return GitCommitResult{}, fmt.Errorf("commit message contains control characters")
		}
	}
	if len(paths) == 0 || len(paths) > 100 {
		return GitCommitResult{}, fmt.Errorf("git commit requires 1 to 100 explicit paths")
	}
	topResult, err := w.RunCommand(ctx, "git", []string{"rev-parse", "--show-toplevel"}, 30*time.Second)
	if err != nil {
		return GitCommitResult{}, err
	}
	if topResult.ExitCode != 0 {
		return GitCommitResult{}, commandFailure("resolve repository", topResult)
	}
	repository := strings.TrimSpace(topResult.Stdout)
	if repository == "" || !filepath.IsAbs(repository) || !within(repository, w.root) {
		return GitCommitResult{}, fmt.Errorf("workspace is not inside the resolved Git repository")
	}
	requested := make(map[string]struct{}, len(paths))
	normalized := make([]string, 0, len(paths))
	for _, name := range paths {
		repositoryPath, pathErr := w.gitCommitPath(repository, name)
		if pathErr != nil {
			return GitCommitResult{}, pathErr
		}
		if _, duplicate := requested[repositoryPath]; duplicate {
			continue
		}
		requested[repositoryPath] = struct{}{}
		normalized = append(normalized, repositoryPath)
	}
	sort.Strings(normalized)
	staged, err := w.gitStagedPaths(ctx)
	if err != nil {
		return GitCommitResult{}, err
	}
	if unexpected := unexpectedPaths(staged, requested); len(unexpected) > 0 {
		return GitCommitResult{}, fmt.Errorf("refusing to include unrelated staged paths: %s", strings.Join(unexpected, ", "))
	}
	addArgs := []string{"add", "--"}
	for _, name := range normalized {
		addArgs = append(addArgs, ":(top,literal)"+name)
	}
	addResult, err := w.RunCommand(ctx, "git", addArgs, 30*time.Second)
	if err != nil {
		return GitCommitResult{}, err
	}
	if addResult.ExitCode != 0 {
		return GitCommitResult{}, commandFailure("stage paths", addResult)
	}
	staged, err = w.gitStagedPaths(ctx)
	if err != nil {
		return GitCommitResult{}, err
	}
	if len(staged) == 0 {
		return GitCommitResult{}, fmt.Errorf("none of the requested paths contain changes to commit")
	}
	if unexpected := unexpectedPaths(staged, requested); len(unexpected) > 0 {
		return GitCommitResult{}, fmt.Errorf("staged paths changed concurrently; refusing to commit: %s", strings.Join(unexpected, ", "))
	}
	commitArgs := []string{"commit", "--only", "-m", message, "--"}
	for _, name := range normalized {
		commitArgs = append(commitArgs, ":(top,literal)"+name)
	}
	commitResult, err := w.RunCommand(ctx, "git", commitArgs, 2*time.Minute)
	if err != nil {
		return GitCommitResult{}, err
	}
	if commitResult.ExitCode != 0 {
		return GitCommitResult{}, fmt.Errorf("%w; requested paths remain staged", commandFailure("create commit", commitResult))
	}
	headResult, err := w.RunCommand(ctx, "git", []string{"rev-parse", "--verify", "HEAD"}, 30*time.Second)
	if err != nil {
		return GitCommitResult{}, err
	}
	if headResult.ExitCode != 0 {
		return GitCommitResult{}, commandFailure("read commit identity", headResult)
	}
	return GitCommitResult{Commit: strings.TrimSpace(headResult.Stdout), Message: message, Paths: staged, Output: strings.TrimSpace(commitResult.Stdout + commitResult.Stderr)}, nil
}

func (w *Workspace) gitCommitPath(repository, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || name == "." || strings.HasPrefix(name, ":") || strings.ContainsAny(name, "*?[") {
		return "", fmt.Errorf("commit paths must be explicit relative workspace paths: %q", name)
	}
	path := filepath.Clean(filepath.Join(w.WorkingDirectory(), filepath.Clean(name)))
	if !within(w.root, path) || path == w.root {
		return "", fmt.Errorf("commit path escapes workspace: %q", name)
	}
	parent := filepath.Dir(path)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve commit path %q: %w", name, err)
	}
	if !within(w.root, resolvedParent) {
		return "", fmt.Errorf("commit path escapes workspace through a symlink: %q", name)
	}
	relative, err := filepath.Rel(repository, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("commit path is outside the Git repository: %q", name)
	}
	return filepath.ToSlash(relative), nil
}

func (w *Workspace) gitStagedPaths(ctx context.Context) ([]string, error) {
	result, err := w.RunCommand(ctx, "git", []string{"diff", "--cached", "--name-only", "-z"}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure("inspect staged paths", result)
	}
	parts := strings.Split(result.Stdout, "\x00")
	paths := make([]string, 0, len(parts))
	for _, path := range parts {
		if path != "" {
			paths = append(paths, filepath.ToSlash(path))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func unexpectedPaths(paths []string, allowed map[string]struct{}) []string {
	var unexpected []string
	for _, path := range paths {
		if _, ok := allowed[path]; !ok {
			unexpected = append(unexpected, path)
		}
	}
	return unexpected
}

func commandFailure(operation string, result CommandResult) error {
	detail := strings.TrimSpace(result.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.Stdout)
	}
	if detail == "" {
		detail = fmt.Sprintf("exit code %d", result.ExitCode)
	}
	return fmt.Errorf("git %s failed: %s", operation, detail)
}

func searchFile(path, root, pattern string, limit int) (matches []SearchMatch, resultErr error) {
	return searchFilePattern(path, root, pattern, nil, limit)
}

func searchFilePattern(path, root, pattern string, expression *regexp.Regexp, limit int) (matches []SearchMatch, resultErr error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxSearchFile {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil
	}
	defer func() {
		resultErr = errors.Join(resultErr, file.Close())
	}()
	probe := make([]byte, 8192)
	read, readErr := file.Read(probe)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	probe = probe[:read]
	if !utf8.Valid(probe) || strings.IndexByte(string(probe), 0) >= 0 {
		return nil, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
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

func (w *Workspace) loadIgnoreMatcher() (ignoreMatcher, error) {
	matcher := ignoreMatcher{patterns: []ignorePattern{
		{pattern: ".icoder.db*"},
	}}
	for _, name := range []string{".gitignore", ".ignore"} {
		data, err := os.ReadFile(filepath.Join(w.root, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return ignoreMatcher{}, err
		}
		if !utf8.Valid(data) {
			return ignoreMatcher{}, fmt.Errorf("%s is not valid UTF-8", name)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			pattern := ignorePattern{}
			if strings.HasPrefix(line, "!") {
				pattern.negated = true
				line = strings.TrimPrefix(line, "!")
			}
			pattern.rooted = strings.HasPrefix(line, "/")
			line = strings.TrimPrefix(line, "/")
			pattern.directory = strings.HasSuffix(line, "/")
			line = strings.TrimSuffix(line, "/")
			if line == "" {
				continue
			}
			pattern.pattern = filepath.ToSlash(filepath.Clean(line))
			matcher.patterns = append(matcher.patterns, pattern)
		}
	}
	return matcher, nil
}

func (m ignoreMatcher) Match(name string, directory bool) bool {
	name = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(name)), "./")
	ignored := false
	for _, pattern := range m.patterns {
		if ignorePatternMatches(pattern, name, directory) {
			ignored = !pattern.negated
		}
	}
	return ignored
}

func ignorePatternMatches(pattern ignorePattern, name string, directory bool) bool {
	if pattern.directory {
		for candidate := name; candidate != "." && candidate != ""; candidate = pathDirectory(candidate) {
			if ignorePathMatches(pattern, candidate) {
				return true
			}
		}
		return directory && ignorePathMatches(pattern, name)
	}
	return ignorePathMatches(pattern, name)
}

func ignorePathMatches(pattern ignorePattern, name string) bool {
	if pattern.rooted || strings.Contains(pattern.pattern, "/") {
		matched, err := globMatch(pattern.pattern, name)
		return err == nil && matched
	}
	for _, component := range strings.Split(name, "/") {
		matched, err := filepath.Match(pattern.pattern, component)
		if err == nil && matched {
			return true
		}
	}
	return false
}

func pathDirectory(name string) string {
	index := strings.LastIndex(name, "/")
	if index < 0 {
		return ""
	}
	return name[:index]
}

func isDefaultIgnoredDirectory(name string) bool {
	_, ignored := defaultIgnoredDirectories[name]
	return ignored
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
