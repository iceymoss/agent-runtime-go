package tool

import (
	"context"
	"fmt"

	"github.com/iceymoss/agent-runtime-go"
)

type AgentInvocationResolver func(context.Context, agent.ToolInvocation) (ExecuteRequest, error)

type AgentBridgeOptions struct {
	ResolveInvocation AgentInvocationResolver
}

// NewAgentRegistry exposes one immutable advanced-tool generation through the
// root Agent registry.
func NewAgentRegistry(generation *Generation, executor *Executor, options AgentBridgeOptions) (*agent.Registry, error) {
	if generation == nil || executor == nil || options.ResolveInvocation == nil {
		return nil, lifecycleError(ErrInvalidConfiguration, nil, "agent bridge", "", "generation, executor, and invocation resolver are required")
	}
	registry := agent.NewRegistry()
	for _, definition := range generation.Definitions() {
		frozen, ok := generation.Tool(definition.Name)
		if !ok {
			return nil, lifecycleError(ErrInvalidConfiguration, nil, "agent bridge", "", fmt.Sprintf("generation omitted tool %q", definition.Name))
		}
		version := generation.Digest()
		if versioned, ok := frozen.(agent.ExecutableVersioner); ok {
			version += ":" + versioned.ExecutableVersion()
		}
		wrapped := &agentExecutorTool{definition: definition, replay: frozen.ReplayPolicy(), executableVersion: version, executor: executor, resolve: options.ResolveInvocation}
		if err := registry.Register(wrapped); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

type agentExecutorTool struct {
	definition        agent.ToolDefinition
	replay            agent.ReplayPolicy
	executableVersion string
	executor          *Executor
	resolve           AgentInvocationResolver
}

func (t *agentExecutorTool) Definition() agent.ToolDefinition { return cloneDefinition(t.definition) }
func (t *agentExecutorTool) ReplayPolicy() agent.ReplayPolicy { return t.replay }
func (t *agentExecutorTool) ExecutableVersion() string        { return t.executableVersion }
func (t *agentExecutorTool) OwnsToolExecutionLifecycle() bool { return true }

func (t *agentExecutorTool) Execute(ctx context.Context, invocation agent.ToolInvocation) (agent.ToolResult, error) {
	request, err := t.resolve(ctx, invocation)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if request.Invocation.CallID == "" {
		request.Invocation.CallID = invocation.CallID
	}
	if request.Invocation.ToolName == "" {
		request.Invocation.ToolName = invocation.Name
	}
	if request.Invocation.RawInput == "" {
		request.Invocation.RawInput = invocation.RawInput
	}
	if request.Invocation.CallID != invocation.CallID || request.Invocation.ToolName != invocation.Name || request.Invocation.RawInput != invocation.RawInput {
		return agent.ToolResult{}, lifecycleError(ErrInvalidConfiguration, nil, "agent bridge resolve", "", "resolver changed call identity or raw input")
	}
	var executed ExecuteResult
	if invocation.Resume != nil {
		// The kind is not checked here. The executor dispatches on it, and an
		// application may define kinds this package does not know about.
		if invocation.Resume.Kind == "" || invocation.Resume.ExecutionKey == "" {
			return agent.ToolResult{}, lifecycleError(ErrInvalidConfiguration, nil, "agent bridge resume", "", "incomplete tool suspension")
		}
		executed, err = t.executor.Resume(ctx, ResumeRequest{
			Execute: request, ExecutionKey: invocation.Resume.ExecutionKey,
			Suspension: Suspension{
				Kind: invocation.Resume.Kind, RequestRef: invocation.Resume.RequestRef,
				ResumeToken: invocation.Resume.ResumeToken, Revision: invocation.Resume.Revision,
			},
		})
	} else {
		executed, err = t.executor.Execute(ctx, request)
	}
	// A parked execution is reported to the runtime as a suspension rather than a
	// failure, so the run is checkpointed and can be continued instead of ended.
	if executed.Suspension != nil {
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{
			Kind: executed.Suspension.Kind, ExecutionKey: executed.Prepared.ExecutionKey,
			RequestRef: executed.Suspension.RequestRef, ResumeToken: executed.Suspension.ResumeToken,
			Revision:   executed.Suspension.Revision,
			StepNumber: executed.Prepared.StepNumber, Ordinal: executed.Prepared.Ordinal,
		}, Cause: err}
	}
	if err != nil {
		return agent.ToolResult{}, err
	}
	if executed.Result == nil {
		return agent.ToolResult{}, lifecycleError(ErrResultInvariant, nil, "agent bridge execute", executed.Prepared.ExecutionKey, "executor returned no tool result")
	}
	return *executed.Result, nil
}
