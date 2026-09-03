// Command guide is the ops assistant built across docs/guide: it reads logs,
// classifies an incident, and restarts a service after a human approves.
//
// It runs with no API key against a scripted model. Set OPENAI_API_KEY (and
// optionally OPENAI_BASE_URL / OPENAI_MODEL) to talk to a real one.
package main

import (
	"context"
	"fmt"
	"os"

	agent "github.com/iceymoss/agent-runtime-go"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	ops := NewOps()
	model, modelName := NewModel()

	// Assembly happens once. Everything below this point is per-turn.
	approved := map[string]bool{}
	registry := agent.NewRegistry()
	for _, tool := range []agent.Tool{
		NewGate(ReadLogsTool(ops), Policy(), func(name string) bool { return approved[name] }),
		NewGate(RestartTool(ops), Policy(), func(name string) bool { return approved[name] }),
	} {
		if err := registry.Register(tool); err != nil {
			return err
		}
	}

	runner, err := agent.New(agent.Config{
		Key:           "ops.assistant",
		ModelName:     modelName,
		MaxSteps:      8,
		ContextWindow: 128_000,
	}, model, registry)
	if err != nil {
		return err
	}

	runs := NewRuns()
	transcript := &Transcript{System: agent.NewSystemMessage(
		"你是运维助手。回答前先用工具确认事实，不要猜测。")}

	for _, question := range []string{
		"checkout 服务好像有问题，看一下？",
		"那就重启它。",
	} {
		fmt.Printf("\n> %s\n", question)
		if err := ask(ctx, runner, runs, transcript, question, approved); err != nil {
			return err
		}
	}

	fmt.Printf("\n[历史 %d 条消息，重启过 %v]\n", transcript.Len(), ops.restarted)
	return nil
}

// ask runs one turn, streams it, and resumes it once if it stopped for approval.
//
// The run is durable because it has to be: ToolResume lives on DurableRunConfig,
// so a suspended run can only be resumed if it was checkpointed. Approval and
// durability are one feature, not two.
func ask(ctx context.Context, runner *agent.Agent, runs *Runs, transcript *Transcript, question string, approved map[string]bool) error {
	runKey, err := runs.NewRunKey()
	if err != nil {
		return err
	}
	emitter := NewProgressEmitter(os.Stdout)
	result, err := runner.Run(ctx, agent.RunRequest{
		Messages:           transcript.Prompt(question),
		ObservationEmitter: emitter,
		DurableRun:         runs.Config(runKey, nil),
	})
	emitter.Close()
	if err != nil {
		return err
	}

	if suspension := approvalSuspension(result); suspension != nil {
		// A real application asks a person here — on a terminal, in a web UI, or
		// through a ticket — and may do it in a different process minutes later.
		// The run is on disk, so nothing is lost while it waits.
		fmt.Printf("\n  [需要审批: %s] 已批准\n", suspension.RequestRef)
		approved["restart_service"] = true

		emitter = NewProgressEmitter(os.Stdout)
		result, err = runner.Run(ctx, agent.RunRequest{
			Messages:           transcript.Prompt(question),
			ObservationEmitter: emitter,
			// Same run key: this is the same run continuing, not a new one.
			DurableRun: runs.Config(runKey, suspension),
		})
		emitter.Close()
		if err != nil {
			return err
		}
	}

	if !transcript.Commit(question, result) {
		fmt.Printf("\n  [这一轮未完成: %s/%s，不写入历史]\n", result.Outcome, result.StopReason)
	}
	return nil
}

// approvalSuspension reports the handle a run parked on, or nil if it did not
// park on an approval.
func approvalSuspension(result *agent.RunResult) *agent.ToolSuspension {
	if result == nil || result.Outcome != agent.OutcomeSuspended || result.StopReason != agent.StopReasonToolSuspended {
		return nil
	}
	if result.Suspension == nil || result.Suspension.Tool == nil {
		return nil
	}
	if result.Suspension.Tool.Kind != agent.ToolSuspensionApproval {
		return nil
	}
	return result.Suspension.Tool
}
