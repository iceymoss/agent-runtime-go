package icoder

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	toollifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

const policyVersion permission.PolicyVersion = "icoder-policy-v1"

func recordRunFact(ctx context.Context, run runContext, suffix, eventType string, payload any) error {
	if run.record == nil {
		return nil
	}
	return run.record(ctx, suffix, eventType, payload)
}

type readFileTool struct{ workspace *Workspace }

func (t readFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "read_file", Description: "Read a range from a UTF-8 text file with line numbers, digest, and truncation metadata.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer", "minimum": 1}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 2000}}, "required": []any{"path"}}}
}

type listFilesTool struct{ workspace *Workspace }

type workingDirectoryTool struct{ workspace *Workspace }

func (t workingDirectoryTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "get_working_directory", Description: "Return the current working directory used by local workspace tools.", Strict: true, Parameters: map[string]any{"type": "object"}}
}
func (workingDirectoryTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t workingDirectoryTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	return agent.ToolResult{Content: t.workspace.WorkingDirectory()}, nil
}

func (t listFilesTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "list_files", Description: "List files inside the workspace.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}}}
}
func (listFilesTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t listFilesTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Limit int `json:"limit"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	files, err := t.workspace.ListFiles(ctx, input.Limit)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := marshalString(files)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data}, nil
}
func (readFileTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t readFileTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	content, err := t.workspace.ReadFileLines(ctx, input.Path, input.Offset, input.Limit)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := marshalString(content)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data}, nil
}

type globFilesTool struct{ workspace *Workspace }

func (t globFilesTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "glob_files", Description: "Find workspace files by glob pattern, for example **/*.go.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 2000}}, "required": []any{"pattern"}}}
}
func (globFilesTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t globFilesTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Pattern string `json:"pattern"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	files, err := t.workspace.Glob(ctx, input.Pattern, input.Limit)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := marshalString(files)
	return agent.ToolResult{Content: data}, err
}

type searchCodeTool struct{ workspace *Workspace }

func (t searchCodeTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "search_code", Description: "Search text or regular expressions in workspace files, optionally filtered by a glob.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}, "regex": map[string]any{"type": "boolean"}, "include": map[string]any{"type": "string"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 500}}, "required": []any{"pattern"}}}
}
func (searchCodeTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t searchCodeTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Pattern string `json:"pattern"`
		Regex   bool   `json:"regex"`
		Include string `json:"include"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	matches, err := t.workspace.SearchPattern(ctx, input.Pattern, input.Include, input.Regex, input.Limit)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := marshalString(matches)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data}, nil
}

type writeFileTool struct{ workspace *Workspace }

type editFileTool struct{ workspace *Workspace }

type applyPatchTool struct{ workspace *Workspace }

type moveFileTool struct{ workspace *Workspace }

func (t moveFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "move_file", Description: "Move one regular file to a new path inside the workspace without overwriting the destination.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"source": map[string]any{"type": "string"}, "destination": map[string]any{"type": "string"}, "expected_digest": map[string]any{"type": "string"}}, "required": []any{"source", "destination"}}}
}

func (moveFileTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t moveFileTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Source         string `json:"source"`
		Destination    string `json:"destination"`
		ExpectedDigest string `json:"expected_digest"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	if err := t.workspace.MoveFile(ctx, input.Source, input.Destination, input.ExpectedDigest); err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf(`{"source":%q,"destination":%q}`, input.Source, input.Destination)}, nil
}

type createDirectoryTool struct{ workspace *Workspace }

func (t createDirectoryTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "create_directory", Description: "Create a directory inside the workspace, optionally including missing parent directories.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "parents": map[string]any{"type": "boolean"}}, "required": []any{"path"}}}
}

func (createDirectoryTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t createDirectoryTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Path    string `json:"path"`
		Parents bool   `json:"parents"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	path, err := t.workspace.CreateDirectory(ctx, input.Path, input.Parents)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf(`{"path":%q}`, path)}, nil
}

func (t applyPatchTool) Definition() agent.ToolDefinition {
	operation := map[string]any{"type": "object", "properties": map[string]any{
		"operation":       map[string]any{"type": "string", "enum": []any{"create", "update", "delete"}},
		"path":            map[string]any{"type": "string"},
		"content":         map[string]any{"type": "string"},
		"old_text":        map[string]any{"type": "string"},
		"new_text":        map[string]any{"type": "string"},
		"expected_digest": map[string]any{"type": "string"},
		"replace_all":     map[string]any{"type": "boolean"},
	}, "required": []any{"operation", "path"}}
	return agent.ToolDefinition{Name: "apply_patch", Description: "Apply a batch of create, exact-text update, and delete operations inside the workspace. The entire batch is validated before execution and rolled back on failure.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"operations": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": operation}}, "required": []any{"operations"}}}
}

func (applyPatchTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t applyPatchTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Operations []PatchOperation `json:"operations"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	results, err := t.workspace.ApplyPatch(ctx, input.Operations)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := marshalString(results)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data}, nil
}

