package icoder

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/iceymoss/agent-runtime-go"
	agentcontext "github.com/iceymoss/agent-runtime-go/context"
)

// stepTokenUsage is the per-step slice of a run's actual token consumption, in
// step order. The aggregate usage a run already reports cannot answer the
// question the context planner needs answered - "how far off was the estimate
// for the request the plan actually described?" - because only the first step
// runs against the planned context; later steps grow with tool results.
type stepTokenUsage struct {
	Step  int         `json:"step"`
	Usage agent.Usage `json:"usage"`
}

func stepUsages(steps []agent.StepResult) []stepTokenUsage {
	if len(steps) == 0 {
		return nil
	}
	usages := make([]stepTokenUsage, 0, len(steps))
	for _, step := range steps {
		usages = append(usages, stepTokenUsage{Step: step.StepNumber, Usage: step.Usage})
	}
	return usages
}

// ContextReport pairs what the context planner predicted for one run with what
// the provider actually charged for it. It exists to make the conservative
// token estimator auditable: a persistent, large gap between Estimate and the
// first step's actual prompt tokens is the evidence that justifies plugging in
// a provider-precise tokenizer, and a gap in the other direction would mean the
// estimator is no longer conservative at all.
type ContextReport struct {
	RunKey          string                    `json:"run_key"`
	PlanDigest      string                    `json:"plan_digest"`
	SessionRevision uint64                    `json:"session_revision"`
	TokenizerID     string                    `json:"tokenizer_id"`
	Estimate        agentcontext.Estimate     `json:"estimate"`
	Budget          agentcontext.Budget       `json:"budget"`
	InputLimit      int                       `json:"input_limit"`
	Compacted       bool                      `json:"compacted"`
	CoveredThrough  uint64                    `json:"covered_through,omitempty"`
	Diagnostics     []agentcontext.Diagnostic `json:"diagnostics,omitempty"`
	// Outcome is empty while the run has not reached a terminal event, which
	// includes a run that was planned but never executed.
	Outcome    string       `json:"outcome,omitempty"`
	StopReason string       `json:"stop_reason,omitempty"`
	Usage      *agent.Usage `json:"usage,omitempty"`
	// StepPromptTokens holds each step's actual prompt tokens in step order;
	// index zero is the request the plan estimate predicted.
	StepPromptTokens []int `json:"step_prompt_tokens,omitempty"`
}

type contextPreparedPayload struct {
	RunKey          string                    `json:"run_key"`
	PlanDigest      string                    `json:"plan_digest"`
	SessionRevision uint64                    `json:"session_revision"`
	TokenizerID     string                    `json:"tokenizer_id"`
	Estimate        agentcontext.Estimate     `json:"estimate"`
	Budget          agentcontext.Budget       `json:"budget"`
	InputLimit      int                       `json:"input_limit"`
	Compacted       bool                      `json:"compacted"`
	CoveredThrough  uint64                    `json:"covered_through"`
	Diagnostics     []agentcontext.Diagnostic `json:"diagnostics"`
}

type runTerminalPayload struct {
	RunKey     string           `json:"run_key"`
	Outcome    string           `json:"outcome"`
	StopReason string           `json:"stop_reason"`
	Usage      agent.Usage      `json:"usage"`
	StepUsage  []stepTokenUsage `json:"step_usage"`
}

// ContextReports returns the active session's context reports, newest first.
// It reads the reliable event stream rather than Observations or the plan
// store: events survive process restarts, and the plan store is content
// addressed, which answers "is this exact plan intact" but not "what happened
// recently".
func (a *App) ContextReports(ctx context.Context, limit int) ([]ContextReport, error) {
	if limit <= 0 {
		limit = 10
	}
	sessionID := a.state.Get()
	byRun := make(map[string]*ContextReport)
	order := make([]string, 0)
	after := uint64(0)
	for {
		envelopes, err := a.store.ReplayEvents(ctx, sessionID, after, 1000)
		if err != nil {
			return nil, err
		}
		if len(envelopes) == 0 {
			break
		}
		for _, envelope := range envelopes {
			after = envelope.Sequence
			switch envelope.Type {
			case "agent.context.prepared":
				var payload contextPreparedPayload
				if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
					return nil, err
				}
				report := &ContextReport{
					RunKey: payload.RunKey, PlanDigest: payload.PlanDigest, SessionRevision: payload.SessionRevision,
					TokenizerID: payload.TokenizerID, Estimate: payload.Estimate, Budget: payload.Budget,
					InputLimit: payload.InputLimit, Compacted: payload.Compacted, CoveredThrough: payload.CoveredThrough,
					Diagnostics: payload.Diagnostics,
				}
				byRun[payload.RunKey] = report
				order = append(order, payload.RunKey)
			case "agent.run.completed", "agent.run.suspended", "agent.run.failed", "agent.run.canceled":
				var payload runTerminalPayload
				if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
					return nil, err
				}
				report, ok := byRun[payload.RunKey]
				if !ok {
					// Events written before this report existed carry no run key,
					// and a resumed attempt terminates a run whose plan was
					// recorded by the original attempt. Nothing to join.
					continue
				}
				outcome := payload.Outcome
				if outcome == "" {
					outcome = strings.TrimPrefix(envelope.Type, "agent.run.")
				}
				usage := payload.Usage
				report.Outcome, report.StopReason, report.Usage = outcome, payload.StopReason, &usage
				report.StepPromptTokens = report.StepPromptTokens[:0]
				for _, step := range payload.StepUsage {
					report.StepPromptTokens = append(report.StepPromptTokens, step.Usage.PromptTokens)
				}
			}
		}
	}
	reports := make([]ContextReport, 0, limit)
	for i := len(order) - 1; i >= 0 && len(reports) < limit; i-- {
		reports = append(reports, *byRun[order[i]])
	}
	return reports, nil
}
