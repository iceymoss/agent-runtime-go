package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder"
)

var (
	accent     = lipgloss.Color("#7C6AF2")
	muted      = lipgloss.Color("#7B8496")
	userStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#58C7B0")).Bold(true)
	agentStyle = lipgloss.NewStyle().Foreground(accent).Bold(true)
	toolStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#E5A84B"))
	errorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B"))
	success    = lipgloss.NewStyle().Foreground(lipgloss.Color("#58C7B0"))
)

type itemKind string
type itemState string

const (
	itemUser      itemKind  = "user"
	itemAssistant itemKind  = "assistant"
	itemTool      itemKind  = "tool"
	itemSystem    itemKind  = "system"
	stateRunning  itemState = "running"
	stateSuccess  itemState = "success"
	stateError    itemState = "error"
)

type transcriptItem struct {
	kind       itemKind
	state      itemState
	id         string
	title      string
	content    string
	detail     string
	resultSeen bool
}

type approvalEvent struct {
	prompt  icoder.ApprovalPrompt
	respond chan icoder.ApprovalDecision
}

type runEvent struct {
	observation *agent.Observation
	approval    *approvalEvent
	result      *agent.RunResult
	err         error
}

type commandResult struct {
	text          string
	history       []agent.Message
	sessions      []icoder.SessionInfo
	showSessions  bool
	replace       bool
	toggleDetails bool
	err           error
}

type Model struct {
	app           *icoder.App
	ctx           context.Context
	viewport      viewport.Model
	input         textarea.Model
	spinner       spinner.Model
	events        chan runEvent
	cancel        context.CancelFunc
	items         []transcriptItem
	streamed      string
	width         int
	height        int
	running       bool
	quitting      bool
	details       bool
	follow        bool
	pending       *approvalEvent
	started       time.Time
	latestStatus  string
	commandIndex  int
	sessionPicker bool
	sessions      []icoder.SessionInfo
	sessionIndex  int
}

var slashCommands = []struct{ name, description string }{
	{"/help", "show keyboard and slash command help"},
	{"/new [id]", "create and switch to a session"},
	{"/sessions", "list persisted sessions"},
	{"/use <id>", "switch session and load its transcript"},
	{"/clear", "clear the active session"},
	{"/pwd", "show tool working directory"},
	{"/cd <path>", "change directory inside the workspace"},
	{"/tools", "list model-visible tools"},
	{"/details", "toggle expanded tool input and output"},
	{"/diff", "show the current workspace diff"},
	{"/status", "show session and runtime status"},
	{"/quit", "exit iCoder"},
}

func New(ctx context.Context, app *icoder.App, history []agent.Message) Model {
	input := textarea.New()
	input.Placeholder, input.Prompt = "Describe a coding task, or type / for commands...", "> "
	input.SetHeight(3)
	input.CharLimit, input.ShowLineNumbers = 32<<10, false
	input.Focus()
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	spin := spinner.New()
	spin.Spinner, spin.Style = spinner.Dot, lipgloss.NewStyle().Foreground(accent)
	m := Model{app: app, ctx: ctx, input: input, spinner: spin, events: make(chan runEvent, 128), follow: true}
	m.loadHistory(history)
	return m
}

