package context

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/iceymoss/agent-runtime-go"
)

type openCall struct {
	name      string
	partIndex int
}

// NormalizeHistory validates canonical ordered history without sorting or
// mutating its input. Only committed terminal facts may repair an interrupted
// final tool exchange.
func NormalizeHistory(request NormalizeRequest) (NormalizeResult, error) {
	if request.Policy == "" {
		request.Policy = RepairReject
	}
	if request.Policy != RepairReject && request.Policy != RepairFromTerminal {
		return NormalizeResult{}, contextError(CodeInvalidRequest, "normalize", "", ErrInvalidRequest, fmt.Errorf("unknown repair policy %q", request.Policy))
	}

	messages := cloneMessages(request.Messages)
	result := NormalizeResult{Messages: make([]agent.Message, 0, len(messages))}
	open := make(map[string]openCall)
	seenCalls := make(map[string]struct{})
	seenResults := make(map[string]struct{})
	exchangeStart := -1
	exchangeSourceStart := -1
	callIDs := make([]string, 0)

	for sourceIndex, message := range messages {
		message, empty, diagnostics, err := sanitizeMessage(message, sourceIndex)
		result.Diagnostics = append(result.Diagnostics, diagnostics...)
		if err != nil {
			return NormalizeResult{}, err
		}
		if empty {
			continue
		}
		if len(open) != 0 && message.Role != agent.RoleTool {
			return NormalizeResult{}, pairingError("normalize", "tool result crossed a message boundary")
		}

		outputIndex := len(result.Messages)
		switch message.Role {
		case agent.RoleAssistant:
			for partIndex := range message.Parts {
				part := &message.Parts[partIndex]
				if part.Type != agent.PartToolCall {
					continue
				}
				call := part.ToolCall
				if _, exists := seenCalls[call.ID]; exists {
					return NormalizeResult{}, pairingError("normalize", "duplicate tool call ID")
				}
				if _, exists := seenResults[call.ID]; exists {
					return NormalizeResult{}, pairingError("normalize", "tool call ID was already closed")
				}
				seenCalls[call.ID] = struct{}{}
				open[call.ID] = openCall{name: call.Name, partIndex: partIndex}
				callIDs = append(callIDs, call.ID)
				if !json.Valid([]byte(call.Input)) {
					result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: DiagnosticMalformedInput, MessageIndex: sourceIndex, PartIndex: partIndex, CallID: call.ID})
				}
			}
			if len(open) != 0 {
				exchangeStart = outputIndex
				exchangeSourceStart = sourceIndex
			}
		case agent.RoleTool:
			if len(open) == 0 {
				return NormalizeResult{}, pairingError("normalize", "orphan tool result")
			}
			for partIndex := range message.Parts {
				part := &message.Parts[partIndex]
				resultPart := part.ToolResult
				call, exists := open[resultPart.ToolCallID]
				if !exists {
					if _, closed := seenResults[resultPart.ToolCallID]; closed {
						return NormalizeResult{}, pairingError("normalize", "duplicate tool result")
					}
					return NormalizeResult{}, pairingError("normalize", "orphan tool result")
				}
				if resultPart.Name == "" {
					resultPart.Name = call.name
					result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: DiagnosticResultNameFilled, MessageIndex: sourceIndex, PartIndex: partIndex, CallID: resultPart.ToolCallID})
				} else if resultPart.Name != call.name {
					return NormalizeResult{}, pairingError("normalize", "tool result name mismatch")
				}
				delete(open, resultPart.ToolCallID)
				seenResults[resultPart.ToolCallID] = struct{}{}
			}
		}

		result.Messages = append(result.Messages, message)
		if message.Role == agent.RoleTool && len(open) == 0 {
			result.Exchanges = append(result.Exchanges, ToolExchange{
				Start: exchangeStart, End: outputIndex, CallIDs: append([]string(nil), callIDs...),
				SourceStart: exchangeSourceStart, SourceEnd: sourceIndex,
			})
			exchangeStart, exchangeSourceStart = -1, -1
			callIDs = callIDs[:0]
		}
	}

	if len(open) != 0 {
		if request.Policy != RepairFromTerminal {
			return NormalizeResult{}, contextError(CodeHistoryInterrupted, "normalize", "", ErrHistoryInterrupted, nil)
		}
		facts, err := terminalFactsFor(open, request.TerminalFacts)
		if err != nil {
			return NormalizeResult{}, err
		}
		parts := make([]agent.ContentPart, 0, len(facts))
		for _, fact := range facts {
			call := open[fact.CallID]
			parts = append(parts, agent.ContentPart{Type: agent.PartToolResult, ToolResult: &agent.ToolResult{
				ToolCallID: fact.CallID,
				Name:       call.name,
				Content:    syntheticTerminalContent(fact),
				IsError:    true,
				StopTurn:   true,
			}})
			result.Diagnostics = append(result.Diagnostics, Diagnostic{Code: DiagnosticTerminalRepaired, MessageIndex: len(messages), PartIndex: call.partIndex, CallID: fact.CallID})
		}
		result.Messages = append(result.Messages, agent.Message{Role: agent.RoleTool, Parts: parts})
		result.Exchanges = append(result.Exchanges, ToolExchange{
			Start: exchangeStart, End: len(result.Messages) - 1, CallIDs: append([]string(nil), callIDs...),
			SourceStart: exchangeSourceStart, SourceEnd: len(messages),
		})
	}

	sort.SliceStable(result.Diagnostics, func(i, j int) bool {
		left, right := result.Diagnostics[i], result.Diagnostics[j]
		if left.MessageIndex != right.MessageIndex {
			return left.MessageIndex < right.MessageIndex
		}
		if left.PartIndex != right.PartIndex {
			return left.PartIndex < right.PartIndex
		}
		return left.Code < right.Code
	})
	digest, err := agent.CanonicalDigest(result.Messages)
	if err != nil {
		return NormalizeResult{}, contextError(CodeHistoryInvalid, "normalize", "", ErrHistoryInvalid, err)
	}
	result.Digest = digest
	return cloneNormalizeResult(result), nil
}

