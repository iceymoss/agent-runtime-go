package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/app"
	icodereval "github.com/iceymoss/agent-runtime-go/demo/icoder/internal/eval"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/ui"
	"github.com/iceymoss/agent-runtime-go/event"
	"github.com/iceymoss/agent-runtime-go/session"
	"github.com/spf13/cobra"
)

var version = "dev"

type options struct {
	config icoder.Config
	json   bool
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := newRootCommand(os.Stdout, os.Stderr).ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "icoder:", err)
		os.Exit(1)
	}
}

func newRootCommand(stdout, stderr io.Writer) *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:           "icoder",
		Short:         "A terminal-native coding agent",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error { return ui.Run(cmd.Context(), app) })
		},
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	flags := root.PersistentFlags()
	flags.StringVarP(&opts.config.Workspace, "workspace", "w", ".", "workspace root")
	flags.StringVar(&opts.config.Database, "db", "", "SQLite database path (default <workspace>/.icoder.db)")
	flags.StringVar(&opts.config.SkillsRoot, "skills", "", "filesystem skills root")
	flags.StringVar(&opts.config.MCPURL, "mcp-url", "", "Streamable HTTP MCP endpoint")
	flags.StringVarP(&opts.config.SessionID, "session", "s", "", "session identifier")
	flags.StringVar(&opts.config.BaseURL, "base-url", "", "OpenAI-compatible base URL")
	flags.StringVarP(&opts.config.Model, "model", "m", "", "provider model ID")
	flags.IntVar(&opts.config.ContextWindow, "context-window", 0, "model context window")
	flags.IntVar(&opts.config.MaxTokens, "max-tokens", 0, "maximum output tokens")
	flags.IntVar(&opts.config.MaxSteps, "max-steps", 0, "maximum agent steps")
	flags.BoolVarP(&opts.config.AllowWrites, "allow-writes", "y", false, "allow write and command tools for this process")
	flags.BoolVar(&opts.json, "json", false, "emit machine-readable JSON where supported")

	root.AddCommand(newChatCommand(opts, stderr))
	root.AddCommand(newRunCommand(opts, stdout, stderr))
	root.AddCommand(newEvalCommand(opts, stdout))
	root.AddCommand(newSessionCommand(opts, stdout, stderr))
	root.AddCommand(newEventsCommand(opts, stdout, stderr))
	root.AddCommand(newRunsCommand(opts, stdout, stderr))
	root.AddCommand(newDelegationsCommand(opts, stdout, stderr))
	root.AddCommand(newQueueCommand(opts, stdout, stderr))
	root.AddCommand(newRuntimeCommand(opts, stdout, stderr))
	root.AddCommand(newDaemonCommand(opts, stdout, stderr))
	root.AddCommand(newOutboxCommand(opts, stdout, stderr))
	root.AddCommand(newToolsCommand(opts, stdout, stderr))
	root.AddCommand(newConfigCommand(opts, stdout))
	root.AddCommand(&cobra.Command{Use: "version", Short: "Print version", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) { fmt.Fprintln(stdout, "icoder "+version) }})
	root.AddCommand(newCompletionCommand(root))
	return root
}

type writerPublisher struct{ output io.Writer }

func (p writerPublisher) Publish(_ context.Context, envelope event.Envelope) error {
	return json.NewEncoder(p.output).Encode(envelope)
}

func newOutboxCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	parent := &cobra.Command{Use: "outbox", Short: "Manage persisted event delivery"}
	var limit int
	dispatch := &cobra.Command{Use: "dispatch", Short: "Publish and acknowledge one bounded outbox batch as JSON Lines", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			stats, err := app.DispatchOutbox(cmd.Context(), writerPublisher{output: stdout}, limit)
			if err != nil {
				return err
			}
			return writeJSON(stdout, map[string]any{"dispatch": stats})
		})
	}}
	dispatch.Flags().IntVar(&limit, "limit", 100, "maximum events to claim")
	parent.AddCommand(dispatch)
	return parent
}

func newEvalCommand(opts *options, stdout io.Writer) *cobra.Command {
	var fixtures string
	command := &cobra.Command{Use: "eval", Short: "Run the deterministic coding suite with the configured model", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		report, err := (icodereval.Harness{Runner: icodereval.ICoderRunner{Config: opts.config}}).Run(cmd.Context(), icodereval.CodingSuite(fixtures))
		if err != nil {
			return err
		}
		if err := writeJSON(stdout, report); err != nil {
			return err
		}
		if report.Failed > 0 {
			return fmt.Errorf("%d eval tasks failed", report.Failed)
		}
		return nil
	}}
	command.Flags().StringVar(&fixtures, "fixtures", "internal/eval", "directory containing the eval testdata folder")
	return command
}

