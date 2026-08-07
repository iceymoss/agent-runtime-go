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

	"github.com/iceymoss/agent-runtime-go"
	icodereval "github.com/iceymoss/agent-runtime-go/demo/icoder/internal/eval"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/ui"
	"github.com/iceymoss/agent-runtime-go/event"
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