func (t editFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "edit_file", Description: "Replace an exact text fragment in an existing file. Fails on ambiguous or stale edits.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "old_text": map[string]any{"type": "string"}, "new_text": map[string]any{"type": "string"}, "expected_digest": map[string]any{"type": "string"}, "replace_all": map[string]any{"type": "boolean"}}, "required": []any{"path", "old_text", "new_text"}}}
}
func (editFileTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t editFileTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Path           string `json:"path"`
		OldText        string `json:"old_text"`
		NewText        string `json:"new_text"`
		ExpectedDigest string `json:"expected_digest"`
		ReplaceAll     bool   `json:"replace_all"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	value, err := t.workspace.EditFile(ctx, input.Path, input.OldText, input.NewText, input.ExpectedDigest, input.ReplaceAll)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: `{"digest":"` + value + `"}`}, nil
}

func (t writeFileTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "write_file", Description: "Replace one file inside the workspace. Requires permission.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}, "expected_digest": map[string]any{"type": "string"}}, "required": []any{"path", "content"}}}
}

type runCommandTool struct{ workspace *Workspace }
type gitCommitTool struct{ workspace *Workspace }

type gitStatusTool struct{ workspace *Workspace }

func (t gitStatusTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "git_status", Description: "Show concise Git workspace status without modifying files.", Strict: true, Parameters: map[string]any{"type": "object"}}
}
func (gitStatusTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t gitStatusTool) Execute(ctx context.Context, _ agent.ToolInvocation) (agent.ToolResult, error) {
	result, err := t.workspace.RunCommand(ctx, "git", []string{"status", "--short", "--branch"}, 30*time.Second)
	if err != nil {
		return agent.ToolResult{}, err
	}
	data, err := marshalString(result)
	return agent.ToolResult{Content: data, IsError: result.ExitCode != 0}, err
}

type gitDiffTool struct{ workspace *Workspace }

func (t gitDiffTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "git_diff", Description: "Show the current Git diff without external diff drivers.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"staged": map[string]any{"type": "boolean"}, "stat": map[string]any{"type": "boolean"}}}}
}
func (gitDiffTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyIdempotent }
func (t gitDiffTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Staged bool `json:"staged"`
		Stat   bool `json:"stat"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	args := []string{"diff", "--no-ext-diff"}
	if input.Staged {
		args = append(args, "--cached")
	}
	if input.Stat {
		args = append(args, "--stat")
	}
	result, err := t.workspace.RunCommand(ctx, "git", args, 30*time.Second)
	if err != nil {
		return agent.ToolResult{}, err
	}
	data, err := marshalString(result)
	return agent.ToolResult{Content: data, IsError: result.ExitCode != 0}, err
}

