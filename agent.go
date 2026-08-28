package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// Config is the configuration needed to assemble one agent. Applications
// typically load it from their own configuration source.
type Config struct {
	// Key identifies the agent, for example assistant.support.
	Key string
	// ModelName is the resolved upstream model name.
	ModelName string
	// MaxSteps caps tool-calling steps within a single run.
	MaxSteps int
	// AllowedTools is the tool allowlist; nil means no restriction.
	AllowedTools []string
	// LoopDetectWindow / LoopDetectThreshold configure loop detection;
	// values <= 0 use the defaults.
	LoopDetectWindow    int
	LoopDetectThreshold int
	// ContextWindow is the model context window size; 0 means unknown and
	// disables budget checks.
	ContextWindow int
	// Temperature / MaxTokens / TopP are optional sampling parameters.
	Temperature *float64
	MaxTokens   *int
	TopP        *float64
	// StopConditions are additional stop conditions, stacked on top of the
	// default StepCountIs(MaxSteps).
	StopConditions []StopCondition
	// ToolChoice is the default portable tool selection behavior.
	ToolChoice *ToolChoice
	// ToolRepairLimit is the number of invalid tool calls allowed before the next
	// invalid call fails the run. Zero uses the default of one; the hard maximum is two.
	ToolRepairLimit int
}

const (
	defaultToolRepairLimit       = 1
	maxToolRepairLimit           = 2
	toolRepairLimitDocumentation = "number of invalid tool calls allowed before failure"
)

// Agent is an assembled agent instance. It holds no session state and can be
// reused across turns and sessions.
type Agent struct {
	cfg      Config
	model    Model
	caps     Capabilities
	tools    *ToolSet
	detector *LoopDetector
	stop     StopCondition
}

// New assembles an agent. Configuration problems surface at assembly time:
// a loop detection window larger than max_steps would silently disable
// detection, so it must fail immediately.
func New(cfg Config, model Model, registry *Registry) (*Agent, error) {
	if model == nil {
		return nil, fmt.Errorf("%w: model is nil", ErrAgentConfigInvalid)
	}
	tools, err := NewToolSet(registry, cfg.AllowedTools)
	if err != nil {
		return nil, err
	}
	return newAgent(cfg, model, tools, model.Capabilities())
}

func newAgent(cfg Config, model Model, tools *ToolSet, caps Capabilities) (*Agent, error) {
	if model == nil || tools == nil {
		return nil, fmt.Errorf("%w: model or tool snapshot is nil", ErrAgentConfigInvalid)
	}
	if cfg.MaxSteps <= 0 {
		return nil, fmt.Errorf("%w: max_steps must be greater than 0", ErrAgentConfigInvalid)
	}
	if err := caps.Validate(); err != nil {
		return nil, fmt.Errorf("%w: model capabilities: %w", ErrAgentConfigInvalid, err)
	}
	if cfg.ToolRepairLimit < 0 || cfg.ToolRepairLimit > maxToolRepairLimit {
		return nil, fmt.Errorf("%w: tool repair limit must be between 0 and %d", ErrAgentConfigInvalid, maxToolRepairLimit)
	}
	if cfg.ToolRepairLimit == 0 {
		cfg.ToolRepairLimit = defaultToolRepairLimit
	}
	if cfg.ToolChoice != nil {
		if err := cfg.ToolChoice.Validate(tools.Definitions(), caps); err != nil {
			return nil, fmt.Errorf("%w: tool choice: %w", ErrAgentConfigInvalid, err)
		}
	}
	detector := NewLoopDetector(cfg.LoopDetectWindow, cfg.LoopDetectThreshold)
	if err := detector.Validate(cfg.MaxSteps); err != nil {
		return nil, err
	}
	conds := append([]StopCondition{StepCountIs(cfg.MaxSteps)}, cfg.StopConditions...)
	return &Agent{
		cfg:      cfg,
		model:    model,
		caps:     caps,
		tools:    tools,
		detector: detector,
		stop:     AnyStopCondition(conds...),
	}, nil
}

// Key returns the agent identifier.
func (a *Agent) Key() string { return a.cfg.Key }

// ContextWindow returns the configured model context capacity.
func (a *Agent) ContextWindow() int { return a.cfg.ContextWindow }

// RunResult is the result of one Run.
type RunResult struct {
	// Messages contains the messages added during this run (assistant / tool),
	// for the application to persist.
	Messages []Message `json:"messages"`
	// Steps contains per-step details.
	Steps []StepResult `json:"steps"`
	// Usage is the accumulated usage for this run.
	Usage Usage `json:"usage"`
	// Text is the final answer text (the text of the last step).
	Text string `json:"text"`
	// StopReason explains why the run ended, for tuning and troubleshooting.
	StopReason StopReason `json:"stop_reason"`
	// ModelName is the model actually used.
	ModelName string `json:"model_name,omitempty"`
	// Outcome distinguishes complete answers, resumable interruption, and failure.
	Outcome    Outcome        `json:"outcome"`
	Suspension *RunSuspension `json:"suspension,omitempty"`
	// DurableCompletion authorizes the domain owner to atomically finalize a
	// completed durable run with its own records. It is nil for ordinary runs.
	DurableCompletion *DurableCompletion `json:"-"`
	// DurableFailure authorizes the domain owner to atomically finalize a
	// permanent runtime failure with its own record.
	DurableFailure    *DurableFailure    `json:"-"`
	DurableSuspension *DurableSuspension `json:"-"`
	DurableFence      uint64             `json:"-"`
}

