package event_test

import (
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/event"
)

// TestMemoryStoreConformance holds this package's reference implementation to
// the shared suite, so the suite and the reference cannot drift apart.
func TestMemoryStoreConformance(t *testing.T) {
	agenttest.TestEventStore(t, func(_ *testing.T, clock func() time.Time) event.Store {
		return event.NewMemoryStore(event.WithClock(clock))
	})
}
