package icoder

import (
	"path/filepath"
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/session"
)

func newSessionService(t *testing.T) *SQLiteSessionService {
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
	return NewSQLiteSessionService(store.db)
}

// TestSQLiteSessionServiceConformance holds the adapter to the aggregate
// contract. A merge applied twice duplicates a turn and a usage fact counted
// twice double-bills, so the suite is what proves this adapter and the library's
// reference implementation decide those the same way.
func TestSQLiteSessionServiceConformance(t *testing.T) {
	agenttest.TestSessionService(t, func(t *testing.T) session.Service { return newSessionService(t) })
}
