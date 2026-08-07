package icoder

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/iceymoss/agent-runtime-go"
)

const maxInstructionFile = 64 << 10

func (w *Workspace) ProjectInstructions(ctx context.Context) ([]agent.Message, error) {
	cwd := w.WorkingDirectory()
	relative, err := filepath.Rel(w.root, cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve instruction scope: %w", err)
	}
	if !within(w.root, cwd) {
		return nil, fmt.Errorf("instruction scope escapes workspace")
	}
	directories := []string{w.root}
	if relative != "." {
		current := w.root
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			directories = append(directories, current)
		}
	}
	messages := make([]agent.Message, 0, len(directories))
	for _, directory := range directories {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		path := filepath.Join(directory, "AGENTS.md")
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s must be a regular file", path)
		}
		if info.Size() > maxInstructionFile {
			return nil, fmt.Errorf("%s exceeds the %d byte instruction limit", path, maxInstructionFile)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
			return nil, fmt.Errorf("%s is not valid UTF-8 text", path)
		}
		source, err := filepath.Rel(w.root, path)
		if err != nil {
			return nil, err
		}
		scope, err := filepath.Rel(w.root, directory)
		if err != nil {
			return nil, err
		}
		if scope == "." {
			scope = "/"
		} else {
			scope = filepath.ToSlash(scope) + "/**"
		}
		content := fmt.Sprintf("<untrusted-project-instructions source=%q scope=%q>\n%s\n</untrusted-project-instructions>", filepath.ToSlash(source), scope, string(data))
		messages = append(messages, agent.NewSystemMessage(content))
	}
	return messages, nil
}
