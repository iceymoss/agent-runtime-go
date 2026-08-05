package event_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
)

type publisherFunc func(context.Context, event.Envelope) error

func (function publisherFunc) Publish(ctx context.Context, envelope event.Envelope) error {
	return function(ctx, envelope)
}

func TestDispatcherRetryThenDeliver(t *testing.T) {
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	store := event.NewMemoryStore(event.WithClock(func() time.Time { return now }))
	if _, err := store.Append(context.Background(), event.AppendCommand{Envelope: testEnvelope(agent.TenantKey("tenant"), "stream", "event")}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ids []string
	publisher := publisherFunc(func(_ context.Context, envelope event.Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, envelope.EventID)
		if len(ids) == 1 {
			return errors.Join(event.ErrPublishRetryable, errors.New("temporary"))
		}
		return nil
	})
	dispatcher, err := event.NewDispatcher(store, publisher, event.DispatcherConfig{
		TenantKey: agent.TenantKey("tenant"), Owner: "dispatcher", BatchSize: 10,
		LeaseDuration: time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Minute, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := dispatcher.RunOnce(context.Background())
	if err != nil || first.Retried != 1 {
		t.Fatalf("first dispatch = %#v, %v", first, err)
	}
	now = now.Add(time.Second)
	second, err := dispatcher.DispatchBatch(context.Background())
	if err != nil || second.Delivered != 1 {
		t.Fatalf("second dispatch = %#v, %v", second, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("delivery IDs = %v", ids)
	}
}
