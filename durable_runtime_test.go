package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRunDurableCompletedRetrySkipsProvider(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "stored", usage: Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}}}
	a := newTestAgent(t, Config{Key: "runtime-test", ModelName: "test", MaxSteps: 4}, model)
	store := newRuntimeMemoryStore()
	request := durableRuntimeRequest(store, "completed-retry")

	first, err := a.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if first.DurableCompletion == nil {
		t.Fatal("first Run() did not return a completion guard")
	}
	if _, err := store.Complete(context.Background(), first.DurableCompletion.Guard, first.DurableCompletion.Checkpoint); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	retry, err := a.Run(context.Background(), request)
	if err != nil || retry.Text != "stored" || model.calls != 1 {
		t.Fatalf("retry = %+v, calls = %d, error = %v", retry, model.calls, err)
	}
}

func TestRunDurableRestoresExactCheckpointAndTransitionsBeforeModel(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "done", usage: Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}}}
	a := newTestAgent(t, Config{Key: "runtime-test", ModelName: "test", MaxSteps: 12}, model)
	store := newRuntimeMemoryStore()
	request := durableRuntimeRequest(store, "restore")
	checkpoint := Checkpoint{
		History:        []Message{NewSystemMessage("persisted"), NewUserMessage("input"), NewAssistantMessage("old")},
		NewMessages:    []Message{NewAssistantMessage("old")},
		CompletedSteps: []StepResult{{StepNumber: 7, Message: NewAssistantMessage("old")}},
		Usage:          Usage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14},
		RepairCount:    1,
		NextStep:       8,
	}
	inputDigest, configDigest, err := a.durableDigests(request)
	if err != nil {
		t.Fatal(err)
	}
	store.snapshot = RunSnapshot{SchemaVersion: RunSnapshotSchemaVersion, Identity: request.DurableRun.Identity, InputDigest: inputDigest, ConfigDigest: configDigest, Status: RunStatusSuspended, Phase: RunPhaseModelReady, Revision: 4, FenceToken: 2, Checkpoint: checkpoint}

	result, err := a.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if model.lastReq == nil || len(model.lastReq.Messages) != len(checkpoint.History) {
		t.Fatalf("model history = %+v", model.lastReq)
	}
	if result.Steps[0].StepNumber != 7 || result.Steps[1].StepNumber != 8 || result.Usage.PromptTokens != 12 {
		t.Fatalf("restored result = %+v", result)
	}
	if len(store.transitions) < 2 || store.transitions[0] != "acquire" || store.transitions[1] != "model_inflight" {
		t.Fatalf("transitions = %v", store.transitions)
	}
}

func TestRunOrdinaryAndDurableInvalidToolInputParity(t *testing.T) {
	definition := ToolDefinition{Name: "score", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"score": map[string]any{"type": "integer"}}, "required": []any{"score"},
	}, Strict: true}
	for _, input := range []string{`{"score":`, `{"score":"bad"}`, ""} {
		t.Run(input, func(t *testing.T) {
			ordinaryTool := &schemaTool{definition: definition}
			ordinaryModel := &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "bad", Name: "score", Input: input}}}, {text: "corrected"}}}
			ordinary := newTestAgent(t, Config{Key: "runtime-test", ModelName: "test", MaxSteps: 4, ToolRepairLimit: 1}, ordinaryModel, ordinaryTool)
			ordinaryResult, ordinaryErr := ordinary.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("input")}})

			durableTool := &schemaTool{definition: definition}
			durableModel := &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "bad", Name: "score", Input: input}}}, {text: "corrected"}}}
			durable := newTestAgent(t, Config{Key: "runtime-test", ModelName: "test", MaxSteps: 4, ToolRepairLimit: 1}, durableModel, durableTool)
			store := newRuntimeMemoryStore()
			durableResult, durableErr := durable.Run(context.Background(), durableRuntimeRequest(store, "repair-"+input))

			if ordinaryErr != nil || durableErr != nil || ordinaryResult.Outcome != OutcomeCompleted || durableResult.Outcome != OutcomeCompleted {
				t.Fatalf("ordinary = %+v/%v, durable = %+v/%v", ordinaryResult, ordinaryErr, durableResult, durableErr)
			}
			ordinaryRepair := ordinaryResult.Steps[0].ToolResults
			durableRepair := durableResult.Steps[0].ToolResults
			if !reflect.DeepEqual(ordinaryRepair, durableRepair) || len(durableRepair) != 1 || !durableRepair[0].IsError || durableRepair[0].StopTurn {
				t.Fatalf("repair results = ordinary %#v, durable %#v", ordinaryRepair, durableRepair)
			}
			if ordinaryTool.calls != 0 || durableTool.calls != 0 || store.snapshot.Checkpoint.RepairCount != 1 || len(store.executions) != 1 {
				t.Fatalf("tool calls = %d/%d, repair count = %d, ledger = %#v", ordinaryTool.calls, durableTool.calls, store.snapshot.Checkpoint.RepairCount, store.executions)
			}
			for _, execution := range store.executions {
				if execution.Status != ToolExecutionCompleted || execution.Result == nil || !reflect.DeepEqual(*execution.Result, durableRepair[0]) {
					t.Fatalf("execution = %#v", execution)
				}
			}
		})
	}
}

