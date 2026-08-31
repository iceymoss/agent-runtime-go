package icoder

import (
	"context"
	"unicode/utf8"

	"github.com/iceymoss/agent-runtime-go"
)

// byteCounter is iCoder's conservative token estimator. It deliberately
// overestimates - roughly one token per four ASCII bytes and one per non-ASCII
// rune - because the estimate guards the context budget: an overestimate
// compacts a little early, while an underestimate sends a request the provider
// rejects. It is the application's single TokenCounter: the planner, the
// compactor, and the TokenizerID recorded on every context plan must all come
// from the same implementation, or a stored plan would verify against a counter
// that never produced it.
//
// A provider-precise tokenizer plugs in by replacing this one value at App
// assembly; the `icoder context show` report exists to say when the
// conservative estimate is far enough off to justify that upgrade.
type byteCounter struct{}

func (byteCounter) ID() string { return "icoder/conservative-bytes-v1" }

func (byteCounter) CountTokens(ctx context.Context, messages []agent.Message) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tokens := 0
	for _, message := range messages {
		tokens += 4 + conservativeTextTokens(string(message.Role)) + conservativeTextTokens(message.Text())
		for _, call := range message.ToolCalls() {
			tokens += 6 + conservativeTextTokens(call.ID) + conservativeTextTokens(call.Name) + conservativeTextTokens(call.Input)
		}
		for _, result := range message.ToolResults() {
			tokens += 6 + conservativeTextTokens(result.ToolCallID) + conservativeTextTokens(result.Name) + conservativeTextTokens(result.Content)
		}
	}
	return tokens, nil
}

func conservativeTextTokens(value string) int {
	ascii, nonASCII := 0, 0
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 || r < utf8.RuneSelf {
			ascii++
		} else {
			nonASCII++
		}
		value = value[size:]
	}
	return (ascii+3)/4 + nonASCII
}
