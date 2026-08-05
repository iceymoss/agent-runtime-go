package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestCanonicalDigestIsDeterministic(t *testing.T) {
	left := map[string]any{"b": []any{float64(2), "x"}, "a": map[string]any{"z": true, "n": float64(1)}}
	right := map[string]any{"a": map[string]any{"n": float64(1), "z": true}, "b": []any{float64(2), "x"}}

	leftDigest, err := CanonicalDigest(left)
	if err != nil {
		t.Fatalf("CanonicalDigest(left) error = %v", err)
	}
	rightDigest, err := CanonicalDigest(right)
	if err != nil {
		t.Fatalf("CanonicalDigest(right) error = %v", err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("digests differ: %q != %q", leftDigest, rightDigest)
	}
	if leftDigest != "sha256:dbfef76b43cf2665f887d9d09a2b50350dbf9b0c128078a606051d49084e1ff3" {
		t.Fatalf("digest = %q, want stable fixture", leftDigest)
	}
}

func TestImmutableDigestsAndToolKeyAreStable(t *testing.T) {
	input := ImmutableRunInput{Messages: []Message{NewSystemMessage("system"), NewUserMessage("hello")}}
	config := ImmutableRunConfig{
		AgentKey:      "assistant.support",
		ModelName:     "model-v1",
		MaxSteps:      4,
		PromptVersion: "prompt-v2",
		PolicyVersion: "policy-v1",
		Tools: []ToolDefinition{{Name: "score", Description: "score reply", Parameters: map[string]any{
			"required": []any{"text"}, "type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}},
		}}},
	}

	inputDigest, err := DigestRunInput(input)
	if err != nil {
		t.Fatalf("DigestRunInput() error = %v", err)
	}
	configDigest, err := DigestRunConfig(config)
	if err != nil {
		t.Fatalf("DigestRunConfig() error = %v", err)
	}
	inputHash, err := DigestToolInput(` { "b": 2, "a": 1 } `)
	if err != nil {
		t.Fatalf("DigestToolInput() error = %v", err)
	}
	otherInputHash, err := DigestToolInput(`{"a":1,"b":2}`)
	if err != nil {
		t.Fatalf("DigestToolInput() second error = %v", err)
	}
	if inputHash != otherInputHash {
		t.Fatalf("canonical input hashes differ: %q != %q", inputHash, otherInputHash)
	}
	if inputDigest != "sha256:1d2e8fad0e9e21b419b6f0f1a23026504d5cb0beaa202c9a9062e421004bf170" {
		t.Errorf("input digest = %q, want stable fixture", inputDigest)
	}
	if configDigest != "sha256:0b106bb18ab80f24f815f10ac296c7a3431efd0443146c6c298d55bc149eacdb" {
		t.Errorf("config digest = %q, want stable fixture", configDigest)
	}
	if inputHash != "sha256:43258cff783fe7036d8a43033f830adfc60ec037382473548ac742b888292777" {
		t.Errorf("valid tool input digest = %q, want stable fixture", inputHash)
	}
	malformedInputHash, err := DigestToolInput(`{"score":`)
	if err != nil {
		t.Fatalf("DigestToolInput(malformed) error = %v", err)
	}
	if malformedInputHash != "sha256:0d0b2d4e55c0d0a262eb21cb1489a2f48d2482c50053497f4a3aacd2bdfb143b" {
		t.Errorf("invalid tool input digest = %q, want stable fixture", malformedInputHash)
	}

	identity := RunIdentity{RunKey: "run-123", AgentKey: "assistant.support", SessionID: "session-7", RequestID: "request-9"}
	key := ToolExecutionKey(identity, 2, 1, ToolCall{ID: "call-4", Name: "score", Input: `{"a":1,"b":2}`})
	keyAgain := ToolExecutionKey(identity, 2, 1, ToolCall{ID: "call-4", Name: "score", Input: ` { "b": 2, "a": 1 } `})
	if key != keyAgain {
		t.Fatalf("idempotency keys differ: %q != %q", key, keyAgain)
	}
	if key != "tool:a6fe12335d4ca867f31c23a04b960ffb5c0bdafe6f3607824e90c0a54a32669b" {
		t.Fatalf("idempotency key = %q, want stable fixture", key)
	}
	execution, err := NewToolExecution(identity, 2, 1, ToolCall{ID: "call-4", Name: "score", Input: `{"a":1,"b":2}`})
	if err != nil {
		t.Fatalf("NewToolExecution() error = %v", err)
	}
	if execution.Status != ToolExecutionPrepared || execution.IdempotencyKey != key || execution.InputHash != inputHash {
		t.Fatalf("NewToolExecution() = %#v", execution)
	}
	if inputDigest == configDigest || key == "" {
		t.Fatalf("unexpected digests input=%q config=%q key=%q", inputDigest, configDigest, key)
	}
}

func TestRunSnapshotV1ExactFixtures(t *testing.T) {
	const fixturePrefix = `{"schema_version":1,"identity":{"run_key":"run-fixture","agent_key":"agent-fixture","session_id":"session-fixture","request_id":"request-fixture"},"input_digest":"sha256:input","config_digest":"sha256:config","status":"`
	const fixtureMiddle = `","phase":"`
	const fixtureSuffix = `","revision":0,"fence_token":0,"lease_until":"0001-01-01T00:00:00Z","checkpoint":{"history":null,"new_messages":null,"completed_steps":null,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"repair_count":0,"next_step":0,"pending_tool_calls":null,"outcome":{"messages":null,"steps":null,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"text":"","stop_reason":"","outcome":""}}}`

	states := []struct {
		name   string
		status RunStatus
		phase  RunPhase
	}{
		{name: "claimed model ready", status: RunStatusClaimed, phase: RunPhaseModelReady},
		{name: "running model ready", status: RunStatusRunning, phase: RunPhaseModelReady},
		{name: "running model inflight", status: RunStatusRunning, phase: RunPhaseModelInflight},
		{name: "running tools ready", status: RunStatusRunning, phase: RunPhaseToolsReady},
		{name: "running tool inflight", status: RunStatusRunning, phase: RunPhaseToolInflight},
		{name: "running finalizing", status: RunStatusRunning, phase: RunPhaseFinalizing},
		{name: "suspended model ready", status: RunStatusSuspended, phase: RunPhaseModelReady},
		{name: "suspended tools ready", status: RunStatusSuspended, phase: RunPhaseToolsReady},
		{name: "completed terminal", status: RunStatusCompleted, phase: RunPhaseTerminal},
		{name: "failed terminal", status: RunStatusFailed, phase: RunPhaseTerminal},
		{name: "abandoned terminal", status: RunStatusAbandoned, phase: RunPhaseTerminal},
	}
	for _, tt := range states {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := emptyFixtureRunSnapshot(tt.status, tt.phase)
			want := []byte(fixturePrefix + string(tt.status) + fixtureMiddle + string(tt.phase) + fixtureSuffix)
			assertRunSnapshotFixture(t, snapshot, want)
		})
	}

	t.Run("populated unicode ordered tools and empty slices", func(t *testing.T) {
		snapshot := emptyFixtureRunSnapshot(RunStatusRunning, RunPhaseToolsReady)
		snapshot.Revision = 9
		snapshot.FenceToken = 4
		snapshot.LeaseOwner = "worker-北京"
		snapshot.LeaseUntil = time.Date(2026, 8, 1, 1, 2, 3, 456000000, time.FixedZone("fixture", 8*60*60))
		snapshot.Checkpoint.History = []Message{NewUserMessage("你好")}
		snapshot.Checkpoint.NewMessages = []Message{}
		snapshot.Checkpoint.CompletedSteps = []StepResult{}
		snapshot.Checkpoint.PendingToolCalls = []ToolCall{
			{ID: "call-一", Name: "first", Input: `{"value":1}`},
			{ID: "call-二", Name: "second", Input: `{"value":2}`},
		}
		snapshot.Checkpoint.Outcome.Messages = []Message{}
		snapshot.Checkpoint.Outcome.Steps = []StepResult{}
		want := []byte(`{"schema_version":1,"identity":{"run_key":"run-fixture","agent_key":"agent-fixture","session_id":"session-fixture","request_id":"request-fixture"},"input_digest":"sha256:input","config_digest":"sha256:config","status":"running","phase":"tools_ready","revision":9,"fence_token":4,"lease_owner":"worker-北京","lease_until":"2026-08-01T01:02:03.456+08:00","checkpoint":{"history":[{"role":"user","parts":[{"type":"text","text":"你好"}]}],"new_messages":[],"completed_steps":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"repair_count":0,"next_step":0,"pending_tool_calls":[{"id":"call-一","name":"first","input":"{\"value\":1}"},{"id":"call-二","name":"second","input":"{\"value\":2}"}],"outcome":{"messages":[],"steps":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0},"text":"","stop_reason":"","outcome":""}}}`)
		assertRunSnapshotFixture(t, snapshot, want)
	})
}