func (m Model) Init() tea.Cmd { return textarea.Blink }

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var commands []tea.Cmd
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width, m.viewport.Height = max(20, msg.Width-4), max(5, msg.Height-10)
		m.input.SetWidth(max(20, msg.Width-6))
		m.syncViewport(true)
	case tea.KeyMsg:
		if m.sessionPicker {
			switch msg.String() {
			case "esc", "ctrl+l":
				m.sessionPicker = false
				return m, nil
			case "up", "k", "ctrl+p":
				m.sessionIndex = max(0, m.sessionIndex-1)
				return m, nil
			case "down", "j", "ctrl+n":
				if len(m.sessions) > 0 {
					m.sessionIndex = min(len(m.sessions)-1, m.sessionIndex+1)
				}
				return m, nil
			case "n":
				m.sessionPicker = false
				return m, m.slashCommand("/new")
			case "enter":
				if len(m.sessions) > 0 {
					id := m.sessions[m.sessionIndex].ID
					m.sessionPicker = false
					return m, m.slashCommand("/use " + id)
				}
			}
			return m, nil
		}
		if m.pending != nil {
			switch msg.String() {
			case "y", "1", "enter":
				m.answerApproval(icoder.ApprovalApproveOnce)
				return m, waitEvent(m.events)
			case "n", "2", "esc":
				m.answerApproval(icoder.ApprovalDeny)
				return m, waitEvent(m.events)
			case "ctrl+c":
				m.cancelRun()
				return m, nil
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c":
			if m.running {
				m.cancelRun()
				return m, nil
			}
			if m.input.Value() != "" {
				m.input.Reset()
				return m, nil
			}
			m.quitting = true
			return m, tea.Quit
		case "esc":
			if m.running {
				m.cancelRun()
				return m, nil
			}
		case "ctrl+o":
			m.details = !m.details
			m.syncViewport(false)
			return m, nil
		case "ctrl+p":
			if !m.running {
				m.input.SetValue("/")
				m.input.CursorEnd()
				return m, nil
			}
		case "ctrl+l":
			if !m.running {
				return m, m.slashCommand("/sessions")
			}
		case "pgup", "pgdown", "home":
			m.follow = false
		case "end":
			m.follow = true
		case "up":
			if m.commandSuggestions() != nil {
				m.commandIndex = max(0, m.commandIndex-1)
				return m, nil
			}
		case "down":
			if suggestions := m.commandSuggestions(); suggestions != nil {
				m.commandIndex = min(len(suggestions)-1, m.commandIndex+1)
				return m, nil
			}
		case "enter":
			if !m.running {
				if suggestions := m.commandSuggestions(); len(suggestions) > 0 && !m.exactCommand() && !strings.Contains(strings.TrimPrefix(m.input.Value(), "/"), " ") {
					m.input.SetValue(strings.Fields(suggestions[m.commandIndex].name)[0] + " ")
					m.input.CursorEnd()
					return m, nil
				}
				value := strings.TrimSpace(m.input.Value())
				if value != "" {
					m.input.Reset()
					m.commandIndex = 0
					if strings.HasPrefix(value, "/") {
						return m, m.slashCommand(value)
					}
					m.items = append(m.items, transcriptItem{kind: itemUser, content: value})
					m.streamed, m.running, m.follow, m.started = "", true, true, time.Now()
					runCtx, cancel := context.WithCancel(m.ctx)
					m.cancel = cancel
					m.syncViewport(true)
					commands = append(commands, m.spinner.Tick, m.startRun(runCtx, value), waitEvent(m.events))
				}
				return m, tea.Batch(commands...)
			}
		}
	case runEvent:
		if msg.approval != nil {
			m.pending, m.latestStatus = msg.approval, "approval required for "+msg.approval.prompt.ToolName
			return m, waitEvent(m.events)
		}
		if msg.observation != nil {
			m.applyObservation(*msg.observation)
			m.syncViewport(false)
			return m, waitEvent(m.events)
		}
		m.running, m.pending = false, nil
		if m.cancel != nil {
			m.cancel()
			m.cancel = nil
		}
		if msg.err != nil {
			if m.streamed != "" {
				m.items = append(m.items, transcriptItem{kind: itemAssistant, content: m.streamed})
			}
			m.items = append(m.items, transcriptItem{kind: itemSystem, state: stateError, title: "run failed", content: msg.err.Error()})
			m.latestStatus = "failed: " + msg.err.Error()
		} else if msg.result != nil {
			m.reconcileResult(msg.result)
			m.latestStatus = fmt.Sprintf("%s | %d steps | %d tokens | %s", msg.result.Outcome, len(msg.result.Steps), msg.result.Usage.TotalTokens, time.Since(m.started).Round(time.Second))
		}
		m.streamed = ""
		m.syncViewport(true)
	case commandResult:
		if msg.err != nil {
			m.items = append(m.items, transcriptItem{kind: itemSystem, state: stateError, title: "command failed", content: msg.err.Error()})
		} else if msg.replace {
			m.loadHistory(msg.history)
			if msg.text != "" {
				m.items = append(m.items, transcriptItem{kind: itemSystem, content: msg.text})
			}
		} else if msg.showSessions {
			m.sessions = msg.sessions
			m.sessionIndex, m.sessionPicker = 0, true
			for i, session := range m.sessions {
				if session.ID == m.app.SessionID() {
					m.sessionIndex = i
					break
				}
			}
		} else if msg.toggleDetails {
			m.details = !m.details
			if m.details {
				m.latestStatus = "tool details expanded"
			} else {
				m.latestStatus = "tool details collapsed"
			}
		} else if msg.text != "" {
			m.items = append(m.items, transcriptItem{kind: itemSystem, content: msg.text})
		}
		m.syncViewport(true)
	case spinner.TickMsg:
		if m.running {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			commands = append(commands, cmd)
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(message)
	commands = append(commands, cmd)
	m.viewport, cmd = m.viewport.Update(message)
	commands = append(commands, cmd)
	if m.viewport.AtBottom() {
		m.follow = true
	}
	return m, tea.Batch(commands...)
}

func (m Model) View() string {
	if m.quitting {
		return ""
	}
	header := lipgloss.NewStyle().Bold(true).Foreground(accent).Render(" iCoder")
	meta := lipgloss.NewStyle().Foreground(muted).Render(fmt.Sprintf("  %s  |  %s", m.app.SessionID(), m.app.WorkingDirectory()))
	body := m.viewport.View()
	separator := strings.Repeat("-", max(1, m.width))
	if m.sessionPicker {
		return header + meta + "\n" + separator + "\n" + m.sessionPickerView() + "\n" + separator + "\n" + lipgloss.NewStyle().Foreground(muted).Render("Up/Down select | Enter open | n new | Esc close")
	}
	if m.pending != nil {
		return header + meta + "\n" + separator + "\n" + body + "\n" + m.approvalView() + "\n" + separator + "\n" + m.statusView()
	}
	view := header + meta + "\n" + separator + "\n" + body + "\n" + separator + "\n" + m.input.View()
	if suggestions := m.commandSuggestions(); len(suggestions) > 0 {
		view += "\n" + m.suggestionView(suggestions)
	}
	return view + "\n" + m.statusView()
}

func (m *Model) applyObservation(observation agent.Observation) {
	switch observation.Type {
	case agent.ObservationTextDelta:
		m.streamed += observation.Text
	case agent.ObservationToolCall:
		if observation.ToolCall != nil {
			m.items = append(m.items, transcriptItem{kind: itemTool, state: stateRunning, id: observation.ToolCall.ID, title: toolSummary(*observation.ToolCall), detail: prettyJSON(observation.ToolCall.Input)})
		}
	case agent.ObservationToolResult:
		if observation.ToolResult != nil {
			index := m.toolIndex(observation.ToolResult.ToolCallID)
			state := stateSuccess
			if observation.ToolResult.IsError {
				state = stateError
			}
			if index >= 0 {
				m.items[index].state = state
				m.items[index].detail += "\n\nResult:\n" + truncate(observation.ToolResult.Content, 8000)
				m.items[index].resultSeen = true
			} else {
				m.items = append(m.items, transcriptItem{kind: itemTool, state: state, id: observation.ToolResult.ToolCallID, title: observation.ToolResult.Name, detail: truncate(observation.ToolResult.Content, 8000)})
			}
		}
	}
}

func (m *Model) reconcileResult(result *agent.RunResult) {
	for _, message := range result.Messages {
		for _, call := range message.ToolCalls() {
			if m.toolIndex(call.ID) < 0 {
				m.items = append(m.items, transcriptItem{kind: itemTool, state: stateRunning, id: call.ID, title: toolSummary(call), detail: prettyJSON(call.Input)})
			}
		}
		for _, toolResult := range message.ToolResults() {
			index := m.toolIndex(toolResult.ToolCallID)
			state := stateSuccess
			if toolResult.IsError {
				state = stateError
			}
			if index >= 0 {
				m.items[index].state = state
				if !m.items[index].resultSeen {
					m.items[index].detail += "\n\nResult:\n" + truncate(toolResult.Content, 8000)
				}
				m.items[index].resultSeen = true
			}
		}
	}
	text := result.Text
	if text == "" {
		text = m.streamed
	}
	if strings.TrimSpace(text) != "" {
		m.items = append(m.items, transcriptItem{kind: itemAssistant, content: text})
	}
}

func (m *Model) loadHistory(history []agent.Message) {
	m.items = nil
	m.streamed = ""
	for _, message := range history {
		if text := strings.TrimSpace(message.Text()); text != "" {
			kind := itemAssistant
			if message.Role == agent.RoleUser {
				kind = itemUser
			}
			m.items = append(m.items, transcriptItem{kind: kind, content: text})
		}
		for _, call := range message.ToolCalls() {
			m.items = append(m.items, transcriptItem{kind: itemTool, state: stateRunning, id: call.ID, title: toolSummary(call), detail: prettyJSON(call.Input)})
		}
		for _, result := range message.ToolResults() {
			index := m.toolIndex(result.ToolCallID)
			state := stateSuccess
			if result.IsError {
				state = stateError
			}
			if index >= 0 {
				m.items[index].state = state
				m.items[index].detail += "\n\nResult:\n" + truncate(result.Content, 8000)
				m.items[index].resultSeen = true
			}
		}
	}
}

func (m *Model) syncViewport(forceBottom bool) {
	content := m.renderItems()
	if m.streamed != "" {
		if content != "" {
			content += "\n\n"
		}
		content += agentStyle.Render("assistant") + "\n" + m.streamed
	}
	m.viewport.SetContent(content)
	if forceBottom || m.follow {
		m.viewport.GotoBottom()
	}
}
func (m Model) renderItems() string {
	parts := make([]string, 0, len(m.items))
	for _, item := range m.items {
		parts = append(parts, m.renderItem(item))
	}
	return strings.Join(parts, "\n\n")
}
func (m Model) renderItem(item transcriptItem) string {
	switch item.kind {
	case itemUser:
		return userStyle.Render("you") + "\n" + item.content
	case itemAssistant:
		return agentStyle.Render("assistant") + "\n" + item.content
	case itemTool:
		icon, style := "*", toolStyle
		if item.state == stateSuccess {
			icon, style = "+", success
		} else if item.state == stateError {
			icon, style = "!", errorStyle
		}
		value := style.Render(icon + " " + item.title)
		if m.details && item.detail != "" {
			value += "\n" + lipgloss.NewStyle().Foreground(muted).Render(item.detail)
		}
		return value
	default:
		style := lipgloss.NewStyle().Foreground(muted)
		if item.state == stateError {
			style = errorStyle
		}
		title := item.title
		if title != "" {
			title += "\n"
		}
		return style.Render(title + item.content)
	}
}

func (m Model) toolIndex(id string) int {
	for i := len(m.items) - 1; i >= 0; i-- {
		if m.items[i].kind == itemTool && m.items[i].id == id {
			return i
		}
	}
	return -1
}
func (m *Model) answerApproval(decision icoder.ApprovalDecision) {
	event := m.pending
	m.pending = nil
	if event != nil {
		select {
		case event.respond <- decision:
		default:
			{
			}
		}
	}
	m.latestStatus = "approval submitted"
}
func (m *Model) cancelRun() {
	if m.cancel != nil {
		m.cancel()
	}
	m.pending = nil
	m.latestStatus = "cancelling current run..."
}

func (m Model) approvalView() string {
	p := m.pending.prompt
	return "\n" + errorStyle.Bold(true).Render("Permission required") + "\n" + toolStyle.Render(p.ToolName) + "  " + p.Action + "\nResource: " + p.Resource + "\n" + truncate(prettyJSON(p.Input), 3000) + "\n\n" + success.Render("[y/1/Enter] allow once") + "  " + errorStyle.Render("[n/2/Esc] reject") + "  [Ctrl+C] cancel run"
}
func (m Model) statusView() string {
	if m.pending != nil {
		return errorStyle.Render("waiting for approval")
	}
	if m.running {
		return m.spinner.View() + fmt.Sprintf(" working | %s | Esc cancel | Ctrl+O details", time.Since(m.started).Round(time.Second))
	}
	status := m.latestStatus
	if status == "" {
		status = "ready"
	}
	return lipgloss.NewStyle().Foreground(muted).Render(status + " | Enter send | Ctrl+P commands | Ctrl+L sessions | Ctrl+O details")
}

func (m Model) commandSuggestions() []struct{ name, description string } {
	value := strings.TrimSpace(m.input.Value())
	if !strings.HasPrefix(value, "/") || strings.Contains(strings.TrimPrefix(value, "/"), " ") {
		return nil
	}
	var result []struct{ name, description string }
	for _, command := range slashCommands {
		if strings.HasPrefix(command.name, value) {
			result = append(result, command)
		}
	}
	return result
}

func (m Model) exactCommand() bool {
	value := strings.TrimSpace(m.input.Value())
	for _, command := range slashCommands {
		if value == strings.Fields(command.name)[0] {
			return true
		}
	}
	return false
}
func (m Model) suggestionView(values []struct{ name, description string }) string {
	var lines []string
	for i, value := range values {
		prefix := "  "
		if i == m.commandIndex {
			prefix = "> "
		}
		lines = append(lines, prefix+toolStyle.Render(value.name)+"  "+lipgloss.NewStyle().Foreground(muted).Render(value.description))
	}
	return strings.Join(lines, "\n")
}

func (m Model) sessionPickerView() string {
	lines := []string{agentStyle.Render("Sessions")}
	for i, session := range m.sessions {
		cursor := "  "
		if i == m.sessionIndex {
			cursor = "> "
		}
		marker := " "
		if session.ID == m.app.SessionID() {
			marker = "*"
		}
		lines = append(lines, fmt.Sprintf("%s%s %-20s  %4d messages  %s", cursor, marker, session.ID, session.Messages, session.UpdatedAt))
	}
	if len(m.sessions) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(muted).Render("No persisted sessions. Press n to create one."))
	}
	return strings.Join(lines, "\n")
}

