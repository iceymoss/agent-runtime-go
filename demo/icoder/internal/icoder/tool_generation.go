package icoder

import (
	"context"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/permission"
	toollifecycle "github.com/iceymoss/agent-runtime-go/tool"
)

// toolGenerationVersion is the version stamped on every registered tool. It is
// part of the frozen generation digest, so bumping it makes previously prepared
// executions and previously granted approvals inapplicable on purpose.
const toolGenerationVersion = "icoder-tools-v1"

// toolSchemaVersion tracks the shape of tool inputs independently of the tool
// implementations, so a schema change invalidates approvals even when the code
// behind the tool did not change.
const toolSchemaVersion = "icoder-tool-schema-v1"

// approvalTTL bounds how long a human approval stays usable. An approval that
// is confirmed long after it was requested no longer describes the workspace the
// user was looking at.
const approvalTTL = 15 * time.Minute

// lifecycleEntry pairs one tool implementation with the immutable metadata the
// executor needs to classify its effect, plus the resource that permission
// decisions are made about.
type lifecycleEntry struct {
	tool     agent.Tool
	metadata toollifecycle.Metadata
	resource permission.Resource
}

// readMetadata describes a tool that only observes the workspace. Reads are
// replayable because repeating them cannot change anything.
func readMetadata(action string) toollifecycle.Metadata {
	return toollifecycle.Metadata{
		Version: toolGenerationVersion, SchemaVersion: toolSchemaVersion, Action: action,
		EffectGroup: "workspace.read", EffectClass: toollifecycle.EffectRead,
		Idempotency: toollifecycle.IdempotencyExecutionKey, Concurrency: toollifecycle.ConcurrencyParallel,
		ReplayPolicy: agent.ReplayPolicyIdempotent,
	}
}

// writeMetadata describes a tool that changes workspace files. Such a tool is
// never replayed automatically: a half-applied edit repeated blindly is exactly
// the failure mode the effect ledger exists to prevent.
func writeMetadata(action, group string) toollifecycle.Metadata {
	return toollifecycle.Metadata{
		Version: toolGenerationVersion, SchemaVersion: toolSchemaVersion, Action: action,
		EffectGroup: group, EffectClass: toollifecycle.EffectWrite,
		Idempotency: toollifecycle.IdempotencyNone, Concurrency: toollifecycle.ConcurrencyExclusive,
		ReplayPolicy: agent.ReplayPolicyNever,
	}
}

// externalMetadata describes a tool that reaches outside the process. The effect
// is classified as external even when it looks read-only, because the runtime
// cannot verify what the far side did.
func externalMetadata(action, group string, idempotent bool) toollifecycle.Metadata {
	metadata := toollifecycle.Metadata{
		Version: toolGenerationVersion, SchemaVersion: toolSchemaVersion, Action: action,
		EffectGroup: group, EffectClass: toollifecycle.EffectExternal,
		Idempotency: toollifecycle.IdempotencyNone, Concurrency: toollifecycle.ConcurrencySequential,
		ReplayPolicy: agent.ReplayPolicyNever,
	}
	if idempotent {
		metadata.Idempotency = toollifecycle.IdempotencyExecutionKey
		metadata.ReplayPolicy = agent.ReplayPolicyIdempotent
	}
	return metadata
}

// ToolCatalog is one frozen tool generation together with the executor that runs
// it and the root registry the agent loop sees.
//
// Freezing matters: the generation digest covers every tool definition, its
// metadata, and the interceptor chain, so an approval granted under one
// generation cannot authorize a call under a different one.
type ToolCatalog struct {
	generation *toollifecycle.Generation
	executor   *toollifecycle.Executor
	registry   *agent.Registry
	resources  map[string]permission.Resource
	names      []string
	now        func() time.Time
}

// ToolCatalogOptions supplies the entries and the ports the executor needs.
type ToolCatalogOptions struct {
	Entries     []lifecycleEntry
	Permissions *PermissionGate
	Ledger      toollifecycle.ExecutionLedger
	Now         func() time.Time
}

