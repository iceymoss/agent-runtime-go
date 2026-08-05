package icoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/iceymoss/agent-runtime-go"
)

func TestOpenAIModelProjectsToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request = %s, authorization = %q", request.URL.Path, request.Header.Get("Authorization"))
		}
		response.Header().Set("Content-Type", "application/json")
		_, err := response.Write([]byte(`{"model":"demo","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"go.mod\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`))
		if err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	model := NewOpenAIModel("secret", server.URL)
	stream, err := model.Stream(context.Background(), &agent.GenerateRequest{Model: "demo", Messages: []agent.Message{agent.NewUserMessage("read")}, Tools: []agent.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}})
	if err != nil {
		t.Fatal(err)
	}
	var chunks []agent.StreamChunk
	for chunk := range stream {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 2 || chunks[0].Type != agent.ChunkToolCall || chunks[1].Response == nil || chunks[1].Response.FinishReason != agent.FinishToolCalls {
		t.Fatalf("chunks = %#v", chunks)
	}
}
