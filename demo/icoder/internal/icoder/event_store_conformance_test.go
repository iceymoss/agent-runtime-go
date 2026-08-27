package icoder

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/event"
)

// TestSQLiteEventStoreConformance holds the outbox that actually delivers
// iCoder's reliable events to the same contract as the library's reference
// implementation. At-least-once delivery is only a guarantee if the store keeps
// its half of it.
func TestSQLiteEventStoreConformance(t *testing.T) {
	agenttest.TestEventStore(t, func(t *testing.T, clock func() time.Time) event.Store {
		store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		return NewSQLiteEventStore(store.db, clock)
	})
}