func TestToolExecutionV1ExactFixtures(t *testing.T) {
	statuses := []struct {
		status ToolExecutionStatus
		result *ToolResult
		want   string
	}{
		{status: ToolExecutionPrepared, want: `{"run_key":"run-工具","step_number":2,"ordinal":1,"tool_call":{"id":"call-1","name":"lookup","input":"{\"城市\":\"北京\"}"},"idempotency_key":"tool:key","input_hash":"sha256:input","status":"prepared"}`},
		{status: ToolExecutionExecuting, want: `{"run_key":"run-工具","step_number":2,"ordinal":1,"tool_call":{"id":"call-1","name":"lookup","input":"{\"城市\":\"北京\"}"},"idempotency_key":"tool:key","input_hash":"sha256:input","status":"executing"}`},
		{status: ToolExecutionCompleted, result: &ToolResult{ToolCallID: "call-1", Name: "lookup", Content: "晴天"}, want: `{"run_key":"run-工具","step_number":2,"ordinal":1,"tool_call":{"id":"call-1","name":"lookup","input":"{\"城市\":\"北京\"}"},"idempotency_key":"tool:key","input_hash":"sha256:input","status":"completed","result":{"tool_call_id":"call-1","name":"lookup","content":"晴天"}}`},
		{status: ToolExecutionUnknown, want: `{"run_key":"run-工具","step_number":2,"ordinal":1,"tool_call":{"id":"call-1","name":"lookup","input":"{\"城市\":\"北京\"}"},"idempotency_key":"tool:key","input_hash":"sha256:input","status":"unknown"}`},
	}
	for _, tt := range statuses {
		t.Run(string(tt.status), func(t *testing.T) {
			execution := ToolExecution{
				RunKey:         "run-工具",
				StepNumber:     2,
				Ordinal:        1,
				ToolCall:       ToolCall{ID: "call-1", Name: "lookup", Input: `{"城市":"北京"}`},
				IdempotencyKey: "tool:key",
				InputHash:      "sha256:input",
				Status:         tt.status,
				Result:         tt.result,
			}
			got, err := json.Marshal(execution)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if !bytes.Equal(got, []byte(tt.want)) {
				t.Fatalf("Marshal() = %s\nwant      = %s", got, tt.want)
			}
		})
	}
}

