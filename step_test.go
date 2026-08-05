package agent

import (
	"errors"
	"fmt"
	"testing"
)

// makeStep 构造一步：一次工具调用 + 对应结果。
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
		{name: "StepCountIs 未达步数", cond: StepCountIs(3), want: false},
		{name: "StepCountIs 已达步数", cond: StepCountIs(2), want: true},
		{name: "HasToolCall 命中", cond: HasToolCall("score_round"), want: true},
		{name: "HasToolCall 未命中", cond: HasToolCall("gen_options"), want: false},
		{name: "MaxTokensUsed 未超", cond: MaxTokensUsed(300), want: false},
		{name: "MaxTokensUsed 达到上限", cond: MaxTokensUsed(250), want: true},
		{name: "MaxTokensUsed 已超", cond: MaxTokensUsed(200), want: true},
		{
			name: "AnyStopCondition 任一命中即停",
			cond: AnyStopCondition(StepCountIs(99), HasToolCall("score_round")),
			want: true,
		},
		{
			name: "AnyStopCondition 全不命中",
			cond: AnyStopCondition(StepCountIs(99), HasToolCall("nope")),
			want: false,
		},
		{
			name: "AnyStopCondition 容忍 nil 条件",
			cond: AnyStopCondition(nil, StepCountIs(2)),
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cond(steps); got != tt.want {
				t.Errorf("条件判定 = %v, 期望 %v", got, tt.want)
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
			name:      "同签名重复超阈值即中断",
			window:    4,
			threshold: 2,
			// 同名同参同结果连续 3 次：counts=3 > threshold=2 命中。
			// 注意 3 步 < window=4，若实现照抄 crush 的窗口未满提前返回，这里会假绿。
			steps: []StepResult{
				makeStep(0, "score_round", `{"a":1}`, "ok"),
				makeStep(1, "score_round", `{"a":1}`, "ok"),
				makeStep(2, "score_round", `{"a":1}`, "ok"),
			},
			wantDetected: true,
		},
		{
			name:      "重复未超阈值不中断",
			window:    4,
			threshold: 2,
			steps: []StepResult{
				makeStep(0, "score_round", `{"a":1}`, "ok"),
				makeStep(1, "score_round", `{"a":1}`, "ok"),
			},
			wantDetected: false,
		},
		{
			name:      "参数不同的连续调用不误判",
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
			name:      "同参数但结果在变不误判",
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
			name:      "工具名不同不误判",
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
			name:      "滑动窗口外的重复不计入",
			window:    2,
			threshold: 1,
			// 窗口只看最后 2 步：前两步的重复被移出窗口。
			steps: []StepResult{
				makeStep(0, "score_round", `{"a":1}`, "ok"),
				makeStep(1, "score_round", `{"a":1}`, "ok"),
				makeStep(2, "gen_options", `{"b":1}`, "ok"),
				makeStep(3, "score_round", `{"a":1}`, "ok"),
			},
			wantDetected: false,
		},
		{
			name:      "无工具调用的步骤不参与判定",
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
			name:      "同一步只生成一个有序聚合签名",
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
			// 并行调用不同工具是正常用法，不能误判。
			name:      "同一步内并行调用不同工具不误判",
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
				t.Fatalf("Detect() detected = %v (sig=%s count=%d), 期望 %v",
					detected, sig, count, tt.wantDetected)
			}
			if detected {
				if sig == "" {
					t.Error("命中时应返回签名，实际为空")
				}
				if count <= tt.threshold {
					t.Errorf("命中时 count = %d, 应大于阈值 %d", count, tt.threshold)
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
		{name: "窗口小于 max_steps 合法", window: 4, threshold: 2, maxSteps: 8},
		{name: "窗口等于 max_steps 合法", window: 8, threshold: 2, maxSteps: 8},
		{
			name: "窗口大于 max_steps 非法", window: 10, threshold: 5, maxSteps: 8,
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name: "阈值不小于窗口非法", window: 4, threshold: 4, maxSteps: 8,
			wantErr: ErrAgentConfigInvalid,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewLoopDetector(tt.window, tt.threshold).Validate(tt.maxSteps)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Validate() error = %v, 期望 %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() 意外报错: %v", err)
			}
		})
	}
}

func TestLoopDetectorDefaults(t *testing.T) {
	d := NewLoopDetector(0, 0)
	if d.Window() != DefaultLoopDetectWindow || d.Threshold() != DefaultLoopDetectThreshold {
		t.Errorf("默认值 = %d/%d, 期望 %d/%d",
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
		{name: "大窗口留固定 buffer 未超", contextWindow: 200_000, used: 179_000, want: false},
		{name: "大窗口留固定 buffer 已超", contextWindow: 200_000, used: 181_000, want: true},
		{name: "小窗口按比例留未超", contextWindow: 8_000, used: 6_000, want: false},
		{name: "小窗口按比例留已超", contextWindow: 8_000, used: 6_500, want: true},
		{name: "窗口未知时不判定", contextWindow: 0, used: 999_999, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ContextBudgetExceeded(tt.contextWindow, tt.used); got != tt.want {
				t.Errorf("ContextBudgetExceeded(%d, %d) = %v, 期望 %v",
					tt.contextWindow, tt.used, got, tt.want)
			}
		})
	}
}