type RunSuspension struct {
	Reason StopReason      `json:"reason"`
	Tool   *ToolSuspension `json:"tool,omitempty"`
}

// DurableCompletion is returned after the accepted terminal response has been
// checkpointed. Domain code passes it to CompleteInTransaction.
type DurableCompletion struct {
	Guard      MutationGuard
	Checkpoint Checkpoint
}

type DurableFailure struct {
	Guard      MutationGuard
	Checkpoint Checkpoint
	Failure    RunFailure
}

// DurableSuspension authorizes the domain owner to atomically suspend a
// retryable attempt with its admission and product projection.
type DurableSuspension struct {
	Guard      MutationGuard
	Checkpoint Checkpoint
}

// Outcome is the typed lifecycle result of a run.
type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeSuspended Outcome = "suspended"
	OutcomeFailed    Outcome = "failed"
)

// RunRequest contains v2 runtime inputs. Messages are copied and never mutated.
type RunRequest struct {
	Messages           []Message           `json:"messages"`
	ToolChoice         *ToolChoice         `json:"tool_choice,omitempty"`
	StepPolicy         StepPolicy          `json:"-"`
	ObservationEmitter *ObservationEmitter `json:"-"`
	DurableRun         *DurableRunConfig   `json:"-"`
}

// DurableRunConfig enables checkpointed execution. Leaving it nil preserves the
// ordinary in-memory Run behavior.
type DurableRunConfig struct {
	Identity        RunIdentity
	CheckpointStore CheckpointStore
	LeaseOwner      string
	LeaseDuration   time.Duration
	InputDigest     string
	ConfigDigest    string
	PromptVersion   string
	PolicyVersion   string
	// ToolResume carries the exact persisted blocker being resumed. The
	// authoritative approval decision remains in the permission service.
	ToolResume   *ToolSuspension
	AutoComplete bool
	// DeferFailureFinalization leaves retryable suspension to the domain
	// transaction instead of persisting it independently.
	DeferFailureFinalization bool
	// AttemptAcquired runs immediately after Acquire and before any observation,
	// model call, or tool call. Returning an error aborts the acquired attempt.
	AttemptAcquired func(RunIdentity, uint64) error
	// ObservationEmitterFactory binds attempt metadata only after Acquire returns
	// the authoritative fence token. It must return a non-blocking sink.
	ObservationEmitterFactory func(RunIdentity, uint64) *ObservationEmitter
}

func (r RunRequest) observer() *observer {
	return &observer{sink: r.ObservationEmitter}
}

// PrepareDurableRun returns the immutable values used by durable Run Begin. Domain
// owners use it when run creation must share their claim transaction.
func (a *Agent) PrepareDurableRun(request RunRequest) (string, string, Checkpoint, error) {
	if request.DurableRun == nil {
		return "", "", Checkpoint{}, fmt.Errorf("%w: durable run configuration is required", ErrAgentConfigInvalid)
	}
	if err := ValidateRunRequestCapabilities(request, a.caps); err != nil {
		return "", "", Checkpoint{}, err
	}
	inputDigest, configDigest, err := a.durableDigests(request)
	if err != nil {
		return "", "", Checkpoint{}, err
	}
	return inputDigest, configDigest, Checkpoint{History: cloneMessages(request.Messages)}, nil
}

// ValidateRunRequest centralizes request and supported content validation.
func ValidateRunRequest(req RunRequest) error {
	if len(req.Messages) == 0 {
		return fmt.Errorf("run request has no messages")
	}
	for i, message := range req.Messages {
		if err := ValidateMessage(message); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
	}
	return nil
}

// ValidateRunRequestCapabilities rejects requests requiring unsupported model
// effects before execution starts.
func ValidateRunRequestCapabilities(req RunRequest, caps Capabilities) error {
	if err := ValidateRunRequest(req); err != nil {
		return err
	}
	return validateImageInputCapability(req.Messages, caps)
}

// StopReason is the reason a run ended.
type StopReason string

const (
	// StopReasonComplete means the model stopped calling tools and finished normally.
	StopReasonComplete StopReason = "complete"
	// StopReasonMaxSteps means the maximum step count forced the run to end.
	StopReasonMaxSteps StopReason = "max_steps"
	// StopReasonStopCondition means a stop condition matched.
	StopReasonStopCondition StopReason = "stop_condition"
	// StopReasonToolStopTurn means a tool requested an immediate end of turn.
	StopReasonToolStopTurn StopReason = "tool_stop_turn"
	// StopReasonLoopDetected means a tool call loop was detected.
	StopReasonLoopDetected StopReason = "loop_detected"
	// StopReasonContextBudget means the context budget was exhausted.
	StopReasonContextBudget StopReason = "context_budget"
	// StopReasonOutputLimit indicates the provider reached its generation limit.
	StopReasonOutputLimit StopReason = "output_limit"
	// StopReasonToolSuspended indicates that a tool is waiting on an external blocker.
	StopReasonToolSuspended StopReason = "tool_suspended"
)

