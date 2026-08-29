// Command chat is a multi-turn conversation with tools and persisted history:
// the program most people write second, after examples/hello.
//
// It shows the three things a single run does not: how history is carried from
// one turn to the next, what the application is responsible for storing, and
// where a tool fits into that. Everything here is deterministic and needs no
// API key; swap the model for providers/openaicompat to go live.
//
//	go run ./examples/chat
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	agent "github.com/iceymoss/agent-runtime-go"
)

// transcript is the whole persistence story for a conversation.
//
// The runtime is stateless: it never stores anything, and RunResult.Messages
// contains only what this turn produced. So the application appends the user's
// message and the run's output, in that order, and passes the accumulated list
// back on the next turn. A real application writes this to a database instead of
// a slice - the shape does not change, only where Append and Messages read and
// write.
type transcript struct {
	system   agent.Message
	messages []agent.Message
}

// Prompt builds the input for one turn: the system message, everything said so
// far, and the new question.
func (t *transcript) Prompt(input string) []agent.Message {
	messages := make([]agent.Message, 0, len(t.messages)+2)
	messages = append(messages, t.system)
	messages = append(messages, t.messages...)
	return append(messages, agent.NewUserMessage(input))
}

// Append commits one finished turn.
//
// The user message is stored by the application, not by the runtime, because
// only the application knows whether a turn that failed halfway should be kept.
// Storing it together with the run's output keeps the transcript coherent: a
// tool call is always followed by its result.
func (t *transcript) Append(input string, result *agent.RunResult) {
	t.messages = append(t.messages, agent.NewUserMessage(input))
	t.messages = append(t.messages, result.Messages...)
}

type noteInput struct {
	Text string `json:"text" description:"The note to remember"`
}

func main() {
	notes := &noteBook{}
	remember := agent.MustNewTool("remember", "Store a note for later.",
		func(_ context.Context, input noteInput) (agent.ToolResult, error) {
			notes.add(input.Text)
			return agent.ToolResult{Content: fmt.Sprintf("stored note %d", notes.count())}, nil
		})

	registry := agent.NewRegistry()
	if err := registry.Register(remember); err != nil {
		fail(err)
	}

	runner, err := agent.New(agent.Config{
		Key:       "example.chat",
		ModelName: "scripted-v1",
		MaxSteps:  6,
	}, &scriptedModel{notes: notes}, registry)
	if err != nil {
		fail(err)
	}

	history := &transcript{system: agent.NewSystemMessage("You are a concise assistant.")}
	ctx := context.Background()

	for _, input := range []string{
		"Remember that the deploy window is Thursday.",
		"What did I ask you to remember?",
	} {
		fmt.Printf("\n> %s\n", input)
		result, err := runner.Run(ctx, agent.RunRequest{Messages: history.Prompt(input)})
		if err != nil {
			fail(err)
		}
		// A turn that did not finish must not be committed: its tool calls have no
		// results yet, and a half-written exchange corrupts the history the next
		// turn is built from.
		if result.Outcome != agent.OutcomeCompleted {
			fmt.Printf("[turn did not complete: %s/%s]\n", result.Outcome, result.StopReason)
			continue
		}
		history.Append(input, result)
		fmt.Println(result.Text)
	}

	fmt.Printf("\n[%d messages stored across %d turns]\n", len(history.messages), 2)
}

// noteBook is the application's own state, reached through a tool.
type noteBook struct{ notes []string }

func (n *noteBook) add(note string) { n.notes = append(n.notes, note) }
func (n *noteBook) count() int      { return len(n.notes) }
func (n *noteBook) all() string     { return strings.Join(n.notes, "; ") }

// scriptedModel stands in for a real provider so the example is deterministic.
// It answers from the conversation it is given, which is what makes the point:
// the second turn only works because the first turn was stored.
type scriptedModel struct{ notes *noteBook }

func (scriptedModel) Name() string { return "scripted" }
func (scriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}

func (m *scriptedModel) Stream(_ context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	// Look only at this turn - everything after the last user message. The
	// history also contains earlier turns, including their tool results.
	turn := request.Messages
	for i := len(turn) - 1; i >= 0; i-- {
		if turn[i].Role == agent.RoleUser {
			turn = turn[i:]
			break
		}
	}
	for _, message := range turn[1:] {
		if message.Role == agent.RoleTool {
			return say("Noted."), nil
		}
	}
	last := turn[0].Text()
	if strings.HasPrefix(last, "Remember ") {
		call := agent.ToolCall{ID: "call-1", Name: "remember", Input: `{"text":"the deploy window is Thursday"}`}
		message := agent.Message{Role: agent.RoleAssistant, FinishReason: agent.FinishToolCalls,
			Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}}
		return agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishToolCalls}), nil
	}
	// The answer is only possible because the earlier turn is in the history.
	return say("You asked me to remember: " + m.notes.all() + "."), nil
}

func say(text string) <-chan agent.StreamChunk {
	message := agent.NewAssistantMessage(text)
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{
		Message: message, FinishReason: agent.FinishStop, ModelName: "scripted-v1",
	})
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
