package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedStep is one step of the fake Model's script.
type scriptedStep struct {
	// text is the text delta (emitted as multiple chunks to simulate streaming).
	text string
	// calls are the tool calls issued in this step.
	calls []ToolCall
	// usage is the usage for this step.
	usage Usage
	// streamErr, when non-nil, emits a ChunkError.
	streamErr error
	// openErr, when non-nil, makes Stream() return an error directly (the connection is never established).
	openErr error
	// noFinish, when true, skips the ChunkFinish to test fallback assembly.
	noFinish bool
}

// fakeModel replays the script step by step; once the script is exhausted it repeats the last step (simulating a stuck model).
type fakeModel struct {
	name    string
	steps   []scriptedStep
	calls   int
	lastReq *GenerateRequest
}

func (m *fakeModel) Name() string {
	if m.name == "" {
		return "fake"
	}
	return m.name
}

func (m *fakeModel) Capabilities() Capabilities {
	return Capabilities{
		Tools:              true,
		ToolChoiceNone:     true,
		ToolChoiceRequired: true,
		ToolChoiceNamed:    true,
		UsageDetails:       true,
	}
}

func (m *fakeModel) script() scriptedStep {
	if len(m.steps) == 0 {
		return scriptedStep{text: "empty script"}
	}
	if m.calls-1 < len(m.steps) {
		return m.steps[m.calls-1]
	}
	return m.steps[len(m.steps)-1]
}

func (m *fakeModel) Generate(ctx context.Context, req *GenerateRequest) (*Response, error) {
	ch, err := m.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	var resp *Response
	for c := range ch {
		if c.Type == ChunkFinish {
			resp = c.Response
		}
		if c.Type == ChunkError {
			return nil, c.Err
		}
	}
	return resp, nil
}

func (m *fakeModel) Stream(_ context.Context, req *GenerateRequest) (<-chan StreamChunk, error) {
	m.calls++
	m.lastReq = req
	s := m.script()
	if s.openErr != nil {
		return nil, s.openErr
	}

	ch := make(chan StreamChunk, len(s.calls)+8)
	go func() {
		defer close(ch)
		if s.streamErr != nil {
			ch <- StreamChunk{Type: ChunkError, Err: s.streamErr}
			return
		}
		// Split the text into per-character chunks to simulate real streaming deltas.
		for _, r := range s.text {
			ch <- StreamChunk{Type: ChunkText, TextDelta: string(r)}
		}
		for i := range s.calls {
			call := s.calls[i]
			ch <- StreamChunk{Type: ChunkToolCall, ToolCall: &call}
		}
		if s.noFinish {
			return
		}
		finish := FinishStop
		if len(s.calls) > 0 {
			finish = FinishToolCalls
		}
		message := assembleMessage(s.text, s.calls)
		message.FinishReason = finish
		ch <- StreamChunk{Type: ChunkFinish, Response: &Response{
			Message:      message,
			Usage:        s.usage,
			FinishReason: finish,
			ModelName:    "fake-large",
		}}
	}()
	return ch, nil
}

// newTestAgent assembles an agent for testing.
func newTestAgent(t *testing.T, cfg Config, model Model, tools ...Tool) *Agent {
	t.Helper()
	r := NewRegistry()
	for _, tool := range tools {
		if err := r.Register(tool); err != nil {
			t.Fatalf("Register failed: %v", err)
		}
	}
	if cfg.MaxSteps == 0 {
		cfg.MaxSteps = 8
	}
	a, err := New(cfg, model, r)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	return a
}

// collectObservations returns an emitter that records the sequence of observation types.
func collectObservations(types *[]ObservationType, texts *strings.Builder) *ObservationEmitter {
	return NewObservationEmitter(defaultObservationEmitterSize, func(observation Observation) {
		*types = append(*types, observation.Type)
		if observation.Type == ObservationTextDelta && texts != nil {
			texts.WriteString(observation.Text)
		}
	})
}

