package agent

import (
	"errors"
	"fmt"
	"testing"
)

// makeStep builds one step: a single tool call plus its matching result.
func makeStep(n int, toolName, input, output string) StepResult {
	callID := fmt.Sprintf("c%d", n)
	return StepResult{
		StepNumber:  n,
		ToolCalls:   []ToolCall{{ID: callID, Name: toolName, Input: input}},
		ToolResults: []ToolResult{{ToolCallID: callID, Name: toolName, Content: output}},
	}
}

func TestStopConditions(t *testing.T) {
	steps := []StepResult{
		{StepNumber: 0, ToolCalls: []ToolCall{{Name: "score_round"}}, Usage: Usage{TotalTokens: 100}},
		{StepNumber: 1, Usage: Usage{TotalTokens: 150}},
	}

	tests := []struct {
		name string
		cond StopCondition
		want bool
	}{
		{name: "StepCountIs below count", cond: StepCountIs(3), want: false},
		{name: "StepCountIs count reached", cond: StepCountIs(2), want: true},
		{name: "HasToolCall hit", cond: HasToolCall("score_round"), want: true},
		{name: "HasToolCall miss", cond: HasToolCall("gen_options"), want: false},
		{name: "MaxTokensUsed under limit", cond: MaxTokensUsed(300), want: false},
		{name: "MaxTokensUsed at limit", cond: MaxTokensUsed(250), want: true},
		{name: "MaxTokensUsed over limit", cond: MaxTokensUsed(200), want: true},
		{
			name: "AnyStopCondition stops when any hits",
			cond: AnyStopCondition(StepCountIs(99), HasToolCall("score_round")),
			want: true,
		},
		{
			name: "AnyStopCondition none hit",
			cond: AnyStopCondition(StepCountIs(99), HasToolCall("nope")),
			want: false,
		},
		{
			name: "AnyStopCondition tolerates nil condition",
			cond: AnyStopCondition(nil, StepCountIs(2)),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cond(steps); got != tt.want {
				t.Errorf("condition = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoopDetectorDetect(t *testing.T) {
	tests := []struct {
		name         string
		window       int
		threshold    int
		steps        []StepResult
		wantDetected bool
	}{
		{
			name:      "same signature repeated past threshold aborts",
			window:    4,
			threshold: 2,
			// Same name, input, and result 3 times in a row: counts=3 > threshold=2 → hit.
			// Note 3 steps < window=4: if the implementation copied crush's early return
			// on a non-full window, this test would be a false green.
			steps: []StepResult{
				makeStep(0, "score_round", `{"a":1}`, "ok"),
				makeStep(1, "score_round", `{"a":1}`, "ok"),
				makeStep(2, "score_round", `{"a":1}`, "ok"),
			},
			wantDetected: true,
		},
		{
			name:      "repeats within threshold do not abort",
			window:    4,
			threshold: 2,
			steps: []StepResult{
				makeStep(0, "score_round", `{"a":1}`, "ok"),
				makeStep(1, "score_round", `{"a":1}`, "ok"),
			},
			wantDetected: false,
		},
		{
			name:      "consecutive calls with different inputs are not misjudged",
			window:    4,
			threshold: 2,
			steps: []StepResult{
				makeStep(0, "score_round", `{"a":1}`, "ok"),
				makeStep(1, "score_round", `{"a":2}`, "ok"),
				makeStep(2, "score_round", `{"a":3}`, "ok"),
				makeStep(3, "score_round", `{"a":4}`, "ok"),
			},
			wantDetected: false,
		},
		{
			name:      "same input with changing results is not misjudged",
			window:    4,
			threshold: 2,
			steps: []StepResult{
				makeStep(0, "poll", `{}`, "r1"),
				makeStep(1, "poll", `{}`, "r2"),
				makeStep(2, "poll", `{}`, "r3"),
			},
			wantDetected: false,
		},
		{
			name:      "different tool names are not misjudged",
			window:    4,
			threshold: 2,
			steps: []StepResult{
				makeStep(0, "score_round", `{}`, "ok"),
				makeStep(1, "gen_options", `{}`, "ok"),
				makeStep(2, "score_round", `{}`, "ok"),
			},
			wantDetected: false,
		},
		{
			name:      "repeats outside the sliding window are not counted",
			window:    2,
			threshold: 1,
			// The window only sees the last 2 steps: the repeats in the first two steps slide out.
			steps: []StepResult{
				makeStep(0, "score_round", `{"a":1}`, "ok"),
				makeStep(1, "score_round", `{"a":1}`, "ok"),
				makeStep(2, "gen_options", `{"b":1}`, "ok"),
				makeStep(3, "score_round", `{"a":1}`, "ok"),
			},
			wantDetected: false,
		},
		{
			name:      "steps without tool calls are excluded",
			window:    4,
			threshold: 2,
			steps: []StepResult{
				{StepNumber: 0, Message: NewAssistantMessage("hi")},
				{StepNumber: 1, Message: NewAssistantMessage("hi")},
				{StepNumber: 2, Message: NewAssistantMessage("hi")},
			},
			wantDetected: false,
		},
		{
			name:      "one ordered aggregate signature per step",
			window:    4,
			threshold: 1,
			steps: []StepResult{
				{
					StepNumber: 0,
					ToolCalls: []ToolCall{
						{ID: "a", Name: "score_round", Input: `{}`},
						{ID: "b", Name: "score_round", Input: `{}`},
					},
					ToolResults: []ToolResult{
						{ToolCallID: "a", Content: "ok"},
						{ToolCallID: "b", Content: "ok"},
					},
				},
			},
			wantDetected: false,
		},
		{
			// Calling different tools in parallel is normal usage and must not be misjudged.
			name:      "parallel calls to different tools within one step are not misjudged",
			window:    4,
			threshold: 2,
			steps: []StepResult{
				{
					StepNumber: 0,
					ToolCalls: []ToolCall{
						{ID: "a", Name: "score_round", Input: `{}`},
						{ID: "b", Name: "gen_options", Input: `{}`},
					},
					ToolResults: []ToolResult{
						{ToolCallID: "a", Content: "ok"},
						{ToolCallID: "b", Content: "ok"},
					},
				},
			},
			wantDetected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := NewLoopDetector(tt.window, tt.threshold)
			sig, count, detected := d.Detect(tt.steps)
			if detected != tt.wantDetected {
				t.Fatalf("Detect() detected = %v (sig=%s count=%d), want %v",
					detected, sig, count, tt.wantDetected)
			}
			if detected {
				if sig == "" {
					t.Error("a hit should return a signature, got empty")
				}
				if count <= tt.threshold {
					t.Errorf("on hit count = %d, should exceed threshold %d", count, tt.threshold)
				}
			}
		})
	}
}

func TestLoopDetectorValidate(t *testing.T) {
	tests := []struct {
		name      string
		window    int
		threshold int
		maxSteps  int
		wantErr   error
	}{
		{name: "window smaller than max_steps is valid", window: 4, threshold: 2, maxSteps: 8},
		{name: "window equal to max_steps is valid", window: 8, threshold: 2, maxSteps: 8},
		{
			name: "window greater than max_steps is invalid", window: 10, threshold: 5, maxSteps: 8,
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name: "threshold not less than window is invalid", window: 4, threshold: 4, maxSteps: 8,
			wantErr: ErrAgentConfigInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewLoopDetector(tt.window, tt.threshold).Validate(tt.maxSteps)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() unexpected error: %v", err)
			}
		})
	}
}

func TestLoopDetectorDefaults(t *testing.T) {
	d := NewLoopDetector(0, 0)
	if d.Window() != DefaultLoopDetectWindow || d.Threshold() != DefaultLoopDetectThreshold {
		t.Errorf("defaults = %d/%d, want %d/%d",
			d.Window(), d.Threshold(), DefaultLoopDetectWindow, DefaultLoopDetectThreshold)
	}
}

func TestContextBudgetExceeded(t *testing.T) {
	tests := []struct {
		name          string
		contextWindow int
		used          int
		want          bool
	}{
		{name: "large window fixed buffer not exceeded", contextWindow: 200_000, used: 179_000, want: false},
		{name: "large window fixed buffer exceeded", contextWindow: 200_000, used: 181_000, want: true},
		{name: "small window proportional reserve not exceeded", contextWindow: 8_000, used: 6_000, want: false},
		{name: "small window proportional reserve exceeded", contextWindow: 8_000, used: 6_500, want: true},
		{name: "unknown window never triggers", contextWindow: 0, used: 999_999, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContextBudgetExceeded(tt.contextWindow, tt.used); got != tt.want {
				t.Errorf("ContextBudgetExceeded(%d, %d) = %v, want %v",
					tt.contextWindow, tt.used, got, tt.want)
			}
		})
	}
}
