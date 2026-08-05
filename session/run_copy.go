package session

import "github.com/iceymoss/agent-runtime-go"

func cloneMessages(messages []agent.Message) []agent.Message {
	cloned := make([]agent.Message, len(messages))
	for i, message := range messages {
		cloned[i] = message
		cloned[i].Parts = make([]agent.ContentPart, len(message.Parts))
		for j, part := range message.Parts {
			cloned[i].Parts[j] = part
			if part.ToolCall != nil {
				value := *part.ToolCall
				cloned[i].Parts[j].ToolCall = &value
			}
			if part.ToolResult != nil {
				value := *part.ToolResult
				cloned[i].Parts[j].ToolResult = &value
			}
			if part.Image != nil {
				value := *part.Image
				value.Data = append([]byte(nil), part.Image.Data...)
				cloned[i].Parts[j].Image = &value
			}
		}
	}
	return cloned
}

func cloneCoreResult(result *agent.RunResult) *agent.RunResult {
	if result == nil {
		return nil
	}
	cloned := *result
	cloned.Messages = cloneMessages(result.Messages)
	cloned.Steps = make([]agent.StepResult, len(result.Steps))
	for i, step := range result.Steps {
		cloned.Steps[i] = step
		messages := cloneMessages([]agent.Message{step.Message})
		cloned.Steps[i].Message = messages[0]
		cloned.Steps[i].ToolCalls = append([]agent.ToolCall(nil), step.ToolCalls...)
		cloned.Steps[i].ToolResults = append([]agent.ToolResult(nil), step.ToolResults...)
	}
	cloned.DurableCompletion = nil
	return &cloned
}

func cloneExecution(execution ResolvedExecution) (ResolvedExecution, error) {
	cloned := execution
	cloned.Artifacts = append([]ArtifactRef(nil), execution.Artifacts...)
	if execution.Definition != nil {
		// RuntimeDefinition is immutable and only exposes copied values. Rebuilding
		// is intentionally unavailable because its model/tool snapshots are opaque.
		cloned.Definition = execution.Definition
	}
	return cloned, nil
}

func cloneFailure(failure *Failure) *Failure {
	if failure == nil {
		return nil
	}
	cloned := *failure
	return &cloned
}
