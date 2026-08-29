# 4. Conversation and context

## Where you are stuck

Every `Run` so far was an isolated turn; the model does not remember the previous one. Multi-turn conversation means joining the history yourself — the runtime stores nothing.

## Carrying history

`RunResult.Messages` contains **only the assistant and tool messages this turn produced**. You store the user message. So one complete turn is:

```go
// input = system + stored history + this turn's question
messages := append([]agent.Message{systemMessage}, stored...)
messages = append(messages, agent.NewUserMessage(input))

result, err := runner.Run(ctx, agent.RunRequest{Messages: messages})
if err != nil {
	return err
}

// commit only a turn that actually finished
if result.Outcome == agent.OutcomeCompleted {
	stored = append(stored, agent.NewUserMessage(input))
	stored = append(stored, result.Messages...)
}
```

The user message is excluded from `RunResult.Messages` deliberately: only your application knows whether a turn that failed halfway should be kept.

An unfinished turn must not be committed — its tool calls have no results yet, which makes the next turn's input incoherent.

Where it is stored is up to you: an in-memory slice, your own tables, or the `message` subpackage from chapter 9. The runtime only takes a `[]agent.Message`. A complete example is [`examples/chat`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/chat/main.go).

## When history no longer fits

The longer the conversation, the longer the history, until it outgrows the model's context window. The runtime can **detect** this — set `Config.ContextWindow` and an over-budget run stops with `OutcomeSuspended` + `StopReasonContextBudget` — but it will not compact history for you. Compaction is a policy, and policy is yours.

The `context` subpackage provides that policy: token budgets, history normalization, compaction plans, and replacing old turns with a summary (a pivot).

```go
import agentcontext "github.com/iceymoss/agent-runtime-go/context"
```

It does three things. `NormalizeHistory` repairs incoherent history, such as a tool call missing its result. `Planner` decides which messages this turn carries and how many tokens they cost. `Compactor` turns old messages into a summary artifact when they no longer fit, recording a verifiable pivot so a later run can prove where it picked up.

Short conversations do not need any of it — set `ContextWindow` and reach for this when you actually hit the limit.

## Worth knowing

**Token counting is an estimate by default.** The library binds to no tokenizer and lets you supply your own counter. Counting bytes works, but drifts near the limit; production should use a real tokenizer.

**Compaction loses information, which is why it is verifiable.** The artifact carries a digest and a covered range that are checked on resume. If you implement your own summarizer, do not bypass that check, or the recovered history cannot be proven to correspond to the original conversation.

## Going deeper

- [context](../packages/context.md) — budgets, normalization, compaction plans, pivots
- [Persisting a conversation](../packages/agent.md)
