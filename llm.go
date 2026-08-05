package agent

import (
	"context"
	"errors"
	"fmt"
)

// 内核哨兵错误。agent-runtime-go 不得 import internal/error_codes（铁律），
// 故内核只暴露哨兵错误，由业务侧用 errors.Is 映射到 7001-7008 业务码。
var (
	// ErrAgentConfigInvalid agent 配置非法（如 loop_detect_window > max_steps）。
	ErrAgentConfigInvalid = errors.New("agent: 配置非法")
	// ErrToolNotAllowed 工具不在白名单。
	ErrToolNotAllowed = errors.New("agent: 工具不在白名单")
	// ErrToolNotFound 工具未注册。
	ErrToolNotFound = errors.New("agent: 工具未注册")
	// ErrLoopDetected 检测到工具调用死循环。
	ErrLoopDetected = errors.New("agent: 检测到工具调用死循环")
	// ErrToolInputInvalid 工具参数在有限纠错预算内仍不符合 schema。
	ErrToolInputInvalid = errors.New("agent: 工具参数非法")
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

// GenerateRequest 是内核发给 Model 的一次请求（与具体上游协议解耦）。
// 可选标量字段用指针 + omitempty：nil 省略，*0 照常发送到上游。
type GenerateRequest struct {
	// Model 上游模型名（由业务侧按档位 large/small 解析后填入）。
	Model string `json:"model"`
	// Messages 完整对话历史，含 system 消息。
	Messages []Message `json:"messages"`
	// Tools 本次可用的工具定义（已过白名单）。
	Tools []ToolDefinition `json:"tools,omitempty"`
	// ToolChoice is omitted for portable auto behavior unless explicitly set.
	ToolChoice *ToolChoice `json:"tool_choice,omitempty"`
	// Temperature 采样温度。
	Temperature *float64 `json:"temperature,omitempty"`
	// MaxTokens 单次生成上限。
	MaxTokens *int `json:"max_tokens,omitempty"`
	// TopP 核采样。
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

// Usage 是 token 用量。
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

// Add 累加另一份用量，用于跨步累计。
func (u *Usage) Add(other Usage) {
	u.PromptTokens += other.PromptTokens
	u.CompletionTokens += other.CompletionTokens
	u.TotalTokens += other.TotalTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.CacheCreationTokens += other.CacheCreationTokens
	u.CacheReadTokens += other.CacheReadTokens
}

// Response 是模型一步生成的结果。
type Response struct {
	// Message 模型产出的 assistant 消息（可能含文本与工具调用）。
	Message Message `json:"message"`
	// Usage 本步用量。
	Usage Usage `json:"usage"`
	// FinishReason 停止原因。
	FinishReason FinishReason `json:"finish_reason"`
	// ModelName 实际使用的模型名，成本核算用。
	ModelName string `json:"model_name,omitempty"`
}

// ToolCalls 返回本步的工具调用。
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

// StreamChunk 是流式响应的一个增量片段。
// 适配器负责把上游的分片（OpenAI 按 index 归组、Anthropic 按 content_block 缓冲
// input_json_delta）拼装成完整的 ToolCall 后再发出 ChunkToolCall。
type StreamChunk struct {
	Type ChunkType `json:"type"`
	// TextDelta 仅 Type=ChunkText 时有效。
	TextDelta string `json:"text_delta,omitempty"`
	// ToolCall 仅 Type=ChunkToolCall 时有效，且参数已拼接完整。
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	// Response 仅 Type=ChunkFinish 时有效，含完整消息与归一后的用量。
	Response *Response `json:"response,omitempty"`
	// Err 仅 Type=ChunkError 时有效，原样携带上游错误信息，不得吞掉。
	Err error `json:"-"`
}

// ChunkType 是流式片段类型。
type ChunkType string

const (
	// ChunkText 文本增量。
	ChunkText ChunkType = "text"
	// ChunkToolCall 一个已拼装完整的工具调用。
	ChunkToolCall ChunkType = "tool_call"
	// ChunkFinish 本步结束，携带完整 Response。
	ChunkFinish ChunkType = "finish"
	// ChunkError 出错，携带上游原始错误。
	ChunkError ChunkType = "error"
)

// Model 是大模型接入接口。实现放在 internal/relay/channel（依赖倒置：
// agent-runtime-go 只定接口，adapter 反向实现，内核才拿得出去）。
type Model interface {
	// Name 返回 provider 标识。
	Name() string
	// Capabilities returns a coherent provider capability declaration.
	Capabilities() Capabilities
	// Stream 流式生成一步。实现必须在返回前或流末尾关闭 channel，
	// 上游出错时先发 ChunkError 再关闭，错误信息原样传播。
	Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamChunk, error)
}

// Generator is the optional non-streaming model capability. The Runtime only
// depends on Model and never requires or probes this interface.
type Generator interface {
	Generate(ctx context.Context, req *GenerateRequest) (*Response, error)
}
