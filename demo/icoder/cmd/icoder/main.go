package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "icoder:", err)
		os.Exit(1)
	}
}

func run() error {
	var config icoder.Config
	var task string
	var events, interactive bool
	flag.StringVar(&task, "task", "", "coding task to perform")
	flag.StringVar(&config.Workspace, "workspace", ".", "workspace root")
	flag.StringVar(&config.Database, "db", "", "SQLite database path")
	flag.StringVar(&config.SkillsRoot, "skills", "", "filesystem skills root")
	flag.StringVar(&config.MCPURL, "mcp-url", "", "optional Streamable HTTP MCP endpoint")
	flag.StringVar(&config.SessionID, "session", "", "session identifier; defaults to a random 16-character ID")
	flag.StringVar(&config.BaseURL, "base-url", "", "OpenAI-compatible base URL")
	flag.StringVar(&config.Model, "model", "", "provider model ID")
	flag.BoolVar(&config.AllowWrites, "allow-writes", false, "allow workspace writes without an approval blocker")
	flag.BoolVar(&events, "events", false, "replay persisted terminal events")
	flag.BoolVar(&interactive, "interactive", false, "start an interactive session")
	flag.BoolVar(&interactive, "i", false, "start an interactive session")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app, err := icoder.NewApp(ctx, config)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := app.Close(context.Background()); closeErr != nil {
			fmt.Fprintln(os.Stderr, "icoder cleanup:", closeErr)
		}
	}()
	if events {
		items, err := app.Events(ctx, 0)
		if err != nil {
			return err
		}
		for _, item := range items {
			fmt.Println(item)
		}
		return nil
	}
	if interactive || task == "" {
		return repl(ctx, app)
	}
	return runTask(ctx, app, task)
}

func runTask(ctx context.Context, app *icoder.App, task string) error {
	streamed := false
	result, err := app.Run(ctx, task, func(observation agent.Observation) {
		if observation.Type == agent.ObservationTextDelta {
			streamed = true
			fmt.Print(observation.Text)
		}
	})
	if err != nil {
		return err
	}
	if result.Text != "" && !streamed {
		fmt.Println(result.Text)
	}
	fmt.Printf("\n[%s: %s, steps=%d, tokens=%d]\n", result.Outcome, result.StopReason, len(result.Steps), result.Usage.TotalTokens)
	return nil
}

func repl(ctx context.Context, app *icoder.App) error {
	fmt.Println("iCoder interactive mode. Type /help for commands.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for {
		fmt.Printf("icoder[%s:%s]> ", app.SessionID(), displayPath(app.WorkingDirectory()))
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return err
			}
			return nil
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			exit, err := runSlashCommand(ctx, app, line)
			if err != nil {
				fmt.Fprintln(os.Stderr, "command:", err)
			}
			if exit {
				return nil
			}
			continue
		}
		if err := runTask(ctx, app, line); err != nil {
			fmt.Fprintln(os.Stderr, "run:", err)
		}
	}
}

func runSlashCommand(ctx context.Context, app *icoder.App, line string) (bool, error) {
	name, argument, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	argument = strings.TrimSpace(argument)
	switch name {
	case "help", "?":
		printHelp()
	case "exit", "quit", "q":
		return true, nil
	case "pwd":
		fmt.Println(app.WorkingDirectory())
	case "cd":
		path, err := app.ChangeDirectory(argument)
		if err != nil {
			return false, err
		}
		fmt.Println(path)
	case "session":
		fmt.Println(app.SessionID())
	case "sessions":
		sessions, err := app.Sessions(ctx)
		if err != nil {
			return false, err
		}
		for _, session := range sessions {
			marker := " "
			if session.ID == app.SessionID() {
				marker = "*"
			}
			fmt.Printf("%s %-24s revision=%d messages=%d updated=%s\n", marker, session.ID, session.Revision, session.Messages, session.UpdatedAt)
		}
	case "use":
		if err := app.UseSession(ctx, argument); err != nil {
			return false, err
		}
		fmt.Println("using session", app.SessionID())
	case "new":
		if argument == "" {
			generated, err := icoder.NewSessionID()
			if err != nil {
				return false, err
			}
			argument = generated
		}
		if err := app.UseSession(ctx, argument); err != nil {
			return false, err
		}
		fmt.Println("created session", app.SessionID())
	case "history":
		history, err := app.History(ctx)
		if err != nil {
			return false, err
		}
		for index, message := range history {
			fmt.Printf("%d %-9s %s\n", index+1, message.Role, summarize(message))
		}
	case "clear":
		if err := app.ClearSession(ctx); err != nil {
			return false, err
		}
		fmt.Println("cleared session", app.SessionID())
	case "events":
		events, err := app.Events(ctx, 0)
		if err != nil {
			return false, err
		}
		for _, item := range events {
			fmt.Println(item)
		}
	case "tools":
		tools := app.Tools()
		sort.Strings(tools)
		for _, tool := range tools {
			fmt.Println(tool)
		}
	case "skills":
		for _, skill := range app.Skills() {
			first, _, _ := strings.Cut(skill, "\n")
			fmt.Println(first)
		}
	default:
		return false, fmt.Errorf("unknown command /%s", name)
	}
	return false, nil
}

func printHelp() {
	fmt.Println(`/help                 show commands
/pwd                  show the current working directory
/cd <path>            change directory inside the workspace; /cd / returns to root
/session              show the active session
/sessions             list persisted sessions
/use <id>             switch to or create a session
/new [id]             create and switch; omitted ID is a random 16-character hex value
/history              show active session messages
/clear                delete active session history and events
/events               replay active session terminal events
/tools                list model-visible tools
/skills               list loaded skill capability messages
/exit                 leave interactive mode`)
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

func displayPath(path string) string {
	if len(path) <= 40 {
		return path
	}
	return "..." + path[len(path)-37:]
}
