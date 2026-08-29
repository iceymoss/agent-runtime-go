package coordinator_test

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/coordinator"
)

// TestMemoryManifestStoreConformance holds this package's reference generation
// store to the shared suite, so the suite and the reference cannot drift apart.
func TestMemoryManifestStoreConformance(t *testing.T) {
	agenttest.TestManifestStore(t, func(*testing.T) coordinator.ManifestStore {
		return coordinator.NewMemoryManifestStore()
	})
}