func TestDigestToolInputCanonicalizesValidAndPreservesMalformedRawInput(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		equal bool
	}{
		{name: "empty means object", left: "", right: `{}`, equal: true},
		{name: "valid whitespace", left: "  {} \n", right: `{}`, equal: true},
		{name: "valid key order", left: `{"a":1,"b":2}`, right: ` { "b":2, "a":1 } `, equal: true},
		{name: "malformed exact bytes", left: `{"a":`, right: `{"a":`, equal: true},
		{name: "malformed whitespace preserved", left: `{"a":`, right: ` {"a":`, equal: false},
		{name: "whitespace-only raw", left: " ", right: "  ", equal: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left, err := DigestToolInput(tt.left)
			if err != nil {
				t.Fatal(err)
			}
			right, err := DigestToolInput(tt.right)
			if err != nil {
				t.Fatal(err)
			}
			if (left == right) != tt.equal {
				t.Fatalf("digests = %q, %q, want equal %v", left, right, tt.equal)
			}
		})
	}

	identity := RunIdentity{RunKey: "run", AgentKey: "agent"}
	call := ToolCall{ID: "bad", Name: "score", Input: `{"score":`}
	execution, err := NewToolExecution(identity, 1, 0, call)
	if err != nil {
		t.Fatalf("NewToolExecution() error = %v", err)
	}
	if execution.InputHash != digestBytes([]byte(call.Input)) || execution.IdempotencyKey != ToolExecutionKey(identity, 1, 0, call) || execution.ToolCall.Input != call.Input || execution.Status != ToolExecutionPrepared {
		t.Fatalf("execution = %#v", execution)
	}
}