func (m Model) startRun(ctx context.Context, prompt string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			approve := func(ctx context.Context, prompt icoder.ApprovalPrompt) (icoder.ApprovalDecision, error) {
				response := make(chan icoder.ApprovalDecision, 1)
				select {
				case m.events <- runEvent{approval: &approvalEvent{prompt: prompt, respond: response}}:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				select {
				case decision := <-response:
					return decision, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			result, err := m.app.RunWithApproval(ctx, prompt, func(observation agent.Observation) {
				copy := observation
				select {
				case m.events <- runEvent{observation: &copy}:
				case <-ctx.Done():
				}
			}, approve)
			m.events <- runEvent{result: result, err: err}
		}()
		return nil
	}
}
func waitEvent(events <-chan runEvent) tea.Cmd { return func() tea.Msg { return <-events } }

func (m Model) slashCommand(line string) tea.Cmd {
	name, argument, _ := strings.Cut(strings.TrimPrefix(line, "/"), " ")
	argument = strings.TrimSpace(argument)
	return func() tea.Msg {
		switch name {
		case "help", "?":
			return commandResult{text: "Ctrl+P commands | Ctrl+L sessions | Ctrl+O tool details | PgUp/PgDn scroll | Esc cancel\n/new [id] /use <id> /sessions /clear /pwd /cd <path> /tools /details /diff /status /quit"}
		case "quit", "exit", "q":
			return tea.Quit()
		case "pwd":
			return commandResult{text: m.app.WorkingDirectory()}
		case "cd":
			path, err := m.app.ChangeDirectory(argument)
			return commandResult{text: path, err: err}
		case "details":
			return commandResult{toggleDetails: true}
		case "diff":
			value, err := m.app.GitDiff(m.ctx)
			if err == nil && strings.TrimSpace(value) == "" {
				value = "Working tree has no unstaged diff."
			}
			return commandResult{text: value, err: err}
		case "status":
			return commandResult{text: fmt.Sprintf("session: %s\ncwd: %s\ntools: %d\nrun: idle", m.app.SessionID(), m.app.WorkingDirectory(), len(m.app.Tools()))}
		case "use":
			if err := m.app.UseSession(m.ctx, argument); err != nil {
				return commandResult{err: err}
			}
			history, err := m.app.History(m.ctx)
			return commandResult{text: "using " + m.app.SessionID(), history: history, replace: true, err: err}
		case "new":
			if argument == "" {
				var err error
				argument, err = icoder.NewSessionID()
				if err != nil {
					return commandResult{err: err}
				}
			}
			if err := m.app.UseSession(m.ctx, argument); err != nil {
				return commandResult{err: err}
			}
			return commandResult{text: "new session " + m.app.SessionID(), replace: true}
		case "clear":
			err := m.app.ClearSession(m.ctx)
			return commandResult{text: "session cleared", replace: true, err: err}
		case "tools":
			tools := m.app.Tools()
			sort.Strings(tools)
			return commandResult{text: strings.Join(tools, "\n")}
		case "sessions":
			sessions, err := m.app.Sessions(m.ctx)
			if err != nil {
				return commandResult{err: err}
			}
			return commandResult{sessions: sessions, showSessions: true}
		default:
			return commandResult{err: fmt.Errorf("unknown command /%s", name)}
		}
	}
}

func toolSummary(call agent.ToolCall) string {
	var input map[string]any
	if json.Unmarshal([]byte(call.Input), &input) == nil {
		if path, ok := input["path"].(string); ok {
			return call.Name + " " + path
		}
		if program, ok := input["program"].(string); ok {
			return call.Name + " " + program
		}
	}
	return call.Name
}
func prettyJSON(value string) string {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) != nil {
		return value
	}
	data, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		return value
	}
	return string(data)
}
func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n... truncated"
}

func Run(ctx context.Context, app *icoder.App) error {
	history, err := app.History(ctx)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(New(ctx, app, history), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithMouseCellMotion()).Run()
	return err
}
