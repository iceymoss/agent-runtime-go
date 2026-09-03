# Build your agent

This guide takes you from one minimal call to an agent you can put in production. Read it in order; at the end of every chapter the program on your disk runs.

By the end you will know how to let a model call your Go functions, how to reach any OpenAI-compatible model, how conversation history is carried between turns, how to stream output to a user, how to require human approval for dangerous operations, and how a run survives the process that started it.

Requires Go `1.25.0`. Chapters 1 and 2 need no API key.

## The short path

In a hurry? Read the first three chapters. That gives you an agent that talks to a real model and calls your tools, which covers most applications.

## Chapters

| # | Chapter | The problem it solves |
|---|---|---|
| 1 | [Your first run](./01-first-run.md) | What the model/tool loop is, and why not to write it yourself |
| 2 | [Letting the model call your code](./02-tools.md) | Defining tools, and the two kinds of failure |
| 3 | [Connecting a real model](./03-real-model.md) | Switching providers, retries, several models, prompt versions |
| 4 | [Conversation and context](./04-conversation.md) | Carrying history, and what to do when it no longer fits |
| 5 | [Showing progress](./05-streaming.md) | Streaming output, and why the default loses text |
| 6 | [Output your code can consume](./06-structured-output.md) | Structured output and reasoning models |
| 7 | [Asking a human first](./07-permission.md) | Permissions, approval, and an effect ledger |
| 8 | [More sources of tools](./08-tool-sources.md) | Remote tools and untrusted instructions |
| 9 | [Surviving a crash](./09-durability.md) | Checkpoints, message and session persistence |
| 10 | [Many agents, reproducibly](./10-orchestration.md) | Delegation and immutable composition |
| 11 | [Going to production](./11-production.md) | Event delivery, readiness, graceful shutdown |
| 12 | [Testing your adapter](./12-testing.md) | Conformance suites |

## Looking for one subpackage

Every subpackage has its own reference page (what it is → why you need it → how to use it → FAQ). This table says which chapter introduces it:

| Subpackage | Chapter | Reference |
|---|---|---|
| root `agent` | 1, 2, 5, 6 | [agent](../packages/agent.md) |
| `providers/openaicompat` | 3 | [openaicompat](../packages/openaicompat.md) |
| `providers/retry` | 3 | [retry](../packages/retry.md) |
| `provider` | 3 | [provider](../packages/provider.md) |
| `prompt` | 3 | [prompt](../packages/prompt.md) |
| `context` | 4 | [context](../packages/context.md) |
| `permission` | 7 | [permission](../packages/permission.md) |
| `tool` | 7 | [tool](../packages/tool.md) |
| `mcp` | 8 | [mcp](../packages/mcp.md) |
| `skills` | 8 | [skills](../packages/skills.md) |
| `durable` | 9 | [durable](../packages/durable.md) |
| `message` | 9 | [message](../packages/message.md) |
| `session` | 9 | [session](../packages/session.md) |
| `subagent` | 10 | [subagent](../packages/subagent.md) |
| `coordinator` | 10 | [coordinator](../packages/coordinator.md) |
| `event` | 11 | [event](../packages/event.md) |
| `app` | 11 | [app](../packages/app.md) |
| `agenttest` | 12 | [agenttest](../packages/agenttest.md) |

For the mental model first, read [Core concepts](../concepts.md). For a real application that composes all of this, read the [iCoder tutorial](../icoder.md).
