package tool

import (
	"encoding/json"

	"github.com/iceymoss/agent-runtime-go"
)

func cloneDefinition(value agent.ToolDefinition) agent.ToolDefinition {
	if value.Parameters == nil {
		return value
	}
	data, err := json.Marshal(value.Parameters)
	if err != nil {
		return value
	}
	var parameters map[string]any
	if err := json.Unmarshal(data, &parameters); err != nil {
		return value
	}
	value.Parameters = parameters
	return value
}

func cloneResult(value agent.ToolResult) agent.ToolResult { return value }

func cloneRecord(value ExecutionRecord) ExecutionRecord {
	cloned := value
	if value.Result != nil {
		result := cloneResult(*value.Result)
		cloned.Result = &result
	}
	if value.Failure != nil {
		failure := *value.Failure
		cloned.Failure = &failure
	}
	if value.Suspension != nil {
		suspension := *value.Suspension
		cloned.Suspension = &suspension
	}
	return cloned
}
