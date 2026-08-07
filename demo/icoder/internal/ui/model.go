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
	accent       = lipgloss.Color("#8B8CF8")
	accentSoft   = lipgloss.Color("#686AA8")
	muted        = lipgloss.Color("#73788A")
	subtle       = lipgloss.Color("#4B5060")
	userStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#D6D7FF")).Bold(true)
	agentStyle   = lipgloss.NewStyle().Foreground(accent).Bold(true)
	toolStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#A6ADC8"))
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#F38BA8"))
	success      = lipgloss.NewStyle().Foreground(lipgloss.Color("#94E2D5"))
	warningStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F9E2AF"))
	dimStyle     = lipgloss.NewStyle().Foreground(muted)
)

const (
	defaultInputPlaceholder  = `Try "fix lint errors"`
	queuedInputPlaceholder   = "Queue a follow-up"
	feedbackInputPlaceholder = "Tell iCoder what to do instead"
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
	prompt    icoder.ApprovalPrompt
	respond   chan icoder.ApprovalDecision
	submitted bool
}

type runEvent struct {
	observation *agent.Observation
	approval    *approvalEvent
	result      *agent.RunResult
	err         error
}

type commandResult struct {
	text           string
	history        []agent.Message
	sessions       []icoder.SessionInfo
	showSessions   bool
	replace        bool
	toggleDetails  bool
	clearApprovals bool
	err            error
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
	approvalIndex int
	feedbackMode  bool
	queuedPrompts []string
	autoScopes    map[string]bool
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
	{"/permissions [clear]", "show or clear automatic approval scopes"},
	{"/quit", "exit iCoder"},
}

func New(ctx context.Context, app *icoder.App, history []agent.Message) Model {
	input := textarea.New()
	input.Placeholder, input.Prompt = defaultInputPlaceholder, "❯ "
	input.SetHeight(1)
	input.CharLimit, input.ShowLineNumbers = 32<<10, false
	input.Focus()
	input.KeyMap.InsertNewline.SetKeys("alt+enter", "ctrl+j")
	spin := spinner.New()
	spin.Spinner, spin.Style = spinner.Dot, lipgloss.NewStyle().Foreground(accent)
	m := Model{app: app, ctx: ctx, input: input, spinner: spin, events: make(chan runEvent, 128), follow: true, autoScopes: make(map[string]bool)}
	m.loadHistory(history)
	return m
}

