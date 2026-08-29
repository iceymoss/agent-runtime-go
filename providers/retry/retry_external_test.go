package retry_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/providers/retry"
)

// scriptedModel replays one scripted attempt per call, so a test can describe
// exactly how the upstream misbehaves.
type scriptedModel struct {
	attempts []func(chan<- agent.StreamChunk) error
	calls    atomic.Int64
	generate []func() (*agent.Response, error)
}

func (*scriptedModel) Name() string { return "scripted" }
func (*scriptedModel) Capabilities() agent.Capabilities {
	return agent.Capabilities{Tools: true}
}

func (m *scriptedModel) Stream(ctx context.Context, _ *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	index := int(m.calls.Add(1)) - 1
	if index >= len(m.attempts) {
		return nil, errors.New("upstream called more times than scripted")
	}
	chunks := make(chan agent.StreamChunk, 8)
	if err := m.attempts[index](chunks); err != nil {
		close(chunks)
		return nil, err
	}
	close(chunks)
	return chunks, nil
}

func (m *scriptedModel) Generate(context.Context, *agent.GenerateRequest) (*agent.Response, error) {
	index := int(m.calls.Add(1)) - 1
	if index >= len(m.generate) {
		return nil, errors.New("upstream called more times than scripted")
	}
	return m.generate[index]()
}

func transient(message string) error {
	return agent.NewModelError(agent.ModelErrorKindTransport, true, 0, 0, message, nil)
}

func permanent(message string) error {
	return agent.NewModelError(agent.ModelErrorKindRejected, false, 0, 0, message, nil)
}

func answer(text string) func(chan<- agent.StreamChunk) error {
	return func(chunks chan<- agent.StreamChunk) error {
		message := agent.NewAssistantMessage(text)
		message.FinishReason = agent.FinishStop
		for chunk := range agent.StreamResponse(&agent.Response{Message: message, FinishReason: agent.FinishStop}) {
			chunks <- chunk
		}
		return nil
	}
}

func failMidStream(prefix string, cause error) func(chan<- agent.StreamChunk) error {
	return func(chunks chan<- agent.StreamChunk) error {
		if prefix != "" {
			chunks <- agent.StreamChunk{Type: agent.ChunkText, TextDelta: prefix}
		}
		chunks <- agent.StreamChunk{Type: agent.ChunkError, Err: cause}
		return nil
	}
}

func failToOpen(cause error) func(chan<- agent.StreamChunk) error {
	return func(chan<- agent.StreamChunk) error { return cause }
}

func testOptions() retry.Options {
	return retry.Options{
		MaxAttempts: 3,
		Sleep:       func(context.Context, time.Duration) error { return nil },
		Jitter:      func(delay time.Duration) time.Duration { return delay },
	}
}

func collect(t *testing.T, model agent.Model) (string, error) {
	t.Helper()
	chunks, err := model.Stream(context.Background(), &agent.GenerateRequest{Model: "scripted"})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for chunk := range chunks {
		switch chunk.Type {
		case agent.ChunkText:
			text.WriteString(chunk.TextDelta)
		case agent.ChunkError:
			return text.String(), chunk.Err
		}
	}
	return text.String(), nil
}

// TestRetriesAStreamThatFailsBeforeDeliveringAnything is the case a wrapper
// around Stream() alone never sees: the HTTP call succeeded and the connection
// dropped afterwards, which is how SSE usually fails.
func TestRetriesAStreamThatFailsBeforeDeliveringAnything(t *testing.T) {
	model := &scriptedModel{attempts: []func(chan<- agent.StreamChunk) error{
		failMidStream("", transient("connection reset")),
		answer("the answer"),
	}}
	text, err := collect(t, retry.New(model, testOptions()))
	if err != nil || text != "the answer" {
		t.Fatalf("text = %q, error = %v", text, err)
	}
	if model.calls.Load() != 2 {
		t.Fatalf("upstream was called %d times, want 2", model.calls.Load())
	}
}

// TestDoesNotRetryAfterDeliveringContent is the safety boundary. The runtime
// compares the chunks it saw against the terminal response, so a second attempt
// grafted onto a half-delivered stream would either fail that check or, worse,
// pass it with the text duplicated.
func TestDoesNotRetryAfterDeliveringContent(t *testing.T) {
	model := &scriptedModel{attempts: []func(chan<- agent.StreamChunk) error{
		failMidStream("half an ", transient("connection reset")),
		answer("the answer"),
	}}
	text, err := collect(t, retry.New(model, testOptions()))
	if err == nil {
		t.Fatal("a partially delivered stream was retried")
	}
	if text != "half an " {
		t.Fatalf("downstream saw %q, want only what was already delivered", text)
	}
	if model.calls.Load() != 1 {
		t.Fatalf("upstream was called %d times, want 1", model.calls.Load())
	}
}

