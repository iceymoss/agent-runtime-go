package message_test

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go/agenttest"
	"github.com/iceymoss/agent-runtime-go/message"
)

// TestMemoryConformance holds this package's reference implementation to the
// shared suite, so the suite and the reference cannot drift apart.
func TestMemoryConformance(t *testing.T) {
	agenttest.TestMessageService(t, func(*testing.T) message.Service { return message.NewMemory() })
}
