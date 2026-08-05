package durable

import (
	"context"
	"reflect"
	"sort"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type EffectStatus string

const (
	EffectPrepared  EffectStatus = "prepared"
	EffectRunning   EffectStatus = "running"
	EffectSucceeded EffectStatus = "succeeded"
	EffectFailed    EffectStatus = "failed"
	EffectUnknown   EffectStatus = "unknown"
)

type EffectFailure struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type EffectRecord struct {
	ExecutionKey ExecutionKey      `json:"execution_key"`
	Digest       string            `json:"digest"`
	RunKey       RunKey            `json:"run_key"`
	AttemptKey   AttemptKey        `json:"attempt_key"`
	StepNumber   int               `json:"step_number"`
	Ordinal      int               `json:"ordinal"`
	ToolCall     agent.ToolCall    `json:"tool_call"`
	Status       EffectStatus      `json:"status"`
	FenceToken   uint64            `json:"fence_token,omitempty"`
	Result       *agent.ToolResult `json:"result,omitempty"`
	Failure      *EffectFailure    `json:"failure,omitempty"`
	PreparedAt   time.Time         `json:"prepared_at"`
	StartedAt    time.Time         `json:"started_at,omitempty"`
	FinishedAt   time.Time         `json:"finished_at,omitempty"`
}

type PrepareEffectRequest struct {
	Guard      Guard
	AttemptKey AttemptKey
	StepNumber int
	Ordinal    int
	ToolCall   agent.ToolCall
	PreparedAt time.Time
}

type CompleteEffectRequest struct {
	Guard        Guard
	ExecutionKey ExecutionKey
	Result       *agent.ToolResult
	Failure      *EffectFailure
	FinishedAt   time.Time
}

type ExecutionLedger interface {
	PrepareEffect(context.Context, PrepareEffectRequest) (EffectRecord, bool, error)
	BeginEffect(context.Context, Guard, ExecutionKey, time.Time) (EffectRecord, error)
	CompleteEffect(context.Context, CompleteEffectRequest) (EffectRecord, error)
	MarkEffectUnknown(context.Context, Guard, ExecutionKey, time.Time) (EffectRecord, error)
	LoadEffect(context.Context, ExecutionKey) (EffectRecord, error)
	ListEffects(context.Context, RunKey) ([]EffectRecord, error)
}

type UsageFact struct {
	TenantKey        TenantKey    `json:"tenant_key"`
	UsageKey         UsageKey     `json:"usage_key"`
	RunKey           RunKey       `json:"run_key"`
	AttemptKey       AttemptKey   `json:"attempt_key"`
	ExecutionKey     ExecutionKey `json:"execution_key,omitempty"`
	Kind             string       `json:"kind"`
	Provider         string       `json:"provider"`
	Model            string       `json:"model"`
	PricingVersion   string       `json:"pricing_version"`
	InputTokens      int64        `json:"input_tokens"`
	OutputTokens     int64        `json:"output_tokens"`
	CacheReadTokens  int64        `json:"cache_read_tokens"`
	CacheWriteTokens int64        `json:"cache_write_tokens"`
	CostMicros       int64        `json:"cost_micros"`
	Currency         string       `json:"currency"`
	OccurredAt       time.Time    `json:"occurred_at"`
}

type UsageLedger interface {
	RecordUsage(context.Context, UsageFact) (UsageFact, bool, error)
	LoadUsage(context.Context, TenantKey, UsageKey) (UsageFact, error)
	ListAttemptUsage(context.Context, TenantKey, AttemptKey) ([]UsageFact, error)
}

func newEffectRecord(snapshot Snapshot, request PrepareEffectRequest) (EffectRecord, error) {
	inputDigest, err := DigestToolInput(request.ToolCall.Input)
	if err != nil {
		return EffectRecord{}, err
	}
	key := ToolExecutionKey(snapshot.Identity, request.StepNumber, request.Ordinal, request.ToolCall)
	record := EffectRecord{
		ExecutionKey: key, RunKey: RunKey(snapshot.Identity.RunKey), AttemptKey: request.AttemptKey,
		StepNumber: request.StepNumber, Ordinal: request.Ordinal, ToolCall: request.ToolCall,
		Status: EffectPrepared, PreparedAt: request.PreparedAt,
	}
	digestValue := struct {
		ExecutionKey ExecutionKey   `json:"execution_key"`
		RunKey       RunKey         `json:"run_key"`
		AttemptKey   AttemptKey     `json:"attempt_key"`
		StepNumber   int            `json:"step_number"`
		Ordinal      int            `json:"ordinal"`
		ToolCall     agent.ToolCall `json:"tool_call"`
		InputDigest  string         `json:"input_digest"`
	}{key, record.RunKey, record.AttemptKey, record.StepNumber, record.Ordinal, record.ToolCall, inputDigest}
	record.Digest, err = CanonicalDigest(digestValue)
	return record, err
}

func cloneEffect(record EffectRecord) EffectRecord {
	cloned := record
	if record.Result != nil {
		result := *record.Result
		cloned.Result = &result
	}
	if record.Failure != nil {
		failure := *record.Failure
		cloned.Failure = &failure
	}
	return cloned
}

func samePreparedEffect(left, right EffectRecord) bool {
	left.Status, right.Status = EffectPrepared, EffectPrepared
	left.FenceToken, right.FenceToken = 0, 0
	left.Result, right.Result, left.Failure, right.Failure = nil, nil, nil, nil
	left.StartedAt, right.StartedAt, left.FinishedAt, right.FinishedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

func validateUsage(fact UsageFact) error {
	if fact.TenantKey == "" || fact.UsageKey == "" || fact.RunKey == "" || fact.AttemptKey == "" || fact.Kind == "" || fact.Provider == "" || fact.Model == "" || fact.PricingVersion == "" || fact.Currency == "" || fact.OccurredAt.IsZero() {
		return durableError(ErrUsageConflict, "record usage", fact.RunKey, "missing immutable usage field")
	}
	if fact.InputTokens < 0 || fact.OutputTokens < 0 || fact.CacheReadTokens < 0 || fact.CacheWriteTokens < 0 || fact.CostMicros < 0 {
		return durableError(ErrUsageConflict, "record usage", fact.RunKey, "negative usage delta")
	}
	return nil
}

func sortEffects(records []EffectRecord) {
	sort.Slice(records, func(i, j int) bool { return records[i].ExecutionKey < records[j].ExecutionKey })
}
