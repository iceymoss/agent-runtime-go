package ui

import (
	"context"
	"strings"
	"testing"

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
	if !strings.Contains(status, "ready") || strings.Contains(status, "commands") || lipgloss.Width(status) > model.contentWidth() {
		t.Fatalf("status = %q width=%d", status, lipgloss.Width(status))
	}
}
