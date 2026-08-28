package agent

import (
	"context"
	"sync"
	"sync/atomic"
)

// ObservationType identifies a best-effort runtime observation. Terminal run
// state is deliberately absent; callers must use RunResult and durable state.
type ObservationType string

const (
	ObservationTextDelta ObservationType = "text_delta"
	// ObservationReasoningDelta carries a model's thinking as it arrives. It is
	// separate from ObservationTextDelta so a UI can render, hide, or redact it
	// without having to guess which half of the stream it is looking at.
	ObservationReasoningDelta ObservationType = "reasoning_delta"
	ObservationToolCall       ObservationType = "tool_call_start"
	ObservationToolResult     ObservationType = "tool_result"
	ObservationStepFinished   ObservationType = "step_finish"
)

// Observation is a detached snapshot of non-authoritative runtime progress.
// The runtime deep-clones it before handing it to a sink.
type Observation struct {
	Type       ObservationType `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolCall   *ToolCall       `json:"tool_call,omitempty"`
	ToolResult *ToolResult     `json:"tool_result,omitempty"`
	Step       *StepResult     `json:"step,omitempty"`
}

const defaultObservationEmitterSize = 16

// ObservationEmitter is the runtime's concrete bounded observation sink. The
// runtime only enqueues snapshots; delivery runs on one bounded worker.
//
// By default enqueueing never blocks model, tool, or checkpoint progress: an
// observation that finds the queue full is dropped and counted. That is the
// right trade for progress telemetry, and the wrong one for text a person is
// reading - see ObservationOptions.Lossless.
type ObservationEmitter struct {
	mu       sync.RWMutex
	queue    chan Observation
	done     chan struct{}
	once     sync.Once
	lossless bool
	dropped  atomic.Uint64
}

// ObservationOptions configures an emitter.
type ObservationOptions struct {
	// QueueSize bounds how many observations may await delivery. Values <= 0 use
	// the default.
	QueueSize int
	// Lossless makes enqueueing wait for room instead of dropping.
	//
	// Set it when the observations are the product rather than telemetry - text
	// streamed to a person over SSE or a WebSocket is the usual case, and losing
	// deltas there truncates the answer the reader sees while RunResult.Text
	// stays complete. The cost is real: a consumer slower than the model now
	// slows the run, so keep consume cheap (hand the observation to a buffered
	// writer, do not perform the network write inline). Waiting is bounded by the
	// run's context, so a cancelled run never blocks on a stalled consumer.
	Lossless bool
}

// NewObservationEmitter creates a bounded, lossy emitter. consume must not
// retain or mutate observations after it returns.
//
// Use NewObservationEmitterWith when dropped observations would be visible to a
// user; this constructor drops silently under backpressure by design.
func NewObservationEmitter(queueSize int, consume func(Observation)) *ObservationEmitter {
	return NewObservationEmitterWith(ObservationOptions{QueueSize: queueSize}, consume)
}

// NewObservationEmitterWith creates an emitter with explicit delivery options.
func NewObservationEmitterWith(options ObservationOptions, consume func(Observation)) *ObservationEmitter {
	if options.QueueSize <= 0 {
		options.QueueSize = defaultObservationEmitterSize
	}
	emitter := &ObservationEmitter{
		queue:    make(chan Observation, options.QueueSize),
		done:     make(chan struct{}),
		lossless: options.Lossless,
	}
	go func() {
		defer close(emitter.done)
		for observation := range emitter.queue {
			if consume != nil {
				consume(observation)
			}
		}
	}()
	return emitter
}

// Dropped reports how many observations were discarded because the queue was
// full or the emitter was closing.
//
// It exists so loss is detectable rather than silent: an application streaming
// text to a person can compare it against zero and know whether what the reader
// saw was the whole answer.
func (e *ObservationEmitter) Dropped() uint64 {
	if e == nil {
		return 0
	}
	return e.dropped.Load()
}

// observe enqueues one observation, waiting for room only in lossless mode.
//
// The read lock is never taken blockingly: Close takes the write lock, so an
// emitter that is shutting down drops rather than racing a closed channel, in
// both modes.
func (e *ObservationEmitter) observe(ctx context.Context, observation Observation) bool {
	if e == nil {
		return false
	}
	if !e.mu.TryRLock() {
		e.dropped.Add(1)
		return false
	}
	defer e.mu.RUnlock()
	if !e.lossless {
		select {
		case e.queue <- observation:
			return true
		default:
			e.dropped.Add(1)
			return false
		}
	}
	if ctx == nil {
		e.queue <- observation
		return true
	}
	select {
	case e.queue <- observation:
		return true
	case <-ctx.Done():
		// A cancelled run must not be held up by a consumer that stopped reading.
		e.dropped.Add(1)
		return false
	}
}

// Close stops admission and waits for already queued observations to drain.
func (e *ObservationEmitter) Close() {
	if e == nil {
		return
	}
	e.closeAdmission()
	<-e.done
}

func (e *ObservationEmitter) closeAdmission() {
	if e == nil {
		return
	}
	e.once.Do(func() {
		e.mu.Lock()
		close(e.queue)
		e.mu.Unlock()
	})
}

type observer struct {
	sink *ObservationEmitter
}

func (o *observer) emit(ctx context.Context, observation Observation) {
	if o == nil || o.sink == nil {
		return
	}
	o.sink.observe(ctx, cloneObservation(observation))
}

func cloneObservation(observation Observation) Observation {
	cloned := observation
	if observation.ToolCall != nil {
		call := *observation.ToolCall
		cloned.ToolCall = &call
	}
	if observation.ToolResult != nil {
		result := *observation.ToolResult
		cloned.ToolResult = &result
	}
	if observation.Step != nil {
		step := cloneSteps([]StepResult{*observation.Step})[0]
		cloned.Step = &step
	}
	return cloned
}
