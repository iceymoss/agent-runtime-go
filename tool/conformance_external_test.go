package tool_test

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	lifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

// TestMemoryLedgerConformance holds this package's reference execution ledger to
// the shared suite, so the suite and the reference cannot drift apart.
func TestMemoryLedgerConformance(t *testing.T) {
	agenttest.TestToolExecutionLedger(t, func(*testing.T) lifecycle.ExecutionLedger {
		return lifecycle.NewMemoryLedger()
	})
}
