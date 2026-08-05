// Package agent 是与业务解耦的通用 agent runtime：
// 多轮 tool-calling 循环、流式输出、会话持久化接口、提示词渲染、工具注册与白名单。
//
// 铁律：本包及其子包不得 import 任何 internal/ 包（由 deps_test.go 断言）。
// 业务侧通过实现 Model / Tool / SessionStore / MessageStore 接口接入。
package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// Role 是消息角色。
type Role string

const (
	// RoleSystem 系统提示词。
	RoleSystem Role = "system"
	// RoleUser 用户输入。
	RoleUser Role = "user"
	// RoleAssistant 模型输出（可能带工具调用）。
	RoleAssistant Role = "assistant"
	// RoleTool 工具执行结果，回灌给模型。
	RoleTool Role = "tool"
)

// PartType 是消息内容块类型。
type PartType string

const (
	// PartText 纯文本块。
	PartText PartType = "text"
	// PartToolCall 模型发起的工具调用块。
	PartToolCall PartType = "tool_call"
	// PartToolResult 工具执行结果块。
	PartToolResult PartType = "tool_result"
	// PartImage is a provider-neutral user image input.
	PartImage PartType = "image"
)

var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ImageContent contains either inline bytes or an opaque immutable reference.
type ImageContent struct {
	MediaType string `json:"media_type"`
	Data      []byte `json:"data,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Digest    string `json:"digest,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
}

// ContentPart 是消息的一个内容块。
// 一条 assistant 消息可能同时含文本与多个工具调用，故用块列表而非单一字符串。
type ContentPart struct {
	Type PartType `json:"type"`
	// Text 仅 Type=PartText 时有效。
	Text string `json:"text,omitempty"`
	// ToolCall 仅 Type=PartToolCall 时有效。
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	// ToolResult 仅 Type=PartToolResult 时有效。
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	// Image is valid only when Type=PartImage.
	Image *ImageContent `json:"image,omitempty"`
}

// ToolCall 是模型发起的一次工具调用。
type ToolCall struct {
	// ID 上游给出的调用标识，用于与 ToolResult 配对。
	ID string `json:"id"`
	// Name 工具名。
	Name string `json:"name"`
	// Input 原始参数 JSON。不预先解析：可能非法，需回灌错误给模型自我修正。
	Input string `json:"input"`
}

// ToolResult 是一次工具调用的执行结果。
type ToolResult struct {
	// ToolCallID 对应的 ToolCall.ID。
	ToolCallID string `json:"tool_call_id"`
	// Name 工具名，便于日志与事件展示。
	Name string `json:"name"`
	// Content 结果内容，回灌给模型的文本（结构化结果自行序列化为 JSON 字符串）。
	Content string `json:"content"`
	// IsError 标记该结果是错误（参数非法、工具不可用、执行失败）。
	// 错误同样回灌给模型，给它换路或自我修正的机会。
	IsError bool `json:"is_error,omitempty"`
	// StopTurn 为 true 时立即收尾本轮，不再进入下一步。
	StopTurn bool `json:"stop_turn,omitempty"`
}

// Message 是一条对话消息。
type Message struct {
	Role Role `json:"role"`
	// Parts 内容块列表。
	Parts []ContentPart `json:"parts"`
	// FinishReason 仅 assistant 消息有意义：stop/tool_calls/length/error。
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// FinishReason 是模型停止生成的原因。
type FinishReason string

const (
	// FinishStop 正常结束。
	FinishStop FinishReason = "stop"
	// FinishToolCalls 因发起工具调用而停止。
	FinishToolCalls FinishReason = "tool_calls"
	// FinishLength 达到长度上限。
	FinishLength FinishReason = "length"
	// FinishError 出错中断。
	FinishError FinishReason = "error"
)

// NewSystemMessage 构造系统消息。
func NewSystemMessage(text string) Message {
	return Message{Role: RoleSystem, Parts: []ContentPart{{Type: PartText, Text: text}}}
}

// NewUserMessage 构造用户消息。
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Parts: []ContentPart{{Type: PartText, Text: text}}}
}

// NewAssistantMessage 构造纯文本 assistant 消息。
func NewAssistantMessage(text string) Message {
	return Message{
		Role:         RoleAssistant,
		Parts:        []ContentPart{{Type: PartText, Text: text}},
		FinishReason: FinishStop,
	}
}

// NewToolMessage 把一批工具结果包成一条 tool 消息。
func NewToolMessage(results ...ToolResult) Message {
	parts := make([]ContentPart, 0, len(results))
	for i := range results {
		parts = append(parts, ContentPart{Type: PartToolResult, ToolResult: &results[i]})
	}
	return Message{Role: RoleTool, Parts: parts}
}

// Text 拼接消息里所有文本块。
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls 返回消息里的所有工具调用。
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, p := range m.Parts {
		if p.Type == PartToolCall && p.ToolCall != nil {
			calls = append(calls, *p.ToolCall)
		}
	}
	return calls
}

// ToolResults 返回消息里的所有工具结果。
func (m Message) ToolResults() []ToolResult {
	var results []ToolResult
	for _, p := range m.Parts {
		if p.Type == PartToolResult && p.ToolResult != nil {
			results = append(results, *p.ToolResult)
		}
	}
	return results
}

// ValidateMessage validates the currently supported text/tool content model.
func ValidateMessage(m Message) error {
	if m.Role != RoleSystem && m.Role != RoleUser && m.Role != RoleAssistant && m.Role != RoleTool {
		return fmt.Errorf("unknown message role %q", m.Role)
	}
	if len(m.Parts) == 0 {
		return fmt.Errorf("message has no content parts")
	}
	for _, part := range m.Parts {
		switch part.Type {
		case PartText:
			if part.ToolCall != nil || part.ToolResult != nil || part.Image != nil || m.Role == RoleTool {
				return fmt.Errorf("invalid text part for role %q", m.Role)
			}
		case PartToolCall:
			if m.Role != RoleAssistant || part.ToolCall == nil || part.ToolResult != nil || part.Image != nil || part.Text != "" || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
				return fmt.Errorf("invalid tool call part")
			}
		case PartToolResult:
			if m.Role != RoleTool || part.ToolResult == nil || part.ToolCall != nil || part.Image != nil || part.Text != "" || part.ToolResult.ToolCallID == "" || part.ToolResult.Name == "" {
				return fmt.Errorf("invalid tool result part")
			}
		case PartImage:
			if err := validateImagePart(m.Role, part); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown content part %q", part.Type)
		}
	}
	return nil
}

func validateImagePart(role Role, part ContentPart) error {
	if role != RoleUser || part.Image == nil || part.Text != "" || part.ToolCall != nil || part.ToolResult != nil {
		return fmt.Errorf("invalid image part")
	}
	image := part.Image
	if image.MediaType != "image/jpeg" && image.MediaType != "image/png" && image.MediaType != "image/webp" {
		return fmt.Errorf("invalid image media type %q", image.MediaType)
	}
	hasData := len(image.Data) != 0
	hasRef := strings.TrimSpace(image.Ref) != ""
	if hasData == hasRef {
		return fmt.Errorf("image must contain exactly one source")
	}
	if hasData {
		if image.Ref != "" || image.Digest != "" || image.SizeBytes != 0 {
			return fmt.Errorf("inline image contains reference metadata")
		}
		return nil
	}
	if !imageDigestPattern.MatchString(image.Digest) || image.SizeBytes <= 0 {
		return fmt.Errorf("invalid referenced image metadata")
	}
	return nil
}
