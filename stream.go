package agent

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

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
			observer.emit(Observation{Type: ObservationTextDelta, Text: chunk.TextDelta})
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
		return nil, newProtocolError("streamed chunks do not match terminal response", nil)
	}
	if err := resp.Usage.Validate(); err != nil {
		return nil, newProtocolError("invalid usage", err)
	}
	return resp, nil
}