func (t runCommandTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "run_command", Description: "Run an approved build, test, lint, format, or read-only Git command without a shell. cwd must be relative to the workspace. Requires permission.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"program": map[string]any{"type": "string", "enum": []any{"go", "gofmt", "git", "npm", "pnpm", "yarn", "pytest", "ruff", "cargo", "rustfmt", "make"}}, "args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "cwd": map[string]any{"type": "string", "description": "Relative directory inside the workspace; omit for the current workspace directory."}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120}}, "required": []any{"program", "args"}}}
}
func (runCommandTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t runCommandTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Program        string   `json:"program"`
		Args           []string `json:"args"`
		CWD            string   `json:"cwd"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	if err := validateCommand(input.Program, input.Args); err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	result, err := t.workspace.RunCommandIn(ctx, input.Program, input.Args, input.CWD, time.Duration(input.TimeoutSeconds)*time.Second)
	if err != nil {
		if ctx.Err() != nil {
			return agent.ToolResult{}, ctx.Err()
		}
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := marshalString(result)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data, IsError: result.ExitCode != 0}, nil
}

func (t gitCommitTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "git_commit", Description: "Stage exact relative workspace paths and create one non-amended Git commit. Rejects unrelated staged changes and never pushes. Use only when the user explicitly asks to commit.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string", "minLength": 1, "maxLength": 200}, "paths": map[string]any{"type": "array", "minItems": 1, "maxItems": 100, "items": map[string]any{"type": "string"}}}, "required": []any{"message", "paths"}}}
}
func (gitCommitTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t gitCommitTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	result, err := t.workspace.GitCommit(ctx, input.Message, input.Paths)
	if err != nil {
		if ctx.Err() != nil {
			return agent.ToolResult{}, ctx.Err()
		}
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	data, err := marshalString(result)
	return agent.ToolResult{Content: data}, err
}

func validateCommand(program string, args []string) error {
	switch program {
	case "go":
		if len(args) == 0 {
			break
		}
		switch args[0] {
		case "test", "vet", "build", "fmt", "generate":
			return nil
		}
	case "git":
		if len(args) == 0 {
			break
		}
		switch args[0] {
		case "status", "diff", "log", "show":
			return nil
		}
	case "npm", "pnpm", "yarn":
		if len(args) > 0 {
			switch args[0] {
			case "test", "build", "lint", "check", "typecheck", "format":
				return nil
			}
		}
	case "pytest", "ruff":
		return nil
	case "cargo":
		if len(args) > 0 {
			switch args[0] {
			case "test", "build", "check", "clippy", "fmt":
				return nil
			}
		}
	case "gofmt", "rustfmt", "make":
		return nil
	}
	return fmt.Errorf("command is not approved")
}
func (writeFileTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (t writeFileTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	var input struct {
		Path           string `json:"path"`
		Content        string `json:"content"`
		ExpectedDigest string `json:"expected_digest"`
	}
	if err := json.Unmarshal([]byte(invocation.RawInput), &input); err != nil {
		return agent.ToolResult{}, err
	}
	result, err := t.workspace.WriteFile(ctx, input.Path, input.Content, input.ExpectedDigest)
	if err != nil {
		return agent.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return agent.ToolResult{Content: `{"digest":"` + result + `"}`}, nil
}

// workspaceToolEntries declares every local tool together with the metadata the
// executor uses to classify its effect and the resource the permission policy
// decides about. The declaration lives here, next to the implementations, so a
// new tool cannot be added without stating what it is allowed to do.
func workspaceToolEntries(workspace *Workspace, weather WeatherProvider) []lifecycleEntry {
	resource := permission.Resource{Kind: "workspace", Key: workspace.WorkingDirectory()}
	entries := []lifecycleEntry{
		{workingDirectoryTool{workspace: workspace}, readMetadata("workspace.read"), resource},
		{listFilesTool{workspace: workspace}, readMetadata("workspace.read"), resource},
		{globFilesTool{workspace: workspace}, readMetadata("workspace.read"), resource},
		{readFileTool{workspace: workspace}, readMetadata("workspace.read"), resource},
		{searchCodeTool{workspace: workspace}, readMetadata("workspace.search"), resource},
		{gitStatusTool{workspace: workspace}, readMetadata("workspace.read"), resource},
		{gitDiffTool{workspace: workspace}, readMetadata("workspace.read"), resource},
		// create_directory is idempotent in the only sense that matters here:
		// creating a directory that already exists changes nothing.
		{createDirectoryTool{workspace: workspace}, toolMetadataIdempotentWrite("workspace.write"), resource},
		{writeFileTool{workspace: workspace}, writeMetadata("workspace.write", "workspace.file"), resource},
		{editFileTool{workspace: workspace}, writeMetadata("workspace.write", "workspace.file"), resource},
		{applyPatchTool{workspace: workspace}, writeMetadata("workspace.write", "workspace.file"), resource},
		{moveFileTool{workspace: workspace}, writeMetadata("workspace.write", "workspace.file"), resource},
		{gitCommitTool{workspace: workspace}, writeMetadata("workspace.commit", "workspace.git"), resource},
		// run_command is external rather than write: the runtime cannot know what
		// a subprocess touched, so it is never replayed.
		{runCommandTool{workspace: workspace}, externalMetadata("workspace.command", "workspace.command", false), resource},
	}
	if weather != nil {
		entries = append(entries, lifecycleEntry{
			weatherTool{provider: weather},
			externalMetadata("network.read", "network.http", true),
			permission.Resource{Kind: "network", Key: "open-meteo"},
		})
	}
	return entries
}

// toolMetadataIdempotentWrite describes a write whose repetition is provably
// harmless, so recovery may replay it instead of stopping for a human.
func toolMetadataIdempotentWrite(action string) toollifecycle.Metadata {
	metadata := writeMetadata(action, "workspace.file")
	metadata.Idempotency = toollifecycle.IdempotencyExecutionKey
	metadata.ReplayPolicy = agent.ReplayPolicyIdempotent
	return metadata
}
