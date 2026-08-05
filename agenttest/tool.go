package agenttest

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

type ToolCase struct {
	Name       string
	Invocation agent.ToolInvocation
	WantResult agent.ToolResult
	WantErr    error
}

// TestTool runs reusable public-contract checks against a Tool implementation.
func TestTool(t *testing.T, tool agent.Tool, cases []ToolCase) {
	t.Helper()
	if tool == nil || tool.Definition().Name == "" {
		t.Fatal("tool and definition name are required")
	}
	switch tool.ReplayPolicy() {
	case agent.ReplayPolicyNever, agent.ReplayPolicyIdempotent, agent.ReplayPolicyResolve:
	default:
		t.Fatalf("invalid replay policy %q", tool.ReplayPolicy())
	}
	for _, tt := range cases {
		t.Run(tt.Name, func(t *testing.T) {
			result, err := tool.Execute(context.Background(), tt.Invocation)
			if !errors.Is(err, tt.WantErr) {
				t.Fatalf("Execute() error = %v, want %v", err, tt.WantErr)
			}
			if !reflect.DeepEqual(result, tt.WantResult) {
				t.Fatalf("Execute() = %+v, want %+v", result, tt.WantResult)
			}
		})
	}
}