func TestRunDurableReplayPolicyAndInvocation(t *testing.T) {
	for _, tt := range []struct {
		policy ReplayPolicy
		want   bool
	}{
		{policy: ReplayPolicyNever},
		{policy: ReplayPolicyIdempotent, want: true},
		{policy: ReplayPolicyResolve},
	} {
		t.Run(string(tt.policy), func(t *testing.T) {
			tool := &fakeTool{name: "capture", replayPolicy: tt.policy}
			model := &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "call", Name: "capture", Input: `{}`}}}, {text: "done"}}}
			a := newTestAgent(t, Config{Key: "runtime-test", MaxSteps: 4}, model, tool)
			store := newRuntimeMemoryStore()
			if _, err := a.Run(context.Background(), durableRuntimeRequest(store, "policy-"+string(tt.policy))); err != nil {
				t.Fatal(err)
			}
			if len(store.safeReplay) != 1 || store.safeReplay[0] != tt.want {
				t.Fatalf("safe replay = %v, want %v", store.safeReplay, tt.want)
			}
			if tool.lastInvocation.ExecutionKey == "" || tool.lastInvocation.ExecutionKey != ToolExecutionKey(store.snapshot.Identity, 0, 0, ToolCall{ID: "call", Name: "capture", Input: `{}`}) {
				t.Fatalf("invocation = %+v", tool.lastInvocation)
			}
		})
	}
}

func TestRunDurableToolErrorDisposition(t *testing.T) {
	for _, tt := range []struct {
		name      string
		cause     error
		suspended bool
	}{
		{name: "fatal", cause: errors.New("tool infrastructure failed")},
		{name: "canceled", cause: context.Canceled, suspended: true},
		{name: "deadline", cause: context.DeadlineExceeded, suspended: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tool := &fakeTool{name: "fail", execErr: tt.cause}
			model := &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "call", Name: "fail", Input: `{}`}}}}}
			a := newTestAgent(t, Config{Key: "runtime-test", MaxSteps: 4}, model, tool)
			store := newRuntimeMemoryStore()
			result, err := a.Run(context.Background(), durableRuntimeRequest(store, "error-"+tt.name))
			if !errors.Is(err, tt.cause) || (store.snapshot.Status == RunStatusSuspended) != tt.suspended {
				t.Fatalf("result/error/snapshot = %+v/%v/%+v", result, err, store.snapshot)
			}
			for _, execution := range store.executions {
				if execution.Status == ToolExecutionCompleted || execution.Result != nil {
					t.Fatalf("fatal execution committed: %+v", execution)
				}
			}
		})
	}
}

