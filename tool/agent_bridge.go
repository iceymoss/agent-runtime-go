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
// root Agent registry. This first bridge intentionally rejects approval
// blockers; durable suspension/resume requires the root suspension contract.
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
	executed, err := t.executor.Execute(ctx, request)
	if executed.Blocker != nil {
		return agent.ToolResult{}, &agent.ToolSuspensionError{Suspension: agent.ToolSuspension{Kind: agent.ToolSuspensionApproval, ExecutionKey: executed.Prepared.ExecutionKey, RequestRef: string(executed.Blocker.RequestRef), ResumeToken: string(executed.Blocker.ResumeToken), Revision: executed.Blocker.Revision}, Cause: err}
	}
	if err != nil {
		return agent.ToolResult{}, err
	}
	if executed.Result == nil {
		return agent.ToolResult{}, lifecycleError(ErrResultInvariant, nil, "agent bridge execute", executed.Prepared.ExecutionKey, "executor returned no tool result")
	}
	return *executed.Result, nil
}