func (m Model) Init() tea.Cmd { return textarea.Blink }

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var commands []tea.Cmd
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.viewport.Width, m.viewport.Height = m.contentWidth(), max(3, msg.Height-8)
		m.input.SetWidth(max(16, m.contentWidth()-4))
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
		if m.feedbackMode {
			switch msg.String() {
			case "ctrl+c":
				m.feedbackMode = false
				m.input.Reset()
				if m.running {
					m.input.Placeholder = queuedInputPlaceholder
				} else {
					m.input.Placeholder = defaultInputPlaceholder
				}
				m.cancelRun()
				return m, nil
			case "esc":
				m.feedbackMode = false
				m.input.Reset()
				if m.running {
					m.input.Placeholder = queuedInputPlaceholder
				} else {
					m.input.Placeholder = defaultInputPlaceholder
				}
				m.latestStatus = "permission denied"
				return m, nil
			case "enter":
				value := strings.TrimSpace(m.input.Value())
				if value == "" {
					return m, nil
				}
				m.input.Reset()
				m.feedbackMode = false
				if m.running {
					m.input.Placeholder = queuedInputPlaceholder
					m.queuedPrompts = append(m.queuedPrompts, value)
					m.latestStatus = "feedback queued"
					return m, nil
				}
				commands = append(commands, m.beginPrompt(value)...)
				return m, tea.Batch(commands...)
			}
		}
		if m.pending != nil && !m.feedbackMode {
			if m.pending.submitted {
				if msg.String() == "ctrl+c" {
					m.cancelRun()
				}
				return m, nil
			}
			switch msg.String() {
			case "tab":
				if m.approvalIndex == 1 {
					m.approvalIndex = 0
				} else {
					m.approvalIndex = 1
				}
				return m, nil
			case "right", "down", "l", "j":
				m.approvalIndex = (m.approvalIndex + 1) % 3
				return m, nil
			case "shift+tab", "left", "up", "h", "k":
				m.approvalIndex = (m.approvalIndex + 2) % 3
				return m, nil
			case "y", "1":
				m.approvalIndex = 0
				m.confirmApproval()
				return m, waitEvent(m.events)
			case "2":
				m.approvalIndex = 1
				m.confirmApproval()
				return m, waitEvent(m.events)
			case "n", "3", "esc":
				m.approvalIndex = 2
				m.confirmApproval()
				return m, waitEvent(m.events)
			case "enter":
				m.confirmApproval()
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
			if m.running {
				value := strings.TrimSpace(m.input.Value())
				if value != "" {
					m.input.Reset()
					m.queuedPrompts = append(m.queuedPrompts, value)
					m.latestStatus = fmt.Sprintf("%d follow-up queued", len(m.queuedPrompts))
				}
				return m, nil
			}
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
					commands = append(commands, m.beginPrompt(value)...)
				}
				return m, tea.Batch(commands...)
			}
		}
	case runEvent:
		if msg.approval != nil {
			m.pending, m.approvalIndex, m.latestStatus = msg.approval, 0, "approval required for "+msg.approval.prompt.ToolName
			return m, waitEvent(m.events)
		}
		if msg.observation != nil {
			m.applyObservation(*msg.observation)
			m.syncViewport(false)
			return m, waitEvent(m.events)
		}
		m.running, m.pending = false, nil
		if !m.feedbackMode {
			m.input.Placeholder = defaultInputPlaceholder
		}
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
		if len(m.queuedPrompts) > 0 {
			next := m.queuedPrompts[0]
			m.queuedPrompts = m.queuedPrompts[1:]
			commands = append(commands, m.beginPrompt(next)...)
		}
	case commandResult:
		if msg.clearApprovals {
			m.clearCurrentApprovalScopes()
		}
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
	if !m.sessionPicker && (!m.running || m.pending == nil || m.feedbackMode) {
		m.input, cmd = m.input.Update(message)
		m.input.SetHeight(min(4, max(1, strings.Count(m.input.Value(), "\n")+1)))
		commands = append(commands, cmd)
	}
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
	header := m.headerView()
	if m.sessionPicker {
		content := header + "\n\n" + m.sessionPickerView() + "\n\n" + dimStyle.Render("  up/down select   enter open   n new   esc close")
		return m.columnView(content)
	}
	footer := m.footerView()
	if len(m.items) == 0 && strings.TrimSpace(m.streamed) == "" && !m.running {
		body := m.welcomeTranscriptView(footer)
		return m.columnView(body + "\n" + footer)
	}
	body := m.transcriptView(footer)
	return m.columnView(header + "\n\n" + body + "\n" + footer)
}

func (m Model) footerView() string {
	if m.feedbackMode {
		return m.feedbackView() + "\n" + m.statusView()
	}
	if m.pending != nil {
		return m.approvalView() + "\n" + m.statusView()
	}
	composer := m.composerView()
	if m.running {
		composer = m.queueView()
	}
	view := composer
	if suggestions := m.commandSuggestions(); len(suggestions) > 0 {
		view += "\n" + m.suggestionView(suggestions)
	}
	return view + "\n" + m.statusView()
}

func (m Model) transcriptView(footer string) string {
	viewport := m.viewport
	viewport.Width = m.contentWidth()
	height := m.height
	if height <= 0 {
		height = 24
	}
	viewport.Height = max(3, height-lipgloss.Height(footer)-3)
	return viewport.View()
}

func (m Model) welcomeTranscriptView(footer string) string {
	viewport := m.viewport
	viewport.Width = m.contentWidth()
	height := m.height
	if height <= 0 {
		height = 24
	}
	viewport.Height = max(3, height-lipgloss.Height(footer)-1)
	viewport.SetContent(m.welcomeView())
	return viewport.View()
}

func (m Model) columnView(content string) string {
	return lipgloss.NewStyle().Width(m.contentWidth()).MarginLeft(1).Render(content)
}

func (m Model) contentWidth() int {
	if m.width <= 0 {
		return 76
	}
	return max(20, m.width-2)
}