func TestRunSingleStepNoToolCall(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "hello there", usage: Usage{TotalTokens: 10}}}}
	a := newTestAgent(t, Config{Key: "test", ModelName: "large"}, model)

	var types []ObservationType
	var texts strings.Builder
	emitter := collectObservations(&types, &texts)
	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter})
	emitter.Close()
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}

	if res.Text != "hello there" {
		t.Errorf("Text = %q, want %q", res.Text, "hello there")
	}
	if texts.String() != "hello there" {
		t.Errorf("concatenated stream deltas = %q, want %q", texts.String(), "hello there")
	}
	if res.StopReason != StopReasonComplete {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopReasonComplete)
	}
	if len(res.Steps) != 1 {
		t.Errorf("len(Steps) = %d, want 1", len(res.Steps))
	}
	if res.Usage.TotalTokens != 10 {
		t.Errorf("Usage.TotalTokens = %d, want 10", res.Usage.TotalTokens)
	}
	if model.calls != 1 {
		t.Errorf("model call count = %d, want 1", model.calls)
	}
	// Observation sequence: 11 text_delta + step_finish, no terminal observation.
	if len(types) != 12 || types[11] != ObservationStepFinished {
		t.Errorf("observation sequence = %v, want 11×text_delta + step_finish", types)
	}
}

func TestRunToolCallThenAnswer(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: `{"score":4}`}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"reply":"ok"}`}}, usage: Usage{TotalTokens: 20}},
		{text: "scoring done", usage: Usage{TotalTokens: 5}},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	var types []ObservationType
	emitter := collectObservations(&types, nil)
	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter})
	emitter.Close()
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}

	if tool.calls != 1 {
		t.Errorf("tool execution count = %d, want 1", tool.calls)
	}
	if tool.lastArgs != `{"reply":"ok"}` {
		t.Errorf("tool received input = %q, want it passed through verbatim", tool.lastArgs)
	}
	if res.StopReason != StopReasonComplete || res.Text != "scoring done" {
		t.Errorf("StopReason=%q Text=%q, want complete/scoring done", res.StopReason, res.Text)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2", len(res.Steps))
	}
	// Tool results must be paired back by tool_call_id.
	if got := res.Steps[0].ToolResults; len(got) != 1 || got[0].ToolCallID != "c1" {
		t.Errorf("ToolResults = %+v, want paired with c1", got)
	}
	// Message sequence: assistant(tool_call) → tool(result) → assistant(text).
	if len(res.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, want 3", len(res.Messages))
	}
	wantRoles := []Role{RoleAssistant, RoleTool, RoleAssistant}
	for i, want := range wantRoles {
		if res.Messages[i].Role != want {
			t.Errorf("Messages[%d].Role = %q, want %q", i, res.Messages[i].Role, want)
		}
	}
	// The second-step request must include the fed-back tool message: user + assistant(tool_call) + tool(result).
	if len(model.lastReq.Messages) != 3 {
		t.Errorf("second-step request message count = %d, want 3 (user+assistant+tool)", len(model.lastReq.Messages))
	}
	if last := model.lastReq.Messages[2]; last.Role != RoleTool {
		t.Errorf("last message role in second-step request = %q, want %q", last.Role, RoleTool)
	}
	// The observation sequence includes tool_call_start and tool_result.
	assertHasObservations(t, types, ObservationToolCall, ObservationToolResult, ObservationStepFinished)
}

func TestRunParallelToolCalls(t *testing.T) {
	scoreTool := &fakeTool{name: "score_round", result: ToolResult{Content: "scored"}}
	optsTool := &fakeTool{name: "gen_options", result: ToolResult{Content: "options"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{
			{ID: "c1", Name: "score_round", Input: `{}`},
			{ID: "c2", Name: "gen_options", Input: `{}`},
		}},
		{text: "done"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, scoreTool, optsTool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if scoreTool.calls != 1 || optsTool.calls != 1 {
		t.Errorf("tool execution counts = %d/%d, want 1 each", scoreTool.calls, optsTool.calls)
	}
	results := res.Steps[0].ToolResults
	if len(results) != 2 {
		t.Fatalf("len(ToolResults) = %d, want 2", len(results))
	}
	if results[0].ToolCallID != "c1" || results[1].ToolCallID != "c2" {
		t.Errorf("parallel result pairing is wrong: %+v", results)
	}
	// One tool message carries both result blocks.
	if got := res.Messages[1].ToolResults(); len(got) != 2 {
		t.Errorf("tool message result blocks = %d, want 2", len(got))
	}
}

func TestRunMaxStepsForcedFinish(t *testing.T) {
	// The model calls a tool on every step with changing input (so loop detection
	// does not fire early) until max_steps is exhausted.
	var steps []scriptedStep
	for i := range 10 {
		steps = append(steps, scriptedStep{
			text:  fmt.Sprintf("step%d", i),
			calls: []ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "score_round", Input: fmt.Sprintf(`{"i":%d}`, i)}},
		})
	}
	// Tool results also change per call, so signatures never repeat.
	tool := &countingTool{name: "score_round"}
	model := &fakeModel{steps: steps}
	// The window must be <= max_steps, otherwise assembly fails (that validation is intentional, not something to bypass).
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 3, LoopDetectWindow: 3, LoopDetectThreshold: 1,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if res.StopReason != StopReasonMaxSteps {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopReasonMaxSteps)
	}
	if len(res.Steps) != 3 {
		t.Errorf("len(Steps) = %d, want 3", len(res.Steps))
	}
	// The forced finish must answer with the last step's text, not an empty string.
	if res.Text != "step2" {
		t.Errorf("Text = %q, want the last step's text %q", res.Text, "step2")
	}
}

func TestRunToolStopTurn(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "wrap up", StopTurn: true}}
	model := &fakeModel{steps: []scriptedStep{
		{text: "one thing first", calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}},
		{text: "should never reach this step"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if res.StopReason != StopReasonToolStopTurn {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopReasonToolStopTurn)
	}
	if model.calls != 1 {
		t.Errorf("model call count = %d, want 1 (no further calls after StopTurn)", model.calls)
	}
}

func TestRunToolNotAllowedFeedsBackWithoutAborting(t *testing.T) {
	allowed := &fakeTool{name: "score_round", result: ToolResult{Content: "ok"}}
	danger := &fakeTool{name: "danger", result: ToolResult{Content: "should not be executed"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "danger", Input: `{}`}}},
		{text: "switched course"},
	}}
	// The whitelist only allows score_round; danger is registered but unavailable.
	a := newTestAgent(t, Config{Key: "test", AllowedTools: []string{"score_round"}}, model, allowed, danger)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() must not abort: %v", err)
	}
	if danger.calls != 0 {
		t.Errorf("tool outside the whitelist executed %d times, want 0", danger.calls)
	}
	results := res.Steps[0].ToolResults
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("ToolResults = %+v, want a single IsError result", results)
	}
	if !strings.Contains(results[0].Content, "unavailable") {
		t.Errorf("fed-back content = %q, want a tool-unavailable hint", results[0].Content)
	}
	// No abort: the model gets the feedback, switches course, and finishes normally.
	if res.StopReason != StopReasonComplete || res.Text != "switched course" {
		t.Errorf("StopReason=%q Text=%q, want complete/switched course", res.StopReason, res.Text)
	}
	// Tools outside the whitelist must not appear in the declarations sent to the model.
	for _, def := range model.lastReq.Tools {
		if def.Name == "danger" {
			t.Error("tool outside the whitelist appeared in the request Tools")
		}
	}
}

func TestRunInvalidToolInputFedBackForRepair(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "ok"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"broken":`}}},
		{text: "repaired"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() must not abort: %v", err)
	}
	if tool.calls != 0 {
		t.Errorf("invalid input must not execute the tool, executed %d times", tool.calls)
	}
	results := res.Steps[0].ToolResults
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("ToolResults = %+v, want a single IsError result", results)
	}
	if !strings.Contains(results[0].Content, "JSON") {
		t.Errorf("fed-back content = %q, want a JSON parse error hint", results[0].Content)
	}
	if res.Text != "repaired" {
		t.Errorf("Text = %q, want the model's self-corrected reply", res.Text)
	}
}

