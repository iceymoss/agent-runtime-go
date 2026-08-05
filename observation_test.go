package agent

import (
	"context"
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
