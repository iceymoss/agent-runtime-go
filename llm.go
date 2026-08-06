package agent

import (
	"context"
	"errors"
	"fmt"
)

// Core sentinel errors. The runtime exposes only sentinel errors; consuming
// applications map them to their own error codes with errors.Is.
var (
	// ErrAgentConfigInvalid reports an invalid agent configuration
	// (for example loop_detect_window > max_steps).
	ErrAgentConfigInvalid = errors.New("agent: invalid configuration")
	// ErrToolNotAllowed reports a tool that is not on the allowlist.
	ErrToolNotAllowed = errors.New("agent: tool not allowed")
	// ErrToolNotFound reports a tool that is not registered.
	ErrToolNotFound = errors.New("agent: tool not registered")
	// ErrLoopDetected reports a detected tool call loop.
	ErrLoopDetected = errors.New("agent: tool call loop detected")
	// ErrToolInputInvalid reports tool input that still violates the schema
	// after the bounded repair budget is exhausted.
	ErrToolInputInvalid = errors.New("agent: invalid tool input")
)

// Capabilities declares provider-neutral model features. Adapters must reject
// unsupported requests rather than silently degrading them.
type Capabilities struct {
	Tools              bool `json:"tools"`
	ToolChoiceNone     bool `json:"tool_choice_none"`
	ToolChoiceRequired bool `json:"tool_choice_required"`
	ToolChoiceNamed    bool `json:"tool_choice_named"`
	StructuredOutput   bool `json:"structured_output"`
	Media              bool `json:"media"`
	ImageInput         bool `json:"image_input,omitempty"`
	Reasoning          bool `json:"reasoning"`
	UsageDetails       bool `json:"usage_details"`
}

// Validate rejects internally contradictory capability declarations.
func (c Capabilities) Validate() error {
	if !c.Tools && (c.ToolChoiceNone || c.ToolChoiceRequired || c.ToolChoiceNamed) {
		return fmt.Errorf("tool choice capability requires tools capability")
	}
	return nil
}

// ToolChoiceMode is the portable tool selection mode.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
	ToolChoiceNamed    ToolChoiceMode = "named"
)

// ToolChoice selects whether, and which, tool a model may call.
type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	Name string         `json:"name,omitempty"`
}

// Validate checks choice shape, active tools, and model support.
func (c ToolChoice) Validate(tools []ToolDefinition, caps Capabilities) error {
	if err := caps.Validate(); err != nil {
		return err
	}
	if c.Mode == "" {
		c.Mode = ToolChoiceAuto
	}
	switch c.Mode {
	case ToolChoiceAuto:
		if c.Name != "" {
			return fmt.Errorf("auto tool choice cannot name a tool")
		}
		if len(tools) > 0 && !caps.Tools {
			return fmt.Errorf("model does not support tools")
		}
	case ToolChoiceNone:
		if c.Name != "" || !caps.ToolChoiceNone {
			return fmt.Errorf("model does not support none tool choice")
		}
	case ToolChoiceRequired:
		if c.Name != "" || len(tools) == 0 || !caps.ToolChoiceRequired {
			return fmt.Errorf("required tool choice is unsupported or has no active tools")
		}
	case ToolChoiceNamed:
		if c.Name == "" || !caps.ToolChoiceNamed {
			return fmt.Errorf("named tool choice is invalid or unsupported")
		}
		for _, tool := range tools {
			if tool.Name == c.Name {
				return nil
			}
		}
		return fmt.Errorf("named tool %q is not active", c.Name)
	default:
		return fmt.Errorf("unknown tool choice %q", c.Mode)
	}
	return nil
}

