package agent

// StreamResponse turns one complete response into the chunk stream the runtime
// expects, and closes it.
//
// The runtime requires an adapter's streamed chunks to add up to exactly the
// terminal response it reports: a model that streams one answer and then claims
// a different one is a bug the runtime refuses to hide. That rule is easy to
// violate by accident, because the obvious minimal adapter emits only the
// terminal chunk and no deltas at all.
//
// So every adapter that already has the whole response - a non-streaming
// endpoint, a cached reply, a test fake - should hand it to this function rather
// than assembling chunks by hand. Adapters that genuinely stream emit their own
// deltas and their own terminal chunk instead.
func StreamResponse(response *Response) <-chan StreamChunk {
	if response == nil {
		chunks := make(chan StreamChunk, 1)
		chunks <- StreamChunk{Type: ChunkError, Err: NewModelError(
			ModelErrorKindProtocol, false, 0, 0, "adapter produced no response", nil)}
		close(chunks)
		return chunks
	}
	chunks := make(chan StreamChunk, len(response.Message.Parts)+1)
	for _, part := range response.Message.Parts {
		switch part.Type {
		case PartReasoning:
			chunks <- StreamChunk{Type: ChunkReasoning, TextDelta: part.Text}
		case PartText:
			chunks <- StreamChunk{Type: ChunkText, TextDelta: part.Text}
		case PartToolCall:
			call := *part.ToolCall
			chunks <- StreamChunk{Type: ChunkToolCall, ToolCall: &call}
		}
	}
	chunks <- StreamChunk{Type: ChunkFinish, Response: response}
	close(chunks)
	return chunks
}

// validateReasoningCapability keeps Capabilities.Reasoning honest.
//
// A model that reports its thinking must say so, because the application has to
// decide what to do with it - render it, store it, or redact it - and it can
// only make that decision from the capability, before the first token arrives.
func validateReasoningCapability(resp *Response, streamed string, caps Capabilities) error {
	if caps.Reasoning {
		return nil
	}
	if streamed != "" || resp.Message.Reasoning() != "" {
		return newProtocolError("model produced reasoning without declaring Capabilities.Reasoning", nil)
	}
	return nil
}