func newChatCommand(opts *options, stderr io.Writer) *cobra.Command {
	return &cobra.Command{Use: "chat", Short: "Open the interactive terminal UI", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error { return ui.Run(cmd.Context(), app) })
	}}
}

func newRunCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	var task string
	command := &cobra.Command{Use: "run [prompt]", Short: "Run one task non-interactively", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
		prompt := strings.TrimSpace(strings.Join(args, " "))
		if task != "" {
			if prompt != "" {
				return fmt.Errorf("use either positional prompt or --task")
			}
			prompt = task
		}
		if prompt == "" {
			data, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			prompt = strings.TrimSpace(string(data))
		}
		if prompt == "" {
			return fmt.Errorf("prompt is required")
		}
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error { return runTask(cmd.Context(), app, prompt, stdout, opts.json) })
	}}
	command.Flags().StringVarP(&task, "task", "t", "", "coding task (deprecated alias for positional prompt)")
	return command
}

func newSessionCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	parent := &cobra.Command{Use: "session", Short: "Manage persisted sessions"}
	parent.AddCommand(&cobra.Command{Use: "list", Short: "List sessions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			sessions, err := app.Sessions(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, sessions)
			}
			for _, item := range sessions {
				fmt.Fprintf(stdout, "%-24s revision=%d messages=%d updated=%s\n", item.ID, item.Revision, item.Messages, item.UpdatedAt)
			}
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "history", Short: "Show active session history", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			history, err := app.History(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, history)
			}
			for index, message := range history {
				fmt.Fprintf(stdout, "%d %-9s %s\n", index+1, message.Role, summarize(message))
			}
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "clear", Short: "Clear the active session", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error { return app.ClearSession(cmd.Context()) })
	}})
	return parent
}

func newEventsCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{Use: "events", Short: "Replay active session terminal events", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			items, err := app.Events(cmd.Context(), 0)
			if err != nil {
				return err
			}
			for _, item := range items {
				fmt.Fprintln(stdout, item)
			}
			return nil
		})
	}}
}

func newToolsCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{Use: "tools", Short: "List model-visible tools", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			tools := app.Tools()
			sort.Strings(tools)
			if opts.json {
				return writeJSON(stdout, tools)
			}
			fmt.Fprintln(stdout, strings.Join(tools, "\n"))
			return nil
		})
	}}
}

func newConfigCommand(opts *options, stdout io.Writer) *cobra.Command {
	parent := &cobra.Command{Use: "config", Short: "Inspect configuration"}
	parent.AddCommand(&cobra.Command{Use: "show", Short: "Print resolved non-secret configuration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		config := opts.config
		if err := config.Normalize(); err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"base_url": config.BaseURL, "model": config.Model, "workspace": config.Workspace, "database": config.Database, "skills": config.SkillsRoot, "mcp_url": config.MCPURL, "session": config.SessionID, "allow_writes": config.AllowWrites, "context_window": config.ContextWindow, "max_tokens": config.MaxTokens, "max_steps": config.MaxSteps})
	}})
	parent.AddCommand(&cobra.Command{Use: "validate", Short: "Validate configuration", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		config := opts.config
		if err := config.Normalize(); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "configuration is valid")
		return nil
	}})
	return parent
}

func newCompletionCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{Use: "completion [bash|zsh|fish|powershell]", Short: "Generate shell completion", Args: cobra.ExactArgs(1), ValidArgs: []string{"bash", "zsh", "fish", "powershell"}, RunE: func(cmd *cobra.Command, args []string) error {
		switch args[0] {
		case "bash":
			return root.GenBashCompletion(cmd.OutOrStdout())
		case "zsh":
			return root.GenZshCompletion(cmd.OutOrStdout())
		case "fish":
			return root.GenFishCompletion(cmd.OutOrStdout(), true)
		case "powershell":
			return root.GenPowerShellCompletion(cmd.OutOrStdout())
		default:
			return fmt.Errorf("unsupported shell %q", args[0])
		}
	}}
}

func withApp(ctx context.Context, config icoder.Config, stderr io.Writer, action func(*icoder.App) error) (resultErr error) {
	app, err := icoder.NewApp(ctx, config)
	if err != nil {
		return err
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			fmt.Fprintln(stderr, "icoder cleanup:", err)
		}
	}()
	return action(app)
}