// GenerateRequest is one request the runtime sends to a Model, decoupled from
// any upstream protocol. Optional scalar fields use pointer + omitempty:
// nil is omitted while *0 is sent upstream as-is.
type GenerateRequest struct {
	// Model is the upstream model name resolved by the application.
	Model string `json:"model"`
	// Messages is the full conversation history, including system messages.
	Messages []Message `json:"messages"`
	// Tools lists the tool definitions available for this request (already allowlisted).
	Tools []ToolDefinition `json:"tools,omitempty"`
	// ToolChoice is omitted for portable auto behavior unless explicitly set.
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
	// Temperature is the sampling temperature.
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens caps a single generation.
	MaxTokens *int `json:"max_tokens,omitempty"`
	// TopP is the nucleus sampling parameter.
	TopP *float64 `json:"top_p,omitempty"`
}

// ValidateGenerateRequest centralizes validation for adapter-facing requests.
// Model may be empty for adapters configured with their own default model.
func ValidateGenerateRequest(req *GenerateRequest) error {
	if req == nil {
		return fmt.Errorf("nil generate request")
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("generate request has no messages")
	}
	for i, message := range req.Messages {
		if err := ValidateMessage(message); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
	}
	return nil
}

// ValidateGenerateRequestCapabilities rejects unsupported effects before a
// request reaches Model.
func ValidateGenerateRequestCapabilities(req *GenerateRequest, caps Capabilities) error {
	if err := ValidateGenerateRequest(req); err != nil {
		return err
	}
	return validateImageInputCapability(req.Messages, caps)
}

func validateImageInputCapability(messages []Message, caps Capabilities) error {
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == PartImage && !caps.ImageInput {
				return fmt.Errorf("model does not support image input")
			}
		}
	}
	return nil
}

// Usage is normalized token usage.
type Usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	ReasoningTokens     int `json:"reasoning_tokens,omitempty"`
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
}

// Validate checks the normalized usage contract. PromptTokens excludes cache
// categories; TotalTokens includes prompt, completion, cache creation and cache read.
func (u Usage) Validate() error {
	if u.PromptTokens < 0 || u.CompletionTokens < 0 || u.TotalTokens < 0 || u.ReasoningTokens < 0 || u.CacheCreationTokens < 0 || u.CacheReadTokens < 0 {
		return fmt.Errorf("usage counters cannot be negative")
	}
	components := u.PromptTokens + u.CompletionTokens + u.CacheCreationTokens + u.CacheReadTokens
	// Some test/custom models expose only total tokens. Require consistency when
	// any normalized component is present.
	if components != 0 && u.TotalTokens != components {
		return fmt.Errorf("usage total does not equal prompt plus completion and cache tokens")
	}
	if u.ReasoningTokens > u.CompletionTokens {
		return fmt.Errorf("reasoning tokens exceed completion tokens")
	}
	return nil
}

// InputTokens returns the complete current-request context occupancy.
func (u Usage) InputTokens() int {
	return u.PromptTokens + u.CacheCreationTokens + u.CacheReadTokens
}

// Add accumulates another usage sample, used to total usage across steps.
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.CacheCreationTokens += other.CacheCreationTokens
	u.CacheReadTokens += other.CacheReadTokens
}

// Response is the result of one model generation step.
type Response struct {
	// Message is the assistant message produced by the model
	// (it may contain text and tool calls).
	Message Message `json:"message"`
	// Usage is the usage for this step.
	Usage Usage `json:"usage"`
	// FinishReason is the reason generation stopped.
	FinishReason FinishReason `json:"finish_reason"`
	// ModelName is the model actually used, for cost accounting.
	ModelName string `json:"model_name,omitempty"`
}

// ToolCalls returns the tool calls made in this step.
func (r *Response) ToolCalls() []ToolCall { return r.Message.ToolCalls() }

