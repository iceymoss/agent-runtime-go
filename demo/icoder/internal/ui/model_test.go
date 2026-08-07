package ui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder"
)

func TestModelAcceptsConsecutiveStreamEvents(t *testing.T) {
	model := Model{ctx: context.Background(), events: make(chan runEvent, 1), running: true}
	model.items = append(model.items, transcriptItem{kind: itemUser, content: "test"})

	updated, _ := model.Update(runEvent{observation: &agent.Observation{Type: agent.ObservationTextDelta, Text: "hello"}})
	model = updated.(Model)
	updated, _ = model.Update(runEvent{observation: &agent.Observation{Type: agent.ObservationTextDelta, Text: " world"}})
	model = updated.(Model)

	if model.streamed != "hello world" {
		t.Fatalf("streamed = %q", model.streamed)
	}
	if transcript := model.renderItems(); !strings.Contains(transcript, "test") {
		t.Fatalf("transcript = %q", transcript)
	}
	if _, ok := any(model).(tea.Model); !ok {
		t.Fatal("Model does not implement tea.Model")
	}
}

func TestModelUpdatesToolByCallID(t *testing.T) {
	model := Model{events: make(chan runEvent, 1), running: true}
	call := agent.ToolCall{ID: "call-1", Name: "read_file", Input: `{"path":"main.go"}`}
	updated, _ := model.Update(runEvent{observation: &agent.Observation{Type: agent.ObservationToolCall, ToolCall: &call}})
	model = updated.(Model)
	result := agent.ToolResult{ToolCallID: "call-1", Name: "read_file", Content: "package main"}
	updated, _ = model.Update(runEvent{observation: &agent.Observation{Type: agent.ObservationToolResult, ToolResult: &result}})
	model = updated.(Model)
	if len(model.items) != 1 || model.items[0].state != stateSuccess || !strings.Contains(model.items[0].detail, "package main") {
		t.Fatalf("items = %#v", model.items)
	}
}

func TestModelAnswersApproval(t *testing.T) {
	response := make(chan icoder.ApprovalDecision, 1)
	model := Model{running: true, events: make(chan runEvent, 1), pending: &approvalEvent{respond: response}}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = updated.(Model)
	if model.pending == nil || !model.pending.submitted {
		t.Fatal("approval did not enter submitted state")
	}
	select {
	case decision := <-response:
		if decision != icoder.ApprovalApproveOnce {
			t.Fatalf("decision = %q", decision)
		}
	default:
		t.Fatal("approval response was not sent")
	}
}

func TestModelSelectsAutoApprovalWithTab(t *testing.T) {
	response := make(chan icoder.ApprovalDecision, 1)
	model := New(context.Background(), nil, nil)
	model.running = true
	model.pending = &approvalEvent{respond: response, prompt: icoder.ApprovalPrompt{Action: "workspace.write"}}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	model = updated.(Model)
	if model.approvalIndex != 1 {
		t.Fatalf("approvalIndex = %d, want Auto", model.approvalIndex)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(Model)
	if !model.hasAutoScope("workspace.write") || model.pending == nil || !model.pending.submitted {
		t.Fatalf("scopes=%v pending=%#v", model.currentApprovalScopes(), model.pending)
	}
	select {
	case decision := <-response:
		if decision != icoder.ApprovalApproveAuto {
			t.Fatalf("decision = %q", decision)
		}
	default:
		t.Fatal("auto approval response was not sent")
	}
}

func TestModelAutoApprovalScopesAreActionSpecific(t *testing.T) {
	model := New(context.Background(), nil, nil)
	model.grantAutoScope("workspace.write")
	if !model.hasAutoScope("workspace.write") || model.hasAutoScope("workspace.command") || !strings.Contains(model.statusView(), "AUTO: writes") {
		t.Fatalf("scopes=%v status=%q", model.currentApprovalScopes(), model.statusView())
	}
	model.clearCurrentApprovalScopes()
	if len(model.currentApprovalScopes()) != 0 {
		t.Fatalf("scopes after clear = %v", model.currentApprovalScopes())
	}
}

func TestModelDoesNotBufferTaskInputWhileRunning(t *testing.T) {
	model := New(context.Background(), nil, nil)
	model.running = true
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = updated.(Model)
	if model.input.Value() != "" {
		t.Fatalf("running input = %q, want empty", model.input.Value())
	}
}

func TestModelDoesNotRouteSubmittedApprovalToTaskInput(t *testing.T) {
	response := make(chan icoder.ApprovalDecision, 1)
	model := New(context.Background(), nil, nil)
	model.running = true
	model.pending = &approvalEvent{respond: response}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = updated.(Model)
	if model.input.Value() != "" || model.pending == nil || !model.pending.submitted {
		t.Fatalf("input=%q pending=%#v", model.input.Value(), model.pending)
	}
}

func TestLoadHistoryRestoresToolState(t *testing.T) {
	call := agent.ToolCall{ID: "call-1", Name: "git_status", Input: `{}`}
	result := agent.ToolResult{ToolCallID: "call-1", Name: "git_status", Content: "clean"}
	history := []agent.Message{
		{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}},
		{Role: agent.RoleTool, Parts: []agent.ContentPart{{Type: agent.PartToolResult, ToolResult: &result}}},
	}
	model := Model{}
	model.loadHistory(history)
	if len(model.items) != 1 || model.items[0].state != stateSuccess || !model.items[0].resultSeen {
		t.Fatalf("items = %#v", model.items)
	}
}

