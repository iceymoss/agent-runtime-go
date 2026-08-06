package agent

// StepResult is the result of one step (one model call plus its tool executions).
type StepResult struct {
	// StepNumber is the zero-based step index.
	StepNumber int `json:"step_number"`
	// Message is the assistant message produced in this step.
	Message Message `json:"message"`
	// ToolCalls are the tool calls made in this step.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolResults are this step's tool execution results, paired with
	// ToolCalls by ToolCallID.
	ToolResults []ToolResult `json:"tool_results,omitempty"`
	// Usage is the usage for this step.
	Usage Usage `json:"usage"`
	// FinishReason is the reason this step stopped.
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

// StopCondition decides whether the loop should end. Conditions are
// composable and stack: any match stops the run.
type StopCondition = func(steps []StepResult) bool

// StepCountIs stops when the step count reaches n.
func StepCountIs(n int) StopCondition {
	return func(steps []StepResult) bool { return len(steps) >= n }
}

// HasToolCall stops once any step has called the named tool.
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

// MaxTokensUsed stops when accumulated token usage reaches max.
func MaxTokensUsed(max int) StopCondition {
	return func(steps []StepResult) bool {
		var total int
		for _, s := range steps {
			total += s.Usage.TotalTokens
		}
		return total >= max
	}
}

// AnyStopCondition merges several conditions into one: any match stops the run.
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

// ContextBudgetExceeded reports whether the latest request's prompt occupancy
// is close to the window limit. promptTokens must be the input token count of
// the latest request, not billed usage accumulated across requests.
// Large-window models reserve a fixed buffer while small windows reserve
// proportionally: a fixed buffer would consume most of a small window.
// The runtime only detects and interrupts; it never compacts context itself.
func ContextBudgetExceeded(contextWindow, promptTokens int) bool {
	if contextWindow <= 0 {
		return false
	}
	return promptTokens > contextWindow-contextReserve(contextWindow)
}

// contextReserveRatio is the reserve ratio for small-window models.
const contextReserveRatio = 0.2

// largeContextThreshold marks a model as large-window, switching to a fixed buffer.
const largeContextThreshold = 100_000

// largeContextReserve is the fixed reserve for large-window models.
const largeContextReserve = 20_000

// contextReserve returns the number of tokens to reserve.
func contextReserve(contextWindow int) int {
	if contextWindow > largeContextThreshold {
		return largeContextReserve
	}
	return int(float64(contextWindow) * contextReserveRatio)
}
