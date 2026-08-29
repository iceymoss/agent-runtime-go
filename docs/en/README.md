# Introduction

Agent Runtime for Go is a composable, provider-agnostic Go agent runtime.

It does one thing: **reliably run the model/tool loop**—multi-step cycling, JSON Schema validation, tool allowlists, stop conditions, error classification, and crash recovery. Model adapters, prompts, tool implementations, permission policies, storage, and business APIs belong to your application.

```go
import agent "github.com/iceymoss/agent-runtime-go"
```

> The current release is in `v0.x`; the API may still change. Pin a version in production.

## Why you need it

Calling a model API directly to build an agent quickly surfaces problems that are not business logic but must still be done right:

- When the model requests a tool call, how do you validate arguments, execute the tool, and feed results back into the model correctly?
- When output is invalid, a tool fails, or context exceeds budget, what should retry and what should stop?
- After a process crash, how do you resume an in-flight run without replaying side effects?
- If you switch model providers, how much of that code must you rewrite?

These answers are tightly coupled. Scatter them through business code and every call site drifts. Agent Runtime folds them into one tested runtime; your code only faces the stable `Model` / `Tool` contracts.

## Design philosophy

**Progressive adoption.** The root `agent` package is fully usable on its own: one `Model`, one tool registry, one `Run`. Session persistence, crash recovery, permission approval, MCP, and Sub-Agents live in separate subpackages—pull in only what you need; a simple call does not require the whole stack.

**No decisions for your app.** The root package does not read environment variables, pick credentials, connect to databases, or register global tools implicitly. All policy is assembled explicitly in your composition root, so every layer can be tested and replaced in isolation.

**Provider-agnostic.** The only model port is a three-method `Model` interface. OpenAI-compatible APIs (OpenAI / DeepSeek / Qwen / Kimi / vLLM / Ollama, and others) have an official adapter in `providers/openaicompat`; other protocols adapt to the same interface yourself.

## What you get

- Unified contracts for messages, images, models, streaming responses, and usage
- Multi-step model/tool loop, JSON Schema validation, and tool allowlists
- Max steps, stop conditions, context budget, and tool-loop detection
- Optional Session, Durable, Permission, Event, MCP, Skills, and Sub-Agent packages
- Immutable runtime definitions and reproducible artifact digests
- Testable provider/tool ports, unbound to any vendor or database

## Install

```bash
go get github.com/iceymoss/agent-runtime-go
```

Requires Go `1.25.0` or newer. The repo ships a deterministic example that needs no API key, so you can verify the install immediately:

```bash
go run ./examples/hello
```

Output:

```text
Hello from Agent Runtime for Go.
```

## How to read this documentation

Pick a path for where you are:

- **First contact:** [Build your agent](guide/index.md) — from one minimal call to an agent you can ship, in order, with a runnable program at the end of every chapter. In a hurry, read the first three chapters.
- **Looking up one package:** The 18 package pages are the reference layer, each following: What it is → Why you need it → How to use it → FAQ. Every guide chapter links to the ones it introduced.
- **Going to production:** [Production composition](production.md) covers how providers, permissions, state, and events form a service; [Run-loop internals](internals.md) helps debug stream, tool execution, stop, and recovery issues.
- **Full reference implementation:** The [iCoder end-to-end tutorial](icoder.md) shows a real Code Agent composing all subpackages; the [cheat sheet](reference.md) summarizes enums, error classification, and validation commands.

## Reading conventions

- "Root package" means `package agent` at the module root; "ordinary run" means in-memory execution with `RunRequest.DurableRun == nil`.
- `Observation` is a disposable progress signal; `RunResult` and the durable store are the source of truth for terminal state.
- Docs follow public source APIs and test-enforced constraints.
