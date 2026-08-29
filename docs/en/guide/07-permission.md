# 7. Asking a human first

## Where you are stuck

A tool that reads the weather is one thing. A tool that deletes files, moves money, or emails customers is another. Some operations have to stop and wait for a person, and you need a record of who approved what and when.

## Permission lives in the tool

The important part first: **prompts, skills, and model output are not permission boundaries.** "Do not delete files without confirmation" in a system prompt stops nothing — the model is probabilistic, and user input can override it.

The real check belongs in `Execute`, or in your adapter layer:

```go
func (t *deleteTool) Execute(ctx context.Context, in agent.ToolInvocation) (agent.ToolResult, error) {
	path, err := t.workspace.Resolve(in.Path) // refuse escapes; do not ask the model not to escape
	if err != nil {
		return agent.ToolResult{IsError: true, Content: "path is outside the workspace"}, nil
	}
	...
}
```

## Pausing a run for a person

The `permission` subpackage turns allow / deny / ask into a persisted decision that can be answered across processes — an approval raised by a CLI can be granted by a person in a web UI.

```go
import "github.com/iceymoss/agent-runtime-go/permission"
```

When the decision is `ask`, the tool returns a suspension and the runtime stops the whole run with `suspended` + `tool_suspended`, checkpointing it. Once a decision is made, resuming under the same run key lets the tool continue with the approval in hand.

Suspension is not only for approvals. Waiting on an external event — a child agent finishing, a webhook, a queued job — uses the same mechanism with a different `ToolSuspensionKind`.

## Side effects that happen once

Once a run can be interrupted and resumed, a new question appears: did that transfer actually go through before the crash?

The `tool` subpackage provides a full execution lifecycle and an effect ledger: each invocation is prepared, then executed, then recorded, with a stable `ToolExecutionKey` that downstream systems dedupe on. A tool with side effects should also declare its replay semantics with `WithToolReplayPolicy`.

```go
import "github.com/iceymoss/agent-runtime-go/tool"
```

The runtime does **not** promise exactly-once external side effects. It promises a stable dedupe anchor; real idempotency needs the downstream system to cooperate.

An application with only read-only tools does not need this layer.

## Going deeper

- [permission](../packages/permission.md) — policy, approval, revalidation
- [tool](../packages/tool.md) — execution lifecycle, effect ledger, bridging to the root Registry
