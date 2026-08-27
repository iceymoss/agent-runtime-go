package permission_test

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/permission"
)

// TestMemoryStoreConformance holds this package's reference implementation to
// the shared suite, so the suite and the reference cannot drift apart.
func TestMemoryStoreConformance(t *testing.T) {
	agenttest.TestPermissionStore(t, func(_ *testing.T, clock permission.Clock) permission.Store {
		return permission.NewMemoryStore(clock)
	})
}
