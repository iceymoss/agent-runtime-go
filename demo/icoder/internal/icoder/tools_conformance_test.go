package icoder

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/agenttest"
)

// These run the runtime's shared tool conformance suite against iCoder's own
// tools, so the contract they are held to is the library's rather than a local
// interpretation of it.
//
// The cases deliberately include invalid input and missing resources: a tool that
// turns a model mistake into a fatal error instead of a correctable result would
// end the run rather than let the agent fix itself.

func TestReadFileToolConformance(t *testing.T) {
	root := resolvedTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	tool := readFileTool{workspace: workspace}
	agenttest.TestTool(t, tool, []agenttest.ToolCase{
		{
			Name:       "reads an existing file",
			Invocation: agent.ToolInvocation{CallID: "call-1", Name: "read_file", RawInput: `{"path":"main.go"}`},
			WantResult: agent.ToolResult{Content: `{"path":"main.go","content":"1: package main\n2: ","start_line":1,"end_line":2,"total_lines":2,"truncated":false,"digest":"` + fileDigest(t, filepath.Join(root, "main.go")) + `"}`},
		},
		{
			Name:       "a missing file is correctable, not fatal",
			Invocation: agent.ToolInvocation{CallID: "call-2", Name: "read_file", RawInput: `{"path":"absent.go"}`},
			WantResult: agent.ToolResult{Content: missingFileMessage(root, "absent.go"), IsError: true},
		},
		{
			Name:       "escaping the workspace is refused",
			Invocation: agent.ToolInvocation{CallID: "call-3", Name: "read_file", RawInput: `{"path":"../outside.go"}`},
			WantResult: agent.ToolResult{Content: "path escapes workspace", IsError: true},
		},
	})
}

func TestWorkingDirectoryToolConformance(t *testing.T) {
	root := resolvedTempDir(t)
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	agenttest.TestTool(t, workingDirectoryTool{workspace: workspace}, []agenttest.ToolCase{
		{
			Name:       "reports the tool working directory",
			Invocation: agent.ToolInvocation{CallID: "call-1", Name: "get_working_directory", RawInput: `{}`},
			WantResult: agent.ToolResult{Content: root},
		},
	})
}

func TestRunCommandToolConformance(t *testing.T) {
	workspace, err := NewWorkspace(resolvedTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	agenttest.TestTool(t, runCommandTool{workspace: workspace}, []agenttest.ToolCase{
		{
			Name:       "an unapproved command is correctable, not fatal",
			Invocation: agent.ToolInvocation{CallID: "call-1", Name: "run_command", RawInput: `{"program":"go","args":["run","./..."]}`},
			WantResult: agent.ToolResult{Content: "command is not approved", IsError: true},
		},
		{
			Name:       "an absolute cwd is refused",
			Invocation: agent.ToolInvocation{CallID: "call-2", Name: "run_command", RawInput: `{"program":"go","args":["test"],"cwd":"/etc"}`},
			WantResult: agent.ToolResult{Content: "absolute paths are not allowed", IsError: true},
		},
	})
}

func TestEditFileToolConformance(t *testing.T) {
	root := resolvedTempDir(t)
	if err := os.WriteFile(filepath.Join(root, "value.go"), []byte("const answer = 41\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	agenttest.TestTool(t, editFileTool{workspace: workspace}, []agenttest.ToolCase{
		{
			Name:       "text that is not present is correctable",
			Invocation: agent.ToolInvocation{CallID: "call-1", Name: "edit_file", RawInput: `{"path":"value.go","old_text":"missing","new_text":"x"}`},
			WantResult: agent.ToolResult{Content: "old_text was not found", IsError: true},
		},
		{
			Name:       "a stale digest refuses to overwrite a concurrent change",
			Invocation: agent.ToolInvocation{CallID: "call-2", Name: "edit_file", RawInput: `{"path":"value.go","old_text":"41","new_text":"42","expected_digest":"sha256:stale"}`},
			WantResult: agent.ToolResult{Content: "file changed: expected sha256:stale, got " + fileDigest(t, filepath.Join(root, "value.go")), IsError: true},
		},
	})
}

// missingFileMessage is the exact error a read of an absent in-workspace file
// produces. It stays a tool result rather than a Go error so the model can
// correct the path instead of the run failing.
func missingFileMessage(root, name string) string {
	return "lstat " + filepath.Join(root, name) + ": no such file or directory"
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return digest(data)
}
