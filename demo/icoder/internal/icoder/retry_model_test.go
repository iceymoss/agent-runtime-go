package icoder

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type retryFixtureModel struct {
	attempts  int
	failures  int
	retryable bool
}

func (*retryFixtureModel) Name() string                     { return "fixture" }
func (*retryFixtureModel) Capabilities() agent.Capabilities { return agent.Capabilities{} }
func (m *retryFixtureModel) Stream(context.Context, *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	m.attempts++
	if m.attempts <= m.failures {
		return nil, agent.NewModelError(agent.ModelErrorKindTransport, m.retryable, 503, 0, "fixture", errors.New("upstream"))
	}
	stream := make(chan agent.StreamChunk)
	close(stream)
	return stream, nil
}

func TestRetryModelRetriesExplicitRetryableErrors(t *testing.T) {
	fixture := &retryFixtureModel{failures: 2, retryable: true}
	model := &retryModel{model: fixture, maxAttempts: 3, baseDelay: time.Millisecond}
	if _, err := model.Stream(context.Background(), &agent.GenerateRequest{}); err != nil || fixture.attempts != 3 {
		t.Fatalf("Stream() attempts = %d, error = %v", fixture.attempts, err)
	}
}

func TestRetryModelDoesNotRetryPermanentErrors(t *testing.T) {
	fixture := &retryFixtureModel{failures: 2, retryable: false}
	model := &retryModel{model: fixture, maxAttempts: 3, baseDelay: time.Millisecond}
	if _, err := model.Stream(context.Background(), &agent.GenerateRequest{}); err == nil || fixture.attempts != 1 {
		t.Fatalf("Stream() attempts = %d, error = %v", fixture.attempts, err)
	}
}

func TestRetryModelHonorsCancellation(t *testing.T) {
	fixture := &retryFixtureModel{failures: 3, retryable: true}
	model := &retryModel{model: fixture, maxAttempts: 3, baseDelay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := model.Stream(ctx, &agent.GenerateRequest{}); !errors.Is(err, context.Canceled) || fixture.attempts != 1 {
		t.Fatalf("Stream() attempts = %d, error = %v", fixture.attempts, err)
	}
}
