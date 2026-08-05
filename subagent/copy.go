package subagent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/iceymoss/agent-runtime-go/durable"
)

func digest(value any) (string, error) {
	return durable.CanonicalDigest(value)
}

func derivedKey(prefix string, tenant string, key RequestKey) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s", prefix, tenant, key)))
	return prefix + "/" + hex.EncodeToString(sum[:16])
}

func cloneBytes(value []byte) []byte {
	return append([]byte(nil), value...)
}

func cloneContextRefs(value []ContextRef) []ContextRef {
	return append([]ContextRef(nil), value...)
}

func cloneFailure(value *Failure) *Failure {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneSnapshot(value Snapshot) Snapshot {
	value.Input = cloneBytes(value.Input)
	value.ContextRefs = cloneContextRefs(value.ContextRefs)
	value.Failure = cloneFailure(value.Failure)
	return value
}

func cloneFact(value CommittedFact) CommittedFact {
	value.Payload = cloneBytes(value.Payload)
	return value
}
