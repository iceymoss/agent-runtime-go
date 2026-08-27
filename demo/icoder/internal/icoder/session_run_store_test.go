package icoder

import (
	"path/filepath"
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/session"
)

func newSessionRunStore(t *testing.T) *SQLiteSessionRunStore {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewSQLiteSessionRunStore(store.db)
}

// TestSQLiteSessionRunStoreConformance holds the run queue to its contract. A
// claim that is not exclusive means a run executes twice, and a merge that
// applies twice advances the session revision twice for one turn, so the suite
// is what proves this adapter fences both the same way the reference does.
func TestSQLiteSessionRunStoreConformance(t *testing.T) {
	agenttest.TestSessionRunStore(t, func(t *testing.T) session.Store { return newSessionRunStore(t) })
}