// NewToolCatalog registers every entry, freezes the generation with iCoder's
// interceptor chain, and bridges it into a root agent.Registry.
//
// The interceptor order is deliberate: approval runs first so that a denied call
// never reaches the audit trail as a started execution, and audit runs closest to
// the tool so its After hook observes the real result.
func NewToolCatalog(options ToolCatalogOptions) (*ToolCatalog, error) {
	if options.Permissions == nil || options.Ledger == nil {
		return nil, fmt.Errorf("tool catalog requires a permission service and an execution ledger")
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	registry := toollifecycle.NewRegistry()
	resources := make(map[string]permission.Resource, len(options.Entries))
	actions := make(map[string]string, len(options.Entries))
	for _, entry := range options.Entries {
		name := entry.tool.Definition().Name
		if err := registry.Register(pairedTool{Tool: entry.tool}, entry.metadata); err != nil {
			return nil, err
		}
		resources[name] = entry.resource
		actions[name] = entry.metadata.Action
	}
	catalog := &ToolCatalog{resources: resources, now: now}
	approval := &approvalInterceptor{permissions: options.Permissions, catalog: catalog, actions: actions, now: now}
	generation, err := registry.Freeze(approval, auditInterceptor{})
	if err != nil {
		return nil, err
	}
	executor, err := toollifecycle.NewExecutor(toollifecycle.ExecutorOptions{
		Generation: generation, Ledger: options.Ledger, Permission: options.Permissions.Service(), Now: now,
	})
	if err != nil {
		return nil, err
	}
	bridged, err := toollifecycle.NewAgentRegistry(generation, executor, toollifecycle.AgentBridgeOptions{ResolveInvocation: catalog.resolveInvocation})
	if err != nil {
		return nil, err
	}
	catalog.generation, catalog.executor, catalog.registry = generation, executor, bridged
	catalog.names = bridged.Names()
	return catalog, nil
}

// Registry returns the root registry the agent loop executes against.
func (c *ToolCatalog) Registry() *agent.Registry { return c.registry }

// Names returns the model-visible tool names in the frozen generation.
func (c *ToolCatalog) Names() []string { return append([]string(nil), c.names...) }

// GenerationDigest identifies this exact set of tools, metadata, and
// interceptors. It is recorded on every run so an audit can tell which tools a
// past run could actually reach.
func (c *ToolCatalog) GenerationDigest() string { return c.generation.Digest() }

// resource returns the permission resource a tool acts on. It is fixed at
// assembly time so a tool cannot widen its own authorization scope at runtime.
func (c *ToolCatalog) resource(name string) permission.Resource { return c.resources[name] }

// resolveInvocation binds one model tool call to the run that issued it. The
// executor requires a stable tenant, run, attempt, and call identity; everything
// else it derives itself from the frozen generation.
//
// StepNumber and Ordinal stay zero because the root runtime does not expose them
// to a tool. The executor's execution key already includes the model-assigned
// CallID, which is unique within a run, and the durable effect ledger carries the
// step and ordinal separately.
func (c *ToolCatalog) resolveInvocation(ctx context.Context, invocation agent.ToolInvocation) (toollifecycle.ExecuteRequest, error) {
	run := currentRunContext(ctx)
	if run.id == "" || run.attempt == "" {
		return toollifecycle.ExecuteRequest{}, fmt.Errorf("tool %q was invoked outside a run", invocation.Name)
	}
	return toollifecycle.ExecuteRequest{
		Invocation: toollifecycle.InvocationIdentity{
			TenantKey: tenantKey, RunKey: run.id, AttemptKey: run.attempt, FenceToken: run.fenceToken(),
			CallID: invocation.CallID, ToolName: invocation.Name, RawInput: invocation.RawInput,
			PrincipalKey: principalKey, SessionRef: run.session, Resource: c.resource(invocation.Name),
		},
		PolicyVersion:     policyVersion,
		ApprovalExpiresAt: c.now().Add(approvalTTL),
	}, nil
}

// pairedTool fills in the pairing metadata the advanced executor requires. The
// root agent loop stamps a result's call id and name after a tool returns, but
// the executor validates them before its own After hooks run, so the stamping has
// to happen closer to the tool.
type pairedTool struct{ agent.Tool }

func (t pairedTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	result, err := t.Tool.Execute(ctx, invocation)
	if err != nil {
		return result, err
	}
	result.ToolCallID, result.Name = invocation.CallID, invocation.Name
	return result, nil
}

// auditInterceptor turns the executor's lifecycle into reliable session events.
// It deliberately records only identities and outcomes, never tool input or
// output, so the event stream stays safe to ship off the machine.
type auditInterceptor struct{}

func (auditInterceptor) Name() string    { return "icoder.audit" }
func (auditInterceptor) Version() string { return "v1" }

func (auditInterceptor) Before(ctx context.Context, invocation toollifecycle.InvocationIdentity) (toollifecycle.PreflightResult, error) {
	run := currentRunContext(ctx)
	if err := recordRunFact(ctx, run, invocation.CallID+":started", "agent.tool.started", map[string]any{
		"tool": invocation.ToolName, "call_id": invocation.CallID,
	}); err != nil {
		return toollifecycle.PreflightResult{}, err
	}
	return toollifecycle.PreflightResult{Decision: toollifecycle.PreflightContinue}, nil
}

func (auditInterceptor) After(ctx context.Context, prepared toollifecycle.PreparedExecution, result agent.ToolResult) (agent.ToolResult, error) {
	run := currentRunContext(ctx)
	// The completion fact must survive cancellation of the run context: the
	// effect already happened and the record is what makes it auditable.
	err := recordRunFact(context.WithoutCancel(ctx), run, prepared.CallID+":terminal", "agent.tool.completed", map[string]any{
		"tool": prepared.ToolName, "execution_key": prepared.ExecutionKey, "effect_class": prepared.EffectClass,
		"input_digest": prepared.InputDigest, "is_error": result.IsError, "stop_turn": result.StopTurn,
	})
	return result, err
}

func (auditInterceptor) OnError(ctx context.Context, prepared toollifecycle.PreparedExecution, cause error) error {
	run := currentRunContext(ctx)
	return recordRunFact(context.WithoutCancel(ctx), run, prepared.CallID+":terminal", "agent.tool.failed", map[string]any{
		"tool": prepared.ToolName, "execution_key": prepared.ExecutionKey, "error": cause.Error(),
	})
}
