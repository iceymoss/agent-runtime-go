package agent_test

import (
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

func TestTenantKeyValidity(t *testing.T) {
	if agent.TenantKey("").Valid() {
		t.Fatal("empty tenant key is valid")
	}
	if !agent.TenantKey("opaque-value").Valid() {
		t.Fatal("non-empty tenant key is invalid")
	}
}
