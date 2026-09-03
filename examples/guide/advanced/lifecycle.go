package advanced

import (
	"context"
	"database/sql"
	"time"

	"github.com/iceymoss/agent-runtime-go/app"
)

// storeComponent is one node in the startup graph.
//
// Ready actually touches the database. A component that reports ready because
// it was configured is worse than no probe at all: it makes a broken deployment
// look healthy.
type storeComponent struct{ db *sql.DB }

func (storeComponent) Name() string                              { return "store" }
func (storeComponent) Dependencies() []string                    { return nil }
func (storeComponent) Criticality() app.Criticality              { return app.Required }
func (storeComponent) Start(context.Context, app.Reporter) error { return nil }
func (storeComponent) Close(context.Context) error               { return nil }

func (c storeComponent) Ready(ctx context.Context) app.ComponentHealth {
	if err := c.db.PingContext(ctx); err != nil {
		return app.ComponentHealth{Configured: true, Reason: "database unreachable: " + err.Error()}
	}
	return app.ComponentHealth{Configured: true, Ready: true, Generation: "schema-v1"}
}

// NewLifecycle assembles the process around the agent.
//
// The budgets are the point. Shutdown runs stop admission → cancel → checkpoint
// → wait → flush → close, and each step gets its own deadline, so a slow flush
// cannot eat the time reserved for checkpointing runs that are still in flight.
// Stopping admission first is what keeps a closing process from accepting a run
// it will kill a moment later.
func NewLifecycle(db *sql.DB, runs app.RunController, checkpoints app.Checkpointer, events app.Flusher) (*app.App, error) {
	return app.New(
		app.Config{Budgets: app.Budgets{
			Startup:    30 * time.Second,
			Drain:      20 * time.Second,
			Cancel:     5 * time.Second,
			Checkpoint: 10 * time.Second,
			Wait:       20 * time.Second,
			Flush:      5 * time.Second,
			Close:      5 * time.Second,
		}},
		// These three are yours: what a run is, how it is checkpointed, and how
		// pending events are flushed. The library owns the ordering, not the
		// meaning.
		app.Dependencies{Runs: runs, Checkpoints: checkpoints, Events: events},
		storeComponent{db: db},
	)
}
