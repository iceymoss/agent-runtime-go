package main

import (
	"context"
	"encoding/json"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

// Incident is what the classifier must produce. It is consumed by code, so the
// shape is enforced by the provider rather than requested in a prompt.
type Incident struct {
	Service  string `json:"service"`
	Severity string `json:"severity"`
	Summary  string `json:"summary"`
}

// incidentSchema is the JSON Schema the model's output must satisfy.
var incidentSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "service":  {"type": "string"},
    "severity": {"type": "string", "enum": ["low", "high"]},
    "summary":  {"type": "string"}
  },
  "required": ["service", "severity", "summary"],
  "additionalProperties": false
}`)

// Classify runs one turn whose answer is parsed rather than read.
//
// ResponseFormat is set per run instead of on the agent, so the same assistant
// can hold an ordinary conversation and produce machine-readable output only
// where a caller needs it.
func Classify(ctx context.Context, runner *agent.Agent, logs string) (Incident, error) {
	result, err := runner.Run(ctx, agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("把日志归纳成一条事件记录。"),
			agent.NewUserMessage(logs),
		},
		ResponseFormat: &agent.ResponseFormat{
			Kind: agent.ResponseFormatJSONSchema, Name: "incident",
			Schema: incidentSchema, Strict: true,
		},
	})
	if err != nil {
		return Incident{}, err
	}
	// The runtime does not check the model's output against the schema —
	// enforcement is the provider's job — so an application that must be certain
	// still parses and checks.
	var incident Incident
	if err := json.Unmarshal([]byte(result.Text), &incident); err != nil {
		return Incident{}, fmt.Errorf("模型返回的不是合法 JSON: %w", err)
	}
	return incident, nil
}