func TestRunToolExecErrorIsFatal(t *testing.T) {
	cause := errors.New("upstream scoring service is down")
	tool := &fakeTool{name: "score_round", execErr: cause}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}},
		{text: "degraded reply"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if !errors.Is(err, cause) || res.Outcome != OutcomeFailed || model.calls != 1 {
		t.Fatalf("result/error/calls = %+v/%v/%d", res, err, model.calls)
	}
	results := res.Steps[0].ToolResults
	if len(results) != 0 {
		t.Fatalf("fatal tool error fabricated results: %+v", results)
	}
	if tool.lastInvocation != (ToolInvocation{CallID: "c1", Name: "score_round", RawInput: `{}`}) {
		t.Fatalf("invocation = %+v", tool.lastInvocation)
	}
}

func TestRunUpstreamErrorPropagated(t *testing.T) {
	tests := []struct {
		name string
		step scriptedStep
	}{
		{name: "error mid-stream", step: scriptedStep{streamErr: errors.New("upstream 503 service unavailable")}},
		{name: "stream open fails", step: scriptedStep{openErr: errors.New("upstream 503 service unavailable")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &fakeModel{steps: []scriptedStep{tt.step}}
			a := newTestAgent(t, Config{Key: "test"}, model)

			_, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
			if err == nil {
				t.Fatal("want the upstream error propagated, got nil")
			}
			var modelErr *ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != ModelErrorKindTransport || !modelErr.Retryable {
				t.Errorf("error = %v, want retryable transport ModelError", err)
			}
			if modelErr.Cause == nil || modelErr.Cause.Error() != "upstream 503 service unavailable" {
				t.Errorf("cause = %v, want original upstream cause", modelErr.Cause)
			}
			if strings.Contains(err.Error(), "upstream 503 service unavailable") {
				t.Errorf("error = %v exposed unsafe cause", err)
			}
		})
	}
}