// ValidateResponse validates a provider's terminal response before the runtime
// can treat it as complete or execute any tool call.
func ValidateResponse(r *Response) error {
	if r == nil {
		return fmt.Errorf("nil terminal response")
	}
	if r.Message.Role != RoleAssistant {
		return fmt.Errorf("terminal message role is %q, want assistant", r.Message.Role)
	}
	if err := ValidateMessage(r.Message); err != nil {
		return fmt.Errorf("invalid terminal message: %w", err)
	}
	if r.FinishReason == "" || r.Message.FinishReason != r.FinishReason {
		return fmt.Errorf("inconsistent finish reasons %q and %q", r.FinishReason, r.Message.FinishReason)
	}
	calls := r.ToolCalls()
	if r.FinishReason == FinishToolCalls {
		if len(calls) == 0 {
			return fmt.Errorf("tool_calls finish without tool calls")
		}
	} else if len(calls) != 0 {
		return fmt.Errorf("finish %q contains tool calls", r.FinishReason)
	}
	if r.FinishReason != FinishStop && r.FinishReason != FinishToolCalls && r.FinishReason != FinishLength {
		return fmt.Errorf("incomplete terminal finish %q", r.FinishReason)
	}
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.ID == "" || call.Name == "" {
			return fmt.Errorf("tool call has empty id or name")
		}
		if _, ok := seen[call.ID]; ok {
			return fmt.Errorf("duplicate tool call id %q", call.ID)
		}
		seen[call.ID] = struct{}{}
	}
	return nil
}

func validateEffectiveToolChoice(choice ToolChoice, response *Response) error {
	calls := response.ToolCalls()
	switch choice.Mode {
	case "", ToolChoiceAuto:
		return nil
	case ToolChoiceNone:
		if len(calls) != 0 {
			return fmt.Errorf("none tool choice received %d tool calls", len(calls))
		}
	case ToolChoiceRequired:
		if len(calls) == 0 {
			return fmt.Errorf("required tool choice received no tool calls")
		}
	case ToolChoiceNamed:
		if len(calls) == 0 {
			return fmt.Errorf("named tool choice %q received no tool calls", choice.Name)
		}
		for _, call := range calls {
			if call.Name != choice.Name {
				return fmt.Errorf("named tool choice %q received call to %q", choice.Name, call.Name)
			}
		}
	}
	return nil
}

// StreamChunk is one incremental fragment of a streamed response.
// Adapters are responsible for assembling upstream fragments (OpenAI groups by
// index, Anthropic buffers input_json_delta per content_block) into a complete
// ToolCall before emitting ChunkToolCall.
type StreamChunk struct {
	Type ChunkType `json:"type"`
	// TextDelta is valid only when Type=ChunkText.
	TextDelta string `json:"text_delta,omitempty"`
	// ToolCall is valid only when Type=ChunkToolCall; its input is fully assembled.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	// Response is valid only when Type=ChunkFinish; it carries the complete
	// message and normalized usage.
	Response *Response `json:"response,omitempty"`
	// Err is valid only when Type=ChunkError. It carries the upstream error
	// as-is and must never be swallowed.
	Err error `json:"-"`
}

// ChunkType is the streamed fragment type.
type ChunkType string

const (
	// ChunkText is a text delta.
	ChunkText ChunkType = "text"
	// ChunkToolCall is one fully assembled tool call.
	ChunkToolCall ChunkType = "tool_call"
	// ChunkFinish ends the step and carries the complete Response.
	ChunkFinish ChunkType = "finish"
	// ChunkError reports a failure and carries the original upstream error.
	ChunkError ChunkType = "error"
)

// Model is the provider integration port. agent-runtime-go defines only this
// interface; adapters implement it in the consuming application, keeping the
// core portable (dependency inversion).
type Model interface {
	// Name returns the provider identifier.
	Name() string
	// Capabilities returns a coherent provider capability declaration.
	Capabilities() Capabilities
	// Stream generates one step as a stream. Implementations must close the
	// channel before returning or at end of stream. On upstream failure, send
	// ChunkError first and then close; propagate the error message as-is.
	Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamChunk, error)
}

// Generator is the optional non-streaming model capability. The Runtime only
// depends on Model and never requires or probes this interface.
type Generator interface {
	Generate(ctx context.Context, req *GenerateRequest) (*Response, error)
}
