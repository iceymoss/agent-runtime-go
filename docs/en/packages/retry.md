# providers/retry

## What it is

Wraps an `agent.Model` so transient upstream failures are retried.

```go
import "github.com/iceymoss/agent-runtime-go/providers/retry"

model := retry.New(openaicompat.New(baseURL, apiKey), retry.Options{})
```

## Why it exists

Hand-written retry usually wraps only what `Stream()` returns — and that retries almost nothing.

An SSE request usually *succeeds*: the HTTP response arrives, `Stream()` returns its channel, and the connection drops partway through. That failure surfaces as an `agent.ChunkError` *inside* the stream, where a wrapper that never looks inside cannot see it. The most common transient failure there is happens to be exactly the one the naive implementation misses.

This package retries both, and refuses to retry the one case where retrying would corrupt the result.

## How to use it

```go
model := retry.New(openaicompat.New(baseURL, apiKey), retry.Options{
	MaxAttempts: 3,                      // including the first; 1 disables retrying
	BaseDelay:   200 * time.Millisecond, // doubled each attempt
	MaxDelay:    30 * time.Second,
})

runner, err := agent.New(agent.Config{Key: "assistant", ModelName: "gpt-4o-mini", MaxSteps: 8},
	model, registry)
```

A zero `Options{}` uses the defaults (3 attempts, 200ms base, 30s cap, up to 25% jitter).

### What is retried

Only failures the adapter itself classified as retryable (`agent.ModelError.Retryable`). Classification belongs to the adapter: it knows a 429 or a dropped connection is worth repeating and an invalid request is not. Guessing from the error text would retry an invalid request forever.

A provider's `Retry-After` wins even when it exceeds `MaxDelay` — the provider knows when it will accept work again better than the backoff curve does.

### What is not retried

**Nothing is retried once a chunk has been handed downstream.** The runtime accumulates the chunks it saw and compares them against the terminal response, so grafting a fresh stream onto a half-delivered one would either fail that check or — worse — pass it with the text duplicated. In that case the error is forwarded as-is and the layer above decides.

So retrying helps most with "the connection died just after opening" and not at all with "it died halfway through the answer". The latter needs the application to run the turn again, not a patch in the middle of a stream.

### Optional capabilities are preserved

If the wrapped model implements `agent.Generator` (non-streaming generation), the wrapper does too; if it does not, the wrapper does not pretend otherwise. A type assertion gets the same answer it would without the wrapper, so wrapping never silently removes a capability something downstream depends on.

## FAQ

**Q: Can a retry run two upstream requests at once?**
No. The abandoned attempt is cancelled and drained before the next one starts.

**Q: What are `Sleep` and `Jitter` for?**
Tests. Leave them nil in production; a test replaces them so verifying backoff behavior does not cost real time.

**Q: What happens when the run is cancelled?**
Backoff waits are bounded by the context, so cancellation ends the wait immediately and hands the error downstream rather than paying for retries nobody is waiting on.