func TestRunDurableToolSuspensionResumesWithoutRootEffectBoundary(t *testing.T) {
	tool := &durableSuspendingTool{}
	model := &fakeModel{steps: []scriptedStep{{calls: []ToolCall{{ID: "call", Name: "approve", Input: `{}`}}}, {text: "done"}}}
	a := newTestAgent(t, Config{Key: "runtime-test", MaxSteps: 4}, model, tool)
	store := newRuntimeMemoryStore()
	request := durableRuntimeRequest(store, "tool-suspension")

	first, err := a.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if first.Outcome != OutcomeSuspended || first.StopReason != StopReasonToolSuspended || first.Suspension == nil || first.Suspension.Tool == nil {
		t.Fatalf("first result = %+v, want tool suspension", first)
	}
	if store.snapshot.Status != RunStatusSuspended || len(store.safeReplay) != 0 || tool.calls != 1 {
		t.Fatalf("snapshot=%+v root begins=%v tool calls=%d", store.snapshot, store.safeReplay, tool.calls)
	}
	wakeup, err := a.Run(context.Background(), request)
	if err != nil || wakeup.Outcome != OutcomeSuspended || tool.calls != 1 || model.calls != 1 {
		t.Fatalf("unresolved Run() = %+v, %v, tool calls=%d model calls=%d", wakeup, err, tool.calls, model.calls)
	}
	wrong := *first.Suspension.Tool
	wrong.ResumeToken = "wrong"
	request.DurableRun.ToolResume = &wrong
	if _, err := a.Run(context.Background(), request); !errors.Is(err, ErrAgentConfigInvalid) || tool.calls != 1 || model.calls != 1 {
		t.Fatalf("mismatched Run() error=%v tool calls=%d model calls=%d", err, tool.calls, model.calls)
	}

	resume := *first.Suspension.Tool
	request.DurableRun.ToolResume = &resume
	second, err := a.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("resumed Run() error = %v", err)
	}
	if second.Outcome != OutcomeCompleted || second.Text != "done" || tool.calls != 2 || len(store.safeReplay) != 0 || model.calls != 2 {
		t.Fatalf("resumed result=%+v tool calls=%d root begins=%v model calls=%d", second, tool.calls, store.safeReplay, model.calls)
	}
}

type durableSuspendingTool struct {
	calls int
}

func (*durableSuspendingTool) Definition() ToolDefinition {
	return ToolDefinition{Name: "approve", Strict: true, Parameters: map[string]any{"type": "object", "additionalProperties": false}}
}
func (*durableSuspendingTool) ReplayPolicy() ReplayPolicy { return ReplayPolicyNever }
func (*durableSuspendingTool) ExecutableVersion() string  { return "v1" }
func (*durableSuspendingTool) OwnsToolExecutionLifecycle() bool {
	return true
}
func (t *durableSuspendingTool) Execute(_ context.Context, invocation ToolInvocation) (ToolResult, error) {
	t.calls++
	if invocation.Resume == nil {
		return ToolResult{}, &ToolSuspensionError{Suspension: ToolSuspension{Kind: ToolSuspensionApproval, ExecutionKey: "advanced-key", RequestRef: "approval", ResumeToken: "opaque", Revision: 1}}
	}
	return ToolResult{ToolCallID: invocation.CallID, Name: invocation.Name, Content: "approved"}, nil
}

func durableRuntimeRequest(store CheckpointStore, key string) RunRequest {
	return RunRequest{Messages: []Message{NewUserMessage("input")}, DurableRun: &DurableRunConfig{
		Identity:        RunIdentity{RunKey: key, AgentKey: "runtime-test", SessionID: "1", RequestID: key},
		CheckpointStore: store, LeaseOwner: "worker", LeaseDuration: time.Minute,
	}}
}

type runtimeMemoryStore struct {
	mu          sync.Mutex
	snapshot    RunSnapshot
	transitions []string
	executions  map[string]ToolExecution
	safeReplay  []bool
}

func newRuntimeMemoryStore() *runtimeMemoryStore {
	return &runtimeMemoryStore{executions: make(map[string]ToolExecution)}
}

func (s *runtimeMemoryStore) Begin(_ context.Context, identity RunIdentity, inputDigest, configDigest string, checkpoint Checkpoint) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.SchemaVersion == 0 {
		s.snapshot = RunSnapshot{SchemaVersion: RunSnapshotSchemaVersion, Identity: identity, InputDigest: inputDigest, ConfigDigest: configDigest, Status: RunStatusClaimed, Phase: RunPhaseModelReady, Checkpoint: checkpoint.Clone()}
	}
	if s.snapshot.Identity != identity || s.snapshot.InputDigest != inputDigest || s.snapshot.ConfigDigest != configDigest {
		return RunSnapshot{}, ErrCheckpointConflict
	}
	return s.snapshot.Clone(), nil
}

func (s *runtimeMemoryStore) Acquire(_ context.Context, runKey, owner string, until time.Time) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Identity.RunKey != runKey || s.snapshot.Status == RunStatusCompleted {
		return RunSnapshot{}, ErrCheckpointConflict
	}
	s.snapshot.Status, s.snapshot.LeaseOwner, s.snapshot.LeaseUntil = RunStatusRunning, owner, until
	s.snapshot.FenceToken++
	s.snapshot.Revision++
	s.transitions = append(s.transitions, "acquire")
	return s.snapshot.Clone(), nil
}

