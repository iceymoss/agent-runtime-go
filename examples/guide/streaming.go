package main

import (
	"bufio"
	"fmt"
	"io"

	agent "github.com/iceymoss/agent-runtime-go"
)

// NewProgressEmitter streams a run's progress to a writer.
//
// Lossless is not optional here: the default emitter drops observations when the
// consumer falls behind the model, which shows the reader a truncated answer
// while RunResult.Text stays complete — and nothing reports that it happened.
// The cost is that a slow consumer now slows the run, which is why the writer is
// buffered rather than written to directly.
func NewProgressEmitter(out io.Writer) *agent.ObservationEmitter {
	buffered := bufio.NewWriter(out)
	return agent.NewObservationEmitterWith(
		agent.ObservationOptions{QueueSize: 64, Lossless: true},
		func(observation agent.Observation) {
			switch observation.Type {
			case agent.ObservationTextDelta:
				fmt.Fprint(buffered, observation.Text)
			case agent.ObservationReasoningDelta:
				// A reasoning model's thinking is a separate stream so a UI can
				// collapse or hide it. This one simply drops it.
			case agent.ObservationToolCall:
				fmt.Fprintf(buffered, "\n  [调用 %s %s]\n", observation.ToolCall.Name, observation.ToolCall.Input)
			case agent.ObservationStepFinished:
				buffered.Flush()
			}
		})
}
