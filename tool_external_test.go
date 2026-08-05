package agent_test

import (
	"context"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

type externallyVersionedTool struct{}

func (externallyVersionedTool) ExecutableVersion() string { return "v1" }
func (externallyVersionedTool) Definition() agent.ToolDefinition {
	return agent.ToolDefinition{Name: "external", Parameters: map[string]any{"type": "object"}}
}
func (externallyVersionedTool) ReplayPolicy() agent.ReplayPolicy { return agent.ReplayPolicyNever }
func (externallyVersionedTool) Execute(context.Context, agent.ToolInvocation) (agent.ToolResult, error) {
	return agent.ToolResult{}, nil
}

func TestExecutableVersionPublicContract(t *testing.T) {
	var versioned agent.ExecutableVersioner = externallyVersionedTool{}
	if versioned.ExecutableVersion() != "v1" {
		t.Fatalf("ExecutableVersion() = %q", versioned.ExecutableVersion())
	}
	registry := agent.NewRegistry()
	if err := registry.Register(versioned.(agent.Tool)); err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewToolSet(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.ValidateExecutableVersions(); err != nil {
		t.Fatal(err)
	}
}
