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

// scriptedStep 是假 Model 的一步剧本。
type scriptedStep struct {
	// text 文本增量（会被拆成多个 chunk 发出，模拟流式）。
	text string
	// calls 本步发起的工具调用。
	calls []ToolCall
	// usage 本步用量。
	usage Usage
	// streamErr 非 nil 时发 ChunkError。
	streamErr error
	// openErr 非 nil 时 Stream() 直接返回错误（连接都没建起来）。
	openErr error
	// noFinish 为 true 时不发 ChunkFinish，测试兜底组装。
	noFinish bool
}

// fakeModel 按剧本逐步返回；剧本用尽后重复最后一步（模拟模型卡死）。
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
		return scriptedStep{text: "空剧本"}
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
		// 文本按字符拆片，模拟真实流式增量。
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

// newTestAgent 装配一个测试用 agent。
func newTestAgent(t *testing.T, cfg Config, model Model, tools ...Tool) *Agent {
	t.Helper()
	r := NewRegistry()
	for _, tool := range tools {
		if err := r.Register(tool); err != nil {
			t.Fatalf("Register 报错: %v", err)
		}
	}
	if cfg.MaxSteps == 0 {
		cfg.MaxSteps = 8
	}
	a, err := New(cfg, model, r)
	if err != nil {
		t.Fatalf("New() 报错: %v", err)
	}
	return a
}

// collectObservations 返回一个记录观测类型序列的 emitter。
func collectObservations(types *[]ObservationType, texts *strings.Builder) *ObservationEmitter {
	return NewObservationEmitter(defaultObservationEmitterSize, func(observation Observation) {
		*types = append(*types, observation.Type)
		if observation.Type == ObservationTextDelta && texts != nil {
			texts.WriteString(observation.Text)
		}
	})
}

func TestRunSingleStepNoToolCall(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "你好呀", usage: Usage{TotalTokens: 10}}}}
	a := newTestAgent(t, Config{Key: "test", ModelName: "large"}, model)

	var types []ObservationType
	var texts strings.Builder
	emitter := collectObservations(&types, &texts)
	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter})
	emitter.Close()
	if err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}

	if res.Text != "你好呀" {
		t.Errorf("Text = %q, 期望 %q", res.Text, "你好呀")
	}
	if texts.String() != "你好呀" {
		t.Errorf("流式增量拼接 = %q, 期望 %q", texts.String(), "你好呀")
	}
	if res.StopReason != StopReasonComplete {
		t.Errorf("StopReason = %q, 期望 %q", res.StopReason, StopReasonComplete)
	}
	if len(res.Steps) != 1 {
		t.Errorf("len(Steps) = %d, 期望 1", len(res.Steps))
	}
	if res.Usage.TotalTokens != 10 {
		t.Errorf("Usage.TotalTokens = %d, 期望 10", res.Usage.TotalTokens)
	}
	if model.calls != 1 {
		t.Errorf("模型调用次数 = %d, 期望 1", model.calls)
	}
	// 观测序列：3 个 text_delta + step_finish，不包含终态。
	if len(types) != 4 || types[3] != ObservationStepFinished {
		t.Errorf("观测序列 = %v, 期望 3×text_delta + step_finish", types)
	}
}

