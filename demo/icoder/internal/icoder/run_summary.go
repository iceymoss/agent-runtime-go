package icoder

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/iceymoss/agent-runtime-go"
)

type RunSummary struct {
	ChangedFiles []string          `json:"changed_files"`
	Checks       []ValidationCheck `json:"checks"`
	Verification string            `json:"verification"`
}

type ValidationCheck struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
	Passed   bool   `json:"passed"`
}

type recordedCall struct {
	call  agent.ToolCall
	order int
}

func summarizeRun(messages []agent.Message) RunSummary {
	calls := make(map[string]recordedCall)
	changed := make(map[string]struct{})
	lastChange, lastPassedCheck := -1, -1
	summary := RunSummary{Verification: "not_required"}
	order := 0
	for _, message := range messages {
		for _, call := range message.ToolCalls() {
			calls[call.ID] = recordedCall{call: call, order: order}
			order++
		}
		for _, result := range message.ToolResults() {
			recorded, ok := calls[result.ToolCallID]
			if !ok || result.IsError {
				order++
				continue
			}
			paths := changedPaths(recorded.call)
			for _, path := range paths {
				changed[path] = struct{}{}
				lastChange = order
			}
			if check, ok := validationCheck(recorded.call, result); ok {
				summary.Checks = append(summary.Checks, check)
				if check.Passed {
					lastPassedCheck = order
				}
			}
			order++
		}
	}
	for path := range changed {
		summary.ChangedFiles = append(summary.ChangedFiles, path)
	}
	sort.Strings(summary.ChangedFiles)
	if len(summary.ChangedFiles) > 0 {
		summary.Verification = "unverified"
		if lastPassedCheck > lastChange {
			summary.Verification = "verified"
		}
	}
	return summary
}

func changedPaths(call agent.ToolCall) []string {
	switch call.Name {
	case "write_file", "edit_file":
		var input struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(call.Input), &input) == nil && input.Path != "" {
			return []string{input.Path}
		}
	case "move_file":
		var input struct {
			Source      string `json:"source"`
			Destination string `json:"destination"`
		}
		if json.Unmarshal([]byte(call.Input), &input) == nil {
			return []string{input.Source, input.Destination}
		}
	case "apply_patch":
		var input struct {
			Operations []PatchOperation `json:"operations"`
		}
		if json.Unmarshal([]byte(call.Input), &input) == nil {
			paths := make([]string, 0, len(input.Operations))
			for _, operation := range input.Operations {
				if operation.Path != "" {
					paths = append(paths, operation.Path)
				}
			}
			return paths
		}
	}
	return nil
}

func validationCheck(call agent.ToolCall, result agent.ToolResult) (ValidationCheck, bool) {
	if call.Name != "run_command" {
		return ValidationCheck{}, false
	}
	var input struct {
		Program string   `json:"program"`
		Args    []string `json:"args"`
	}
	var commandResult CommandResult
	if json.Unmarshal([]byte(call.Input), &input) != nil || json.Unmarshal([]byte(result.Content), &commandResult) != nil {
		return ValidationCheck{}, false
	}
	if input.Program == "git" {
		return ValidationCheck{}, false
	}
	command := strings.TrimSpace(strings.Join(append([]string{input.Program}, input.Args...), " "))
	return ValidationCheck{Command: command, ExitCode: commandResult.ExitCode, TimedOut: commandResult.TimedOut, Passed: commandResult.ExitCode == 0 && !commandResult.TimedOut}, true
}