func (s *runtimeMemoryStore) ModelInflight(_ context.Context, guard MutationGuard, checkpoint Checkpoint) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, err
	}
	s.snapshot.Phase = RunPhaseModelInflight
	s.snapshot.Checkpoint = checkpoint.Clone()
	s.snapshot.Revision++
	s.transitions = append(s.transitions, "model_inflight")
	return s.snapshot.Clone(), nil
}

func (s *runtimeMemoryStore) CommitModelResponse(_ context.Context, guard MutationGuard, response Response, checkpoint Checkpoint) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, err
	}
	s.snapshot.Checkpoint = checkpoint.Clone()
	if len(response.ToolCalls()) == 0 {
		s.snapshot.Phase = RunPhaseFinalizing
	} else {
		s.snapshot.Phase = RunPhaseToolsReady
	}
	s.snapshot.Revision++
	return s.snapshot.Clone(), nil
}

func (s *runtimeMemoryStore) PrepareTools(_ context.Context, guard MutationGuard, executions []ToolExecution, checkpoint Checkpoint) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, err
	}
	for _, execution := range executions {
		if existing, ok := s.executions[execution.IdempotencyKey]; ok && !reflect.DeepEqual(existing, execution) {
			return RunSnapshot{}, ErrCheckpointConflict
		}
		s.executions[execution.IdempotencyKey] = execution
	}
	s.snapshot.Checkpoint = checkpoint.Clone()
	s.snapshot.Revision++
	return s.snapshot.Clone(), nil
}
func (s *runtimeMemoryStore) BeginTool(_ context.Context, guard MutationGuard, key string, safeReplay bool) (RunSnapshot, ToolExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, ToolExecution{}, err
	}
	execution, ok := s.executions[key]
	if !ok {
		return RunSnapshot{}, ToolExecution{}, ErrCheckpointConflict
	}
	if execution.Status != ToolExecutionCompleted {
		s.safeReplay = append(s.safeReplay, safeReplay)
		execution.Status = ToolExecutionExecuting
		s.executions[key] = execution
		s.snapshot.Phase = RunPhaseToolInflight
		s.snapshot.Revision++
	}
	return s.snapshot.Clone(), execution, nil
}
func (s *runtimeMemoryStore) CommitTool(_ context.Context, guard MutationGuard, execution ToolExecution, checkpoint Checkpoint) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, err
	}
	existing, ok := s.executions[execution.IdempotencyKey]
	if !ok || existing.InputHash != execution.InputHash || existing.ToolCall != execution.ToolCall {
		return RunSnapshot{}, ErrCheckpointConflict
	}
	s.executions[execution.IdempotencyKey] = execution
	s.snapshot.Checkpoint = checkpoint.Clone()
	if len(checkpoint.PendingToolCalls) == 0 {
		s.snapshot.Phase = RunPhaseModelReady
	} else {
		s.snapshot.Phase = RunPhaseToolsReady
	}
	s.snapshot.Revision++
	return s.snapshot.Clone(), nil
}
func (s *runtimeMemoryStore) Suspend(_ context.Context, guard MutationGuard, checkpoint Checkpoint) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, err
	}
	s.snapshot.Status, s.snapshot.Phase = RunStatusSuspended, RunPhaseToolsReady
	s.snapshot.Checkpoint = checkpoint.Clone()
	s.snapshot.Revision++
	return s.snapshot.Clone(), nil
}
func (s *runtimeMemoryStore) Fail(_ context.Context, guard MutationGuard, checkpoint Checkpoint, failure RunFailure) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, err
	}
	s.snapshot.Status, s.snapshot.Phase = RunStatusFailed, RunPhaseTerminal
	s.snapshot.Checkpoint = checkpoint.Clone()
	s.snapshot.Failure = &failure
	s.snapshot.Revision++
	s.transitions = append(s.transitions, "fail")
	return s.snapshot.Clone(), nil
}
func (s *runtimeMemoryStore) Load(_ context.Context, _ string) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot.Clone(), nil
}
func (s *runtimeMemoryStore) Complete(_ context.Context, guard MutationGuard, checkpoint Checkpoint) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.guard(guard); err != nil {
		return RunSnapshot{}, err
	}
	s.snapshot.Status, s.snapshot.Phase = RunStatusCompleted, RunPhaseTerminal
	s.snapshot.Checkpoint = checkpoint.Clone()
	s.snapshot.Revision++
	return s.snapshot.Clone(), nil
}

func (s *runtimeMemoryStore) guard(guard MutationGuard) error {
	if guard != s.snapshot.Guard() {
		return ErrCheckpointConflict
	}
	return nil
}
