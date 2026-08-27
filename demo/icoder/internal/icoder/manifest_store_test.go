package icoder

import (
	"path/filepath"
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/coordinator"
)

// TestSQLiteManifestStoreConformance holds this adapter to the same generation
// contract as the library's reference implementation.
func TestSQLiteManifestStoreConformance(t *testing.T) {
	agenttest.TestManifestStore(t, func(t *testing.T) coordinator.ManifestStore {
		store, err := OpenStore(filepath.Join(t.TempDir(), "icoder.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		return store.ManifestStore()
	})
}