func runTask(ctx context.Context, app *icoder.App, task string, output io.Writer, jsonOutput bool) error {
	var streamed strings.Builder
	result, err := app.Run(ctx, task, func(observation agent.Observation) {
		if observation.Type == agent.ObservationTextDelta {
			streamed.WriteString(observation.Text)
			if !jsonOutput {
				fmt.Fprint(output, observation.Text)
			}
		}
		if !jsonOutput && observation.Type == agent.ObservationToolCall && observation.ToolCall != nil {
			fmt.Fprintf(output, "\n[tool: %s]\n", observation.ToolCall.Name)
		}
	})
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(output, result)
	}
	// Observation delivery can drop deltas. Print the missing authoritative suffix.
	if result.Text != "" {
		seen := streamed.String()
		if strings.HasPrefix(result.Text, seen) {
			fmt.Fprint(output, strings.TrimPrefix(result.Text, seen))
		} else if seen == "" {
			fmt.Fprint(output, result.Text)
		}
	}
	fmt.Fprintf(output, "\n\n[%s: %s, steps=%d, tokens=%d]\n", result.Outcome, result.StopReason, len(result.Steps), result.Usage.TotalTokens)
	if result.Outcome == agent.OutcomeSuspended && result.StopReason == agent.StopReasonToolSuspended {
		// A non-interactive run cannot answer an approval, so it parks instead of
		// guessing. Tell the user exactly how to continue it.
		fmt.Fprintln(output, "A tool is waiting for approval. Review it with 'icoder runs list', then run")
		fmt.Fprintln(output, "'icoder runs approve <run-key>' to continue or 'icoder runs deny <run-key>' to stop.")
	}
	return nil
}

// newRunsCommand exposes the durable run lifecycle: what is unfinished, what it
// already did, and how a human decides its fate.
func newRunsCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	parent := &cobra.Command{Use: "runs", Short: "Inspect and resolve durable runs"}
	parent.AddCommand(&cobra.Command{Use: "list", Short: "List runs that are suspended or unfinished", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			runs, err := app.PendingRuns(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, runs)
			}
			if len(runs) == 0 {
				fmt.Fprintln(stdout, "no unfinished runs")
				return nil
			}
			for _, run := range runs {
				state := run.Status + "/" + run.Phase
				switch {
				case run.AwaitingApproval:
					state = "awaiting-approval"
				case run.AwaitingDelegation:
					state = "awaiting-delegate"
				}
				fmt.Fprintf(stdout, "%s  session=%s  %s\n", run.RunKey, run.SessionID, state)
			}
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "effects <run-key>", Short: "Show what a run's tool calls actually did", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			effects, err := app.RunEffects(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, effects)
			}
			for _, effect := range effects {
				fmt.Fprintf(stdout, "%-10s step=%d ordinal=%d tool=%s\n", effect.Status, effect.StepNumber, effect.Ordinal, effect.ToolCall.Name)
			}
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "approve <run-key>", Short: "Approve the pending tool call and continue the run", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			if err := app.ResolveRunApproval(cmd.Context(), args[0], true); err != nil {
				return err
			}
			return runResumed(cmd.Context(), app, args[0], stdout, opts.json)
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "deny <run-key>", Short: "Reject the pending tool call and end the run", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			if err := app.ResolveRunApproval(cmd.Context(), args[0], false); err != nil {
				return err
			}
			// The decision is recorded first, then the run is ended. Resuming a
			// denied approval would only fail the attempt with no new information.
			if err := app.AbandonRun(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "denied and abandoned %s\n", args[0])
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "resume <run-key>", Short: "Continue a run that parked on delegated work", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			// A run parked on a child agent needs the child driven, not a decision,
			// so this resumes without answering an approval. Resuming a run that is
			// waiting on a human instead fails in ResumeRun with the reason.
			return runResumed(cmd.Context(), app, args[0], stdout, opts.json)
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "abandon <run-key>", Short: "End an unfinished run without resuming it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			if err := app.AbandonRun(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "abandoned %s\n", args[0])
			return nil
		})
	}})
	return parent
}