func TestRunLoopDetectionAborts(t *testing.T) {
	// The model keeps calling the same tool with identical input and gets identical results — a repeated signature.
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "same result"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"same":1}`}}},
	}}
	// window=4 <= max_steps=8, so detection fires before steps run out.
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 8, LoopDetectWindow: 4, LoopDetectThreshold: 2,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("error = %v, want ErrLoopDetected", err)
	}
	if res.StopReason != StopReasonLoopDetected {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopReasonLoopDetected)
	}
	// The 3rd repeat triggers the abort; max_steps=8 must not be burned through.
	if len(res.Steps) != 3 {
		t.Errorf("len(Steps) = %d, want 3 (abort on the 3rd repeat)", len(res.Steps))
	}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		model   Model
		wantErr error
	}{
		{
			name:    "window greater than max_steps fails assembly",
			cfg:     Config{MaxSteps: 8, LoopDetectWindow: 10, LoopDetectThreshold: 5},
			model:   &fakeModel{},
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name:    "zero max_steps fails assembly",
			cfg:     Config{MaxSteps: 0},
			model:   &fakeModel{},
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name:    "nil model fails assembly",
			cfg:     Config{MaxSteps: 8},
			model:   nil,
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name:    "whitelist referencing an unregistered tool fails assembly",
			cfg:     Config{MaxSteps: 8, AllowedTools: []string{"not_exist"}},
			model:   &fakeModel{},
			wantErr: ErrToolNotFound,
		},
		{
			name:  "valid config assembles",
			cfg:   Config{MaxSteps: 8, LoopDetectWindow: 4, LoopDetectThreshold: 2},
			model: &fakeModel{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg, tt.model, NewRegistry())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("New() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() unexpected error: %v", err)
			}
		})
	}
}

func TestRunObservationOrder(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "ok"}}
	model := &fakeModel{steps: []scriptedStep{
		{text: "a", calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}},
		{text: "b"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	var types []ObservationType
	emitter := collectObservations(&types, nil)
	if _, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter}); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	emitter.Close()

	want := []ObservationType{
		ObservationTextDelta, // first-step text "a"
		ObservationToolCall,  // tool execution
		ObservationToolResult,
		ObservationStepFinished, // first step ends
		ObservationTextDelta,    // second-step text "b"
		ObservationStepFinished, // second step ends
	}
	if len(types) != len(want) {
		t.Fatalf("event sequence = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event sequence = %v, want %v", types, want)
		}
	}
}

func TestRunSlowObservationConsumerDoesNotBlock(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "abc"}}}
	a := newTestAgent(t, Config{Key: "test"}, model)

	release := make(chan struct{})
	emitter := NewObservationEmitter(1, func(Observation) {
		<-release
	})
	done := make(chan struct{})
	var res *RunResult
	var err error
	go func() {
		res, err = a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run() blocked on observation consumer")
	}
	close(release)
	emitter.Close()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.Text != "abc" || res.Outcome != OutcomeCompleted {
		t.Fatalf("result = %+v", res)
	}
}

func TestRunContextCanceled(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "abc"}}}
	a := newTestAgent(t, Config{Key: "test"}, model)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Run(ctx, RunRequest{Messages: []Message{NewUserMessage("hi")}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRunRejectsStreamWithoutFinishChunk(t *testing.T) {
	// Missing the finish chunk usually means the network stream was truncated;
	// partial text must not be treated as a successful result.
	model := &fakeModel{steps: []scriptedStep{{text: "fallback text", noFinish: true}}}
	a := newTestAgent(t, Config{Key: "test"}, model)

	_, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	var modelErr *ModelError
	if !errors.As(err, &modelErr) || modelErr.Kind != ModelErrorKindProtocol || modelErr.Retryable {
		t.Fatalf("error = %v, want non-retryable protocol ModelError", err)
	}
}

type blockingStreamModel struct {
	canceled chan struct{}
	started  chan struct{}
	once     sync.Once
}

func (m *blockingStreamModel) Name() string { return "blocking" }

func (m *blockingStreamModel) Capabilities() Capabilities { return Capabilities{} }

func (m *blockingStreamModel) Generate(context.Context, *GenerateRequest) (*Response, error) {
	return nil, errors.New("not implemented")
}

func (m *blockingStreamModel) Stream(ctx context.Context, _ *GenerateRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk)
	close(m.started)
	go func() {
		<-ctx.Done()
		m.once.Do(func() { close(m.canceled) })
	}()
	return ch, nil
}

func TestRunCancellationUnblocksMisbehavingStream(t *testing.T) {
	model := &blockingStreamModel{canceled: make(chan struct{}), started: make(chan struct{})}
	a := newTestAgent(t, Config{Key: "test"}, model)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, RunRequest{Messages: []Message{NewUserMessage("hi")}})
		done <- err
	}()
	<-model.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run blocked on stream receive after cancellation")
	}
	select {
	case <-model.canceled:
	case <-time.After(time.Second):
		t.Fatal("per-step stream context was not canceled")
	}
}

func TestRunRejectsInvalidTerminalResponsesBeforeToolExecution(t *testing.T) {
	tests := []struct {
		name     string
		response *Response
	}{
		{name: "wrong role", response: &Response{Message: Message{Role: RoleUser, FinishReason: FinishStop}, FinishReason: FinishStop}},
		{name: "length", response: &Response{Message: Message{Role: RoleAssistant, FinishReason: FinishLength}, FinishReason: FinishLength}},
		{name: "calls with stop", response: &Response{Message: Message{Role: RoleAssistant, Parts: []ContentPart{{Type: PartToolCall, ToolCall: &ToolCall{ID: "c", Name: "score_round", Input: `{}`}}}, FinishReason: FinishStop}, FinishReason: FinishStop}},
		{name: "tool finish without calls", response: &Response{Message: Message{Role: RoleAssistant, FinishReason: FinishToolCalls}, FinishReason: FinishToolCalls}},
		{name: "invalid usage", response: &Response{Message: NewAssistantMessage("done"), FinishReason: FinishStop, Usage: Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 99}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tool := &fakeTool{name: "score_round"}
			model := modelFunc(func(context.Context, *GenerateRequest) (<-chan StreamChunk, error) {
				ch := make(chan StreamChunk, 1)
				ch <- StreamChunk{Type: ChunkFinish, Response: tt.response}
				close(ch)
				return ch, nil
			})
			a := newTestAgent(t, Config{Key: "test"}, model, tool)
			if _, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}}); !isModelErrorKind(err, ModelErrorKindProtocol, false) {
				t.Fatalf("Run() error = %v, want protocol ModelError", err)
			}
			if tool.calls != 0 {
				t.Fatalf("tool executed %d times", tool.calls)
			}
		})
	}
}

func isModelErrorKind(err error, kind ModelErrorKind, retryable bool) bool {
	var modelErr *ModelError
	return errors.As(err, &modelErr) && modelErr.Kind == kind && modelErr.Retryable == retryable
}

type modelFunc func(context.Context, *GenerateRequest) (<-chan StreamChunk, error)

func (f modelFunc) Name() string               { return "func" }
func (f modelFunc) Capabilities() Capabilities { return Capabilities{Tools: true} }
func (f modelFunc) Generate(context.Context, *GenerateRequest) (*Response, error) {
	return nil, errors.New("not implemented")
}
func (f modelFunc) Stream(ctx context.Context, req *GenerateRequest) (<-chan StreamChunk, error) {
	return f(ctx, req)
}

type streamOnlyModel struct{}

func (streamOnlyModel) Name() string               { return "stream-only" }
func (streamOnlyModel) Capabilities() Capabilities { return Capabilities{} }
func (streamOnlyModel) Stream(context.Context, *GenerateRequest) (<-chan StreamChunk, error) {
	ch := make(chan StreamChunk, 2)
	ch <- StreamChunk{Type: ChunkText, TextDelta: "done"}
	ch <- StreamChunk{Type: ChunkFinish, Response: terminalResponse(NewAssistantMessage("done"))}
	close(ch)
	return ch, nil
}

func TestRunAcceptsStreamOnlyModel(t *testing.T) {
	a := newTestAgent(t, Config{Key: "test"}, streamOnlyModel{})
	result, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "done" {
		t.Fatalf("Run() text = %q", result.Text)
	}
}

func TestRunRejectsNonObjectToolInput(t *testing.T) {
	tests := []string{`[]`, `null`, `"text"`, `1`, `true`}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			tool := &fakeTool{name: "score_round", result: ToolResult{Content: "ok"}}
			model := &fakeModel{steps: []scriptedStep{
				{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: input}}},
				{text: "corrected"},
			}}
			a := newTestAgent(t, Config{Key: "test"}, model, tool)

			res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
			if err != nil {
				t.Fatalf("Run() failed: %v", err)
			}
			if tool.calls != 0 {
				t.Fatalf("non-object input executed the tool %d times", tool.calls)
			}
			if got := res.Steps[0].ToolResults[0]; !got.IsError || !strings.Contains(got.Content, "JSON object") {
				t.Fatalf("ToolResult = %+v, want a JSON object error", got)
			}
		})
	}
}

func TestRunContextBudgetUsesLatestStepUsage(t *testing.T) {
	tool := &countingTool{name: "score_round"}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"step":1}`}}, usage: Usage{PromptTokens: 60, CompletionTokens: 5, TotalTokens: 65}},
		{calls: []ToolCall{{ID: "c2", Name: "score_round", Input: `{"step":2}`}}, usage: Usage{PromptTokens: 70, CompletionTokens: 5, TotalTokens: 75}},
		{text: "done", usage: Usage{PromptTokens: 75, CompletionTokens: 5, TotalTokens: 80}},
	}}
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 4, ContextWindow: 200, LoopDetectWindow: 4, LoopDetectThreshold: 2,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if res.StopReason != StopReasonComplete {
		t.Fatalf("StopReason = %q, want complete", res.StopReason)
	}
}