func (m Model) welcomeView() string {
	width := m.contentWidth()
	model, session, workspace, tools := "configured model", "new session", "workspace", 0
	if m.app != nil {
		model, session, workspace, tools = m.app.ModelName(), m.app.SessionID(), m.app.WorkingDirectory(), len(m.app.Tools())
	}
	left := lipgloss.NewStyle().Align(lipgloss.Center).Render(
		agentStyle.Render("Welcome back!") + "\n\n" +
			warningStyle.Render("  ▐▛███▜▌  ") + "\n" +
			warningStyle.Render(" ▝▜█████▛▘ ") + "\n" +
			warningStyle.Render("   ▘▘ ▝▝   ") + "\n\n" +
			dimStyle.Render(model+" · "+fmt.Sprintf("%d tools", tools)) + "\n" +
			dimStyle.Render(truncateLine(session, 30)) + "\n" +
			dimStyle.Render(truncateLine(workspace, 42)),
	)
	right := toolStyle.Bold(true).Render("Tips for getting started") + "\n\n" +
		"Ask iCoder to inspect, edit, and verify your code." + "\n" +
		dimStyle.Render(strings.Repeat("─", 34)) + "\n\n" +
		toolStyle.Bold(true).Render("Quick commands") + "\n" +
		"Type / to browse commands" + "\n" +
		"Use /permissions to review AUTO scopes" + "\n" +
		"Press Ctrl+O to expand tool details"
	if width < 90 {
		return titledFrame("iCoder", left, width)
	}
	leftWidth := min(46, max(34, width/3))
	rightWidth := max(30, width-leftWidth-5)
	columns := lipgloss.JoinHorizontal(lipgloss.Top,
		lipgloss.NewStyle().Width(leftWidth).Render(left),
		dimStyle.Render(" │ "),
		lipgloss.NewStyle().Width(rightWidth).Render(right),
	)
	return titledFrame("iCoder", columns, width)
}

func titledFrame(title, content string, width int) string {
	inner := max(1, width-2)
	title = "─── " + title + " "
	top := "╭" + title + strings.Repeat("─", max(0, inner-lipgloss.Width(title))) + "╮"
	lines := strings.Split(lipgloss.NewStyle().Width(inner).Render(content), "\n")
	for index, line := range lines {
		lines[index] = "│" + line + strings.Repeat(" ", max(0, inner-lipgloss.Width(line))) + "│"
	}
	return top + "\n" + strings.Join(lines, "\n") + "\n╰" + strings.Repeat("─", inner) + "╯"
}

func (m Model) headerView() string {
	brand := agentStyle.Render("icoder")
	if m.app == nil {
		return " " + brand
	}
	sessionWidth := max(4, min(20, m.contentWidth()-lipgloss.Width(brand)-4))
	session := dimStyle.Render(truncateLine(m.app.SessionID(), sessionWidth))
	left := " " + brand + "  " + session
	pathWidth := m.contentWidth() - lipgloss.Width(left) - 2
	if pathWidth < 12 {
		return left
	}
	right := dimStyle.Render(truncateLine(m.app.WorkingDirectory(), pathWidth))
	gap := strings.Repeat(" ", max(1, m.contentWidth()-lipgloss.Width(left)-lipgloss.Width(right)))
	return left + gap + right
}

func (m Model) composerView() string {
	rule := dimStyle.Render(strings.Repeat("─", m.contentWidth()))
	return rule + "\n" + m.input.View() + "\n" + rule
}

func (m Model) queueView() string {
	rule := dimStyle.Render(strings.Repeat("─", m.contentWidth()))
	return rule + "\n" + m.input.View() + "\n" + rule
}

