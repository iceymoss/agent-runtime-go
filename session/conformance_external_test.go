package session_test

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/session"
)

// TestMemoryConformance holds this package's reference session aggregate to the
// shared suite, so the suite and the reference cannot drift apart.
func TestMemoryConformance(t *testing.T) {
	agenttest.TestSessionService(t, func(*testing.T) session.Service { return session.NewMemory() })
}

// TestMemoryRunStoreConformance holds the reference run queue to the shared
// suite, so the suite and the reference cannot drift apart.
func TestMemoryRunStoreConformance(t *testing.T) {
	agenttest.TestSessionRunStore(t, func(*testing.T) session.Store { return session.NewMemoryRunStore() })
}
