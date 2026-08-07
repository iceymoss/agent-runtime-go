package tool

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
)

const maxRewrites = 2

type ExecutorOptions struct {
	Generation *Generation
	Ledger     ExecutionLedger
	Permission permission.Service
	Observer   Observer
	Now        func() time.Time
}

type Executor struct {
	generation *Generation
	ledger     ExecutionLedger
	permission permission.Service
	observer   Observer
	now        func() time.Time
}

func NewExecutor(options ExecutorOptions) (*Executor, error) {
	if options.Generation == nil || options.Ledger == nil {
		return nil, lifecycleError(ErrInvalidConfiguration, nil, "new executor", "", "generation and ledger are required")
	}
	if options.Now == nil {
		options.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Executor{generation: options.Generation, ledger: options.Ledger, permission: options.Permission, observer: options.Observer, now: options.Now}, nil
}

func (e *Executor) Execute(ctx context.Context, request ExecuteRequest) (ExecuteResult, error) {
	if err := validateInvocation(request.Invocation); err != nil {
		return ExecuteResult{}, err
	}
	e.observe(PhasePrepare, PreparedExecution{ToolName: request.Invocation.ToolName, StepNumber: request.Invocation.StepNumber, Ordinal: request.Invocation.Ordinal})
	prepared, entry, entered, immediate, err := e.prepare(ctx, request.Invocation)
	if err != nil {
		return ExecuteResult{}, err
	}
	if immediate != nil {
		return ExecuteResult{Prepared: prepared, Status: StatusSucceeded, Result: immediate}, nil
	}
	record, created, err := e.ledger.Prepare(ctx, prepared)
	if err != nil {
		return ExecuteResult{Prepared: prepared}, lifecycleError(ErrExecutionConflict, err, "prepare ledger", prepared.ExecutionKey, "immutable execution differs or fence is stale")
	}
	record = cloneRecord(record)
	if !created || record.Status != StatusPrepared {
		return e.existing(record)
	}
	e.observe(PhaseAuthorize, prepared)
	authorization, err := e.authorize(ctx, request, prepared)
	if err != nil {
		hookErr := e.runErrorHooks(ctx, entered, prepared, err)
		if hookErr != nil {
			err = errors.Join(err, hookErr)
		}
		if errors.Is(err, ErrPermissionDenied) {
			failure := Failure{Code: "permission_denied", Message: "permission denied"}
			if _, rejectErr := e.ledger.Reject(ctx, prepared.ExecutionKey, prepared.FenceToken, failure); rejectErr != nil {
				return ExecuteResult{Prepared: prepared, Status: StatusPrepared}, errors.Join(err, lifecycleError(ErrExecutionUnknown, rejectErr, "record denial", prepared.ExecutionKey, "permission denial could not be recorded"))
			}
			return ExecuteResult{Prepared: prepared, Status: StatusFailed}, err
		}
		return ExecuteResult{Prepared: prepared, Status: StatusPrepared}, err
	}
	if authorization != nil {
		return *authorization, lifecycleError(ErrApprovalPending, nil, "authorize", prepared.ExecutionKey, "approval is required")
	}
	return e.executePrepared(ctx, prepared, entry, entered)
}

// ResumeApproval revalidates one resolved approval against the exact prepared
// execution before crossing the effect boundary.
func (e *Executor) ResumeApproval(ctx context.Context, request ResumeApprovalRequest) (ExecuteResult, error) {
	if e.permission == nil || request.Approval.RequestRef == "" || request.Approval.ResumeToken == "" || request.Approval.Revision == 0 {
		return ExecuteResult{}, lifecycleError(ErrInvalidConfiguration, nil, "resume approval", "", "permission service and complete approval receipt are required")
	}
	if err := validateInvocation(request.Execute.Invocation); err != nil {
		return ExecuteResult{}, err
	}
	if request.ExecutionKey != "" {
		record, err := e.ledger.Load(ctx, request.ExecutionKey)
		if err != nil {
			return ExecuteResult{}, lifecycleError(ErrExecutionConflict, err, "resume approval", request.ExecutionKey, "prepared execution is unavailable")
		}
		if err := validateResumeInvocation(request.Execute.Invocation, record.Prepared); err != nil {
			return ExecuteResult{Prepared: record.Prepared, Status: record.Status}, err
		}
		request.Execute.Invocation = invocationFromPrepared(record.Prepared)
	}
	prepared, entry, entered, immediate, err := e.prepare(ctx, request.Execute.Invocation)
	if err != nil {
		return ExecuteResult{}, err
	}
	if immediate != nil {
		return ExecuteResult{}, lifecycleError(ErrExecutionConflict, nil, "resume approval", prepared.ExecutionKey, "preflight no longer requires execution")
	}
	record, err := e.ledger.Load(ctx, prepared.ExecutionKey)
	if err != nil {
		return ExecuteResult{Prepared: prepared}, lifecycleError(ErrExecutionConflict, err, "resume approval", prepared.ExecutionKey, "prepared execution is unavailable")
	}
	if !samePrepared(record.Prepared, prepared) {
		return ExecuteResult{Prepared: prepared, Status: record.Status}, lifecycleError(ErrExecutionConflict, nil, "resume approval", prepared.ExecutionKey, "prepared execution changed")
	}
	if record.Status != StatusPrepared {
		return e.existing(record)
	}
	result, err := e.permission.Revalidate(ctx, permission.RevalidateCommand{TenantKey: prepared.TenantKey, RequestKey: request.Approval.RequestRef, ResumeToken: request.Approval.ResumeToken, AttemptRef: permission.AttemptRef(prepared.AttemptKey), FenceToken: prepared.FenceToken, InputDigest: permission.InputDigest(prepared.InputDigest), PolicyVersion: request.Execute.PolicyVersion, ToolGeneration: prepared.ToolGeneration})
	if err != nil {
		if errors.Is(err, permission.ErrPermissionDenied) || errors.Is(err, permission.ErrRequestExpired) || errors.Is(err, permission.ErrRequestCanceled) {
			failure := Failure{Code: "approval_denied", Message: "approval did not authorize execution"}
			if _, rejectErr := e.ledger.Reject(ctx, prepared.ExecutionKey, prepared.FenceToken, failure); rejectErr != nil {
				return ExecuteResult{Prepared: prepared, Status: StatusPrepared}, errors.Join(lifecycleError(ErrAuthorizationFailed, err, "resume approval", prepared.ExecutionKey, "approval revalidation failed"), lifecycleError(ErrExecutionUnknown, rejectErr, "record denial", prepared.ExecutionKey, "approval denial could not be recorded"))
			}
			return ExecuteResult{Prepared: prepared, Status: StatusFailed}, lifecycleError(ErrPermissionDenied, err, "resume approval", prepared.ExecutionKey, "approval did not authorize execution")
		}
		return ExecuteResult{Prepared: prepared, Status: StatusPrepared}, lifecycleError(ErrAuthorizationFailed, err, "resume approval", prepared.ExecutionKey, "approval revalidation failed")
	}
	if result.Decision != permission.DecisionAllow {
		return ExecuteResult{Prepared: prepared, Status: StatusPrepared}, lifecycleError(ErrAuthorizationFailed, nil, "resume approval", prepared.ExecutionKey, "approval revalidation did not allow execution")
	}
	return e.executePrepared(ctx, prepared, entry, entered)
}

func validateResumeInvocation(current InvocationIdentity, prepared PreparedExecution) error {
	if current.TenantKey != prepared.TenantKey || current.RunKey != prepared.RunKey || current.StepNumber != prepared.StepNumber || current.Ordinal != prepared.Ordinal || current.CallID != prepared.CallID || current.ToolName != prepared.ToolName || current.RawInput != prepared.RawInput || current.PrincipalKey != prepared.PrincipalKey || current.SessionRef != prepared.SessionRef {
		return lifecycleError(ErrExecutionConflict, nil, "resume approval", prepared.ExecutionKey, "invocation identity changed")
	}
	currentResource, currentErr := agent.CanonicalDigest(current.Resource)
	preparedResource, preparedErr := agent.CanonicalDigest(prepared.Resource)
	if currentErr != nil || preparedErr != nil || currentResource != preparedResource {
		return lifecycleError(ErrExecutionConflict, errors.Join(currentErr, preparedErr), "resume approval", prepared.ExecutionKey, "resource changed")
	}
	return nil
}

func invocationFromPrepared(prepared PreparedExecution) InvocationIdentity {
	return InvocationIdentity{TenantKey: prepared.TenantKey, RunKey: prepared.RunKey, AttemptKey: prepared.AttemptKey, FenceToken: prepared.FenceToken, StepNumber: prepared.StepNumber, Ordinal: prepared.Ordinal, CallID: prepared.CallID, ToolName: prepared.ToolName, RawInput: prepared.RawInput, PrincipalKey: prepared.PrincipalKey, SessionRef: prepared.SessionRef, Resource: prepared.Resource}
}

func (e *Executor) executePrepared(ctx context.Context, prepared PreparedExecution, entry registration, entered []Interceptor) (ExecuteResult, error) {
	record, err := e.ledger.Begin(ctx, prepared.ExecutionKey, prepared.FenceToken)
	if err != nil {
		return ExecuteResult{Prepared: prepared}, lifecycleError(ErrStaleFence, err, "begin", prepared.ExecutionKey, "execution could not cross the effect boundary")
	}
	if record.Status != StatusRunning || record.FenceToken != prepared.FenceToken {
		return ExecuteResult{Prepared: prepared}, lifecycleError(ErrExecutionConflict, nil, "begin", prepared.ExecutionKey, "ledger did not return the running execution")
	}
	e.observe(PhaseExecute, prepared)
	result, toolErr := executeTool(ctx, entry.tool, prepared)
	if toolErr != nil {
		return e.finishError(ctx, entered, prepared, toolErr)
	}
	result, err = e.runAfter(ctx, entered, prepared, result)
	if err != nil {
		if hookErr := e.runErrorHooks(ctx, entered, prepared, err); hookErr != nil {
			err = errors.Join(err, hookErr)
		}
		failure := Failure{Code: "interceptor_failed", Message: "tool result processing failed"}
		if _, markErr := e.ledger.MarkUnknown(ctx, prepared.ExecutionKey, prepared.FenceToken, failure); markErr != nil {
			err = errors.Join(err, lifecycleError(ErrExecutionUnknown, markErr, "mark unknown", prepared.ExecutionKey, "effect completed but result could not be recorded"))
		}
		return ExecuteResult{Prepared: prepared, Status: StatusUnknown}, err
	}
	e.observe(PhaseRecord, prepared)
	record, err = e.ledger.Complete(ctx, CompleteExecution{ExecutionKey: prepared.ExecutionKey, FenceToken: prepared.FenceToken, Result: &result})
	if err != nil {
		return ExecuteResult{Prepared: prepared, Status: StatusUnknown}, lifecycleError(ErrExecutionUnknown, err, "complete", prepared.ExecutionKey, "effect finished but durable completion failed")
	}
	e.observe(PhaseComplete, prepared)
	completed := cloneResult(result)
	return ExecuteResult{Prepared: prepared, Status: record.Status, Result: &completed}, nil
}

func samePrepared(left, right PreparedExecution) bool {
	leftDigest, leftErr := agent.CanonicalDigest(left)
	rightDigest, rightErr := agent.CanonicalDigest(right)
	return leftErr == nil && rightErr == nil && leftDigest == rightDigest
}

func (e *Executor) prepare(ctx context.Context, invocation InvocationIdentity) (PreparedExecution, registration, []Interceptor, *agent.ToolResult, error) {
	current := invocation
	seen := make(map[string]struct{}, maxRewrites+1)
	for rewrites := 0; ; rewrites++ {
		entry, ok := e.generation.tools[current.ToolName]
		if !ok {
			return PreparedExecution{}, registration{}, nil, nil, lifecycleError(ErrToolNotFound, nil, "lookup", "", "tool is not in the frozen generation")
		}
		canonical, digest, err := canonicalInput(entry, current.RawInput)
		if err != nil {
			return PreparedExecution{}, registration{}, nil, nil, err
		}
		semantic := current.ToolName + "\x00" + digest
		if _, exists := seen[semantic]; exists {
			return PreparedExecution{}, registration{}, nil, nil, lifecycleError(ErrRewriteLoop, nil, "preflight", "", "rewrite repeated a semantic input")
		}
		seen[semantic] = struct{}{}
		entered := make([]Interceptor, 0, len(e.generation.interceptors))
		e.observe(PhasePreflight, PreparedExecution{ToolName: current.ToolName, StepNumber: current.StepNumber, Ordinal: current.Ordinal})
		rewrite := false
		for _, interceptor := range e.generation.interceptors {
			decision, beforeErr := callBefore(ctx, interceptor, current)
			if beforeErr != nil {
				return PreparedExecution{}, registration{}, entered, nil, lifecycleError(ErrInterceptorFailed, beforeErr, "preflight "+interceptor.Name(), "", "interceptor failed")
			}
			entered = append(entered, interceptor)
			switch decision.Decision {
			case "", PreflightContinue:
			case PreflightReturn:
				if decision.Result == nil {
					return PreparedExecution{}, registration{}, entered, nil, lifecycleError(ErrResultInvariant, nil, "preflight", "", "short circuit omitted result")
				}
				result := cloneResult(*decision.Result)
				if err := validateResult(result, current.CallID, current.ToolName); err != nil {
					return PreparedExecution{}, registration{}, entered, nil, err
				}
				prepared, prepareErr := e.freeze(current, entry, canonical, digest)
				return prepared, entry, entered, &result, prepareErr
			case PreflightRewrite:
				if rewrites >= maxRewrites {
					return PreparedExecution{}, registration{}, entered, nil, lifecycleError(ErrRewriteLoop, nil, "preflight", "", "rewrite limit exceeded")
				}
				if decision.Name != "" {
					current.ToolName = decision.Name
				}
				current.RawInput = decision.Input
				rewrite = true
			default:
				return PreparedExecution{}, registration{}, entered, nil, lifecycleError(ErrRewriteInvalid, nil, "preflight", "", "unknown interceptor decision")
			}
			if rewrite {
				break
			}
		}
		if rewrite {
			continue
		}
		prepared, err := e.freeze(current, entry, canonical, digest)
		return prepared, entry, entered, nil, err
	}
}

func (e *Executor) freeze(invocation InvocationIdentity, entry registration, canonical, inputDigest string) (PreparedExecution, error) {
	base := struct {
		TenantKey     agent.TenantKey `json:"tenant_key"`
		RunKey        string          `json:"run_key"`
		StepNumber    uint32          `json:"step_number"`
		Ordinal       uint32          `json:"ordinal"`
		CallID        string          `json:"call_id"`
		ToolName      string          `json:"tool_name"`
		InputDigest   string          `json:"input_digest"`
		Generation    string          `json:"generation"`
		Definition    string          `json:"definition"`
		SchemaVersion string          `json:"schema_version"`
		ToolVersion   string          `json:"tool_version"`
	}{invocation.TenantKey, invocation.RunKey, invocation.StepNumber, invocation.Ordinal, invocation.CallID, invocation.ToolName, inputDigest, e.generation.digest, entry.definitionDigest, entry.metadata.SchemaVersion, entry.metadata.Version}
	executionKey, err := agent.CanonicalDigest(base)
	if err != nil {
		return PreparedExecution{}, lifecycleError(ErrExecutionConflict, err, "freeze", "", "execution key digest failed")
	}
	effectDigest, err := agent.CanonicalDigest(struct {
		ExecutionKey string              `json:"execution_key"`
		Action       string              `json:"action"`
		EffectGroup  string              `json:"effect_group"`
		EffectClass  SideEffectClass     `json:"effect_class"`
		Idempotency  IdempotencyClass    `json:"idempotency"`
		Resource     permission.Resource `json:"resource"`
	}{executionKey, entry.metadata.Action, entry.metadata.EffectGroup, entry.metadata.EffectClass, entry.metadata.Idempotency, invocation.Resource})
	if err != nil {
		return PreparedExecution{}, lifecycleError(ErrExecutionConflict, err, "freeze", executionKey, "effect digest failed")
	}
	return PreparedExecution{TenantKey: invocation.TenantKey, RunKey: invocation.RunKey, AttemptKey: invocation.AttemptKey, FenceToken: invocation.FenceToken, StepNumber: invocation.StepNumber, Ordinal: invocation.Ordinal, CallID: invocation.CallID, ToolName: invocation.ToolName, RawInput: invocation.RawInput, CanonicalInput: canonical, InputDigest: inputDigest, ExecutionKey: executionKey, EffectDigest: effectDigest, ToolGeneration: e.generation.digest, DefinitionDigest: entry.definitionDigest, SchemaVersion: entry.metadata.SchemaVersion, ToolVersion: entry.metadata.Version, Action: entry.metadata.Action, EffectGroup: entry.metadata.EffectGroup, EffectClass: entry.metadata.EffectClass, Idempotency: entry.metadata.Idempotency, ReplayPolicy: entry.metadata.ReplayPolicy, PrincipalKey: invocation.PrincipalKey, SessionRef: invocation.SessionRef, Resource: invocation.Resource}, nil
}

func (e *Executor) authorize(ctx context.Context, request ExecuteRequest, prepared PreparedExecution) (*ExecuteResult, error) {
	if e.permission == nil {
		return nil, nil
	}
	check := permission.CheckRequest{RequestKey: permission.RequestKey(prepared.ExecutionKey), Subject: permission.Subject{TenantKey: prepared.TenantKey, PrincipalKey: prepared.PrincipalKey}, Resource: prepared.Resource, SessionRef: prepared.SessionRef, RunRef: permission.RunRef(prepared.RunKey), AttemptRef: permission.AttemptRef(prepared.AttemptKey), ExecutionRef: permission.ExecutionRef(prepared.ExecutionKey), FenceToken: prepared.FenceToken, StepNumber: prepared.StepNumber, Ordinal: prepared.Ordinal, ToolCallID: prepared.CallID, ToolName: prepared.ToolName, Action: prepared.Action, InputDigest: permission.InputDigest(prepared.InputDigest), ToolGeneration: prepared.ToolGeneration, DefinitionDigest: prepared.DefinitionDigest, PolicyVersion: request.PolicyVersion, ApprovalExpiresAt: request.ApprovalExpiresAt}
	result, err := e.permission.Check(ctx, check)
	if err != nil {
		kind := ErrAuthorizationFailed
		if errors.Is(err, permission.ErrPermissionDenied) {
			kind = ErrPermissionDenied
		}
		return nil, lifecycleError(kind, err, "authorize", prepared.ExecutionKey, "permission check failed")
	}
	switch result.Decision {
	case permission.DecisionAllow:
		return nil, nil
	case permission.DecisionDeny:
		return nil, lifecycleError(ErrPermissionDenied, nil, "authorize", prepared.ExecutionKey, result.ReasonCode)
	case permission.DecisionAsk:
		if result.Blocker == nil {
			return nil, lifecycleError(ErrApprovalPending, nil, "authorize", prepared.ExecutionKey, "permission service omitted suspension blocker")
		}
		blocker := *result.Blocker
		return &ExecuteResult{Prepared: prepared, Status: StatusPrepared, Blocker: &blocker}, nil
	default:
		return nil, lifecycleError(ErrPermissionDenied, nil, "authorize", prepared.ExecutionKey, "permission service returned an unknown decision")
	}
}

func (e *Executor) existing(record ExecutionRecord) (ExecuteResult, error) {
	result := ExecuteResult{Prepared: record.Prepared, Status: record.Status}
	if record.Result != nil {
		cloned := cloneResult(*record.Result)
		result.Result = &cloned
	}
	switch record.Status {
	case StatusSucceeded:
		return result, nil
	case StatusFailed:
		return result, lifecycleError(ErrToolFatal, nil, "load", record.Prepared.ExecutionKey, failureMessage(record.Failure))
	case StatusUnknown:
		return result, lifecycleError(ErrExecutionUnknown, nil, "load", record.Prepared.ExecutionKey, "automatic replay is forbidden")
	case StatusPrepared, StatusRunning:
		return result, lifecycleError(ErrExecutionInProgress, nil, "load", record.Prepared.ExecutionKey, "another invocation owns this execution")
	default:
		return result, lifecycleError(ErrExecutionConflict, nil, "load", record.Prepared.ExecutionKey, "ledger returned an unknown status")
	}
}

func (e *Executor) finishError(ctx context.Context, entered []Interceptor, prepared PreparedExecution, toolErr error) (ExecuteResult, error) {
	hookErr := e.runErrorHooks(ctx, entered, prepared, toolErr)
	if hookErr != nil {
		toolErr = errors.Join(toolErr, hookErr)
	}
	disposition := classifyError(toolErr)
	failure := Failure{Code: "tool_failed", Message: "tool execution failed", Retryable: disposition == DispositionRetryable}
	if disposition == DispositionUnknown {
		if _, err := e.ledger.MarkUnknown(ctx, prepared.ExecutionKey, prepared.FenceToken, failure); err != nil {
			toolErr = errors.Join(toolErr, err)
		}
		return ExecuteResult{Prepared: prepared, Status: StatusUnknown}, lifecycleError(ErrExecutionUnknown, toolErr, "execute", prepared.ExecutionKey, "tool outcome is ambiguous")
	}
	if _, err := e.ledger.Complete(ctx, CompleteExecution{ExecutionKey: prepared.ExecutionKey, FenceToken: prepared.FenceToken, Failure: &failure}); err != nil {
		return ExecuteResult{Prepared: prepared, Status: StatusUnknown}, lifecycleError(ErrExecutionUnknown, errors.Join(toolErr, err), "record failure", prepared.ExecutionKey, "tool failure could not be recorded")
	}
	return ExecuteResult{Prepared: prepared, Status: StatusFailed}, lifecycleError(ErrToolFatal, toolErr, "execute", prepared.ExecutionKey, "tool returned an error")
}

func (e *Executor) runAfter(ctx context.Context, entered []Interceptor, prepared PreparedExecution, result agent.ToolResult) (agent.ToolResult, error) {
	if err := validateResult(result, prepared.CallID, prepared.ToolName); err != nil {
		return agent.ToolResult{}, err
	}
	for index := len(entered) - 1; index >= 0; index-- {
		previous := cloneResult(result)
		updated, err := callAfter(ctx, entered[index], prepared, previous)
		if err != nil {
			return agent.ToolResult{}, lifecycleError(ErrInterceptorFailed, err, "after "+entered[index].Name(), prepared.ExecutionKey, "interceptor failed")
		}
		updated.IsError = updated.IsError || previous.IsError
		updated.StopTurn = updated.StopTurn || previous.StopTurn
		if err := validateResult(updated, prepared.CallID, prepared.ToolName); err != nil {
			return agent.ToolResult{}, err
		}
		result = updated
	}
	return result, nil
}

func (e *Executor) runErrorHooks(ctx context.Context, entered []Interceptor, prepared PreparedExecution, original error) error {
	var hookErrors []error
	for index := len(entered) - 1; index >= 0; index-- {
		if err := callOnError(ctx, entered[index], prepared, original); err != nil {
			hookErrors = append(hookErrors, lifecycleError(ErrInterceptorFailed, err, "on error "+entered[index].Name(), prepared.ExecutionKey, "interceptor failed"))
		}
	}
	return errors.Join(hookErrors...)
}

func (e *Executor) observe(phase Phase, prepared PreparedExecution) {
	if e.observer == nil {
		return
	}
	observation := Observation{Phase: phase, ExecutionKey: prepared.ExecutionKey, ToolName: prepared.ToolName, StepNumber: prepared.StepNumber, Ordinal: prepared.Ordinal, At: e.now()}
	func() {
		defer func() { _ = recover() }()
		e.observer.TryObserve(observation)
	}()
}

func validateInvocation(invocation InvocationIdentity) error {
	if !invocation.TenantKey.Valid() || invocation.RunKey == "" || invocation.AttemptKey == "" || invocation.CallID == "" || invocation.ToolName == "" {
		return lifecycleError(ErrToolInputInvalid, nil, "prepare", "", "tenant, run, attempt, call, and tool identities are required")
	}
	return nil
}

func validateResult(result agent.ToolResult, callID, name string) error {
	if result.ToolCallID != callID || result.Name != name {
		return lifecycleError(ErrResultInvariant, nil, "result", "", "tool call id or name changed")
	}
	return nil
}

func executeTool(ctx context.Context, implementation agent.Tool, prepared PreparedExecution) (result agent.ToolResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = &panicError{value: fmt.Sprint(recovered), stack: debug.Stack()}
		}
	}()
	return implementation.Execute(ctx, agent.ToolInvocation{CallID: prepared.CallID, Name: prepared.ToolName, RawInput: prepared.CanonicalInput, ExecutionKey: prepared.ExecutionKey})
}

