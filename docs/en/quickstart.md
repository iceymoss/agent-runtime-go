# Quickstart

Goal: run an agent that calls tools within 10 minutes. Requires Go `1.25.0`.

## See it work first (no API key)

The repo ships two deterministic examples; run them right after cloning:

```bash
go run ./examples/hello        # one-shot model reply
go run ./examples/tool-agent   # full two-step tool loop
```

Output:

```text
$ go run ./examples/hello
Hello from Agent Runtime for Go.

$ go run ./examples/tool-agent
Hangzhou is sunny and 28 C.
```

With any OpenAI-compatible API key (OpenAI / DeepSeek / Qwen / Kimi, or local Ollama) you can run a real model:

```bash
OPENAI_API_KEY=sk-... go run ./examples/openai-compat
```

Sample output (varies by model):

```text
[tool call] get_time {"timezone":"Asia/Shanghai"}
The current date and time in Shanghai is Thu, 06 Aug 2026 12:45:10 CST.

[gpt-4o-mini] stop=complete steps=2 tokens=421
```

## Build one from scratch in your project

### 1. Install

```bash
mkdir my-agent && cd my-agent
go mod init my-agent
go get github.com/iceymoss/agent-runtime-go
```

### 2. Write main.go

This program wires a real model, defines one tool, and runs one turn—about 60 lines:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/openaicompat"
)

// Tool input: JSON Schema is generated from this struct automatically.
// Non-pointer fields without omitempty become required.
type TimeInput struct {
	Timezone string `json:"timezone,omitempty" description:"IANA 时区名，如 Asia/Shanghai"`
}

func main() {
	// Model: change baseURL and model name to switch any OpenAI-compatible service
	model := openaicompat.New("https://api.deepseek.com/v1", os.Getenv("DEEPSEEK_API_KEY"))

	// Tool: one function + one input struct
	getTime := agent.MustNewTool("get_time", "获取当前时间",
		func(ctx context.Context, in TimeInput) (agent.ToolResult, error) {
			loc := time.Local
			if in.Timezone != "" {
				l, err := time.LoadLocation(in.Timezone)
				if err != nil {
					// IsError=true: model-visible, correctable error; the loop continues
					return agent.ToolResult{IsError: true, Content: "未知时区: " + in.Timezone}, nil
				}
				loc = l
			}
			return agent.ToolResult{Content: time.Now().In(loc).Format(time.RFC3339)}, nil
		})

	registry := agent.NewRegistry()
	if err := registry.Register(getTime); err != nil {
		panic(err)
	}

	// Assemble: config errors (invalid schema, missing allowlist entries, etc.) fail here
	runner, err := agent.New(agent.Config{
		Key:       "quickstart.time",
		ModelName: "deepseek-chat",
		MaxSteps:  8,
	}, model, registry)
	if err != nil {
		panic(err)
	}

	// One turn: the model decides whether to call get_time
	result, err := runner.Run(context.Background(), agent.RunRequest{
		Messages: []agent.Message{
			agent.NewUserMessage("现在东京几点了？"),
		},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(result.Text)
	fmt.Printf("(steps=%d tokens=%d)\n", len(result.Steps), result.Usage.TotalTokens)
}
```

### 3. Run

```bash
DEEPSEEK_API_KEY=sk-... go run .
```

Expected output looks like:

```text
东京现在是 2026年8月6日 12:26（JST）。
(steps=2 tokens=873)
```

`steps=2` means a full tool loop ran: step 1 the model requested `get_time`, the runtime validated args and executed; step 2 the model saw the tool result and answered.

## What happened

```text
Your message
  → model returns tool call: get_time({"timezone":"Asia/Tokyo"})
  → runtime validates args against Schema → runs your function
  → tool result appended to message history → model called again
  → model emits final answer → RunResult{Outcome: completed}
```

In between, the runtime feeds invalid args back for the model to fix, detects repeated same-tool calls as loops, and stops with `OutcomeSuspended` when `MaxSteps` is hit—none of that requires your code.

## Next steps

- Core types and execution semantics → [Core concepts](concepts.md)
- Full adapter options (local Ollama, OpenRouter, non-streaming fallback) → [providers/openaicompat](packages/openaicompat.md)
- Core execution capabilities (allowlists, stop conditions, progress observation) → [agent](packages/agent.md)
- Carrying history and storing a turn → [`examples/chat`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/chat/main.go) and [Persisting a conversation](packages/agent.md)
- Cross-process sessions and crash recovery → [session](packages/session.md), [durable](packages/durable.md)
- Full reference app (permissions, SQLite, Skills, MCP, sub-agents) → [iCoder tutorial](icoder.md)
