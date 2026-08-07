package icoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
)

const policyVersion permission.PolicyVersion = "icoder-policy-v1"

type authorizedTool struct {
	tool        agent.Tool
	permission  permission.Service
	action      string
	resource    func(string) permission.Resource
	sessionID   func() string
	now         func() time.Time
	approvalTTL time.Duration
}

func (t *authorizedTool) Definition() agent.ToolDefinition { return t.tool.Definition() }
func (t *authorizedTool) ReplayPolicy() agent.ReplayPolicy { return t.tool.ReplayPolicy() }

func (t *authorizedTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	inputDigest := permission.InputDigest(digest([]byte(invocation.RawInput)))
	run := currentRunContext(ctx)
	sessionID := run.session
	if sessionID == "" {
		sessionID = t.sessionID()
	}
	if run.id == "" {
		run.id = sessionID
	}
	requestKey := permission.RequestKey(digest([]byte(run.id + "\x00" + invocation.CallID + "\x00" + invocation.Name + "\x00" + invocation.RawInput)))
	check := permission.CheckRequest{
		RequestKey: requestKey,
		Subject:    permission.Subject{TenantKey: "local", PrincipalKey: "cli-user", ActorType: "human"},
		Resource:   t.resource(invocation.RawInput), SessionRef: sessionID,
		RunRef: permission.RunRef(run.id), AttemptRef: permission.AttemptRef(run.id),
		ExecutionRef: permission.ExecutionRef(requestKey), FenceToken: 1,
		ToolCallID: invocation.CallID, ToolName: invocation.Name, Action: t.action,
		InputDigest: inputDigest, ToolGeneration: "icoder-tools-v1", DefinitionDigest: "icoder-v1",
		PolicyVersion: policyVersion, ApprovalExpiresAt: t.approvalExpiresAt(),
	}
	decision, err := t.permission.Check(ctx, check)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if err := recordRunFact(ctx, run, invocation.CallID+":permission-checked", "agent.permission.checked", map[string]any{"tool": invocation.Name, "action": t.action, "resource": check.Resource, "input_digest": inputDigest, "decision": decision.Decision, "reason_code": decision.ReasonCode}); err != nil {
		return agent.ToolResult{}, err
	}
	switch decision.Decision {
	case permission.DecisionAllow:
		return t.executeAndRecord(ctx, invocation, run)
	case permission.DecisionAsk:
		if err := recordRunFact(ctx, run, invocation.CallID+":approval-requested", "agent.approval.requested", map[string]any{"tool": invocation.Name, "action": t.action, "resource": check.Resource, "input_digest": inputDigest}); err != nil {
			return agent.ToolResult{}, err
		}
		return t.askAndExecute(ctx, invocation, check, decision, run.approve)
	default:
		return agent.ToolResult{IsError: true, StopTurn: true, Content: "permission denied: " + decision.ReasonCode}, nil
	}
}

func (t *authorizedTool) approvalExpiresAt() time.Time {
	now := time.Now
	if t.now != nil {
		now = t.now
	}
	ttl := t.approvalTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	return now().Add(ttl)
}

func (t *authorizedTool) executeAndRecord(ctx context.Context, invocation agent.ToolInvocation, run runContext) (agent.ToolResult, error) {
	if err := recordRunFact(ctx, run, invocation.CallID+":started", "agent.tool.started", map[string]any{"tool": invocation.Name}); err != nil {
		return agent.ToolResult{}, err
	}
	result, err := t.tool.Execute(ctx, invocation)
	eventType := "agent.tool.completed"
	payload := map[string]any{"tool": invocation.Name, "is_error": result.IsError, "stop_turn": result.StopTurn}
	if err != nil {
		eventType, payload["error"] = "agent.tool.failed", err.Error()
	}
	if eventErr := recordRunFact(context.WithoutCancel(ctx), run, invocation.CallID+":terminal", eventType, payload); eventErr != nil {
		return result, errorsJoin(err, eventErr)
	}
	return result, err
}

