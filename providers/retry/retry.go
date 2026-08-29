// Package retry wraps a model so transient upstream failures are retried.
//
// Every application talking to a real provider needs this, and the obvious
// implementation is wrong in a way that is easy to miss: wrapping only the call
// that opens the stream retries almost nothing. An SSE request usually succeeds
// - the HTTP response arrives - and then fails partway through, which reaches
// the runtime as an agent.ChunkError inside the stream. A wrapper that never
// looks inside the stream therefore has no effect on the most common transient
// failure there is.
//
// This package retries both, and refuses to retry the one case where retrying
// would corrupt the answer: once any chunk has been handed downstream, a second
// attempt would duplicate content the runtime already accumulated, so the error
// is forwarded instead.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

// Defaults used when Options leaves a field zero.
const (
	DefaultMaxAttempts = 3
	DefaultBaseDelay   = 200 * time.Millisecond
	DefaultMaxDelay    = 30 * time.Second
)

// Options configures the retry policy.
type Options struct {
	// MaxAttempts is the total number of attempts, including the first. Values
	// <= 0 use DefaultMaxAttempts; 1 disables retrying without changing anything
	// else about the wrapper.
	MaxAttempts int
	// BaseDelay is the first backoff interval, doubled on each further attempt.
	BaseDelay time.Duration
	// MaxDelay caps the backoff. A provider's Retry-After is honored even when it
	// exceeds this, because the provider knows better than the cap does.
	MaxDelay time.Duration
	// Sleep waits for one backoff interval and reports why it stopped. It exists
	// so tests do not have to spend real time; production leaves it nil.
	Sleep func(context.Context, time.Duration) error
	// Jitter spreads retries from many callers that failed together. Leave it nil
	// for uniform jitter up to a quarter of the delay; a deterministic test may
	// supply the identity function.
	Jitter func(time.Duration) time.Duration
}

func (o Options) normalize() Options {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	if o.BaseDelay <= 0 {
		o.BaseDelay = DefaultBaseDelay
	}
	if o.MaxDelay <= 0 {
		o.MaxDelay = DefaultMaxDelay
	}
	if o.Sleep == nil {
		o.Sleep = sleep
	}
	if o.Jitter == nil {
		o.Jitter = jitter
	}
	return o
}

// New wraps a model with the given retry policy.
//
// The result preserves the optional agent.Generator capability: a caller that
// type-asserts for non-streaming generation gets the same answer it would have
// got from the unwrapped model, so wrapping never silently removes a capability
// something downstream depends on.
func New(model agent.Model, options Options) agent.Model {
	wrapped := &retrying{model: model, options: options.normalize()}
	if generator, ok := model.(agent.Generator); ok {
		return &retryingGenerator{retrying: wrapped, generator: generator}
	}
	return wrapped
}

type retrying struct {
	model   agent.Model
	options Options
}

var _ agent.Model = (*retrying)(nil)

func (r *retrying) Name() string                     { return r.model.Name() }
func (r *retrying) Capabilities() agent.Capabilities { return r.model.Capabilities() }

// Stream opens the upstream stream, retrying an attempt that fails before it
// delivers anything.
//
// The first attempt is opened synchronously so a failure that never produced a
// stream is reported as an error from Stream itself, the way an unwrapped
// adapter reports it.
func (r *retrying) Stream(ctx context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	upstream, cancel, err := r.open(ctx, request, 1)
	if err != nil {
		return nil, err
	}
	out := make(chan agent.StreamChunk)
	go r.forward(ctx, request, upstream, cancel, out)
	return out, nil
}

// open makes attempts until one returns a stream or the policy gives up.
func (r *retrying) open(ctx context.Context, request *agent.GenerateRequest, attempt int) (<-chan agent.StreamChunk, context.CancelFunc, error) {
	for {
		attemptCtx, cancel := context.WithCancel(ctx)
		upstream, err := r.model.Stream(attemptCtx, request)
		if err == nil {
			return upstream, cancel, nil
		}
		cancel()
		next, wait := r.next(attempt, err)
		if !wait {
			return nil, nil, err
		}
		if sleepErr := r.options.Sleep(ctx, next); sleepErr != nil {
			return nil, nil, sleepErr
		}
		attempt++
	}
}

