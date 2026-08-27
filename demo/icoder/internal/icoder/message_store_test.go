package icoder

import (
	"path/filepath"
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/message"
)

// TestSQLiteMessageServiceConformance holds this adapter to the same message
// aggregate contract as the library's reference implementation.
func TestSQLiteMessageServiceConformance(t *testing.T) {
	agenttest.TestMessageService(t, func(t *testing.T) message.Service {
		store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		return store.Messages()
	})
}
