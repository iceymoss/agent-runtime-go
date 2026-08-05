package agent

import "testing"

func TestStepSignatureCharacterization(t *testing.T) {
	step := func(calls []ToolCall, results []ToolResult) StepResult {
		return StepResult{ToolCalls: calls, ToolResults: results}
	}
	call := func(id, input string) ToolCall {
		return ToolCall{ID: id, Name: "lookup", Input: input}
	}
	result := func(id, content string) ToolResult {
		return ToolResult{ToolCallID: id, Name: "lookup", Content: content}
	}
	tests := []struct {
		name  string
		left  StepResult
		right StepResult
		equal bool
	}{
		{
			name:  "object key order is canonicalized",
			left:  step([]ToolCall{call("a", `{"a":1,"b":2}`)}, []ToolResult{result("a", "ok")}),
			right: step([]ToolCall{call("a", `{"b":2,"a":1}`)}, []ToolResult{result("a", "ok")}),
			equal: true,
		},
		{
			name:  "valid whitespace is canonicalized",
			left:  step([]ToolCall{call("a", `{"a":1}`)}, []ToolResult{result("a", "ok")}),
			right: step([]ToolCall{call("a", ` { "a": 1 } `)}, []ToolResult{result("a", "ok")}),
			equal: true,
		},
		{
			name:  "array order is significant",
			left:  step([]ToolCall{call("a", `{"values":[1,2]}`)}, []ToolResult{result("a", "ok")}),
			right: step([]ToolCall{call("a", `{"values":[2,1]}`)}, []ToolResult{result("a", "ok")}),
		},
		{
			name:  "tool order is significant",
			left:  step([]ToolCall{call("a", `{}`), call("b", `{}`)}, []ToolResult{result("a", "one"), result("b", "two")}),
			right: step([]ToolCall{call("b", `{}`), call("a", `{}`)}, []ToolResult{result("a", "one"), result("b", "two")}),
		},
		{
			name:  "results pair by call ID",
			left:  step([]ToolCall{call("a", `{}`), call("b", `{}`)}, []ToolResult{result("a", "one"), result("b", "two")}),
			right: step([]ToolCall{call("a", `{}`), call("b", `{}`)}, []ToolResult{result("b", "two"), result("a", "one")}),
			equal: true,
		},
		{
			name:  "invalid JSON raw bytes are significant",
			left:  step([]ToolCall{call("a", `{"a":`)}, []ToolResult{result("a", "error")}),
			right: step([]ToolCall{call("a", ` {"a":`)}, []ToolResult{result("a", "error")}),
		},
		{
			name:  "missing result differs from present result",
			left:  step([]ToolCall{call("a", `{}`)}, nil),
			right: step([]ToolCall{call("a", `{}`)}, []ToolResult{result("a", "ok")}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := stepSignature(tt.left)
			right := stepSignature(tt.right)
			if left == "" || right == "" || (left == right) != tt.equal {
				t.Fatalf("signatures = %q, %q, want equal %v", left, right, tt.equal)
			}
		})
	}
}
