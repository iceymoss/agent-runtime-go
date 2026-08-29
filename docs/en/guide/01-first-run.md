# 1. Your first run

## Run it first

The bundled examples need no API key:

```bash
git clone https://github.com/iceymoss/agent-runtime-go
cd agent-runtime-go
go run ./examples/hello        # the model answers directly
go run ./examples/tool-agent   # the model calls a tool, then answers
```

The second prints `Hangzhou is sunny and 28 C.` — a sentence that exists in no template. The model called a Go function, got the weather, and wrote that itself. The rest of this guide is about making that happen in your project.

## What this library does

> Skip this section if you have built LLM applications before.

Calling a model API is easy: send text, get text. It changes the moment you want the model to **call your code**, because a model executes nothing. It only says "I would like to call `get_weather` with `{"city":"Hangzhou"}`". What actually has to happen is:

```
your message
  → the model asks for get_weather({"city":"Hangzhou"})
  → validate the arguments against the schema you declared
  → run your function
  → append the result to the message history
  → call the model again          ← the loop
  → the model writes the final answer
```

One call to the model is a **step**. The run above took two. A model may call several tools in one turn, which is several steps.

Writing this loop is not hard. Writing it *reliably* is: half-finished JSON arguments, a call to a tool that does not exist, a model stuck calling the same tool forever, history that outgrows the context window, and knowing when to stop. Those answers are coupled to each other, so scattering them through business code guarantees they end up inconsistent.

This library owns that loop and nothing else. Model adapters, prompts, tool implementations, permissions, and storage are yours — it reads no environment variables, chooses no credentials, opens no database.

## The smallest program

```bash
mkdir my-agent && cd my-agent
go mod init my-agent
go get github.com/iceymoss/agent-runtime-go
```

A working agent needs eight symbols: `Config`, `New`, and `Run` assemble and drive it; `Message` and its constructors carry the conversation; `Registry` and `NewTool` provide tools; `RunResult` reports what happened.

This chapter uses a fake model with a fixed answer, so you can run it immediately and see the shape:

```go
package main

import (
	"context"
	"fmt"

	agent "github.com/iceymoss/agent-runtime-go"
)

// The model port has three methods. A real adapter projects the request onto an
// upstream protocol; this one returns a hard-coded answer.
type fakeModel struct{}

func (fakeModel) Name() string                     { return "fake" }
func (fakeModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (fakeModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	message := agent.NewAssistantMessage("Hello, I am an agent.")
	message.FinishReason = agent.FinishStop
	return agent.StreamResponse(&agent.Response{
		Message: message, FinishReason: agent.FinishStop, ModelName: "fake-v1",
	}), nil
}

func main() {
	runner, err := agent.New(agent.Config{
		Key:          "my.first",
		ModelName:    "fake-v1",
		MaxSteps:     4,
		AllowedTools: []string{}, // no tools yet
	}, fakeModel{}, agent.NewRegistry())
	if err != nil {
		panic(err)
	}

	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewSystemMessage("You are a concise assistant."),
			agent.NewUserMessage("Hello"),
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
	fmt.Println(result.Outcome, result.StopReason)
}
```

```bash
$ go run .
Hello, I am an agent.
completed complete
```

## Worth knowing

**An `Agent` holds no conversation state** and is safe to share across goroutines and sessions. Everything that varies per turn goes in through `RunRequest` and comes back in `RunResult`, so a service usually assembles one `Agent` and calls `Run` concurrently.

**When you write a `Model` adapter, the streamed chunks must add up to exactly the terminal response.** That is why the code above uses `agent.StreamResponse`: the most natural implementation — emit only the terminal chunk, no deltas — is rejected by the runtime, and `StreamResponse` turns a complete answer into a compliant chunk sequence for you. Adapters that genuinely stream token by token emit their own deltas instead.

**`Outcome` and `StopReason` are two orthogonal fields** and you need both. `Outcome` is `completed`, `suspended`, or `failed`; `StopReason` says why it stopped. Only `completed` + `complete` is a real final answer. Hitting `MaxSteps` while the model is still calling tools is `suspended` + `max_steps` — not an error, and it is yours to decide whether to continue.

## Going deeper

From the next chapter on, the code comes from [`examples/guide`](https://github.com/iceymoss/agent-runtime-go/tree/main/examples/guide) — a working ops assistant that each chapter adds one thing to. CI compiles and tests it, so the documentation cannot drift away from the implementation.

```bash
go run ./examples/guide        # no API key needed
```

- [agent package reference](../packages/agent.md) — allowlists, stop conditions, loop detection
- [Core concepts](../concepts.md) — the Message / Model / Tool / stopping mental model
