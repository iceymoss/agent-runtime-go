package main

import agent "github.com/iceymoss/agent-runtime-go"

// Transcript is the entire persistence story for a conversation.
//
// The runtime is stateless and RunResult.Messages holds only what one turn
// produced, so joining turns together is the application's job. A real service
// swaps the slice for a table; nothing else changes.
type Transcript struct {
	System   agent.Message
	messages []agent.Message
}

// Prompt builds one turn's input: the system message, everything said so far,
// and the new question.
func (t *Transcript) Prompt(input string) []agent.Message {
	messages := make([]agent.Message, 0, len(t.messages)+2)
	messages = append(messages, t.System)
	messages = append(messages, t.messages...)
	return append(messages, agent.NewUserMessage(input))
}

// Commit stores one finished turn.
//
// The user message is stored here rather than by the runtime because only the
// application knows whether a turn that failed halfway should be kept. A turn
// that did not complete is never committed: its tool calls have no results yet,
// which would make the next turn's input incoherent.
func (t *Transcript) Commit(input string, result *agent.RunResult) bool {
	if result == nil || result.Outcome != agent.OutcomeCompleted {
		return false
	}
	t.messages = append(t.messages, agent.NewUserMessage(input))
	t.messages = append(t.messages, result.Messages...)
	return true
}

// Len reports how many messages are stored, which is what a UI shows as history.
func (t *Transcript) Len() int { return len(t.messages) }
