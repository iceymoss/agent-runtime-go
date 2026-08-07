package icoder

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type retryModel struct {
	model       agent.Model
	maxAttempts int
	baseDelay   time.Duration
}

func newRetryModel(model agent.Model) *retryModel {
	return &retryModel{model: model, maxAttempts: 3, baseDelay: 200 * time.Millisecond}
}

func (m *retryModel) Name() string                     { return m.model.Name() }
func (m *retryModel) Capabilities() agent.Capabilities { return m.model.Capabilities() }
func (m *retryModel) Stream(ctx context.Context, request *agent.GenerateRequest) (<-chan agent.StreamChunk, error) {
	var stream <-chan agent.StreamChunk
	err := m.retry(ctx, func() error {
		var err error
		stream, err = m.model.Stream(ctx, request)
		return err
	})
	return stream, err
}

func (m *retryModel) Generate(ctx context.Context, request *agent.GenerateRequest) (*agent.Response, error) {
	generator, ok := m.model.(agent.Generator)
	if !ok {
		return nil, agent.NewModelError(agent.ModelErrorKindUnsupported, false, 0, 0, "non-streaming generation is unsupported", nil)
	}
	var response *agent.Response
	err := m.retry(ctx, func() error {
		var err error
		response, err = generator.Generate(ctx, request)
		return err
	})
	return response, err
}

func (m *retryModel) retry(ctx context.Context, operation func() error) error {
	for attempt := 1; attempt <= m.maxAttempts; attempt++ {
		err := operation()
		if err == nil || attempt == m.maxAttempts {
			return err
		}
		var modelErr *agent.ModelError
		if !errors.As(err, &modelErr) || !modelErr.Retryable {
			return err
		}
		delay := m.baseDelay << (attempt - 1)
		if modelErr.RetryAfter > delay {
			delay = modelErr.RetryAfter
		}
		delay += time.Duration(rand.Int64N(max(1, int64(delay/4))))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
