package context

import "github.com/iceymoss/agent-runtime-go"

func cloneMessages(messages []agent.Message) []agent.Message {
	if messages == nil {
		return nil
	}
	result := make([]agent.Message, len(messages))
	for i, message := range messages {
		result[i] = message
		result[i].Parts = make([]agent.ContentPart, len(message.Parts))
		for j, part := range message.Parts {
			result[i].Parts[j] = part
			if part.ToolCall != nil {
				value := *part.ToolCall
				result[i].Parts[j].ToolCall = &value
			}
			if part.ToolResult != nil {
				value := *part.ToolResult
				result[i].Parts[j].ToolResult = &value
			}
			if part.Image != nil {
				value := *part.Image
				value.Data = append([]byte(nil), part.Image.Data...)
				result[i].Parts[j].Image = &value
			}
		}
	}
	return result
}

func cloneArtifacts(values []ArtifactRef) []ArtifactRef {
	return append([]ArtifactRef(nil), values...)
}

func cloneFacts(values []ProtectedFact) []ProtectedFact {
	return append([]ProtectedFact(nil), values...)
}

func cloneDiagnostics(values []Diagnostic) []Diagnostic {
	return append([]Diagnostic(nil), values...)
}

func cloneExchanges(values []ToolExchange) []ToolExchange {
	result := append([]ToolExchange(nil), values...)
	for i := range result {
		result[i].CallIDs = append([]string(nil), result[i].CallIDs...)
	}
	return result
}
