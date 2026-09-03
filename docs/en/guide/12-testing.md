# 12. Testing your adapter

## Where you are stuck

You wrote your own `Model` adapter (for Anthropic or Bedrock, say), or connected a storage port to your own database. It works — but "works" and "conforms to the contract" are different things, and the parts of the contract that only surface during a crash, under concurrency, or on retry are hard to think of unaided.

## Use the library's conformance suites

`agenttest` provides reusable suites that hold your adapter to the **same contract** as the library's own reference implementations:

```go
import "github.com/iceymoss/agent-runtime-go/agenttest"

func TestMyModelConformance(t *testing.T) {
	agenttest.TestModel(t, func(t *testing.T, testCase agenttest.ModelCase) agent.Model {
		// build an upstream that behaves as testCase describes, return your adapter
	})
}
```

Ports covered: `TestModel`, `TestTool`, `TestCheckpointStore`, `TestDurableStore`, `TestEventStore`, `TestToolExecutionLedger`, `TestPermissionStore`, `TestMessageService`, `TestSubagentStore`, `TestSessionService`, `TestSessionRunStore`, and `TestManifestStore`.

This is not ceremony. Wiring `TestModel` into the library's own `providers/openaicompat` found three real defects: `Retry-After` was never parsed, the provider's error message was discarded, and the streaming path did not validate usage. Writing the fixtures is what forces you to face the edges you would not have thought of.

`agenttest` may only be imported by test code — a production package that imports it is caught by `deps_test.go`.

## Going deeper

- [agenttest](../packages/agenttest.md) — what each suite covers and how to write fixtures
