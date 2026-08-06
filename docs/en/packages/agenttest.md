# agenttest

The `agenttest` package provides reusable adapter conformance test suites: you write a provider adapter or tool implementation, and it verifies the public contracts of `agent.Model` / `agent.Tool`. This is a test-only package and must appear only in `_test.go` files; production code must not import it.

## What it is

The package has two entry functions and their supporting types:

```go
type ModelFactory func(t *testing.T, testCase ModelCase) agent.Model

func TestModel(t *testing.T, factory ModelFactory)

type ToolCase struct {
	Name       string
	Invocation agent.ToolInvocation
	WantResult agent.ToolResult
	WantErr    error
}

func TestTool(t *testing.T, tool agent.Tool, cases []ToolCase)
```

`TestModel` is the model-side suite. For each `ModelCase` it asks your factory for "an adapter instance backed by a specific upstream behavior": `ModelCaseValidStream` (happy-path stream), `ModelCaseMissingTerminal` / `ModelCaseAfterTerminal` / `ModelCaseInvalidUsage` (protocol violations), `ModelCaseRejected` / `ModelCaseAuth` / `ModelCaseRateLimit` / `ModelCaseTransport` (HTTP error classification), `ModelCaseProviderDetail` (error-message safety), `ModelCaseCancellation` (cancel behavior). Your factory builds a fake upstream per case (usually an `httptest.Server`); the suite asserts adapter output.

`TestTool` is the tool-side suite: it checks `Definition().Name` is non-empty, `ReplayPolicy()` is one of `agent.ReplayPolicyNever` / `ReplayPolicyIdempotent` / `ReplayPolicyResolve`, then runs each `ToolCase`, matching `WantErr` with `errors.Is` and `WantResult` with `reflect.DeepEqual`.

## Why you need it

The `agent.Model.Stream` contract has many details: the stream must close; failures must emit `ChunkError` then close; the terminal chunk must be unique and carry a complete `Response`; HTTP 401/429/503 must map to the correct `agent.ModelErrorKind` and `Retryable`; cancel must return `context.Canceled` and must not be wrapped as `ModelError`. Each adapter author hand-writing these assertions is both repetitive and easy to miss cases. This package freezes the contract into one shared suite for all adapters; `providers/openaicompat` is validated with it as well.

**When you do not need it**: if you are not writing a custom `agent.Model` adapter and do not need batch verification of tool implementations, you will not use this package. It does not help with business-logic tests.

## How to use it

Typical model-side wiring (factory builds a fake upstream per case; fixture details omitted):

```go
package myprovider_test

import (
	"testing"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/agenttest"
)

func TestConformance(t *testing.T) {
	agenttest.TestModel(t, func(t *testing.T, testCase agenttest.ModelCase) agent.Model {
		server := newFixtureServer(t, testCase) // httptest.Server returning the response for this case
		return newAdapter(server.URL)
	})
}
```

Sample output:

```text
=== RUN   TestConformance
=== RUN   TestConformance/valid_stream
=== RUN   TestConformance/missing_terminal
...(other case RUN lines omitted)
--- PASS: TestConformance (0.12s)
    --- PASS: TestConformance/valid_stream (0.03s)
    --- PASS: TestConformance/missing_terminal (0.00s)
    ...(after_terminal / invalid_usage / rejected / auth / rate_limit / transport / provider_detail all PASS)
    --- PASS: TestConformance/cancellation (0.02s)
PASS
ok  	example/myprovider	0.127s
```

The adapter returned by the factory must meet suite expectations for each case:

- **`ModelCaseValidStream`**: `Name()` non-empty; `Capabilities()` passes `Validate()` with `Tools`, `ToolChoiceNone`, `ToolChoiceRequired`, `ToolChoiceNamed`, and `UsageDetails` all `true`. Stream content has hard-coded conventions—text deltas concatenate to `"hello world"`; two tool calls in order with IDs `call_a` / name `first` / input `{"a":0}` and ID `call_b` / name `second` / input `{"b":1}`; usage `PromptTokens: 5, CompletionTokens: 4, TotalTokens: 11, CacheReadTokens: 2`; `FinishReason` is `agent.FinishToolCalls`. Your fixture upstream must produce responses that map exactly to these values.
- **Protocol-violation trio**: missing terminal chunk, data after terminal, or illegal usage must all end the stream with a single `ChunkError` whose error `errors.As` to `*agent.ModelError` with `Kind == agent.ModelErrorKindProtocol` and not retryable.
- **HTTP error classification**: 400 → `ModelErrorKindRejected` (not retryable); 401 → `ModelErrorKindAuth` (not retryable); 429 → `ModelErrorKindRateLimit` (retryable, `RetryAfter` exactly 2 seconds—fixture must return `Retry-After: 2`); 503 → `ModelErrorKindTransport` (retryable). All four must have non-nil `Cause`.
- **`ModelCaseProviderDetail`**: `ModelError.SafeDetail` and `err.Error()` must contain the fixture's `safe-provider-detail`, while `err.Error()` must not leak `unsafe-cause-marker`—raw `Cause` content must not appear in the error string.
- **`ModelCaseCancellation`**: when the upstream hangs and the context is canceled, `Stream` must return within 1 second an error matching `errors.Is(err, context.Canceled)`, and it must not `errors.As` to `*agent.ModelError`.

Tool side is more direct:

```go
func TestMyTool(t *testing.T) {
	agenttest.TestTool(t, newEchoTool(), []agenttest.ToolCase{
		{
			Name:       "echoes input",
			Invocation: agent.ToolInvocation{CallID: "call-1", Name: "echo", RawInput: `{"text":"hi"}`},
			WantResult: agent.ToolResult{Content: "hi"},
		},
	})
}
```

Sample output:

```text
=== RUN   TestMyTool
=== RUN   TestMyTool/echoes_input
--- PASS: TestMyTool (0.00s)
    --- PASS: TestMyTool/echoes_input (0.00s)
PASS
ok  	example/tooltest	0.006s
```

## FAQ

**Q: What does the request the suite sends to the model look like?**
A: Fixed as `Model: "conformance-model"`, one `agent.NewUserMessage("test")`, and two tool definitions `first` and `second` (both parameter schemas `{"type": "object"}`). The fixture upstream need not understand the request; it only replays the preset response for the case.

**Q: Valid-stream assertions fail on `Capabilities()`?**
A: The most common footgun: the suite requires `Tools`, `ToolChoiceNone`, `ToolChoiceRequired`, `ToolChoiceNamed`, and `UsageDetails` all true. If your adapter honestly declares real upstream capabilities and omits one, the valid-stream case fails—adapters built for conformance tests need to be constructed with full capabilities.

**Q: What if the stream hangs?**
A: The suite sets a 1-second timeout on "consume the entire stream"; timeout calls `t.Fatal("model stream did not close")`. Adapters must close the channel on every path (success, failure, cancel).

**Q: In a failure case, can the stream emit both `ChunkError` and `ChunkFinish`?**
A: No. A `ChunkFinish` or a second `ChunkError` on a failure stream fails the test. The contract is: on failure, exactly one `ChunkError`, then close the stream.

**Q: How does `TestTool` match `WantErr`?**
A: `errors.Is(err, tt.WantErr)`. Leave nil for expected success; put the sentinel for a specific expected error. Note `WantResult` uses `reflect.DeepEqual` on the entire `agent.ToolResult`, including `IsError`, `Name`, and every other field.