func (m Model) feedbackView() string {
	rule := errorStyle.Render(strings.Repeat("─", m.contentWidth()))
	return errorStyle.Bold(true).Render("Tell iCoder what to do instead") + "\n" + rule + "\n" + m.input.View() + "\n" + rule
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
		content += agentStyle.Render("iCoder") + "\n" + m.streamed
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
		return lipgloss.NewStyle().BorderLeft(true).BorderStyle(lipgloss.ThickBorder()).BorderForeground(accentSoft).PaddingLeft(1).Render(userStyle.Render("You") + "\n" + item.content)
	case itemAssistant:
		return agentStyle.Render("iCoder") + "\n" + item.content
	case itemTool:
		icon, style := "~", warningStyle
		if item.state == stateSuccess {
			icon, style = "+", success
		} else if item.state == stateError {
			icon, style = "x", errorStyle
		}
		value := "  " + style.Render(icon) + " " + toolStyle.Render(item.title)
		if m.details && item.detail != "" {
			value += "\n" + lipgloss.NewStyle().MarginLeft(4).BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(subtle).PaddingLeft(1).Foreground(muted).Render(item.detail)
		}
		return value
	default:
		style := lipgloss.NewStyle().Foreground(muted).BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(subtle).PaddingLeft(1)
		if item.state == stateError {
			style = style.Foreground(errorStyle.GetForeground()).BorderForeground(errorStyle.GetForeground())
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
	if event != nil {
		event.submitted = true
		select {
		case event.respond <- decision:
		default:
			{
			}
		}
	}
	m.latestStatus = "checking approval"
}
func (m *Model) confirmApproval() {
	switch m.approvalIndex {
	case 1:
		m.grantAutoScope(m.pending.prompt.Action)
		m.answerApproval(icoder.ApprovalApproveAuto)
	case 2:
		m.feedbackMode = true
		m.input.Reset()
		m.input.Placeholder = feedbackInputPlaceholder
		m.answerApproval(icoder.ApprovalDeny)
	default:
		m.answerApproval(icoder.ApprovalApproveOnce)
	}
}
func (m *Model) cancelRun() {
	if m.cancel != nil {
		m.cancel()
	}
	m.pending = nil
	m.latestStatus = "cancelling current run..."
}

func (m *Model) beginPrompt(value string) []tea.Cmd {
	m.items = append(m.items, transcriptItem{kind: itemUser, content: value})
	m.streamed, m.running, m.follow, m.started = "", true, true, time.Now()
	m.input.Placeholder = queuedInputPlaceholder
	runCtx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.syncViewport(true)
	return []tea.Cmd{m.spinner.Tick, m.startRun(runCtx, value), waitEvent(m.events)}
}

func (m Model) approvalView() string {
	p := m.pending.prompt
	panelWidth := m.contentWidth()
	heading := warningStyle.Bold(true).Render("Permission required")
	if !p.ExpiresAt.IsZero() {
		expires := dimStyle.Render("expires " + p.ExpiresAt.Local().Format("15:04:05"))
		if lipgloss.Width(heading)+lipgloss.Width(expires)+2 <= panelWidth {
			heading += strings.Repeat(" ", panelWidth-lipgloss.Width(heading)-lipgloss.Width(expires)) + expires
		}
	}
	meta := toolStyle.Render(p.ToolName) + "  " + dimStyle.Render(p.Action)
	details := approvalDetails(p, panelWidth)
	actions := approvalActions(panelWidth, m.approvalIndex, approvalScopeLabel(p.Action))
	if m.pending.submitted {
		actions = warningStyle.Render(m.spinner.View()+" Checking approval status") + "    " + dimStyle.Render("ctrl+c  cancel")
	}
	rule := dimStyle.Render(strings.Repeat("─", panelWidth))
	content := heading + "\n" + meta + "\n\n" + details + "\n\n" + actions
	content = lipgloss.NewStyle().Width(panelWidth).Render(content)
	return rule + "\n" + content + "\n" + rule
}

func approvalDetails(prompt icoder.ApprovalPrompt, width int) string {
	var input map[string]any
	if json.Unmarshal([]byte(prompt.Input), &input) != nil {
		return dimStyle.Render("scope   "+truncateLine(prompt.Resource, max(8, width-8))) + "\n" + truncate(prompt.Input, 1000)
	}
	lines := make([]string, 0, len(input)+1)
	if target, ok := input["path"].(string); ok && target != "" {
		lines = append(lines, fieldLine("target", target, width))
		delete(input, "path")
	}
	if values, ok := input["paths"].([]any); ok {
		paths := make([]string, 0, len(values))
		for _, value := range values {
			if path, ok := value.(string); ok {
				paths = append(paths, path)
			}
		}
		if len(paths) > 0 {
			lines = append(lines, fieldLine("paths", strings.Join(paths, ", "), width))
		}
		delete(input, "paths")
	}
	if prompt.Resource != "" {
		lines = append(lines, fieldLine("scope", prompt.Resource, width))
	}
	keys := make([]string, 0, len(input))
	for key := range input {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, err := json.Marshal(input[key])
		if err != nil {
			continue
		}
		lines = append(lines, fieldLine(key, strings.Trim(string(value), `"`), width))
	}
	return strings.Join(lines, "\n")
}

func fieldLine(label, value string, width int) string {
	const labelWidth = 9
	label = truncateLine(label, labelWidth-1)
	prefix := dimStyle.Render(fmt.Sprintf("%-*s", labelWidth, label))
	return prefix + truncateLine(value, max(4, width-labelWidth))
}

func approvalActions(width, selected int, scope string) string {
	choices := []struct {
		label string
		style lipgloss.Style
	}{{"1. Yes", success}, {"2. Yes, " + scope, warningStyle}, {"3. No", errorStyle}}
	values := make([]string, len(choices))
	for index, choice := range choices {
		label := "  " + choice.label + "  "
		if index == selected {
			values[index] = choice.style.Copy().Bold(true).Reverse(true).Render(label)
		} else {
			values[index] = choice.style.Render(label)
		}
	}
	hint := dimStyle.Render("tab auto  up/down select  enter confirm")
	if width < 48 {
		hint = dimStyle.Render("tab auto  enter confirm")
	}
	return strings.Join(values, "\n") + "\n\n" + hint
}

func approvalScopeLabel(action string) string {
	switch action {
	case "workspace.write":
		return "allow workspace writes for this session"
	case "workspace.command":
		return "allow safe commands for this session"
	case "workspace.commit":
		return "allow Git commits for this session"
	case "network.read":
		return "allow network reads for this session"
	case "network.tool":
		return "allow network tools for this session"
	default:
		return "allow " + action + " for this session"
	}
}

func approvalScopeName(action string) string {
	switch action {
	case "workspace.write":
		return "writes"
	case "workspace.command":
		return "commands"
	case "workspace.commit":
		return "commits"
	case "network.read":
		return "network"
	case "network.tool":
		return "network tools"
	default:
		return action
	}
}

func (m Model) approvalScopeKey(action string) string {
	session := ""
	if m.app != nil {
		session = m.app.SessionID()
	}
	return session + "\x00" + action
}

func (m Model) hasAutoScope(action string) bool {
	return m.autoScopes[m.approvalScopeKey(action)]
}

func (m *Model) grantAutoScope(action string) {
	if m.autoScopes == nil {
		m.autoScopes = make(map[string]bool)
	}
	m.autoScopes[m.approvalScopeKey(action)] = true
}

func (m *Model) clearCurrentApprovalScopes() {
	prefix := "\x00"
	if m.app != nil {
		prefix = m.app.SessionID() + "\x00"
	}
	for key := range m.autoScopes {
		if strings.HasPrefix(key, prefix) {
			delete(m.autoScopes, key)
		}
	}
}

func (m Model) currentApprovalScopes() []string {
	prefix := "\x00"
	if m.app != nil {
		prefix = m.app.SessionID() + "\x00"
	}
	var scopes []string
	for key := range m.autoScopes {
		if strings.HasPrefix(key, prefix) {
			scopes = append(scopes, approvalScopeName(strings.TrimPrefix(key, prefix)))
		}
	}
	sort.Strings(scopes)
	return scopes
}

func (m Model) statusView() string {
	left, right := "", ""
	if m.feedbackMode {
		left = errorStyle.Render("permission denied")
		right = dimStyle.Render("enter continue · esc dismiss · ctrl+c cancel")
	} else if m.pending != nil {
		if m.pending.submitted {
			left, right = warningStyle.Render("checking approval"), dimStyle.Render("input locked")
		} else {
			left, right = warningStyle.Render("approval required"), dimStyle.Render("input locked")
		}
	} else if m.running {
		left = warningStyle.Render(m.spinner.View() + " working  " + time.Since(m.started).Round(time.Second).String())
		if len(m.queuedPrompts) > 0 {
			left += dimStyle.Render(fmt.Sprintf(" · %d queued", len(m.queuedPrompts)))
		}
		right = dimStyle.Render("enter queue · esc cancel · ctrl+o details")
	} else {
		status := m.latestStatus
		if status == "" {
			status = "ready"
		}
		if scopes := m.currentApprovalScopes(); len(scopes) > 0 {
			left = warningStyle.Bold(true).Render("AUTO: "+strings.Join(scopes, ", ")) + "  " + dimStyle.Render(status)
		} else {
			left = dimStyle.Render("manual mode on")
			if status != "ready" {
				left += dimStyle.Render(" · " + status)
			}
		}
		right = dimStyle.Render("ctrl+p commands · ctrl+l sessions · ctrl+o details")
	}
	if lipgloss.Width(left)+lipgloss.Width(right)+2 > m.contentWidth() {
		return " " + left
	}
	gap := strings.Repeat(" ", m.contentWidth()-lipgloss.Width(left)-lipgloss.Width(right)-1)
	return " " + left + gap + right
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
	return lipgloss.NewStyle().Width(max(16, m.contentWidth()-4)).Padding(0, 1).Border(lipgloss.RoundedBorder()).BorderForeground(subtle).Render(strings.Join(lines, "\n"))
}

func (m Model) sessionPickerView() string {
	lines := []string{agentStyle.Render("Sessions"), ""}
	visible := max(1, m.height-10)
	if m.height <= 0 {
		visible = 10
	}
	start := max(0, m.sessionIndex-visible/2)
	end := min(len(m.sessions), start+visible)
	start = max(0, end-visible)
	if start > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("  ... %d earlier", start)))
	}
	currentSession := ""
	if m.app != nil {
		currentSession = m.app.SessionID()
	}
	for i := start; i < end; i++ {
		session := m.sessions[i]
		cursor := "  "
		if i == m.sessionIndex {
			cursor = "> "
		}
		marker := " "
		if session.ID == currentSession {
			marker = "*"
		}
		lines = append(lines, fmt.Sprintf("%s%s %-20s  %4d messages  %s", cursor, marker, session.ID, session.Messages, session.UpdatedAt))
	}
	if end < len(m.sessions) {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("  ... %d later", len(m.sessions)-end)))
	}
	if len(m.sessions) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(muted).Render("No persisted sessions. Press n to create one."))
	}
	return lipgloss.NewStyle().Width(max(16, m.contentWidth()-4)).Padding(1, 1).Border(lipgloss.RoundedBorder()).BorderForeground(accentSoft).Render(strings.Join(lines, "\n"))
}