// forward relays one stream downstream and re-opens it when an attempt fails
// before delivering anything.
//
// The "before delivering anything" condition is the whole safety argument: the
// runtime accumulates the chunks it has seen and compares them against the
// terminal response, so replacing a half-delivered stream with a fresh one would
// produce an answer that fails that check - or worse, one that passes it while
// containing text twice.
func (r *retrying) forward(ctx context.Context, request *agent.GenerateRequest, upstream <-chan agent.StreamChunk, cancel context.CancelFunc, out chan<- agent.StreamChunk) {
	defer close(out)
	attempt := 1
	delivered := false
	for {
		var failure error
		for chunk := range upstream {
			if chunk.Type == agent.ChunkError && !delivered {
				failure = chunk.Err
				break
			}
			if !send(ctx, out, chunk) {
				cancel()
				drain(upstream)
				return
			}
			delivered = true
		}
		if failure == nil {
			cancel()
			return
		}
		// Stop the abandoned attempt and let its producer finish before the next
		// one starts, so a retry never runs two upstream requests at once.
		cancel()
		drain(upstream)

		next, wait := r.next(attempt, failure)
		if !wait {
			send(ctx, out, agent.StreamChunk{Type: agent.ChunkError, Err: failure})
			return
		}
		if err := r.options.Sleep(ctx, next); err != nil {
			send(ctx, out, agent.StreamChunk{Type: agent.ChunkError, Err: err})
			return
		}
		attempt++
		reopened, reopenedCancel, err := r.open(ctx, request, attempt)
		if err != nil {
			send(ctx, out, agent.StreamChunk{Type: agent.ChunkError, Err: err})
			return
		}
		upstream, cancel = reopened, reopenedCancel
	}
}

// next reports how long to wait before the given attempt is retried, and whether
// it should be retried at all.
//
// Only a failure the provider itself classified as retryable is retried: the
// adapter knows whether a 429 or a dropped connection is worth repeating, and
// guessing from the error text would retry things like an invalid request
// forever.
func (r *retrying) next(attempt int, err error) (time.Duration, bool) {
	if attempt >= r.options.MaxAttempts {
		return 0, false
	}
	var modelErr *agent.ModelError
	if !errors.As(err, &modelErr) || !modelErr.Retryable {
		return 0, false
	}
	delay := r.options.BaseDelay << (attempt - 1)
	if delay > r.options.MaxDelay || delay <= 0 {
		delay = r.options.MaxDelay
	}
	delay = r.options.Jitter(delay)
	if modelErr.RetryAfter > delay {
		delay = modelErr.RetryAfter
	}
	return delay, true
}

// retryingGenerator adds back the optional non-streaming capability.
type retryingGenerator struct {
	*retrying
	generator agent.Generator
}

var _ agent.Generator = (*retryingGenerator)(nil)

// Generate retries a non-streaming call, which has no partial-delivery problem:
// either the whole response arrived or none of it did.
func (r *retryingGenerator) Generate(ctx context.Context, request *agent.GenerateRequest) (*agent.Response, error) {
	for attempt := 1; ; attempt++ {
		response, err := r.generator.Generate(ctx, request)
		if err == nil {
			return response, nil
		}
		next, wait := r.next(attempt, err)
		if !wait {
			return nil, err
		}
		if sleepErr := r.options.Sleep(ctx, next); sleepErr != nil {
			return nil, sleepErr
		}
	}
}

// send hands one chunk downstream, reporting false when the caller stopped
// listening.
func send(ctx context.Context, out chan<- agent.StreamChunk, chunk agent.StreamChunk) bool {
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

// drain releases an abandoned upstream so its producer is not left blocked on a
// channel nobody reads.
func drain(upstream <-chan agent.StreamChunk) {
	go func() {
		for range upstream {
		}
	}()
}

func sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("retry aborted: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func jitter(delay time.Duration) time.Duration {
	spread := int64(delay / 4)
	if spread <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(spread))
}