func TestSessionPickerNavigation(t *testing.T) {
	model := Model{sessionPicker: true, sessions: []icoder.SessionInfo{{ID: "one"}, {ID: "two"}}}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = updated.(Model)
	if model.sessionIndex != 1 {
		t.Fatalf("sessionIndex = %d", model.sessionIndex)
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = updated.(Model)
	if model.sessionPicker {
		t.Fatal("session picker remained open")
	}
}

func TestRenderItemUsesCompactConversationHierarchy(t *testing.T) {
	model := Model{}
	user := model.renderItem(transcriptItem{kind: itemUser, content: "Add a Python example"})
	assistant := model.renderItem(transcriptItem{kind: itemAssistant, content: "I will add it."})
	tool := model.renderItem(transcriptItem{kind: itemTool, state: stateSuccess, title: "write_file examples/start.py"})
	if !strings.Contains(user, "You") || !strings.Contains(user, "Add a Python example") || !strings.Contains(assistant, "iCoder\nI will add it.") || !strings.Contains(tool, "+ write_file examples/start.py") {
		t.Fatalf("user=%q assistant=%q tool=%q", user, assistant, tool)
	}
}

func TestTruncateLineKeepsRightmostWorkspacePath(t *testing.T) {
	value := truncateLine("/home/jeff/projects/agent-runtime-go/demo/icoder", 24)
	if !strings.HasPrefix(value, "...") || !strings.HasSuffix(value, "demo/icoder") || lipgloss.Width(value) > 24 {
		t.Fatalf("truncateLine() = %q width=%d", value, lipgloss.Width(value))
	}
}

func TestNarrowStatusHidesShortcutHints(t *testing.T) {
	model := Model{width: 24, latestStatus: "ready"}
	status := model.statusView()
	if !strings.Contains(status, "manual mode on") || strings.Contains(status, "commands") || lipgloss.Width(status) > model.contentWidth() {
		t.Fatalf("status = %q width=%d", status, lipgloss.Width(status))
	}
}

func TestApprovalViewUsesWorkspaceWidthAndSummarizesTarget(t *testing.T) {
	model := Model{width: 300, pending: &approvalEvent{prompt: icoder.ApprovalPrompt{
		ToolName: "create_directory", Action: "workspace.write", Resource: "/home/jeff/icey/open-source/agent-runtime-go/demo/icoder",
		Input: `{"parents":true,"path":"rust_beginner/src"}`, ExpiresAt: time.Date(2026, 8, 7, 17, 47, 36, 0, time.Local),
	}}}
	view := model.approvalView()
	if lipgloss.Width(view) != model.contentWidth() || !strings.Contains(view, "target") || !strings.Contains(view, "rust_beginner/src") || !strings.Contains(view, "parents") || strings.Contains(view, `"path"`) {
		t.Fatalf("approval width=%d view=%q", lipgloss.Width(view), view)
	}
}

func TestNarrowApprovalStacksActions(t *testing.T) {
	model := Model{width: 40, pending: &approvalEvent{prompt: icoder.ApprovalPrompt{ToolName: "run_command", Action: "workspace.command", Resource: "workspace", Input: `{}`}}}
	view := model.approvalView()
	choicesLine, hintLine := -1, -1
	for index, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "1. Yes") {
			choicesLine = index
		}
		if strings.Contains(line, "tab auto") {
			hintLine = index
		}
	}
	if lipgloss.Width(view) > model.contentWidth() || choicesLine < 0 || hintLine <= choicesLine+2 || !strings.Contains(view, "2. Yes, allow safe commands") || !strings.Contains(view, "session") || !strings.Contains(view, "3. No") {
		t.Fatalf("approval width=%d contentWidth=%d view=%q", lipgloss.Width(view), model.contentWidth(), view)
	}
}

