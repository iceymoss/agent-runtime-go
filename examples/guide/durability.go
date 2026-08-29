package main

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/durable"
)

// Runs owns durable execution: where checkpoints live and how one attempt is
// identified.
//
// durable.MemoryStore keeps them in memory, which is enough to see the
// mechanism. Swapping it for a database changes this file and nothing else —
// the runtime only ever sees the agent.CheckpointStore port.
type Runs struct {
	store *durable.MemoryStore
}

func NewRuns() *Runs { return &Runs{store: durable.NewMemoryStore()} }

// Config builds one attempt's durable configuration.
//
// A CheckpointAdapter is bound to a single attempt, so a fresh one is built per
// attempt rather than shared. resume is nil on the first attempt and carries the
// tool's own handle when a suspended run is being continued.
func (r *Runs) Config(runKey string, resume *agent.ToolSuspension) *agent.DurableRunConfig {
	attemptKey := runKey + ":" + mustNonce()
	adapter, err := durable.NewCheckpointAdapter(durable.CheckpointAdapterOptions{
		Store: r.store, Ledger: r.store, AttemptKey: durable.AttemptKey(attemptKey),
	})
	if err != nil {
		panic(err) // only a misconfigured adapter reaches here
	}
	return &agent.DurableRunConfig{
		Identity: agent.RunIdentity{
			RunKey: runKey, AgentKey: "ops.assistant",
			SessionID: "local", RequestID: runKey,
		},
		CheckpointStore: adapter,
		// The lease is what stops two workers from advancing one run at once.
		LeaseOwner:    attemptKey,
		LeaseDuration: 5 * time.Minute,
		ToolResume:    resume,
	}
}

// NewRunKey mints an identity for one turn.
//
// It carries a nonce so a retry after a permanently failed run is a new run
// rather than an attempt to revive a terminal one. Recovering an interrupted
// run happens by reusing its key, not by asking the question again.
func (r *Runs) NewRunKey() (string, error) {
	return "run-" + mustNonce(), nil
}

func mustNonce() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}
