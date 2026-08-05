package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
	"github.com/iceymoss/agent-runtime-go/demo/icoder/internal/icoder"
)

func TestSlashCommandsChangeHostState(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "pkg", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	app, err := icoder.NewApp(context.Background(), icoder.Config{
		APIKey: "fixture", BaseURL: server.URL, Model: "fixture",
		Workspace: workspace, Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "initial",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()

	exit, err := runSlashCommand(context.Background(), app, "/cd pkg/agent")
	if err != nil || exit || app.WorkingDirectory() != filepath.Join(workspace, "pkg", "agent") {
		t.Fatalf("/cd: exit=%v cwd=%q error=%v", exit, app.WorkingDirectory(), err)
	}
	if _, err := runSlashCommand(context.Background(), app, "/use review"); err != nil || app.SessionID() != "review" {
		t.Fatalf("/use: session=%q error=%v", app.SessionID(), err)
	}
	if _, err := runSlashCommand(context.Background(), app, "/clear"); err != nil {
		t.Fatalf("/clear error=%v", err)
	}
	exit, err = runSlashCommand(context.Background(), app, "/exit")
	if err != nil || !exit {
		t.Fatalf("/exit: exit=%v error=%v", exit, err)
	}
}

func TestSummarizeCanonicalMessages(t *testing.T) {
	if got := summarize(agent.NewUserMessage("hello\nworld")); got != "hello world" {
		t.Fatalf("summarize(text) = %q", got)
	}
	call := agent.ToolCall{ID: "call", Name: "read_file", Input: `{}`}
	message := agent.Message{Role: agent.RoleAssistant, Parts: []agent.ContentPart{{Type: agent.PartToolCall, ToolCall: &call}}}
	if got := summarize(message); got != "tool_call read_file" {
		t.Fatalf("summarize(tool call) = %q", got)
	}
}

func TestNewCommandGeneratesRandomSessionID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	app, err := icoder.NewApp(context.Background(), icoder.Config{
		APIKey: "fixture", BaseURL: server.URL, Model: "fixture",
		Workspace: t.TempDir(), Database: filepath.Join(t.TempDir(), "icoder.db"), SessionID: "initial",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := app.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	if _, err := runSlashCommand(context.Background(), app, "/new"); err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(app.SessionID()) {
		t.Fatalf("SessionID = %q", app.SessionID())
	}
}