// Run executes a runtime request. Observations are best-effort and never
// authoritative; callers use the returned result and error for terminal state.
func (a *Agent) Run(ctx context.Context, request RunRequest) (result *RunResult, runErr error) {
	if request.DurableRun != nil {
		return a.runDurable(ctx, request)
	}
	result = &RunResult{ModelName: a.cfg.ModelName}
	observer := request.observer()
	defer func() {
		if runErr != nil {
			result.Outcome = OutcomeFailed
		}
	}()

	if err := ValidateRunRequestCapabilities(request, a.caps); err != nil {
		return result, err
	}
	// Copy to avoid mutating the caller's slice.
	history := cloneMessages(request.Messages)

	repairErrors := 0

	for step := 0; ; step++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}

		next, err := a.nextRequest(history, result.Steps, request)
		if err != nil {
			return result, err
		}
		resp, err := a.streamStep(ctx, next, observer)
		if err != nil {
			// Propagate upstream errors as-is; never swallow details.
			return result, err
		}

		result.Usage.Add(resp.Usage)
		if resp.ModelName != "" {
			result.ModelName = resp.ModelName
		}
		history = append(history, resp.Message)
		result.Messages = append(result.Messages, resp.Message)

		stepResult := StepResult{
			StepNumber:   step,
			Message:      resp.Message,
			ToolCalls:    resp.ToolCalls(),
			Usage:        resp.Usage,
			FinishReason: resp.FinishReason,
		}

		// Only a stop finish without tool calls is a complete final answer.
		if resp.FinishReason == FinishStop {
			result.Text = resp.Message.Text()
			result.Steps = append(result.Steps, stepResult)
			result.StopReason = StopReasonComplete
			result.Outcome = OutcomeCompleted
			observer.emit(ctx, Observation{Type: ObservationStepFinished, Step: &stepResult})
			return result, nil
		}
		if resp.FinishReason == FinishLength {
			result.Text = resp.Message.Text()
			result.Steps = append(result.Steps, stepResult)
			result.StopReason = StopReasonOutputLimit
			result.Outcome = OutcomeSuspended
			observer.emit(ctx, Observation{Type: ObservationStepFinished, Step: &stepResult})
			return result, nil
		}

		results, stopTurn, invalidCount, err := a.execTools(ctx, next.toolSet, stepResult.ToolCalls, repairErrors, observer)
		repairErrors += invalidCount
		stepResult.ToolResults = results
		if suspension, ok := AsToolSuspension(err); ok {
			result.Text = resp.Message.Text()
			result.Steps = append(result.Steps, stepResult)
			result.StopReason = StopReasonToolSuspended
			result.Outcome = OutcomeSuspended
			result.Suspension = &RunSuspension{Reason: StopReasonToolSuspended, Tool: &suspension}
			observer.emit(ctx, Observation{Type: ObservationStepFinished, Step: &stepResult})
			return result, nil
		}

		toolMsg := NewToolMessage(results...)
		history = append(history, toolMsg)
		result.Messages = append(result.Messages, toolMsg)
		result.Text = resp.Message.Text()
		result.Steps = append(result.Steps, stepResult)

		observer.emit(ctx, Observation{Type: ObservationStepFinished, Step: &stepResult})

		if err != nil {
			return result, err
		}

		// A tool requested end of turn (for example the user denied
		// authorization); finish immediately.
		if stopTurn {
			result.StopReason = StopReasonToolStopTurn
			result.Outcome = OutcomeCompleted
			return result, nil
		}

		// Loop detection: the model may repeatedly call the same tool with the
		// same input, burning steps without making progress.
		if sig, count, detected := a.detector.Detect(result.Steps); detected {
			result.StopReason = StopReasonLoopDetected
			err := fmt.Errorf("%w: signature %s repeated %d times", ErrLoopDetected, sig, count)
			return result, err
		}

		// Context occupancy uses the latest request's prompt tokens; accumulated
		// billed usage cannot represent window occupancy.
		if ContextBudgetExceeded(a.cfg.ContextWindow, resp.Usage.InputTokens()) {
			result.StopReason = StopReasonContextBudget
			result.Outcome = OutcomeSuspended
			return result, nil
		}

		// Stop conditions include StepCountIs(MaxSteps) by default. If the model
		// is still calling tools at the step limit, end the run with the last
		// text as the answer instead of erroring; the application inspects
		// StopReason to decide whether tuning is needed.
		if a.stop(result.Steps) {
			if len(result.Steps) >= a.cfg.MaxSteps {
				result.StopReason = StopReasonMaxSteps
			} else {
				result.StopReason = StopReasonStopCondition
			}
			result.Outcome = OutcomeSuspended
			return result, nil
		}
	}
}

