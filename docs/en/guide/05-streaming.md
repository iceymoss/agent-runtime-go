# 5. Showing progress

## Where you are stuck

`Run` returns only when the whole turn is done. The user watches a spinner for fifteen seconds while the model has been producing text most of that time.

## Subscribing to progress

`ObservationEmitter` reports text deltas, tool calls, tool results, and finished steps while the run is in flight:

```go
emitter := agent.NewObservationEmitterWith(
	agent.ObservationOptions{QueueSize: 64, Lossless: true},
	func(o agent.Observation) {
		switch o.Type {
		case agent.ObservationTextDelta:
			fmt.Print(o.Text)
		case agent.ObservationToolCall:
			fmt.Printf("\n[calling %s]\n", o.ToolCall.Name)
		}
	})

result, err := runner.Run(ctx, agent.RunRequest{
	Messages:           messages,
	ObservationEmitter: emitter,
})
emitter.Close()
```

## Why Lossless is not optional here

`agent.NewObservationEmitter` — the one without options — is **bounded, non-blocking, and lossy**: an observation that finds the queue full is dropped and counted, so delivery can never hold up the model. That is the right trade for progress telemetry.

It is the wrong one for text a person is reading. Any consumer slower than the model — SSE, WebSockets, and slow clients all qualify — loses deltas silently. The reader sees a truncated answer while `RunResult.Text` is complete, and nothing in your logs says so.

`Lossless: true` makes enqueueing wait for room. The cost is real: a slow consumer now slows the run, so keep `consume` cheap — hand the observation to a buffered writer rather than performing the network write inline. Waiting is bounded by the run's context, so a cancelled run never blocks on a consumer that stopped reading.

When in doubt, check the counter:

```go
if emitter.Dropped() > 0 {
	// the observations are incomplete
}
```

## Worth knowing

**`Observation` is progress, not authority.** The outcome is `RunResult` and the error. For delivery you can rely on — events that must reach a downstream system, or be replayed — use the `event` subpackage in chapter 11.

**A reasoning model's thinking is a separate stream.** `ObservationReasoningDelta` is distinct from `ObservationTextDelta`, so a UI can collapse or hide the thinking without guessing which half it is looking at. See [chapter 6](./06-structured-output.md).

## Going deeper

- [agent package reference](../packages/agent.md) — the four observation types in full