func TestRunSnapshotJSONRoundTripAndSchemaRejection(t *testing.T) {
	snapshot := testRunSnapshot()
	data, err := MarshalRunSnapshot(snapshot)
	if err != nil {
		t.Fatalf("MarshalRunSnapshot() error = %v", err)
	}
	got, err := UnmarshalRunSnapshot(data)
	if err != nil {
		t.Fatalf("UnmarshalRunSnapshot() error = %v", err)
	}
	if !reflect.DeepEqual(got, snapshot) {
		t.Fatalf("round trip = %#v, want %#v", got, snapshot)
	}

	for _, data := range [][]byte{
		[]byte(`{"schema_version":0}`),
		[]byte(`{"schema_version":2}`),
	} {
		if _, err := UnmarshalRunSnapshot(data); !errors.Is(err, ErrUnsupportedRunSnapshotSchema) {
			t.Fatalf("UnmarshalRunSnapshot(%s) error = %v, want schema error", data, err)
		}
	}
}

func TestRunSnapshotCloneHasNoAliases(t *testing.T) {
	original := testRunSnapshot()
	cloned := original.Clone()

	cloned.Checkpoint.History[0].Parts[0].Text = "changed"
	cloned.Checkpoint.NewMessages[0].Parts[0].Text = "changed"
	cloned.Checkpoint.CompletedSteps[0].ToolCalls[0].Input = "changed"
	cloned.Checkpoint.PendingToolCalls[0].Input = "changed"
	cloned.Checkpoint.Outcome.Text = "changed"

	if original.Checkpoint.History[0].Text() != "hello" ||
		original.Checkpoint.NewMessages[0].Text() != "answer" ||
		original.Checkpoint.CompletedSteps[0].ToolCalls[0].Input != `{"value":1}` ||
		original.Checkpoint.PendingToolCalls[0].Input != `{"value":2}` ||
		original.Checkpoint.Outcome.Text != "answer" {
		t.Fatalf("Clone() aliases original: %#v", original.Checkpoint)
	}
}

