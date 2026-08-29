/*
Package agent runs the model/tool loop: it asks a model for the next step,
validates and executes the tools the model asked for, feeds the results back,
and decides when to stop.

Everything around that loop belongs to the application. This package reads no
environment variables, chooses no credentials, opens no database, and registers
no implicit tools; those are decisions with no single right answer, so they are
made in your composition root where they can be tested and replaced.

# Start here

A first agent needs eight symbols. Config, New, and Run assemble and drive it;
Message and its constructors carry the conversation; Registry and NewTool
provide the tools; RunResult reports what happened.

	runner, err := agent.New(agent.Config{
		Key: "support.assistant", ModelName: "gpt-4o-mini", MaxSteps: 8,
	}, model, registry)
	if err != nil {
		return err
	}
	result, err := runner.Run(ctx, agent.RunRequest{Messages: messages})

An Agent holds no conversation state and is safe to share across goroutines and
sessions. Everything that varies per turn travels in RunRequest and comes back
in RunResult.

The rest of this package's surface exists for problems you may not have yet.
Reach for it when you hit the problem, not before; the sections below say which
problem each part solves.

# The two ports

Model is the only model port: Name, Capabilities, and Stream. A provider adapter
projects GenerateRequest into an upstream protocol and turns the response into
StreamChunk values. Adapters that already hold a complete response should return
StreamResponse rather than assembling chunks by hand, because the runtime
requires the streamed chunks to add up to exactly the terminal response.
providers/openaicompat implements this port for any OpenAI-compatible endpoint.

Tool is the port for what a model may do: Definition, ReplayPolicy, and Execute.
NewTool and MustNewTool generate the JSON Schema from an input struct, so most
tools are one function. Registry collects tools at assembly time and freezes into
an immutable ToolSet, which is why registering a tool after New has no effect on
an already-assembled agent.

# Knowing why a run stopped

RunResult carries two fields that are easy to confuse and are both needed.
Outcome is completed, suspended, or failed. StopReason says why: complete,
output_limit, tool_suspended, tool_stop_turn, loop_detected, context_budget,
max_steps, or stop_condition.

Only a run that finished with no pending tool calls is a real answer. Reaching
MaxSteps while the model is still calling tools is not an error - it is
suspended with max_steps, and the application decides whether to continue.

RunResult.Messages holds only what this run produced. The application stores the
user's message alongside it; see the "Persisting a conversation" guide and
examples/chat.

# Two kinds of tool failure

A ToolResult with IsError set is a failure the model can see and correct, and
the run continues. A non-nil Go error from Execute is a failure the model cannot
fix, and it ends the attempt with the cause intact. Choosing the wrong one either
hides a real outage from your logs or asks a model to retry something that will
never work.

# Watching a run happen

ObservationEmitter reports text deltas, tool calls, tool results, and finished
steps while a run is in flight. It is bounded and lossy by default so delivery
can never hold up the model; use ObservationOptions.Lossless when the
observations are text a person is reading, where a dropped delta is a truncated
answer. Observations are progress, never authority: the outcome is RunResult.

# Constraining what the model returns

ResponseFormat asks the provider to constrain the model's own output to JSON, or
to a JSON Schema, for agents whose answer is parsed by code rather than read by a
person. It requires Capabilities.StructuredOutput and is refused at assembly
otherwise.

PartReasoning carries a reasoning model's thinking, kept out of Message.Text and
never sent back upstream.

# Durable execution

The remaining surface - Checkpoint, RunSnapshot, MutationGuard, CheckpointStore,
ToolExecution, RunStatus, RunPhase, and the digest helpers - exists for one
question: after a crash, what may safely be re-run?

Most applications never touch it. It becomes relevant when a run must survive the
process that started it, and it is reached through RunRequest.DurableRun with a
CheckpointStore; the durable subpackage provides one over any adapter. The
runtime does not promise exactly-once side effects, and downstream systems dedupe
on ToolExecutionKey.

# What is not a security boundary

Prompts, skills, and model output are not permission boundaries. A tool that must
not delete a file has to refuse inside Execute; a prompt that says so does not.
Enforce authorization in tool implementations and in your adapters.
*/
package agent