func TestRunToolCallThenAnswer(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: `{"score":4}`}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"reply":"好"}`}}, usage: Usage{TotalTokens: 20}},
		{text: "打完分了", usage: Usage{TotalTokens: 5}},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	var types []ObservationType
	emitter := collectObservations(&types, nil)
	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter})
	emitter.Close()
	if err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}

	if tool.calls != 1 {
		t.Errorf("工具执行次数 = %d, 期望 1", tool.calls)
	}
	if tool.lastArgs != `{"reply":"好"}` {
		t.Errorf("工具收到参数 = %q, 期望原样透传", tool.lastArgs)
	}
	if res.StopReason != StopReasonComplete || res.Text != "打完分了" {
		t.Errorf("StopReason=%q Text=%q, 期望 complete/打完分了", res.StopReason, res.Text)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("len(Steps) = %d, 期望 2", len(res.Steps))
	}
	// 工具结果必须按 tool_call_id 配对回灌。
	if got := res.Steps[0].ToolResults; len(got) != 1 || got[0].ToolCallID != "c1" {
		t.Errorf("ToolResults = %+v, 期望配对 c1", got)
	}
	// 消息序列：assistant(tool_call) → tool(result) → assistant(text)。
	if len(res.Messages) != 3 {
		t.Fatalf("len(Messages) = %d, 期望 3", len(res.Messages))
	}
	wantRoles := []Role{RoleAssistant, RoleTool, RoleAssistant}
	for i, want := range wantRoles {
		if res.Messages[i].Role != want {
			t.Errorf("Messages[%d].Role = %q, 期望 %q", i, res.Messages[i].Role, want)
		}
	}
	// 第二步的请求里必须带上回灌的 tool 消息：user + assistant(tool_call) + tool(result)。
	if len(model.lastReq.Messages) != 3 {
		t.Errorf("第二步请求消息数 = %d, 期望 3（user+assistant+tool）", len(model.lastReq.Messages))
	}
	if last := model.lastReq.Messages[2]; last.Role != RoleTool {
		t.Errorf("第二步请求末条消息 Role = %q, 期望 %q", last.Role, RoleTool)
	}
	// 观测序列含 tool_call_start 与 tool_result。
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
		{text: "完成"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, scoreTool, optsTool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}
	if scoreTool.calls != 1 || optsTool.calls != 1 {
		t.Errorf("工具执行次数 = %d/%d, 期望各 1", scoreTool.calls, optsTool.calls)
	}
	results := res.Steps[0].ToolResults
	if len(results) != 2 {
		t.Fatalf("len(ToolResults) = %d, 期望 2", len(results))
	}
	if results[0].ToolCallID != "c1" || results[1].ToolCallID != "c2" {
		t.Errorf("并行结果配对错误: %+v", results)
	}
	// 一条 tool 消息里带两个结果块。
	if got := res.Messages[1].ToolResults(); len(got) != 2 {
		t.Errorf("tool 消息结果块 = %d, 期望 2", len(got))
	}
}

func TestRunMaxStepsForcedFinish(t *testing.T) {
	// 模型每步都调工具且参数递增（避免被死循环检测提前拦住），直到烧完 max_steps。
	var steps []scriptedStep
	for i := range 10 {
		steps = append(steps, scriptedStep{
			text:  fmt.Sprintf("第%d步", i),
			calls: []ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "score_round", Input: fmt.Sprintf(`{"i":%d}`, i)}},
		})
	}
	// 工具结果也随调用变化，签名不重复。
	tool := &countingTool{name: "score_round"}
	model := &fakeModel{steps: steps}
	// 窗口必须 <= max_steps，否则装配就会失败（这是刻意的校验，不是绕过）。
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 3, LoopDetectWindow: 3, LoopDetectThreshold: 1,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}
	if res.StopReason != StopReasonMaxSteps {
		t.Errorf("StopReason = %q, 期望 %q", res.StopReason, StopReasonMaxSteps)
	}
	if len(res.Steps) != 3 {
		t.Errorf("len(Steps) = %d, 期望 3", len(res.Steps))
	}
	// 强制收尾要用最后一次的文本作答，不能是空。
	if res.Text != "第2步" {
		t.Errorf("Text = %q, 期望最后一步的文本 %q", res.Text, "第2步")
	}
}

func TestRunToolStopTurn(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "收尾", StopTurn: true}}
	model := &fakeModel{steps: []scriptedStep{
		{text: "先说一句", calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}},
		{text: "不该跑到这一步"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}
	if res.StopReason != StopReasonToolStopTurn {
		t.Errorf("StopReason = %q, 期望 %q", res.StopReason, StopReasonToolStopTurn)
	}
	if model.calls != 1 {
		t.Errorf("模型调用次数 = %d, 期望 1（StopTurn 后不应再调）", model.calls)
	}
}

func TestRunToolNotAllowedFeedsBackWithoutAborting(t *testing.T) {
	allowed := &fakeTool{name: "score_round", result: ToolResult{Content: "ok"}}
	danger := &fakeTool{name: "danger", result: ToolResult{Content: "不该被执行"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "danger", Input: `{}`}}},
		{text: "换路了"},
	}}
	// 白名单只放 score_round，danger 已注册但不可用。
	a := newTestAgent(t, Config{Key: "test", AllowedTools: []string{"score_round"}}, model, allowed, danger)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() 不应中断: %v", err)
	}
	if danger.calls != 0 {
		t.Errorf("白名单外的工具被执行了 %d 次, 期望 0", danger.calls)
	}
	results := res.Steps[0].ToolResults
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("ToolResults = %+v, 期望一条 IsError 结果", results)
	}
	if !strings.Contains(results[0].Content, "不可用") {
		t.Errorf("回灌内容 = %q, 期望提示工具不可用", results[0].Content)
	}
	// 不中断：模型拿到反馈后换路，正常收尾。
	if res.StopReason != StopReasonComplete || res.Text != "换路了" {
		t.Errorf("StopReason=%q Text=%q, 期望 complete/换路了", res.StopReason, res.Text)
	}
	// 白名单外的工具不应出现在发给模型的声明里。
	for _, def := range model.lastReq.Tools {
		if def.Name == "danger" {
			t.Error("白名单外的工具出现在请求的 Tools 里")
		}
	}
}

func TestRunInvalidToolInputFedBackForRepair(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "ok"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"broken":`}}},
		{text: "修好了"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() 不应中断: %v", err)
	}
	if tool.calls != 0 {
		t.Errorf("非法参数不应执行工具, 实际执行 %d 次", tool.calls)
	}
	results := res.Steps[0].ToolResults
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("ToolResults = %+v, 期望一条 IsError 结果", results)
	}
	if !strings.Contains(results[0].Content, "JSON") {
		t.Errorf("回灌内容 = %q, 期望包含 JSON 解析错误提示", results[0].Content)
	}
	if res.Text != "修好了" {
		t.Errorf("Text = %q, 期望模型自我修正后的答复", res.Text)
	}
}