func TestContentColumnUsesWideTerminalWorkspace(t *testing.T) {
	model := Model{width: 180}
	view := model.columnView("content")
	if model.contentWidth() != 178 || !strings.HasPrefix(view, " content") || lipgloss.Width(view) != 179 {
		t.Fatalf("contentWidth=%d renderedWidth=%d view=%q", model.contentWidth(), lipgloss.Width(view), view)
	}
}

func TestApprovalFooterShrinksTranscriptToTerminalHeight(t *testing.T) {
	model := New(context.Background(), nil, nil)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updated.(Model)
	model.running = true
	model.pending = &approvalEvent{prompt: icoder.ApprovalPrompt{ToolName: "create_directory", Action: "workspace.write", Resource: "/workspace", Input: `{"path":"src"}`}}
	footer := model.footerView()
	transcript := model.transcriptView(footer)
	want := max(3, model.height-lipgloss.Height(footer)-3)
	if lipgloss.Height(transcript) != want || lipgloss.Height(model.View()) > model.height {
		t.Fatalf("footer=%d transcript=%d want=%d view=%d", lipgloss.Height(footer), lipgloss.Height(transcript), want, lipgloss.Height(model.View()))
	}
}

func TestEmptyTranscriptShowsTaskPrompt(t *testing.T) {
	model := New(context.Background(), nil, nil)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = updated.(Model)
	view := model.View()
	if !strings.Contains(view, "Welcome back!") || !strings.Contains(view, "Try \"fix lint errors\"") || strings.Count(view, "❯") != 1 {
		t.Fatalf("view = %q", view)
	}
}

func TestWideWelcomeUsesTwoColumnPanel(t *testing.T) {
	model := New(context.Background(), nil, nil)
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 180, Height: 32})
	model = updated.(Model)
	view := model.View()
	if !strings.Contains(view, "Welcome back!") || !strings.Contains(view, "Tips for getting started") || !strings.Contains(view, " │ ") || !strings.Contains(view, "manual mode on") || lipgloss.Width(view) > model.width {
		t.Fatalf("view width=%d view=%q", lipgloss.Width(view), view)
	}
}

func TestSessionPickerKeepsSelectionVisibleInShortTerminal(t *testing.T) {
	model := Model{width: 80, height: 14, sessionPicker: true, sessionIndex: 8}
	for index := range 12 {
		model.sessions = append(model.sessions, icoder.SessionInfo{ID: fmt.Sprintf("session-%02d", index)})
	}
	view := model.sessionPickerView()
	if !strings.Contains(view, "session-08") || !strings.Contains(view, "earlier") || !strings.Contains(view, "later") || lipgloss.Height(view) > model.height-2 {
		t.Fatalf("picker height=%d view=%q", lipgloss.Height(view), view)
	}
}