// newQueueCommand exposes the durable run queue: work submitted here survives a
// restart with its place in line, the context it was submitted against, and any
// cancellation already recorded for it.
//
// It is separate from `icoder run` on purpose. A person waiting at a terminal
// wants their own turn now; queued work is for a daemon to pick up, possibly in
// a different process than the one that submitted it.
func newQueueCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	parent := &cobra.Command{Use: "queue", Short: "Submit and inspect durable background runs"}
	parent.AddCommand(&cobra.Command{Use: "submit <instruction>", Short: "Queue an instruction as durable background work", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			runKey, err := app.QueueRun(cmd.Context(), strings.Join(args, " "))
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, map[string]string{"run_key": string(runKey)})
			}
			fmt.Fprintf(stdout, "queued %s\n", runKey)
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "work", Short: "Execute one queued run in this process", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			claimed, err := app.RunQueuedWork(cmd.Context())
			if err != nil {
				return err
			}
			if !claimed {
				fmt.Fprintln(stdout, "the queue is empty")
				return nil
			}
			fmt.Fprintln(stdout, "executed one queued run")
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "show <run-key>", Short: "Show a queued run's state and result", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			result, err := app.QueuedRun(cmd.Context(), session.RunKey(args[0]))
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, result)
			}
			fmt.Fprintf(stdout, "%s  %s/%s  revision=%d\n", result.Receipt.RunKey,
				result.Receipt.AdmissionState, result.Receipt.ExecutionState, result.SessionRevision)
			if result.CoreResult != nil {
				fmt.Fprintf(stdout, "\n%s\n", result.CoreResult.Text)
			}
			if result.Failure != nil {
				fmt.Fprintf(stdout, "\nfailure: %s\n", result.Failure.Error())
			}
			return nil
		})
	}})
	parent.AddCommand(&cobra.Command{Use: "cancel <run-key>", Short: "Abandon a queued run", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			result, err := app.CancelQueuedRun(cmd.Context(), session.RunKey(args[0]), session.CancelAbandon, "operator")
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s is %s\n", result.RunKey, result.State)
			return nil
		})
	}})
	return parent
}

// newDelegationsCommand shows what the child agents were asked and what they
// answered, which is the record a reviewer needs when a delegated opinion shaped
// the parent's work.
func newDelegationsCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	var limit int
	command := &cobra.Command{Use: "delegations", Short: "List recorded sub-agent runs and their results", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			items, err := app.Delegations(cmd.Context(), limit)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, items)
			}
			for _, item := range items {
				fmt.Fprintf(stdout, "%s  %s  %s\n", item.CreatedAt, item.AgentKey, item.ResultRef)
			}
			return nil
		})
	}}
	command.Flags().IntVar(&limit, "limit", 20, "maximum delegations to list")
	return command
}

// newDaemonCommand runs iCoder as a supervised process: it reports readiness,
// serves runs until it is signaled, and then shuts down within a bounded budget
// instead of exiting whenever the current work happens to finish.
func newDaemonCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	var drain time.Duration
	command := &cobra.Command{Use: "daemon", Short: "Run iCoder as a supervised process with readiness and bounded shutdown", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(application *icoder.App) error {
			daemon, err := icoder.NewDaemon(application, icoder.DaemonOptions{
				Budgets:   app.Budgets{Drain: drain},
				Publisher: writerPublisher{output: stdout},
			})
			if err != nil {
				return err
			}
			if err := daemon.Start(cmd.Context()); err != nil {
				return err
			}
			health, err := daemon.WaitReady(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "ready state=%s components=%d generation=%d\n", health.State, len(health.Components), health.Generation)
			for _, component := range health.Components {
				fmt.Fprintf(stdout, "  %-8s ready=%t degraded=%t %s\n", component.Name, component.Ready, component.Degraded, component.Reason)
			}
			<-cmd.Context().Done()
			// The signal context is already canceled, so shutdown gets a fresh
			// deadline of its own; otherwise every phase would time out instantly.
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 2*drain)
			defer cancel()
			report, err := daemon.Shutdown(shutdownCtx)
			if opts.json {
				if writeErr := writeJSON(stdout, report); writeErr != nil {
					return writeErr
				}
				return err
			}
			fmt.Fprintf(stdout, "shutdown state=%s\n", report.FinalState)
			for _, phase := range report.Phases {
				fmt.Fprintf(stdout, "  %-11s took=%s timed_out=%t remaining=%d\n",
					phase.Phase, phase.FinishedAt.Sub(phase.StartedAt).Round(time.Millisecond), phase.TimedOut, len(phase.Remaining))
			}
			return err
		})
	}}
	command.Flags().DurationVar(&drain, "drain", 30*time.Second, "how long in-flight runs may finish before they are canceled")
	return command
}