func TestValidateRunTransition(t *testing.T) {
	base := testRunSnapshot()
	base.Status = RunStatusClaimed
	base.Phase = RunPhaseModelReady

	tests := []struct {
		name string
		from RunSnapshot
		to   RunSnapshot
		ok   bool
	}{
		{name: "acquire", from: base, to: withRunState(base, RunStatusRunning, RunPhaseModelInflight), ok: true},
		{name: "commit model tools", from: withRunState(base, RunStatusRunning, RunPhaseModelInflight), to: withRunState(base, RunStatusRunning, RunPhaseToolsReady), ok: true},
		{name: "begin tool", from: withRunState(base, RunStatusRunning, RunPhaseToolsReady), to: withRunState(base, RunStatusRunning, RunPhaseToolInflight), ok: true},
		{name: "complete", from: withRunState(base, RunStatusRunning, RunPhaseFinalizing), to: withRunState(base, RunStatusCompleted, RunPhaseTerminal), ok: true},
		{name: "cannot skip tool preparation", from: withRunState(base, RunStatusRunning, RunPhaseModelReady), to: withRunState(base, RunStatusRunning, RunPhaseToolInflight)},
		{name: "cannot complete before finalizing", from: withRunState(base, RunStatusRunning, RunPhaseModelReady), to: withRunState(base, RunStatusCompleted, RunPhaseTerminal)},
		{name: "cannot return to claimed", from: withRunState(base, RunStatusRunning, RunPhaseModelReady), to: withRunState(base, RunStatusClaimed, RunPhaseModelReady)},
		{name: "terminal is immutable", from: withRunState(base, RunStatusCompleted, RunPhaseTerminal), to: withRunState(base, RunStatusRunning, RunPhaseModelReady)},
		{name: "suspended cannot be inflight", from: withRunState(base, RunStatusRunning, RunPhaseToolsReady), to: withRunState(base, RunStatusSuspended, RunPhaseToolInflight)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.to.Revision = tt.from.Revision + 1
			err := ValidateRunTransition(tt.from, tt.to)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidateRunTransition() error = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestToolExecutionContextExposesOnlyValidKey(t *testing.T) {
	ctx := context.Background()
	if _, ok := ToolExecutionKeyFromContext(ctx); ok {
		t.Fatal("empty context unexpectedly contains a key")
	}
	if got := WithToolExecutionKey(ctx, ""); got != ctx {
		t.Fatal("empty key should not decorate context")
	}
	ctx = WithToolExecutionKey(ctx, "tool:abc")
	if key, ok := ToolExecutionKeyFromContext(ctx); !ok || key != "tool:abc" {
		t.Fatalf("ToolExecutionKeyFromContext() = %q, %v", key, ok)
	}
}

func TestReplayPolicyNormalizationFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		policy ReplayPolicy
		want   ReplayPolicy
	}{
		{policy: ReplayPolicyNever, want: ReplayPolicyNever},
		{policy: ReplayPolicyIdempotent, want: ReplayPolicyIdempotent},
		{policy: ReplayPolicyResolve, want: ReplayPolicyResolve},
		{policy: "", want: ReplayPolicyNever},
		{policy: "unknown", want: ReplayPolicyNever},
	} {
		if got := normalizeReplayPolicy(tt.policy); got != tt.want {
			t.Fatalf("normalizeReplayPolicy(%q) = %q, want %q", tt.policy, got, tt.want)
		}
	}
}

type recoveryTestTool struct{}

func (recoveryTestTool) Definition() ToolDefinition { return ToolDefinition{Name: "regular"} }
func (recoveryTestTool) ReplayPolicy() ReplayPolicy { return ReplayPolicyNever }
func (recoveryTestTool) Execute(context.Context, ToolInvocation) (ToolResult, error) {
	return ToolResult{}, nil
}

func emptyFixtureRunSnapshot(status RunStatus, phase RunPhase) RunSnapshot {
	return RunSnapshot{
		SchemaVersion: RunSnapshotSchemaVersion,
		Identity: RunIdentity{
			RunKey:    "run-fixture",
			AgentKey:  "agent-fixture",
			SessionID: "session-fixture",
			RequestID: "request-fixture",
		},
		InputDigest:  "sha256:input",
		ConfigDigest: "sha256:config",
		Status:       status,
		Phase:        phase,
	}
}

func assertRunSnapshotFixture(t *testing.T, snapshot RunSnapshot, want []byte) {
	t.Helper()
	got, err := MarshalRunSnapshot(snapshot)
	if err != nil {
		t.Fatalf("MarshalRunSnapshot() error = %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("MarshalRunSnapshot() = %s\nwant                 = %s", got, want)
	}
	decoded, err := UnmarshalRunSnapshot(want)
	if err != nil {
		t.Fatalf("UnmarshalRunSnapshot() error = %v", err)
	}
	roundTrip, err := MarshalRunSnapshot(decoded)
	if err != nil {
		t.Fatalf("MarshalRunSnapshot(round trip) error = %v", err)
	}
	if !bytes.Equal(roundTrip, want) {
		t.Fatalf("round trip = %s\nwant       = %s", roundTrip, want)
	}
}

func testRunSnapshot() RunSnapshot {
	call := ToolCall{ID: "call-1", Name: "lookup", Input: `{"value":1}`}
	return RunSnapshot{
		SchemaVersion: RunSnapshotSchemaVersion,
		Identity:      RunIdentity{RunKey: "run-1", AgentKey: "agent", SessionID: "session", RequestID: "request"},
		InputDigest:   "sha256:input",
		ConfigDigest:  "sha256:config",
		Status:        RunStatusRunning,
		Phase:         RunPhaseToolsReady,
		Revision:      3,
		FenceToken:    2,
		LeaseOwner:    "worker-1",
		LeaseUntil:    time.Date(2026, 8, 1, 1, 2, 3, 0, time.UTC),
		Checkpoint: Checkpoint{
			History:          []Message{NewUserMessage("hello")},
			NewMessages:      []Message{NewAssistantMessage("answer")},
			CompletedSteps:   []StepResult{{StepNumber: 0, Message: NewAssistantMessage("step"), ToolCalls: []ToolCall{call}, ToolResults: []ToolResult{{ToolCallID: "call-1", Name: "lookup", Content: "ok"}}}},
			Usage:            Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
			RepairCount:      1,
			NextStep:         1,
			PendingToolCalls: []ToolCall{{ID: "call-2", Name: "lookup", Input: `{"value":2}`}},
			Outcome:          RunResult{Messages: []Message{NewAssistantMessage("answer")}, Text: "answer", Outcome: OutcomeSuspended},
		},
	}
}

func withRunState(snapshot RunSnapshot, status RunStatus, phase RunPhase) RunSnapshot {
	snapshot.Status = status
	snapshot.Phase = phase
	return snapshot
}
