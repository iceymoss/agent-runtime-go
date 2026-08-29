package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// Role is the message role.
type Role string

const (
	// RoleSystem is the system prompt.
	RoleSystem Role = "system"
	// RoleUser is user input.
	RoleUser Role = "user"
	// RoleAssistant is model output, possibly with tool calls.
	RoleAssistant Role = "assistant"
	// RoleTool carries tool execution results fed back to the model.
	RoleTool Role = "tool"
)

// PartType is the message content part type.
type PartType string

const (
	// PartText is a plain text part.
	PartText PartType = "text"
	// PartToolCall is a tool call initiated by the model.
	PartToolCall PartType = "tool_call"
	// PartToolResult is a tool execution result.
	PartToolResult PartType = "tool_result"
	// PartImage is a provider-neutral user image input.
	PartImage PartType = "image"
	// PartReasoning is a model's own thinking, kept separate from the answer.
	//
	// It is not part of Message.Text() and adapters do not send it back upstream:
	// most providers reject their own reasoning as assistant input, and a chain
	// of thought replayed as conversation would change what the model is
	// answering. Keep it for display and audit; treat it as untrusted text like
	// any other model output.
	PartReasoning PartType = "reasoning"
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

// ContentPart is one content part of a message.
// An assistant message may contain text and several tool calls at once, hence
// a part list instead of a single string.
type ContentPart struct {
	Type PartType `json:"type"`
	// Text is valid when Type=PartText or Type=PartReasoning.
	Text string `json:"text,omitempty"`
	// ToolCall is valid only when Type=PartToolCall.
	ToolCall *ToolCall `json:"tool_call,omitempty"`
	// ToolResult is valid only when Type=PartToolResult.
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	// Image is valid only when Type=PartImage.
	Image *ImageContent `json:"image,omitempty"`
}

// ToolCall is one tool call initiated by the model.
type ToolCall struct {
	// ID is the upstream call identifier, used to pair with a ToolResult.
	ID string `json:"id"`
	// Name is the tool name.
	Name string `json:"name"`
	// Input is the raw input JSON. It is not parsed eagerly: it may be invalid,
	// and the error is fed back so the model can correct itself.
	Input string `json:"input"`
}

// ToolResult is the execution result of one tool call.
type ToolResult struct {
	// ToolCallID references the matching ToolCall.ID.
	ToolCallID string `json:"tool_call_id"`
	// Name is the tool name, useful for logs and events.
	Name string `json:"name"`
	// Content is the result text fed back to the model. Serialize structured
	// results to a JSON string yourself.
	Content string `json:"content"`
	// IsError marks the result as an error (invalid input, unavailable tool,
	// or execution failure). Errors are also fed back to the model so it can
	// change course or correct itself.
	IsError bool `json:"is_error,omitempty"`
	// StopTurn, when true, ends the turn immediately without another step.
	StopTurn bool `json:"stop_turn,omitempty"`
}

// Message is one conversation message.
type Message struct {
	Role Role `json:"role"`
	// Parts is the list of content parts.
	Parts []ContentPart `json:"parts"`
	// FinishReason is meaningful only for assistant messages:
	// stop/tool_calls/length/error.
	FinishReason FinishReason `json:"finish_reason,omitempty"`
}

// FinishReason is the reason the model stopped generating.
type FinishReason string

const (
	// FinishStop is a normal finish.
	FinishStop FinishReason = "stop"
	// FinishToolCalls means generation stopped to make tool calls.
	FinishToolCalls FinishReason = "tool_calls"
	// FinishLength means the length limit was reached.
	FinishLength FinishReason = "length"
	// FinishError means generation was interrupted by an error.
	FinishError FinishReason = "error"
)

// NewSystemMessage builds a system message.
func NewSystemMessage(text string) Message {
	return Message{Role: RoleSystem, Parts: []ContentPart{{Type: PartText, Text: text}}}
}

// NewUserMessage builds a user message.
func NewUserMessage(text string) Message {
	return Message{Role: RoleUser, Parts: []ContentPart{{Type: PartText, Text: text}}}
}

// NewAssistantMessage builds a text-only assistant message.
func NewAssistantMessage(text string) Message {
	return Message{
		Role:         RoleAssistant,
		Parts:        []ContentPart{{Type: PartText, Text: text}},
		FinishReason: FinishStop,
	}
}

// NewToolMessage wraps a batch of tool results into one tool message.
func NewToolMessage(results ...ToolResult) Message {
	parts := make([]ContentPart, 0, len(results))
	for i := range results {
		parts = append(parts, ContentPart{Type: PartToolResult, ToolResult: &results[i]})
	}
	return Message{Role: RoleTool, Parts: parts}
}

// Text concatenates all text parts of the message.
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// Reasoning returns the model's thinking, which Text deliberately omits.
//
// An application shows it, stores it, or ignores it; it never feeds it back as
// input, because the adapter that produced it will not send it upstream either.
func (m Message) Reasoning() string {
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == PartReasoning {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// ToolCalls returns all tool calls in the message.
func (m Message) ToolCalls() []ToolCall {
	var calls []ToolCall
	for _, p := range m.Parts {
		if p.Type == PartToolCall && p.ToolCall != nil {
			calls = append(calls, *p.ToolCall)
		}
	}
	return calls
}

// ToolResults returns all tool results in the message.
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
		case PartReasoning:
			if m.Role != RoleAssistant || part.ToolCall != nil || part.ToolResult != nil || part.Image != nil {
				return fmt.Errorf("invalid reasoning part for role %q", m.Role)
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

// cloneMessages deep-copies a message slice.
//
// Everything the runtime hands across its API boundary is copied, so a caller
// that mutates what it received cannot reach back into state the runtime is
// still using - a bug class this package defends against rather than documents.
func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, len(messages))
	for i, message := range messages {
		cloned[i] = message
		cloned[i].Parts = make([]ContentPart, len(message.Parts))
		for j, part := range message.Parts {
			cloned[i].Parts[j] = part
			if part.ToolCall != nil {
				call := *part.ToolCall
				cloned[i].Parts[j].ToolCall = &call
			}
			if part.ToolResult != nil {
				result := *part.ToolResult
				cloned[i].Parts[j].ToolResult = &result
			}
			if part.Image != nil {
				image := *part.Image
				image.Data = append([]byte(nil), part.Image.Data...)
				cloned[i].Parts[j].Image = &image
			}
		}
	}
	return cloned
}