func sanitizeMessage(message agent.Message, messageIndex int) (agent.Message, bool, []Diagnostic, error) {
	if message.Role != agent.RoleSystem && message.Role != agent.RoleUser && message.Role != agent.RoleAssistant && message.Role != agent.RoleTool {
		return agent.Message{}, false, nil, historyError("normalize", "unknown message role")
	}
	parts := make([]agent.ContentPart, 0, len(message.Parts))
	for _, part := range message.Parts {
		switch part.Type {
		case agent.PartText:
			if part.ToolCall != nil || part.ToolResult != nil || part.Image != nil || message.Role == agent.RoleTool {
				return agent.Message{}, false, nil, historyError("normalize", "invalid text part")
			}
			parts = append(parts, agent.ContentPart{Type: agent.PartText, Text: part.Text})
		case agent.PartToolCall:
			if message.Role != agent.RoleAssistant || part.ToolCall == nil || part.ToolResult != nil || part.Image != nil || part.Text != "" || part.ToolCall.ID == "" || part.ToolCall.Name == "" {
				return agent.Message{}, false, nil, historyError("normalize", "invalid tool call part")
			}
			value := *part.ToolCall
			parts = append(parts, agent.ContentPart{Type: agent.PartToolCall, ToolCall: &value})
		case agent.PartToolResult:
			if message.Role != agent.RoleTool || part.ToolResult == nil || part.ToolCall != nil || part.Image != nil || part.Text != "" || part.ToolResult.ToolCallID == "" {
				return agent.Message{}, false, nil, historyError("normalize", "invalid tool result part")
			}
			value := *part.ToolResult
			parts = append(parts, agent.ContentPart{Type: agent.PartToolResult, ToolResult: &value})
		case agent.PartImage:
			if err := agent.ValidateMessage(agent.Message{Role: message.Role, Parts: []agent.ContentPart{part}}); err != nil {
				return agent.Message{}, false, nil, historyError("normalize", "invalid image part")
			}
			value := *part.Image
			value.Data = append([]byte(nil), part.Image.Data...)
			parts = append(parts, agent.ContentPart{Type: agent.PartImage, Image: &value})
		default:
			// Unknown parts are provider extensions (including raw reasoning). They
			// never enter canonical provider-neutral history.
			continue
		}
	}
	message.Parts = parts
	if message.Role == agent.RoleAssistant && assistantEmpty(message) {
		return agent.Message{}, true, []Diagnostic{{Code: DiagnosticEmptyAssistant, MessageIndex: messageIndex, PartIndex: -1}}, nil
	}
	if len(message.Parts) == 0 {
		return agent.Message{}, false, nil, historyError("normalize", "message has no canonical content")
	}
	if message.Role != agent.RoleAssistant && message.FinishReason != "" {
		return agent.Message{}, false, nil, historyError("normalize", "finish reason on non-assistant message")
	}
	return message, false, nil, nil
}

func assistantEmpty(message agent.Message) bool {
	if len(message.Parts) == 0 {
		return true
	}
	for _, part := range message.Parts {
		if part.Type == agent.PartToolCall || (part.Type == agent.PartText && strings.TrimSpace(part.Text) != "") {
			return false
		}
	}
	return true
}

func terminalFactsFor(open map[string]openCall, facts []TerminalFact) ([]TerminalFact, error) {
	byCall := make(map[string]TerminalFact, len(facts))
	for _, fact := range facts {
		if fact.CallID == "" || fact.Reason == "" || fact.SourceRevision == 0 {
			return nil, contextError(CodeInvalidRequest, "normalize", "", ErrInvalidRequest, fmt.Errorf("terminal fact is incomplete"))
		}
		if _, exists := byCall[fact.CallID]; exists {
			return nil, pairingError("normalize", "duplicate terminal fact")
		}
		if _, exists := open[fact.CallID]; !exists {
			return nil, pairingError("normalize", "terminal fact does not match an open call")
		}
		byCall[fact.CallID] = fact
	}
	result := make([]TerminalFact, 0, len(open))
	for callID := range open {
		fact, exists := byCall[callID]
		if !exists {
			return nil, contextError(CodeHistoryInterrupted, "normalize", callID, ErrHistoryInterrupted, nil)
		}
		result = append(result, fact)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CallID < result[j].CallID })
	return result, nil
}

func syntheticTerminalContent(fact TerminalFact) string {
	status := "terminal_error"
	if fact.EffectUnknown {
		status = "terminal_effect_unknown"
	}
	data, err := json.Marshal(struct {
		Status         string `json:"status"`
		Reason         string `json:"reason"`
		SourceRevision uint64 `json:"source_revision"`
	}{Status: status, Reason: fact.Reason, SourceRevision: fact.SourceRevision})
	if err != nil {
		return `{"status":"terminal_error"}`
	}
	return string(data)
}

func cloneNormalizeResult(result NormalizeResult) NormalizeResult {
	result.Messages = cloneMessages(result.Messages)
	result.Exchanges = cloneExchanges(result.Exchanges)
	result.Diagnostics = cloneDiagnostics(result.Diagnostics)
	return result
}

func pairingError(operation, detail string) error {
	return contextError(CodeToolPairing, operation, "", ErrToolPairing, fmt.Errorf("%s", detail))
}

func historyError(operation, detail string) error {
	return contextError(CodeHistoryInvalid, operation, "", ErrHistoryInvalid, fmt.Errorf("%s", detail))
}
