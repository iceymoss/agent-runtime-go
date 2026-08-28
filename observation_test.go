package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunObservationsAreClonedAndNonTerminal(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: `{"score":4}`}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}},
		{text: "done"},
	}}
	runtime := newTestAgent(t, Config{Key: "test"}, model, tool)
	var types []ObservationType
	sink := NewObservationEmitter(16, func(observation Observation) {
		types = append(types, observation.Type)
		if observation.ToolResult != nil {
			observation.ToolResult.Content = "mutated"
		}
		if observation.Step != nil && len(observation.Step.ToolResults) > 0 {
			observation.Step.ToolResults[0].Content = "mutated"
		}
	})

	result, err := runtime.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: sink})
	sink.Close()
	if err != nil {
		t.Fatal(err)
	}
	if result.Steps[0].ToolResults[0].Content != `{"score":4}` {
		t.Fatalf("sink mutated runtime result: %+v", result)
	}
	for _, eventType := range types {
		if eventType == "finish" || eventType == "error" {
			t.Fatalf("runtime emitted authoritative terminal observation %q", eventType)
		}
	}
}

func TestRunDurableObservationConsumerCannotBlockResult(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "done"}}}
	runtime := newTestAgent(t, Config{Key: "runtime-test", MaxSteps: 4}, model)
	release := make(chan struct{})
	sink := NewObservationEmitter(1, func(Observation) {
		<-release
	})
	request := durableRuntimeRequest(newRuntimeMemoryStore(), "blocked-observation")
	request.ObservationEmitter = sink

	done := make(chan struct{})
	var result *RunResult
	var err error
	go func() {
		result, err = runtime.Run(context.Background(), request)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("durable Run blocked on observation consumer")
	}
	close(release)
	sink.Close()
	if err != nil || result.Outcome != OutcomeCompleted {
		t.Fatalf("result/error = %+v/%v", result, err)
	}
}

// TestLossyEmitterCountsWhatItDropped pins the fact that made silent truncation
// possible: the default emitter discards under backpressure, and until now it
// did so without telling anyone.
func TestLossyEmitterCountsWhatItDropped(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "done"}}}
	runtime := newTestAgent(t, Config{Key: "test"}, model)
	release := make(chan struct{})
	var delivered atomic.Int64
	sink := NewObservationEmitter(1, func(Observation) {
		delivered.Add(1)
		<-release
	})

	if _, err := runtime.Run(context.Background(), RunRequest{
		Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: sink,
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	sink.Close()
	if sink.Dropped() == 0 {
		t.Fatalf("a blocked consumer lost observations without counting them: delivered %d", delivered.Load())
	}
}

// TestLosslessEmitterDeliversEveryTextDelta is the guarantee an application
// streaming to a reader needs: what the person saw is what the run produced.
//
// The consumer here is slower than the model, which is the shape of every SSE
// or WebSocket UI and the case the lossy default silently truncates.
func TestLosslessEmitterDeliversEveryTextDelta(t *testing.T) {
	// The fake model emits one chunk per rune, so this is 200 deltas through a
	// queue of 4.
	answer := strings.Repeat("abcdefghij", 20)
	model := &fakeModel{steps: []scriptedStep{{text: answer}}}
	runtime := newTestAgent(t, Config{Key: "test"}, model)

	var streamed strings.Builder
	var mu sync.Mutex
	sink := NewObservationEmitterWith(ObservationOptions{QueueSize: 4, Lossless: true}, func(observation Observation) {
		if observation.Type != ObservationTextDelta {
			return
		}
		time.Sleep(50 * time.Microsecond)
		mu.Lock()
		streamed.WriteString(observation.Text)
		mu.Unlock()
	})

	result, err := runtime.Run(context.Background(), RunRequest{
		Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: sink,
	})
	sink.Close()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := streamed.String()
	mu.Unlock()
	if got != result.Text {
		t.Fatalf("streamed %d bytes, run produced %d bytes", len(got), len(result.Text))
	}
	if sink.Dropped() != 0 {
		t.Fatalf("lossless emitter dropped %d observations", sink.Dropped())
	}
}

// TestLosslessEmitterDoesNotOutliveACancelledRun keeps the escape hatch honest:
// waiting for room is bounded by the run's context, so a consumer that stopped
// reading cannot hold a cancelled run open.
func TestLosslessEmitterDoesNotOutliveACancelledRun(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: strings.Repeat("x", 64)}}}
	runtime := newTestAgent(t, Config{Key: "test"}, model)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var once sync.Once
	sink := NewObservationEmitterWith(ObservationOptions{QueueSize: 1, Lossless: true}, func(Observation) {
		once.Do(func() { close(started) })
		<-ctx.Done()
	})

	done := make(chan struct{})
	go func() {
		_, _ = runtime.Run(ctx, RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: sink})
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled run stayed blocked on a stalled lossless consumer")
	}
	sink.Close()
}