func (a *Agent) runDurable(ctx context.Context, request RunRequest) (result *RunResult, runErr error) {
	result = &RunResult{ModelName: a.cfg.ModelName}
	observer := request.observer()
	defer func() {
		if runErr != nil {
			result.Outcome = OutcomeFailed
		}
		if observer.sink != nil {
			observer.sink.closeAdmission()
		}
	}()
	if err := ValidateRunRequestCapabilities(request, a.caps); err != nil {
		return result, err
	}
	durable := request.DurableRun
	if durable.CheckpointStore == nil || durable.Identity.RunKey == "" || durable.Identity.AgentKey == "" || durable.Identity.SessionID == "" || durable.Identity.RequestID == "" || durable.LeaseOwner == "" || durable.LeaseDuration <= 0 {
		return result, fmt.Errorf("%w: invalid durable run configuration", ErrAgentConfigInvalid)
	}
	inputDigest, configDigest, initial, err := a.PrepareDurableRun(request)
	if err != nil {
		return result, err
	}
	snapshot, err := durable.CheckpointStore.Begin(ctx, durable.Identity, inputDigest, configDigest, initial)
	if err != nil {
		return result, fmt.Errorf("begin durable run: %w", err)
	}
	if _, err := durableToolResume(snapshot.Checkpoint, durable.ToolResume); err != nil {
		return result, err
	}
	if snapshot.Status == RunStatusCompleted {
		stored := snapshot.Checkpoint.Outcome
		stored.DurableCompletion = nil
		return &stored, nil
	}
	if snapshot.Status == RunStatusFailed || snapshot.Status == RunStatusAbandoned {
		return result, fmt.Errorf("agent: durable run is %s", snapshot.Status)
	}
	snapshot, err = durable.CheckpointStore.Acquire(ctx, durable.Identity.RunKey, durable.LeaseOwner, time.Now().Add(durable.LeaseDuration))
	if err != nil {
		loaded, loadErr := durable.CheckpointStore.Load(ctx, durable.Identity.RunKey)
		if loadErr == nil && loaded.Status == RunStatusCompleted {
			stored := loaded.Checkpoint.Outcome
			return &stored, nil
		}
		return result, fmt.Errorf("acquire durable run: %w", errors.Join(err, loadErr))
	}
	result.DurableFence = snapshot.FenceToken
	if durable.AttemptAcquired != nil {
		if err := durable.AttemptAcquired(snapshot.Identity, snapshot.FenceToken); err != nil {
			return result, fmt.Errorf("bind durable attempt: %w", err)
		}
	}
	if durable.ObservationEmitterFactory != nil {
		observer.sink = durable.ObservationEmitterFactory(snapshot.Identity, snapshot.FenceToken)
	}
	cp := snapshot.Checkpoint.Clone()
	result = resultFromCheckpoint(cp, a.cfg.ModelName)
	result.DurableFence = snapshot.FenceToken
	if snapshot.Phase == RunPhaseFinalizing {
		result.DurableCompletion = &DurableCompletion{Guard: snapshot.Guard(), Checkpoint: cp.Clone()}
		return result, nil
	}

	fail := func(cause error, suspend bool) error {
		result.Outcome = OutcomeFailed
		cp.Outcome = *result
		cp.Outcome.DurableCompletion = nil
		var persistErr error
		if suspend {
			if durable.DeferFailureFinalization {
				result.DurableSuspension = &DurableSuspension{Guard: snapshot.Guard(), Checkpoint: cp.Clone()}
			} else {
				_, persistErr = durable.CheckpointStore.Suspend(context.WithoutCancel(ctx), snapshot.Guard(), cp)
			}
		} else {
			failure := RunFailure{Message: cause.Error()}
			result.DurableFailure = &DurableFailure{Guard: snapshot.Guard(), Checkpoint: cp.Clone(), Failure: failure}
		}
		if persistErr != nil {
			return errors.Join(cause, persistErr)
		}
		return cause
	}

	for {
		if err := ctx.Err(); err != nil {
			return result, fail(err, true)
		}
		if snapshot.Phase == RunPhaseToolsReady || snapshot.Phase == RunPhaseToolInflight {
			var toolErr error
			snapshot, cp, toolErr = a.resumeDurableTools(ctx, request, snapshot, cp, observer)
			*result = *resultFromCheckpoint(cp, a.cfg.ModelName)
			result.DurableFence = snapshot.FenceToken
			if toolErr != nil {
				return result, fail(fmt.Errorf("resume durable tools: %w", toolErr), errors.Is(toolErr, ErrToolExecutionUnknown) || errors.Is(toolErr, context.Canceled) || errors.Is(toolErr, context.DeadlineExceeded))
			}
			if result.Outcome == OutcomeCompleted || result.Outcome == OutcomeSuspended {
				break
			}
		}
		if snapshot.Phase == RunPhaseModelReady {
			snapshot, err = durable.CheckpointStore.ModelInflight(ctx, snapshot.Guard(), cp)
			if err != nil {
				return result, fail(fmt.Errorf("checkpoint model inflight: %w", err), false)
			}
		}
		next, err := a.nextRequest(cp.History, cp.CompletedSteps, request)
		if err != nil {
			return result, fail(fmt.Errorf("commit model response: %w", err), false)
		}
		resp, err := a.streamStep(ctx, next, observer)
		if err != nil {
			return result, fail(err, retryableModelError(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
		}
		step := StepResult{StepNumber: cp.NextStep, Message: resp.Message, ToolCalls: resp.ToolCalls(), Usage: resp.Usage, FinishReason: resp.FinishReason}
		cp.History = append(cp.History, resp.Message)
		cp.NewMessages = append(cp.NewMessages, resp.Message)
		cp.Usage.Add(resp.Usage)
		cp.CompletedSteps = append(cp.CompletedSteps, step)
		cp.PendingToolCalls = append([]ToolCall(nil), step.ToolCalls...)
		if resp.FinishReason == FinishStop {
			cp.NextStep++
			cp.Outcome = RunResult{Messages: cloneMessages(cp.NewMessages), Steps: cloneSteps(cp.CompletedSteps), Usage: cp.Usage, Text: resp.Message.Text(), StopReason: StopReasonComplete, ModelName: responseModelName(resp, a.cfg.ModelName), Outcome: OutcomeCompleted}
		} else if resp.FinishReason == FinishLength {
			cp.NextStep++
			cp.Outcome = RunResult{Messages: cloneMessages(cp.NewMessages), Steps: cloneSteps(cp.CompletedSteps), Usage: cp.Usage, Text: resp.Message.Text(), StopReason: StopReasonOutputLimit, ModelName: responseModelName(resp, a.cfg.ModelName), Outcome: OutcomeSuspended}
		}
		snapshot, err = durable.CheckpointStore.CommitModelResponse(ctx, snapshot.Guard(), *resp, cp)
		if err != nil {
			return result, fail(err, false)
		}
		*result = *resultFromCheckpoint(cp, responseModelName(resp, a.cfg.ModelName))
		result.DurableFence = snapshot.FenceToken
		if resp.FinishReason == FinishStop || resp.FinishReason == FinishLength {
			observer.emit(ctx, Observation{Type: ObservationStepFinished, Step: &step})
			break
		}
	}
	if result.Outcome == OutcomeSuspended {
		if _, err := durable.CheckpointStore.Suspend(ctx, snapshot.Guard(), cp); err != nil {
			return result, errors.Join(fmt.Errorf("agent: suspend durable run"), err)
		}
		return result, nil
	}
	result.DurableCompletion = &DurableCompletion{Guard: snapshot.Guard(), Checkpoint: cp.Clone()}
	if durable.AutoComplete {
		completed, err := durable.CheckpointStore.Complete(ctx, snapshot.Guard(), cp)
		if err != nil {
			return result, err
		}
		result.DurableCompletion = nil
		_ = completed
	}
	return result, nil
}

func (a *Agent) durableDigests(request RunRequest) (string, string, error) {
	durable := request.DurableRun
	inputDigest := durable.InputDigest
	var err error
	if inputDigest == "" {
		inputDigest, err = DigestRunInput(ImmutableRunInput{Messages: request.Messages, ToolChoice: request.ToolChoice})
		if err != nil {
			return "", "", err
		}
	}
	configDigest := durable.ConfigDigest
	if configDigest == "" {
		choice := request.ToolChoice
		if choice == nil {
			choice = a.cfg.ToolChoice
		}
		configDigest, err = DigestRunConfig(ImmutableRunConfig{
			AgentKey: a.cfg.Key, ModelName: a.cfg.ModelName, MaxSteps: a.cfg.MaxSteps, ContextWindow: a.cfg.ContextWindow,
			LoopDetectWindow: a.cfg.LoopDetectWindow, LoopDetectThreshold: a.cfg.LoopDetectThreshold, ToolRepairLimit: a.cfg.ToolRepairLimit,
			Generation: GenerationOptions{Temperature: a.cfg.Temperature, MaxTokens: a.cfg.MaxTokens, TopP: a.cfg.TopP}, ToolChoice: choice,
			Tools: a.tools.Definitions(), PromptVersion: durable.PromptVersion, PolicyVersion: durable.PolicyVersion,
		})
	}
	return inputDigest, configDigest, err
}

func resultFromCheckpoint(checkpoint Checkpoint, modelName string) *RunResult {
	result := checkpoint.Outcome
	result.Messages = cloneMessages(checkpoint.NewMessages)
	result.Steps = cloneSteps(checkpoint.CompletedSteps)
	result.Usage = checkpoint.Usage
	if result.ModelName == "" {
		result.ModelName = modelName
	}
	return &result
}

func responseModelName(response *Response, fallback string) string {
	if response.ModelName != "" {
		return response.ModelName
	}
	return fallback
}

func (a *Agent) resumeDurableTools(ctx context.Context, request RunRequest, snapshot RunSnapshot, checkpoint Checkpoint, observer *observer) (RunSnapshot, Checkpoint, error) {
	if len(checkpoint.PendingToolCalls) == 0 || len(checkpoint.CompletedSteps) == 0 {
		return snapshot, checkpoint, fmt.Errorf("%w: tools phase has no pending calls", ErrInvalidRunTransition)
	}
	stepIndex := len(checkpoint.CompletedSteps) - 1
	step := checkpoint.CompletedSteps[stepIndex]
	resume, err := durableToolResume(checkpoint, request.DurableRun.ToolResume)
	if err != nil {
		return snapshot, checkpoint, err
	}
	if checkpoint.Outcome.StopReason == StopReasonToolSuspended && resume == nil {
		return snapshot, checkpoint, nil
	}
	if resume != nil {
		checkpoint.Outcome = RunResult{}
	}
	next, err := a.nextRequest(checkpoint.History[:len(checkpoint.History)-1], checkpoint.CompletedSteps[:stepIndex], request)
	if err != nil {
		return snapshot, checkpoint, err
	}
	executions := make([]ToolExecution, len(checkpoint.PendingToolCalls))
	for i, call := range checkpoint.PendingToolCalls {
		executions[i], err = NewToolExecution(snapshot.Identity, step.StepNumber, i, call)
		if err != nil {
			return snapshot, checkpoint, err
		}
	}
	if snapshot.Phase == RunPhaseToolsReady {
		snapshot, err = request.DurableRun.CheckpointStore.PrepareTools(ctx, snapshot.Guard(), executions, checkpoint)
		if err != nil {
			return snapshot, checkpoint, err
		}
	}

	if len(step.ToolResults) > len(executions) {
		return snapshot, checkpoint, fmt.Errorf("%w: tool results exceed pending calls", ErrInvalidRunTransition)
	}
	results := append([]ToolResult(nil), step.ToolResults...)
	stopTurn := false
	for i := len(results); i < len(executions); i++ {
		call := executions[i].ToolCall
		backendOwned := next.toolSet.ownsExecutionLifecycle(call.Name)
		execution := executions[i]
		if !backendOwned {
			safeReplay := next.toolSet.replayPolicy(call.Name) == ReplayPolicyIdempotent
			snapshot, execution, err = request.DurableRun.CheckpointStore.BeginTool(ctx, snapshot.Guard(), executions[i].IdempotencyKey, safeReplay)
			if err != nil {
				return snapshot, checkpoint, err
			}
		}
		var result ToolResult
		invalid := false
		if execution.Status == ToolExecutionCompleted && execution.Result != nil {
			result = *execution.Result
		} else {
			observer.emit(ctx, Observation{Type: ObservationToolCall, ToolCall: &call})
			var callResume *ToolSuspension
			if resume != nil && resume.StepNumber == uint32(step.StepNumber) && resume.Ordinal == uint32(i) {
				callResume = resume
			}
			result, invalid, err = a.execOneWithResume(ctx, next.toolSet, call, execution.IdempotencyKey, callResume)
			if suspension, ok := AsToolSuspension(err); ok {
				suspension.StepNumber = uint32(step.StepNumber)
				suspension.Ordinal = uint32(i)
				step.ToolResults = append([]ToolResult(nil), results...)
				checkpoint.CompletedSteps[stepIndex] = step
				checkpoint.Outcome = *resultFromCheckpoint(checkpoint, a.cfg.ModelName)
				checkpoint.Outcome.Text = step.Message.Text()
				checkpoint.Outcome.StopReason = StopReasonToolSuspended
				checkpoint.Outcome.Outcome = OutcomeSuspended
				checkpoint.Outcome.Suspension = &RunSuspension{Reason: StopReasonToolSuspended, Tool: &suspension}
				observer.emit(ctx, Observation{Type: ObservationStepFinished, Step: &step})
				return snapshot, checkpoint, nil
			}
			if err != nil {
				return snapshot, checkpoint, err
			}
			execution.Status = ToolExecutionCompleted
			execution.Result = &result
		}
		results = append(results, result)
		if invalid {
			checkpoint.RepairCount++
		}
		if result.StopTurn {
			stopTurn = true
		}
		step.ToolResults = append([]ToolResult(nil), results...)
		checkpoint.CompletedSteps[stepIndex] = step
		last := i == len(executions)-1
		if last {
			toolMessage := NewToolMessage(results...)
			checkpoint.History = append(checkpoint.History, toolMessage)
			checkpoint.NewMessages = append(checkpoint.NewMessages, toolMessage)
			checkpoint.PendingToolCalls = nil
			checkpoint.NextStep = step.StepNumber + 1
			if stopTurn {
				checkpoint.Outcome = *resultFromCheckpoint(checkpoint, a.cfg.ModelName)
				checkpoint.Outcome.Text = step.Message.Text()
				checkpoint.Outcome.StopReason = StopReasonToolStopTurn
				checkpoint.Outcome.Outcome = OutcomeCompleted
			}
		}
		snapshot, err = request.DurableRun.CheckpointStore.CommitTool(ctx, snapshot.Guard(), execution, checkpoint)
		if err != nil {
			return snapshot, checkpoint, err
		}
		observer.emit(ctx, Observation{Type: ObservationToolResult, ToolResult: &result})
		if checkpoint.RepairCount > a.cfg.ToolRepairLimit {
			return snapshot, checkpoint, fmt.Errorf("%w: repair budget %d exhausted", ErrToolInputInvalid, a.cfg.ToolRepairLimit)
		}
	}
	observer.emit(ctx, Observation{Type: ObservationStepFinished, Step: &step})
	if stopTurn {
		return snapshot, checkpoint, nil
	}
	if sig, count, detected := a.detector.Detect(checkpoint.CompletedSteps); detected {
		return snapshot, checkpoint, fmt.Errorf("%w: signature %s repeated %d times", ErrLoopDetected, sig, count)
	}
	if ContextBudgetExceeded(a.cfg.ContextWindow, step.Usage.InputTokens()) {
		checkpoint.Outcome = *resultFromCheckpoint(checkpoint, a.cfg.ModelName)
		checkpoint.Outcome.StopReason, checkpoint.Outcome.Outcome = StopReasonContextBudget, OutcomeSuspended
		return snapshot, checkpoint, nil
	}
	if a.stop(checkpoint.CompletedSteps) {
		checkpoint.Outcome = *resultFromCheckpoint(checkpoint, a.cfg.ModelName)
		checkpoint.Outcome.Outcome = OutcomeSuspended
		if len(checkpoint.CompletedSteps) >= a.cfg.MaxSteps {
			checkpoint.Outcome.StopReason = StopReasonMaxSteps
		} else {
			checkpoint.Outcome.StopReason = StopReasonStopCondition
		}
	}
	return snapshot, checkpoint, nil
}

func durableToolResume(checkpoint Checkpoint, supplied *ToolSuspension) (*ToolSuspension, error) {
	if checkpoint.Outcome.StopReason != StopReasonToolSuspended || checkpoint.Outcome.Suspension == nil || checkpoint.Outcome.Suspension.Tool == nil {
		if supplied != nil {
			return nil, fmt.Errorf("%w: tool resume has no persisted suspension", ErrAgentConfigInvalid)
		}
		return nil, nil
	}
	persisted := checkpoint.Outcome.Suspension.Tool
	if supplied == nil {
		return nil, nil
	}
	if *supplied != *persisted {
		return nil, fmt.Errorf("%w: tool resume does not match persisted suspension", ErrAgentConfigInvalid)
	}
	resume := *supplied
	return &resume, nil
}

type stepRequest struct {
	request *GenerateRequest
	toolSet *ToolSet
	choice  ToolChoice
}

func (a *Agent) nextRequest(history []Message, steps []StepResult, run RunRequest) (stepRequest, error) {
	toolSet := a.tools
	choice := run.ToolChoice
	if choice == nil {
		choice = a.cfg.ToolChoice
	}
	modelName := a.cfg.ModelName
	generation := GenerationOptions{Temperature: a.cfg.Temperature, MaxTokens: a.cfg.MaxTokens, TopP: a.cfg.TopP}
	if run.StepPolicy != nil {
		decision, err := run.StepPolicy(StepPolicyInput{
			CompletedSteps: cloneSteps(steps),
			AvailableTools: append([]ToolDefinition(nil), a.tools.Definitions()...),
		})
		if err != nil {
			return stepRequest{}, fmt.Errorf("step policy: %w", err)
		}
		toolSet, err = a.tools.Subset(decision.ActiveTools)
		if err != nil {
			return stepRequest{}, err
		}
		if decision.ToolChoice != nil {
			choice = decision.ToolChoice
		}
		if decision.Model != "" {
			modelName = decision.Model
		}
		if decision.Generation.Temperature != nil {
			generation.Temperature = decision.Generation.Temperature
		}
		if decision.Generation.MaxTokens != nil {
			generation.MaxTokens = decision.Generation.MaxTokens
		}
		if decision.Generation.TopP != nil {
			generation.TopP = decision.Generation.TopP
		}
	}
	definitions := toolSet.Definitions()
	validatedChoice := ToolChoice{Mode: ToolChoiceAuto}
	if choice != nil {
		validatedChoice = *choice
	}
	if err := validatedChoice.Validate(definitions, a.caps); err != nil {
		return stepRequest{}, fmt.Errorf("%w: %w", ErrAgentConfigInvalid, err)
	}
	request := &GenerateRequest{
		Model: modelName, Messages: history, Tools: definitions, ToolChoice: choice,
		Temperature: generation.Temperature, MaxTokens: generation.MaxTokens, TopP: generation.TopP,
	}
	if err := ValidateGenerateRequestCapabilities(request, a.caps); err != nil {
		return stepRequest{}, err
	}
	return stepRequest{request: request, toolSet: toolSet, choice: validatedChoice}, nil
}

// streamStep runs one streamed step, turning text deltas into observations and
// returning the aggregated response.
func (a *Agent) streamStep(ctx context.Context, next stepRequest, observer *observer) (*Response, error) {
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	chunks, err := a.model.Stream(stepCtx, next.request)
	if err != nil {
		return nil, normalizeModelError(err, "model stream could not be opened")
	}

	var (
		resp  *Response
		text  strings.Builder
		calls []ToolCall
	)
	for {
		var chunk StreamChunk
		var ok bool
		select {
		case <-stepCtx.Done():
			return nil, stepCtx.Err()
		case chunk, ok = <-chunks:
		}
		if !ok {
			break
		}
		if resp != nil {
			return nil, newProtocolError("chunk received after terminal response", nil)
		}
		switch chunk.Type {
		case ChunkText:
			text.WriteString(chunk.TextDelta)
			observer.emit(ctx, Observation{Type: ObservationTextDelta, Text: chunk.TextDelta})
		case ChunkToolCall:
			if chunk.ToolCall != nil {
				calls = append(calls, *chunk.ToolCall)
			}
		case ChunkFinish:
			if resp != nil {
				return nil, newProtocolError("duplicate terminal response", nil)
			}
			resp = chunk.Response
		case ChunkError:
			if chunk.Err != nil {
				return nil, normalizeModelError(chunk.Err, "model stream failed")
			}
			return nil, newProtocolError("error chunk missing cause", nil)
		default:
			return nil, newProtocolError(fmt.Sprintf("unknown stream chunk type %q", chunk.Type), nil)
		}
	}

	if resp == nil {
		return nil, newProtocolError("stream closed before terminal response", nil)
	}
	if resp.Message.FinishReason == "" {
		resp.Message.FinishReason = resp.FinishReason
	}
	if err := ValidateResponse(resp); err != nil {
		return nil, newProtocolError("invalid terminal response", err)
	}
	if err := validateEffectiveToolChoice(next.choice, resp); err != nil {
		return nil, newProtocolError("tool choice violation", err)
	}
	if text.String() != resp.Message.Text() || !reflect.DeepEqual(calls, resp.ToolCalls()) {
		return nil, newProtocolError("streamed chunks do not match terminal response: every text delta and tool call must add up to the terminal response; adapters holding a complete response should return agent.StreamResponse(response)", nil)
	}
	if err := resp.Usage.Validate(); err != nil {
		return nil, newProtocolError("invalid usage", err)
	}
	return resp, nil
}

// assembleMessage builds an assistant message from text and tool calls.
func assembleMessage(text string, calls []ToolCall) Message {
	msg := Message{Role: RoleAssistant}
	if text != "" {
		msg.Parts = append(msg.Parts, ContentPart{Type: PartText, Text: text})
	}
	for i := range calls {
		msg.Parts = append(msg.Parts, ContentPart{Type: PartToolCall, ToolCall: &calls[i]})
	}
	return msg
}

// execTools sequentially executes one accepted tool batch.
func (a *Agent) execTools(ctx context.Context, toolSet *ToolSet, calls []ToolCall, priorInvalid int, observer *observer) ([]ToolResult, bool, int, error) {
	results := make([]ToolResult, 0, len(calls))
	var stopTurn bool
	var invalidCount int

	for i := range calls {
		call := calls[i]
		observer.emit(ctx, Observation{Type: ObservationToolCall, ToolCall: &call})

		res, invalid, err := a.execOne(ctx, toolSet, call, "")
		if err != nil {
			return results, stopTurn, invalidCount, err
		}
		if invalid {
			invalidCount++
		}
		if res.StopTurn {
			stopTurn = true
		}
		results = append(results, res)

		observer.emit(ctx, Observation{Type: ObservationToolResult, ToolResult: &res})
		if priorInvalid+invalidCount > a.cfg.ToolRepairLimit {
			return results, stopTurn, invalidCount, fmt.Errorf("%w: repair budget %d exhausted", ErrToolInputInvalid, a.cfg.ToolRepairLimit)
		}
	}
	return results, stopTurn, invalidCount, nil
}

// execOne keeps availability/input failures model-visible and returns Tool Go
// errors as fatal attempt errors.
func (a *Agent) execOne(ctx context.Context, toolSet *ToolSet, call ToolCall, executionKey string) (ToolResult, bool, error) {
	return a.execOneWithResume(ctx, toolSet, call, executionKey, nil)
}

func (a *Agent) execOneWithResume(ctx context.Context, toolSet *ToolSet, call ToolCall, executionKey string, resume *ToolSuspension) (ToolResult, bool, error) {
	errResult := func(content string) ToolResult {
		return ToolResult{ToolCallID: call.ID, Name: call.Name, Content: content, IsError: true}
	}

	tool, err := toolSet.Get(call.Name)
	if err != nil {
		return errResult(fmt.Sprintf("Tool %s is unavailable: %s. Use another available tool instead.", call.Name, err.Error())), false, nil
	}

	if err := toolSet.validate(call.Name, call.Input); err != nil {
		return errResult(fmt.Sprintf("%s. Reply with a valid JSON object that conforms to the tool's JSON Schema.", err.Error())), true, nil
	}

	res, err := tool.Execute(ctx, ToolInvocation{CallID: call.ID, Name: call.Name, RawInput: call.Input, ExecutionKey: executionKey, Resume: resume})
	if err != nil {
		if _, ok := AsToolSuspension(err); ok {
			return ToolResult{}, false, err
		}
		return ToolResult{}, false, fmt.Errorf("execute tool %q: %w", call.Name, err)
	}
	// Fill in pairing metadata so tool implementations do not have to.
	res.ToolCallID = call.ID
	if res.Name == "" {
		res.Name = call.Name
	}
	return res, false, nil
}

func cloneSteps(steps []StepResult) []StepResult {
	cloned := make([]StepResult, len(steps))
	for i, step := range steps {
		cloned[i] = step
		cloned[i].Message = cloneMessages([]Message{step.Message})[0]
		cloned[i].ToolCalls = append([]ToolCall(nil), step.ToolCalls...)
		cloned[i].ToolResults = append([]ToolResult(nil), step.ToolResults...)
	}
	return cloned
}