func TestRunToolExecErrorIsFatal(t *testing.T) {
	cause := errors.New("上游打分服务挂了")
	tool := &fakeTool{name: "score_round", execErr: cause}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}},
		{text: "降级答复"},
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
		{name: "流中报错", step: scriptedStep{streamErr: errors.New("上游 503 服务不可用")}},
		{name: "建流即失败", step: scriptedStep{openErr: errors.New("上游 503 服务不可用")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &fakeModel{steps: []scriptedStep{tt.step}}
			a := newTestAgent(t, Config{Key: "test"}, model)

			_, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
			if err == nil {
				t.Fatal("期望上游错误被传播, 实际为 nil")
			}
			var modelErr *ModelError
			if !errors.As(err, &modelErr) || modelErr.Kind != ModelErrorKindTransport || !modelErr.Retryable {
				t.Errorf("error = %v, want retryable transport ModelError", err)
			}
			if modelErr.Cause == nil || modelErr.Cause.Error() != "上游 503 服务不可用" {
				t.Errorf("cause = %v, want original upstream cause", modelErr.Cause)
			}
			if strings.Contains(err.Error(), "上游 503 服务不可用") {
				t.Errorf("error = %v exposed unsafe cause", err)
			}
		})
	}
}

func TestRunLoopDetectionAborts(t *testing.T) {
	// 模型反复用完全相同的参数调同一个工具，结果也相同 —— 签名重复。
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "一样的结果"}}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"same":1}`}}},
	}}
	// window=4 <= max_steps=8，检测能在步数烧完前生效。
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 8, LoopDetectWindow: 4, LoopDetectThreshold: 2,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("error = %v, 期望 ErrLoopDetected", err)
	}
	if res.StopReason != StopReasonLoopDetected {
		t.Errorf("StopReason = %q, 期望 %q", res.StopReason, StopReasonLoopDetected)
	}
	// 第 3 次重复即命中，不该把 max_steps=8 烧完。
	if len(res.Steps) != 3 {
		t.Errorf("len(Steps) = %d, 期望 3（第 3 次重复即中断）", len(res.Steps))
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
			name:    "窗口大于 max_steps 装配失败",
			cfg:     Config{MaxSteps: 8, LoopDetectWindow: 10, LoopDetectThreshold: 5},
			model:   &fakeModel{},
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name:    "max_steps 为 0 装配失败",
			cfg:     Config{MaxSteps: 0},
			model:   &fakeModel{},
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name:    "model 为 nil 装配失败",
			cfg:     Config{MaxSteps: 8},
			model:   nil,
			wantErr: ErrAgentConfigInvalid,
		},
		{
			name:    "白名单引用未注册工具装配失败",
			cfg:     Config{MaxSteps: 8, AllowedTools: []string{"not_exist"}},
			model:   &fakeModel{},
			wantErr: ErrToolNotFound,
		},
		{
			name:  "合法配置装配成功",
			cfg:   Config{MaxSteps: 8, LoopDetectWindow: 4, LoopDetectThreshold: 2},
			model: &fakeModel{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.cfg, tt.model, NewRegistry())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("New() error = %v, 期望 %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() 意外报错: %v", err)
			}
		})
	}
}

func TestRunObservationOrder(t *testing.T) {
	tool := &fakeTool{name: "score_round", result: ToolResult{Content: "ok"}}
	model := &fakeModel{steps: []scriptedStep{
		{text: "嗨", calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}},
		{text: "好"},
	}}
	a := newTestAgent(t, Config{Key: "test"}, model, tool)

	var types []ObservationType
	emitter := collectObservations(&types, nil)
	if _, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}, ObservationEmitter: emitter}); err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}
	emitter.Close()

	want := []ObservationType{
		ObservationTextDelta, // 第一步文本「嗨」
		ObservationToolCall,  // 执行工具
		ObservationToolResult,
		ObservationStepFinished, // 第一步结束
		ObservationTextDelta,    // 第二步文本「好」
		ObservationStepFinished, // 第二步结束
	}
	if len(types) != len(want) {
		t.Fatalf("事件序列 = %v, 期望 %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("事件序列 = %v, 期望 %v", types, want)
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
		t.Fatalf("error = %v, 期望 context.Canceled", err)
	}
}

func TestRunRejectsStreamWithoutFinishChunk(t *testing.T) {
	// 未收到终止帧通常意味着网络流被截断，不能把部分文本当成成功结果。
	model := &fakeModel{steps: []scriptedStep{{text: "兜底文本", noFinish: true}}}
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
				{text: "已修正"},
			}}
			a := newTestAgent(t, Config{Key: "test"}, model, tool)

			res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
			if err != nil {
				t.Fatalf("Run() 报错: %v", err)
			}
			if tool.calls != 0 {
				t.Fatalf("非对象参数执行了工具 %d 次", tool.calls)
			}
			if got := res.Steps[0].ToolResults[0]; !got.IsError || !strings.Contains(got.Content, "JSON 对象") {
				t.Fatalf("ToolResult = %+v, 期望 JSON 对象错误", got)
			}
		})
	}
}

func TestRunContextBudgetUsesLatestStepUsage(t *testing.T) {
	tool := &countingTool{name: "score_round"}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{"step":1}`}}, usage: Usage{PromptTokens: 60, CompletionTokens: 5, TotalTokens: 65}},
		{calls: []ToolCall{{ID: "c2", Name: "score_round", Input: `{"step":2}`}}, usage: Usage{PromptTokens: 70, CompletionTokens: 5, TotalTokens: 75}},
		{text: "完成", usage: Usage{PromptTokens: 75, CompletionTokens: 5, TotalTokens: 80}},
	}}
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 4, ContextWindow: 200, LoopDetectWindow: 4, LoopDetectThreshold: 2,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}
	if res.StopReason != StopReasonComplete {
		t.Fatalf("StopReason = %q, 期望 complete", res.StopReason)
	}
}

