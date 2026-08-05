package agent

// StepResult 是一步（一次模型调用 + 其工具执行）的结果。
type StepResult struct {
	// StepNumber 从 0 开始的步序号。
	StepNumber int `json:"step_number"`
	// Message 本步模型产出的 assistant 消息。
	Message Message `json:"message"`
	// ToolCalls 本步发起的工具调用。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolResults 本步工具的执行结果，与 ToolCalls 按 ToolCallID 配对。
	ToolResults []ToolResult `json:"tool_results,omitempty"`
	// Usage 本步用量。
	Usage Usage `json:"usage"`
	// FinishReason 本步停止原因。
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// GenerationOptions are the bounded portable scalar generation controls.
type GenerationOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
}

// StepPolicyInput is an immutable snapshot of completed execution state.
type StepPolicyInput struct {
	CompletedSteps []StepResult     `json:"completed_steps"`
	AvailableTools []ToolDefinition `json:"available_tools"`
}

// StepPolicyDecision may shape only the next model request.
type StepPolicyDecision struct {
	ActiveTools []string          `json:"active_tools,omitempty"`
	ToolChoice  *ToolChoice       `json:"tool_choice,omitempty"`
	Model       string            `json:"model,omitempty"`
	Generation  GenerationOptions `json:"generation"`
}

// StepPolicy is a pure request-selection function. Implementations receive
// copies and cannot replace the Agent's Model or mutate conversation history.
type StepPolicy func(StepPolicyInput) (StepPolicyDecision, error)

// StopCondition 判定是否该结束循环。组合式设计，可叠加：任一命中即停。
type StopCondition = func(steps []StepResult) bool

// StepCountIs 在步数达到 n 时停止。
func StepCountIs(n int) StopCondition {
	return func(steps []StepResult) bool { return len(steps) >= n }
}

// HasToolCall 在某步调用过指定工具时停止。
func HasToolCall(name string) StopCondition {
	return func(steps []StepResult) bool {
		for _, s := range steps {
			for _, c := range s.ToolCalls {
				if c.Name == name {
					return true
				}
			}
		}
		return false
	}
}

// MaxTokensUsed 在累计 token 用量达到 max 时停止。
func MaxTokensUsed(max int) StopCondition {
	return func(steps []StepResult) bool {
		var total int
		for _, s := range steps {
			total += s.Usage.TotalTokens
		}
		return total >= max
	}
}

// AnyStopCondition 把多个条件并成一个：任一命中即停。
func AnyStopCondition(conds ...StopCondition) StopCondition {
	return func(steps []StepResult) bool {
		for _, c := range conds {
			if c != nil && c(steps) {
				return true
			}
		}
		return false
	}
}

// ContextBudgetExceeded 判定最新请求的 prompt 上下文占用是否接近窗口上限。
// promptTokens 必须是最新一次请求的输入 token 数，而非跨请求累计的计费用量。
// 大窗口模型留固定 buffer，小窗口按比例留 —— 固定 buffer 在小窗口模型上会把可用空间吃光。
// 当前只做判定与中断，不在 Runtime 内自动压缩上下文。
func ContextBudgetExceeded(contextWindow, promptTokens int) bool {
	if contextWindow <= 0 {
		return false
	}
	return promptTokens > contextWindow-contextReserve(contextWindow)
}

// contextReserveRatio 小窗口模型的保留比例。
const contextReserveRatio = 0.2

// largeContextThreshold 超过此值视为大窗口模型，改用固定 buffer。
const largeContextThreshold = 100_000

// largeContextReserve 大窗口模型的固定保留 token 数。
const largeContextReserve = 20_000

// contextReserve 返回需要预留的 token 数。
func contextReserve(contextWindow int) int {
	if contextWindow > largeContextThreshold {
		return largeContextReserve
	}
	return int(float64(contextWindow) * contextReserveRatio)
}