func (m Model) startRun(ctx context.Context, prompt string) tea.Cmd {
	return func() tea.Msg {
		go func() {
			autoScopes := make(map[string]bool, len(m.autoScopes))
			for key, allowed := range m.autoScopes {
				autoScopes[key] = allowed
			}
			approve := func(ctx context.Context, prompt icoder.ApprovalPrompt) (icoder.ApprovalDecision, error) {
				key := m.approvalScopeKey(prompt.Action)
				if autoScopes[key] {
					return icoder.ApprovalApproveAuto, nil
				}
				response := make(chan icoder.ApprovalDecision, 1)
				select {
				case m.events <- runEvent{approval: &approvalEvent{prompt: prompt, respond: response}}:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				select {
				case decision := <-response:
					if decision == icoder.ApprovalApproveAuto {
						autoScopes[key] = true
					}
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
			return commandResult{text: "Ctrl+P commands | Ctrl+L sessions | Ctrl+O tool details | PgUp/PgDn scroll | Esc cancel\n/new [id] /use <id> /sessions /clear /pwd /cd <path> /tools /details /diff /status /permissions [clear] /quit"}
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
		case "permissions":
			if argument == "clear" {
				return commandResult{text: "automatic approval scopes cleared for " + m.app.SessionID(), clearApprovals: true}
			}
			if argument != "" {
				return commandResult{err: fmt.Errorf("usage: /permissions [clear]")}
			}
			scopes := m.currentApprovalScopes()
			if len(scopes) == 0 {
				return commandResult{text: "No automatic approval scopes for " + m.app.SessionID() + "."}
			}
			return commandResult{text: "Automatic approval scopes for " + m.app.SessionID() + ":\n- " + strings.Join(scopes, "\n- ")}
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
		if message, ok := input["message"].(string); ok && call.Name == "git_commit" {
			return call.Name + " " + truncateLine(message, 60)
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

func truncateLine(value string, width int) string {
	if width <= 3 {
		return strings.Repeat(".", max(0, width))
	}
	if lipgloss.Width(value) <= width {
		return value
	}
	runes := []rune(value)
	for len(runes) > 0 && lipgloss.Width("..."+string(runes)) > width {
		runes = runes[1:]
	}
	return "..." + string(runes)
}

func Run(ctx context.Context, app *icoder.App) error {
	history, err := app.History(ctx)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(New(ctx, app, history), tea.WithAltScreen(), tea.WithContext(ctx), tea.WithMouseCellMotion()).Run()
	return err
}
