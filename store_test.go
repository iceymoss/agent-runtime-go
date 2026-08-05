package agent

import (
	"context"
	"testing"
)

func TestMemoryStorePersistsMessagesAndAddsUsage(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	session := Session{ID: 1, AgentKey: "test", Status: SessionActive}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession() 报错: %v", err)
	}
	if err := store.AppendMessages(ctx, 1, NewUserMessage("hi")); err != nil {
		t.Fatalf("AppendMessages() 报错: %v", err)
	}
	if err := store.AddUsage(ctx, 1, Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}); err != nil {
		t.Fatalf("AddUsage() 第一次报错: %v", err)
	}
	if err := store.AddUsage(ctx, 1, Usage{PromptTokens: 7, CompletionTokens: 4, TotalTokens: 11}); err != nil {
		t.Fatalf("AddUsage() 第二次报错: %v", err)
	}

	got, err := store.GetSession(ctx, 1)
	if err != nil {
		t.Fatalf("GetSession() 报错: %v", err)
	}
	if got.Usage != (Usage{PromptTokens: 10, CompletionTokens: 6, TotalTokens: 16}) {
		t.Fatalf("Usage = %+v, 期望累计用量", got.Usage)
	}
	messages, err := store.ListMessages(ctx, 1)
	if err != nil {
		t.Fatalf("ListMessages() 报错: %v", err)
	}
	if len(messages) != 1 || messages[0].Text() != "hi" {
		t.Fatalf("Messages = %+v, 期望一条 hi", messages)
	}
}

func TestMemoryStoreReturnsCopies(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.CreateSession(ctx, Session{ID: 1}); err != nil {
		t.Fatalf("CreateSession() 报错: %v", err)
	}
	if err := store.AppendMessages(ctx, 1, NewUserMessage("original")); err != nil {
		t.Fatalf("AppendMessages() 报错: %v", err)
	}

	messages, err := store.ListMessages(ctx, 1)
	if err != nil {
		t.Fatalf("ListMessages() 报错: %v", err)
	}
	messages[0].Parts[0].Text = "mutated"
	again, err := store.ListMessages(ctx, 1)
	if err != nil {
		t.Fatalf("ListMessages() 第二次报错: %v", err)
	}
	if again[0].Text() != "original" {
		t.Fatalf("内存存储被外部切片修改: %q", again[0].Text())
	}
}