func (t *authorizedTool) askAndExecute(ctx context.Context, invocation agent.ToolInvocation, check permission.CheckRequest, result permission.CheckResult, approve ApprovalFunc) (agent.ToolResult, error) {
	if approve == nil || result.Approval == nil || result.Blocker == nil {
		return agent.ToolResult{IsError: true, StopTurn: true, Content: "permission denied: interactive approval is unavailable"}, nil
	}
	choice, err := approve(ctx, ApprovalPrompt{ToolName: invocation.Name, Action: t.action, Resource: check.Resource.Key, Input: approvalInput(invocation.RawInput), ExpiresAt: check.ApprovalExpiresAt})
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_, _, _ = t.permission.Cancel(cleanupCtx, permission.CancelCommand{TenantKey: check.Subject.TenantKey, RequestKey: check.RequestKey, ExpectedRevision: result.Approval.Request.Revision, AttemptRef: check.AttemptRef, FenceToken: check.FenceToken})
		return agent.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	kind, reason := permission.ResolutionDeny, "user-rejected"
	if choice == ApprovalApproveOnce {
		kind, reason = permission.ResolutionApprove, "user-approved-once"
	} else if choice == ApprovalApproveAuto {
		kind, reason = permission.ResolutionApprove, "user-approved-auto"
	}
	commandKey := string(check.RequestKey) + ":" + string(kind)
	_, _, err = t.permission.Resolve(ctx, permission.ResolveCommand{TenantKey: check.Subject.TenantKey, RequestKey: check.RequestKey, CommandKey: commandKey, DecisionKey: permission.DecisionKey(commandKey), ApproverKey: "cli-user", ExpectedRevision: result.Approval.Request.Revision, Kind: kind, ReasonCode: reason})
	if err != nil {
		if terminal, ok := approvalTerminalResult(err); ok {
			return terminal, nil
		}
		return agent.ToolResult{}, err
	}
	run := currentRunContext(ctx)
	if err := recordRunFact(ctx, run, invocation.CallID+":approval-resolved", "agent.approval.resolved", map[string]any{"tool": invocation.Name, "resolution": kind, "reason_code": reason}); err != nil {
		return agent.ToolResult{}, err
	}
	if kind == permission.ResolutionDeny {
		return agent.ToolResult{IsError: true, StopTurn: true, Content: "permission denied by user"}, nil
	}
	revalidated, err := t.permission.Revalidate(ctx, permission.RevalidateCommand{TenantKey: check.Subject.TenantKey, RequestKey: check.RequestKey, ResumeToken: result.Blocker.ResumeToken, AttemptRef: check.AttemptRef, FenceToken: check.FenceToken, InputDigest: check.InputDigest, PolicyVersion: check.PolicyVersion, ToolGeneration: check.ToolGeneration})
	if err != nil {
		if terminal, ok := approvalTerminalResult(err); ok {
			return terminal, nil
		}
		return agent.ToolResult{}, err
	}
	if revalidated.Decision != permission.DecisionAllow {
		return agent.ToolResult{}, fmt.Errorf("permission revalidation did not allow execution")
	}
	if err := recordRunFact(ctx, run, invocation.CallID+":permission-revalidated", "agent.permission.revalidated", map[string]any{"tool": invocation.Name, "decision": revalidated.Decision}); err != nil {
		return agent.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, err
	}
	return t.executeAndRecord(ctx, invocation, run)
}

func approvalTerminalResult(err error) (agent.ToolResult, bool) {
	message := ""
	switch {
	case errors.Is(err, permission.ErrRequestExpired), errors.Is(err, permission.ErrGrantExpired):
		message = "Approval expired before it was confirmed. Run the task again to request a new approval."
	case errors.Is(err, permission.ErrRequestCanceled):
		message = "Approval was canceled. Run the task again if you still want to continue."
	case errors.Is(err, permission.ErrPermissionDenied):
		message = "Permission was denied. The requested action was not performed."
	default:
		return agent.ToolResult{}, false
	}
	return agent.ToolResult{IsError: true, StopTurn: true, Content: message}, true
}