func TestRunContextBudgetUsesPromptOccupancyNotBilledUsage(t *testing.T) {
	tool := &countingTool{name: "score_round"}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}, usage: Usage{PromptTokens: 70, CompletionTokens: 20, TotalTokens: 90}},
		{text: "done", usage: Usage{PromptTokens: 75, CompletionTokens: 20, TotalTokens: 95}},
	}}
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 4, ContextWindow: 100, LoopDetectWindow: 4, LoopDetectThreshold: 2,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if res.StopReason != StopReasonComplete {
		t.Fatalf("StopReason = %q, want complete", res.StopReason)
	}
}

func TestRunDoesNotMutateCallerMessages(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "reply"}}}
	a := newTestAgent(t, Config{Key: "test"}, model)

	input := []Message{NewSystemMessage("sys"), NewUserMessage("hi")}
	if _, err := a.Run(context.Background(), RunRequest{Messages: input}); err != nil {
		t.Fatalf("Run() failed: %v", err)
	}
	if len(input) != 2 {
		t.Errorf("caller slice was mutated: len = %d, want 2", len(input))
	}
}

// countingTool returns different content on every execution, so the loop
// detector does not interfere with step-count tests.
type countingTool struct {
	name  string
	calls int
}

func (c *countingTool) Definition() ToolDefinition {
	return ToolDefinition{Name: c.name, Parameters: map[string]any{"type": "object"}}
}

func (c *countingTool) ReplayPolicy() ReplayPolicy { return ReplayPolicyNever }

func (c *countingTool) Execute(_ context.Context, _ ToolInvocation) (ToolResult, error) {
	c.calls++
	return ToolResult{Name: c.name, Content: fmt.Sprintf("result %d", c.calls)}, nil
}

// assertHasObservations asserts that the observation sequence contains the given
// observations in order (other observations may appear in between).
func assertHasObservations(t *testing.T, got []ObservationType, want ...ObservationType) {
	t.Helper()
	idx := 0
	for _, ev := range got {
		if idx < len(want) && ev == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Errorf("event sequence = %v, want to contain in order %v", got, want)
	}
}
