package agent

import "sync"

// ObservationType identifies a best-effort runtime observation. Terminal run
// state is deliberately absent; callers must use RunResult and durable state.
type ObservationType string

const (
	ObservationTextDelta    ObservationType = "text_delta"
	ObservationToolCall     ObservationType = "tool_call_start"
	ObservationToolResult   ObservationType = "tool_result"
	ObservationStepFinished ObservationType = "step_finish"
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
// runtime only enqueues snapshots; delivery runs on one bounded worker and can
// never block model, tool, or checkpoint progress.
type ObservationEmitter struct {
	mu    sync.RWMutex
	queue chan Observation
	done  chan struct{}
	once  sync.Once
}

// NewObservationEmitter creates a bounded emitter. consume must not retain or
// mutate observations after it returns.
func NewObservationEmitter(queueSize int, consume func(Observation)) *ObservationEmitter {
	if queueSize <= 0 {
		queueSize = defaultObservationEmitterSize
	}
	emitter := &ObservationEmitter{queue: make(chan Observation, queueSize), done: make(chan struct{})}
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

func (e *ObservationEmitter) tryObserve(observation Observation) bool {
	if e == nil {
		return false
	}
	if !e.mu.TryRLock() {
		return false
	}
	defer e.mu.RUnlock()
	select {
	case e.queue <- observation:
		return true
	default:
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

func (o *observer) emit(observation Observation) {
	if o == nil || o.sink == nil {
		return
	}
	o.sink.tryObserve(cloneObservation(observation))
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
