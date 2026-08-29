# 10. Many agents, reproducibly

## Where you are stuck

Two problems arrive together. Some work belongs to a separate agent — let a read-only reviewer judge a change without spending the main conversation's context on its search. And three months later somebody asks which model, prompt, and tool set produced a particular run, and you cannot say.

## Delegating to a child agent

The `subagent` subpackage manages independent child runs: spawn, claim, completion, waking the parent, plus depth, fan-out, cycle detection, and a shared tree budget:

```go
import "github.com/iceymoss/agent-runtime-go/subagent"
```

The parent receives a **structured result**, not the child's full history. That is the point of delegating: the context the child burned does not pollute the parent conversation.

Depth, fan-out, and budget are safety limits, not bookkeeping. Without them an agent that can delegate can delegate an unbounded tree and exhaust your quota. The library exports those decisions as pure functions (`ValidateDepth`, `ValidateFanout`, `ValidateNoCycle`, `BudgetSnapshot.Reserve`), and storage adapters should call them rather than restate them — a restatement does not fail loudly, it quietly widens a limit nobody authorized.

A child takes time, so delegation is usually **asynchronous**: after spawning, the tool returns a `ToolSuspensionExternal` and the parent parks (chapter 7's mechanism), then resumes with the handle once the child finishes. A crash while the child works therefore leaves a resumable parent instead of a lost delegation.

## Making a run reproducible

`coordinator` composes model, prompt, tool set, execution settings, and policy into an **immutable RuntimeDefinition** with a digest:

```go
import "github.com/iceymoss/agent-runtime-go/coordinator"
```

That digest is the run's identity. Record it and you can resolve the same composition back later, and verify whether the current binary still reproduces it. "Which version produced this run" gets a definite answer.

## Going deeper

- [subagent](../packages/subagent.md) — lifecycle, tree budget, cancellation, wake
- [coordinator](../packages/coordinator.md) — composition, manifests, rebuilding by digest
- [iCoder tutorial](../icoder.md) — how a real application wires these together