type panicError struct {
	value string
	stack []byte
}

func (e *panicError) Error() string                          { return "tool panicked: " + e.value }
func (e *panicError) ToolErrorDisposition() ErrorDisposition { return DispositionUnknown }

func classifyError(err error) ErrorDisposition {
	var classified ClassifiedError
	if errors.As(err, &classified) {
		switch classified.ToolErrorDisposition() {
		case DispositionFailed, DispositionRetryable, DispositionUnknown:
			return classified.ToolErrorDisposition()
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return DispositionUnknown
	}
	return DispositionFailed
}

func failureMessage(failure *Failure) string {
	if failure == nil || failure.Message == "" {
		return "recorded tool failure"
	}
	return failure.Message
}

func callBefore(ctx context.Context, interceptor Interceptor, invocation InvocationIdentity) (result PreflightResult, err error) {
	defer recoverInterceptor(&err)
	return interceptor.Before(ctx, invocation)
}

func callAfter(ctx context.Context, interceptor Interceptor, prepared PreparedExecution, result agent.ToolResult) (updated agent.ToolResult, err error) {
	defer recoverInterceptor(&err)
	return interceptor.After(ctx, prepared, cloneResult(result))
}

func callOnError(ctx context.Context, interceptor Interceptor, prepared PreparedExecution, original error) (err error) {
	defer recoverInterceptor(&err)
	return interceptor.OnError(ctx, prepared, original)
}

func recoverInterceptor(err *error) {
	if recovered := recover(); recovered != nil {
		*err = fmt.Errorf("interceptor panic: %v", recovered)
	}
}