func TestRunContextBudgetUsesPromptOccupancyNotBilledUsage(t *testing.T) {
	tool := &countingTool{name: "score_round"}
	model := &fakeModel{steps: []scriptedStep{
		{calls: []ToolCall{{ID: "c1", Name: "score_round", Input: `{}`}}, usage: Usage{PromptTokens: 70, CompletionTokens: 20, TotalTokens: 90}},
		{text: "完成", usage: Usage{PromptTokens: 75, CompletionTokens: 20, TotalTokens: 95}},
	}}
	a := newTestAgent(t, Config{
		Key: "test", MaxSteps: 4, ContextWindow: 100, LoopDetectWindow: 4, LoopDetectThreshold: 2,
	}, model, tool)

	res, err := a.Run(context.Background(), RunRequest{Messages: []Message{NewUserMessage("hi")}})
	if err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}
	if res.StopReason != StopReasonComplete {
		t.Fatalf("StopReason = %q, 期望 complete", res.StopReason)
	}
}

func TestRunDoesNotMutateCallerMessages(t *testing.T) {
	model := &fakeModel{steps: []scriptedStep{{text: "答复"}}}
	a := newTestAgent(t, Config{Key: "test"}, model)

	input := []Message{NewSystemMessage("sys"), NewUserMessage("hi")}
	if _, err := a.Run(context.Background(), RunRequest{Messages: input}); err != nil {
		t.Fatalf("Run() 报错: %v", err)
	}
	if len(input) != 2 {
		t.Errorf("调用方切片被污染: len = %d, 期望 2", len(input))
	}
}

// countingTool 每次执行返回不同内容，用于避免死循环检测干扰步数测试。
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
	return ToolResult{Name: c.name, Content: fmt.Sprintf("第 %d 次结果", c.calls)}, nil
}

// assertHasObservations 断言观测序列里按顺序出现了指定观测（允许中间夹杂其他观测）。
func assertHasObservations(t *testing.T, got []ObservationType, want ...ObservationType) {
	t.Helper()
	idx := 0
	for _, ev := range got {
		if idx < len(want) && ev == want[idx] {
			idx++
		}
	}
	if idx != len(want) {
		t.Errorf("事件序列 = %v, 期望按顺序包含 %v", got, want)
	}
}