func recordRunFact(ctx context.Context, run runContext, suffix, eventType string, payload any) error {
	if run.record == nil {
		return nil
	}
	return run.record(ctx, suffix, eventType, payload)
}

func NewPermissionService(allowWrites bool) (permission.Service, error) {
	policy := permission.PolicyFunc{PolicyVersion: policyVersion, EvaluateFunc: func(_ context.Context, request permission.CheckRequest, _ []permission.Grant) (permission.CheckResult, error) {
		result := permission.CheckResult{PolicyVersion: request.PolicyVersion, InputDigest: request.InputDigest}
		switch request.Action {
		case "workspace.read", "workspace.search", "network.read", "subagent.spawn":
			result.Decision, result.RuleKey = permission.DecisionAllow, "safe-local-operation"
		case "workspace.write", "workspace.command", "network.tool":
			if allowWrites {
				result.Decision, result.RuleKey = permission.DecisionAllow, "cli-write-flag"
			} else {
				result.Decision, result.RuleKey = permission.DecisionAsk, "workspace-write-approval"
				result.Constraint = permission.GrantConstraint{Scope: permission.ScopeInvocation, ExpiresAt: request.ApprovalExpiresAt}
			}
		default:
			result.Decision, result.ReasonCode = permission.DecisionDeny, "no-matching-rule"
		}
		return result, nil
	}}
	return permission.NewService(permission.ServiceOptions{Policy: policy, Store: permission.NewMemoryStore()})
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
	return agent.ToolDefinition{Name: "run_command", Description: "Run an approved build, test, lint, format, or Git inspection command without a shell. Requires permission.", Strict: true, Parameters: map[string]any{"type": "object", "properties": map[string]any{"program": map[string]any{"type": "string", "enum": []any{"go", "gofmt", "git", "npm", "pnpm", "yarn", "pytest", "ruff", "cargo", "rustfmt", "make"}}, "args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "cwd": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120}}, "required": []any{"program", "args"}}}
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
		return agent.ToolResult{}, err
	}
	data, err := marshalString(result)
	if err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: data, IsError: result.ExitCode != 0}, nil
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

func registerTools(registry *agent.Registry, workspace *Workspace, weather WeatherProvider, permissions permission.Service, sessionID func() string) error {
	tools := []struct {
		tool   agent.Tool
		action string
	}{
		{workingDirectoryTool{workspace: workspace}, "workspace.read"},
		{listFilesTool{workspace: workspace}, "workspace.read"},
		{globFilesTool{workspace: workspace}, "workspace.read"},
		{readFileTool{workspace: workspace}, "workspace.read"},
		{searchCodeTool{workspace: workspace}, "workspace.search"},
		{gitStatusTool{workspace: workspace}, "workspace.read"},
		{gitDiffTool{workspace: workspace}, "workspace.read"},
		{writeFileTool{workspace: workspace}, "workspace.write"},
		{editFileTool{workspace: workspace}, "workspace.write"},
		{applyPatchTool{workspace: workspace}, "workspace.write"},
		{moveFileTool{workspace: workspace}, "workspace.write"},
		{createDirectoryTool{workspace: workspace}, "workspace.write"},
		{runCommandTool{workspace: workspace}, "workspace.command"},
		{weatherTool{provider: weather}, "network.read"},
	}
	for _, entry := range tools {
		wrapped := &authorizedTool{tool: entry.tool, permission: permissions, action: entry.action, sessionID: sessionID, resource: func(string) permission.Resource {
			return permission.Resource{Kind: "workspace", Key: workspace.WorkingDirectory()}
		}}
		if err := registry.Register(wrapped); err != nil {
			return err
		}
	}
	return nil
}
