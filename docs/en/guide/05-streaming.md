# 5. Showing progress

## Where you are stuck

`Run` returns only when the whole turn is done. The ops assistant reads logs and then answers, which takes fifteen seconds of a motionless cursor.

## Subscribing to progress

```go
func NewProgressEmitter(out io.Writer) *agent.ObservationEmitter {
	buffered := bufio.NewWriter(out)
	return agent.NewObservationEmitterWith(
		agent.ObservationOptions{QueueSize: 64, Lossless: true},
		func(observation agent.Observation) {
			switch observation.Type {
			case agent.ObservationTextDelta:
				fmt.Fprint(buffered, observation.Text)
			case agent.ObservationReasoningDelta:
				// A reasoning model's thinking is a separate stream; a UI can
				// collapse it or drop it, as this one does.
			case agent.ObservationToolCall:
				fmt.Fprintf(buffered, "\n  [calling %s %s]\n", observation.ToolCall.Name, observation.ToolCall.Input)
			case agent.ObservationStepFinished:
				buffered.Flush()
			}
		})
}
```

```go
emitter := NewProgressEmitter(os.Stdout)
result, err := runner.Run(ctx, agent.RunRequest{
	Messages:           transcript.Prompt(question),
	ObservationEmitter: emitter,
})
emitter.Close()
```

The user now sees:

```text
  [calling read_logs {"service":"checkout","level":"WARN"}]
checkout's payment gateway keeps timing out and the connection pool is exhausted.
```

## Why Lossless is not optional here

`agent.NewObservationEmitter` — the one without options — is **bounded, non-blocking, and lossy**: an observation that finds the queue full is dropped and counted, so delivery can never hold up the model. That is the right trade for progress telemetry.

It is the wrong one for text a person is reading. **Any consumer slower than the model loses deltas silently** — SSE, WebSockets, and slow clients all qualify. The reader sees a truncated answer while `RunResult.Text` is complete, and nothing in your logs says so.

`Lossless: true` makes enqueueing wait for room. The cost is real: a slow consumer now slows the run. That is why the code above uses a `bufio.Writer` — **hand the observation to a buffer; do not perform the network write inside the callback.** Waiting is bounded by the run's context, so a cancelled run never blocks on a consumer that stopped reading.

When in doubt, check the counter:

```go
if emitter.Dropped() > 0 {
	// what the user saw was not the whole answer
}
```

## Worth knowing

**`Observation` is progress, not authority.** The outcome is `RunResult` and the error. For delivery you can rely on, use `event` in chapter 11.

**Call `Close()`, and call it before reading `result`.** It stops admission and waits for the queue to drain; without it the last few deltas may never be consumed.

## Going deeper

- [agent package reference](../packages/agent.md) — the five observation types in full
- Complete runnable code: [`examples/guide/streaming.go`](https://github.com/iceymoss/agent-runtime-go/blob/main/examples/guide/streaming.go)
