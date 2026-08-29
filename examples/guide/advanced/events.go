// Package advanced holds the integrations from the later guide chapters.
//
// They are separate from the runnable ops assistant because each needs
// something the example cannot bring along — an MCP server, a database, a
// worker pool. They compile and are tested, so the guide quotes working code
// rather than sketches.
package advanced

import (
	"context"
	"encoding/json"
	"time"

	agent "github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/event"
)

// RecordRunFinished publishes a fact a downstream system must not miss.
//
// This is the difference from an Observation: observations are progress and may
// be dropped, while this one is billed on, audited, and replayed. In production
// the append shares a transaction with the business write, so there is no window
// where one succeeded and the other did not.
func RecordRunFinished(ctx context.Context, store event.Store, sessionKey string, result *agent.RunResult) (event.Envelope, error) {
	payload, err := json.Marshal(map[string]any{
		"outcome":     result.Outcome,
		"stop_reason": result.StopReason,
		"steps":       len(result.Steps),
		"tokens":      result.Usage.TotalTokens,
	})
	if err != nil {
		return event.Envelope{}, err
	}
	return store.Append(ctx, event.AppendCommand{Envelope: event.Envelope{
		TenantKey: "local",
		// The event id is the idempotency anchor: appending the same id twice is
		// the same event, which is what makes a retry safe.
		EventID:       sessionKey + ":run-finished",
		StreamKey:     sessionKey,
		Type:          "agent.run.finished",
		SchemaVersion: 1,
		// Terminal facts are the ones a consumer may not lose.
		Reliability:   event.ReliabilityTerminal,
		AggregateType: "session",
		AggregateKey:  sessionKey,
		OccurredAt:    time.Now().UTC(),
		Payload:       payload,
	}})
}

// ReplayStream reads a stream back from the beginning.
//
// Replay is why the outbox is worth its cost: a consumer that was down, or a new
// one added later, can rebuild its view from the same events instead of asking
// every producer to resend.
func ReplayStream(ctx context.Context, store event.Store, sessionKey string, limit int) ([]event.Envelope, error) {
	result, err := store.Replay(ctx, event.ReplayQuery{
		Cursor: event.Cursor{TenantKey: "local", StreamKey: sessionKey},
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	return result.Events, nil
}
