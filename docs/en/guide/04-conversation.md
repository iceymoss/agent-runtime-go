# 4. Conversation and context

## Where you are stuck

Every `Run` so far was an isolated turn. When the user says "then restart it", the model has no idea what "it" is — the previous turn was never sent.

## Carrying history

The runtime stores nothing, and `RunResult.Messages` holds **only the assistant and tool messages this turn produced**. Joining them together is your job:

```go
type Transcript struct {
	System   agent.Message
	messages []agent.Message
}

// One turn's input = system + stored history + this turn's question
func (t *Transcript) Prompt(input string) []agent.Message {
	messages := make([]agent.Message, 0, len(t.messages)+2)
	messages = append(messages, t.System)
	messages = append(messages, t.messages...)
	return append(messages, agent.NewUserMessage(input))
}

func (t *Transcript) Commit(input string, result *agent.RunResult) bool {
	if result == nil || result.Outcome != agent.OutcomeCompleted {
		return false
	}
	t.messages = append(t.messages, agent.NewUserMessage(input))
	t.messages = append(t.messages, result.Messages...)
	return true
}
```

Using it:

```go
result, err := runner.Run(ctx, agent.RunRequest{Messages: transcript.Prompt(question)})
if err != nil {
	return err
}
if !transcript.Commit(question, result) {
	fmt.Printf("[turn did not complete: %s/%s, not stored]\n", result.Outcome, result.StopReason)
}
```

**You store the user message; it is not in `RunResult.Messages`.** That is deliberate: only your application knows whether a turn that failed halfway should be kept.

**An unfinished turn must not be committed.** Its tool calls have no results yet, so writing it to history makes the next turn's input incoherent — the model would see a tool call that was never answered. `Commit` returning false is what prevents that.

Where it is stored is up to you: an in-memory slice, your own tables, or the `message` subpackage from chapter 9. The runtime only takes a `[]agent.Message`, so changing storage changes the inside of this struct and nothing else.

## When history no longer fits

The longer the conversation, the longer the history, until it outgrows the context window. The runtime can **detect** this — set `Config.ContextWindow` and an over-budget run stops with `OutcomeSuspended` + `StopReasonContextBudget`:

```go
runner, err := agent.New(agent.Config{
	Key: "ops.assistant", ModelName: modelName, MaxSteps: 8,
	ContextWindow: 128_000,
}, model, registry)
```

But it will not compact history for you. Compaction is a policy, and policy is yours. The `context` subpackage provides it:

```go
import agentcontext "github.com/iceymoss/agent-runtime-go/context"
```

It does three things. `NormalizeHistory` repairs incoherent history, such as a tool call missing its result. `Planner` decides which messages this turn carries and what they cost. `Compactor` turns old messages into a summary artifact when they no longer fit, recording a verifiable pivot so a later run can prove where it picked up.

Short conversations just set `ContextWindow` and reach for this when they actually hit the limit.

## Worth knowing

**Token counting is an estimate by default.** The library binds to no tokenizer and lets you supply your own counter. Counting bytes works but drifts near the limit.

**Compaction loses information, which is why it is verifiable.** The artifact carries a digest and a covered range that are checked on resume. If you write your own summarizer, do not bypass that check, or the recovered history cannot be proven to correspond to the original conversation.

## Going deeper

- [context](../packages/context.md) — budgets, normalization, compaction plans, pivots
- Complete runnable code: [`examples/guide/conversation.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/conversation.go)