// TestRetriesAStreamThatNeverOpened keeps the ordinary case working: a failure
// with no stream is still reported as an error from Stream itself.
func TestRetriesAStreamThatNeverOpened(t *testing.T) {
	model := &scriptedModel{attempts: []func(chan<- agent.StreamChunk) error{
		failToOpen(transient("dial failed")),
		failToOpen(transient("dial failed")),
		answer("recovered"),
	}}
	text, err := collect(t, retry.New(model, testOptions()))
	if err != nil || text != "recovered" {
		t.Fatalf("text = %q, error = %v", text, err)
	}

	exhausted := &scriptedModel{attempts: []func(chan<- agent.StreamChunk) error{
		failToOpen(transient("dial failed")),
		failToOpen(transient("dial failed")),
		failToOpen(transient("dial failed")),
	}}
	if _, err := collect(t, retry.New(exhausted, testOptions())); err == nil {
		t.Fatal("an exhausted retry budget reported success")
	}
	if exhausted.calls.Load() != 3 {
		t.Fatalf("upstream was called %d times, want the full budget", exhausted.calls.Load())
	}
}

// TestDoesNotRetryWhatTheProviderCalledPermanent leaves classification to the
// adapter: guessing from the message would repeat an invalid request forever.
func TestDoesNotRetryWhatTheProviderCalledPermanent(t *testing.T) {
	tests := []struct {
		name    string
		attempt func(chan<- agent.StreamChunk) error
	}{
		{name: "a stream that never opened", attempt: failToOpen(permanent("invalid request"))},
		{name: "a stream that failed midway", attempt: failMidStream("", permanent("invalid request"))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			model := &scriptedModel{attempts: []func(chan<- agent.StreamChunk) error{test.attempt, answer("unreachable")}}
			if _, err := collect(t, retry.New(model, testOptions())); err == nil {
				t.Fatal("a permanent failure was reported as success")
			}
			if model.calls.Load() != 1 {
				t.Fatalf("upstream was called %d times, want 1", model.calls.Load())
			}
		})
	}
}

// TestHonorsRetryAfter lets a rate-limited provider set the pace, since it knows
// when it will accept work again and the backoff curve does not.
func TestHonorsRetryAfter(t *testing.T) {
	var waited []time.Duration
	options := testOptions()
	options.BaseDelay = time.Millisecond
	options.Sleep = func(_ context.Context, delay time.Duration) error {
		waited = append(waited, delay)
		return nil
	}
	rateLimited := agent.NewModelError(agent.ModelErrorKindRateLimit, true, 429, 7*time.Second, "slow down", nil)
	model := &scriptedModel{attempts: []func(chan<- agent.StreamChunk) error{
		failMidStream("", rateLimited),
		answer("ok"),
	}}
	if _, err := collect(t, retry.New(model, options)); err != nil {
		t.Fatal(err)
	}
	if len(waited) != 1 || waited[0] != 7*time.Second {
		t.Fatalf("waited %v, want the provider's Retry-After", waited)
	}
}

// TestPreservesTheGeneratorCapability matters because wrapping must not silently
// remove something a caller type-asserts for.
func TestPreservesTheGeneratorCapability(t *testing.T) {
	message := agent.NewAssistantMessage("generated")
	message.FinishReason = agent.FinishStop
	model := &scriptedModel{generate: []func() (*agent.Response, error){
		func() (*agent.Response, error) { return nil, transient("dial failed") },
		func() (*agent.Response, error) {
			return &agent.Response{Message: message, FinishReason: agent.FinishStop}, nil
		},
	}}
	wrapped := retry.New(model, testOptions())
	generator, ok := wrapped.(agent.Generator)
	if !ok {
		t.Fatal("wrapping removed the Generator capability")
	}
	response, err := generator.Generate(context.Background(), &agent.GenerateRequest{Model: "scripted"})
	if err != nil || response.Message.Text() != "generated" {
		t.Fatalf("Generate() = %+v, error %v", response, err)
	}

	// A model that cannot generate must not start claiming it can.
	if _, ok := retry.New(streamOnlyModel{}, testOptions()).(agent.Generator); ok {
		t.Fatal("wrapping added a Generator capability the model does not have")
	}
}

type streamOnlyModel struct{}

func (streamOnlyModel) Name() string                     { return "stream-only" }
func (streamOnlyModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (streamOnlyModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	return agent.StreamResponse(nil), nil
}

// TestStopsWhenTheCallerGivesUp keeps a cancelled run from paying for a backoff
// nobody is waiting on.
func TestStopsWhenTheCallerGivesUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	options := testOptions()
	options.Sleep = func(sleepCtx context.Context, _ time.Duration) error {
		cancel()
		return sleepCtx.Err()
	}
	model := &scriptedModel{attempts: []func(chan<- agent.StreamChunk) error{
		failToOpen(transient("dial failed")),
		answer("unreachable"),
	}}
	if _, err := retry.New(model, options).Stream(ctx, &agent.GenerateRequest{Model: "scripted"}); err == nil {
		t.Fatal("Stream() succeeded after the caller gave up")
	}
	_ = ctx
}