// newRuntimeCommand exposes the frozen runtime generation: what this process is
// running, what it has run before, and whether an older generation can still be
// reconstructed by this binary.
func newRuntimeCommand(opts *options, stdout, stderr io.Writer) *cobra.Command {
	parent := &cobra.Command{Use: "runtime", Short: "Inspect the frozen runtime generation"}
	parent.AddCommand(&cobra.Command{Use: "show", Short: "Show the generation this process resolved", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			info := app.RuntimeInfo()
			if opts.json {
				return writeJSON(stdout, info)
			}
			fmt.Fprintf(stdout, "definition  %s\n", info.DefinitionDigest)
			fmt.Fprintf(stdout, "manifest    %s\n", info.ManifestDigest)
			fmt.Fprintf(stdout, "recipe      %s\n", info.RecipeVersion)
			fmt.Fprintf(stdout, "model       %s\n", info.Model)
			fmt.Fprintf(stdout, "tools       %d\n", len(info.Tools))
			for _, artifact := range info.Artifacts {
				fmt.Fprintf(stdout, "artifact    %-8s %-16s %s\n", artifact.Kind, artifact.Generation, artifact.Digest)
			}
			return nil
		})
	}})
	var limit int
	list := &cobra.Command{Use: "list", Short: "List recorded runtime generations", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			manifests, err := app.RuntimeGenerations(cmd.Context(), limit)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, manifests)
			}
			for _, manifest := range manifests {
				fmt.Fprintf(stdout, "%s  recipe=%s  model=%s  created=%s\n",
					manifest.Wire.DefinitionDigest, manifest.Wire.RecipeVersion, manifest.Wire.Model, manifest.CreatedAt.Format("2006-01-02T15:04:05Z"))
			}
			return nil
		})
	}}
	list.Flags().IntVar(&limit, "limit", 20, "maximum generations to list")
	parent.AddCommand(list)
	parent.AddCommand(&cobra.Command{Use: "verify <definition-digest>", Short: "Reconstruct a recorded generation and confirm it still matches", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withApp(cmd.Context(), opts.config, stderr, func(app *icoder.App) error {
			info, err := app.ReconstructRuntime(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(stdout, info)
			}
			fmt.Fprintf(stdout, "reconstructed %s (manifest %s)\n", info.DefinitionDigest, info.ManifestDigest)
			return nil
		})
	}})
	return parent
}

// runResumed continues a suspended run and reports its outcome the same way a
// fresh task does.
func runResumed(ctx context.Context, app *icoder.App, runKey string, output io.Writer, jsonOutput bool) error {
	result, err := app.ResumeRun(ctx, runKey, func(observation agent.Observation) {
		if !jsonOutput && observation.Type == agent.ObservationTextDelta {
			fmt.Fprint(output, observation.Text)
		}
	}, nil)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writeJSON(output, result)
	}
	fmt.Fprintf(output, "\n\n[%s: %s, steps=%d, tokens=%d]\n", result.Outcome, result.StopReason, len(result.Steps), result.Usage.TotalTokens)
	return nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// runSlashCommand remains the command implementation used by tests and simple embedders.
func runSlashCommand(ctx context.Context, app *icoder.App, line string) (bool, error) {
	name, argument, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	argument = strings.TrimSpace(argument)
	switch name {
	case "exit", "quit", "q":
		return true, nil
	case "pwd":
		fmt.Println(app.WorkingDirectory())
	case "cd":
		_, err := app.ChangeDirectory(argument)
		return false, err
	case "session":
		fmt.Println(app.SessionID())
	case "use":
		return false, app.UseSession(ctx, argument)
	case "new":
		if argument == "" {
			var err error
			argument, err = icoder.NewSessionID()
			if err != nil {
				return false, err
			}
		}
		return false, app.UseSession(ctx, argument)
	case "clear":
		return false, app.ClearSession(ctx)
	case "sessions":
		_, err := app.Sessions(ctx)
		return false, err
	case "history":
		_, err := app.History(ctx)
		return false, err
	case "events":
		_, err := app.Events(ctx, 0)
		return false, err
	case "tools", "skills", "help", "?":
		return false, nil
	default:
		return false, fmt.Errorf("unknown command /%s", name)
	}
	return false, nil
}

func summarize(message agent.Message) string {
	if text := strings.TrimSpace(message.Text()); text != "" {
		return strings.ReplaceAll(text, "\n", " ")
	}
	if calls := message.ToolCalls(); len(calls) > 0 {
		return "tool_call " + calls[0].Name
	}
	if results := message.ToolResults(); len(results) > 0 {
		return "tool_result " + results[0].Name
	}
	return "<empty>"
}
