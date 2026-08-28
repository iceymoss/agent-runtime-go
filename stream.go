package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

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

func collectStream(ctx context.Context, chunks <-chan StreamChunk, choice ToolChoice, observer *observer) (*Response, error) {
	var resp *Response
	var text strings.Builder
	var calls []ToolCall
	for {
		var chunk StreamChunk
		var ok bool
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, ok = <-chunks:
		}
		if !ok {
			break
		}
		if resp != nil {
			return nil, newProtocolError("chunk received after terminal response", nil)
		}
		switch chunk.Type {
		case ChunkText:
			text.WriteString(chunk.TextDelta)
			observer.emit(ctx, Observation{Type: ObservationTextDelta, Text: chunk.TextDelta})
		case ChunkToolCall:
			if chunk.ToolCall != nil {
				calls = append(calls, *chunk.ToolCall)
			}
		case ChunkFinish:
			if resp != nil {
				return nil, newProtocolError("duplicate terminal response", nil)
			}
			resp = chunk.Response
		case ChunkError:
			if chunk.Err != nil {
				return nil, normalizeModelError(chunk.Err, "model stream failed")
			}
			return nil, newProtocolError("error chunk missing cause", nil)
		default:
			return nil, newProtocolError(fmt.Sprintf("unknown stream chunk type %q", chunk.Type), nil)
		}
	}
	if resp == nil {
		return nil, newProtocolError("stream closed before terminal response", nil)
	}
	if resp.Message.FinishReason == "" {
		resp.Message.FinishReason = resp.FinishReason
	}
	if err := ValidateResponse(resp); err != nil {
		return nil, newProtocolError("invalid terminal response", err)
	}
	if err := validateEffectiveToolChoice(choice, resp); err != nil {
		return nil, newProtocolError("tool choice violation", err)
	}
	if text.String() != resp.Message.Text() || !reflect.DeepEqual(calls, resp.ToolCalls()) {
		return nil, newProtocolError("streamed chunks do not match terminal response: every text delta and tool call must add up to the terminal response; adapters holding a complete response should return agent.StreamResponse(response)", nil)
	}
	if err := resp.Usage.Validate(); err != nil {
		return nil, newProtocolError("invalid usage", err)
	}
	return resp, nil
}
