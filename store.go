package agent

import (
	"context"
	"errors"
	"sync"
)

// ErrSessionNotFound indicates that a runtime session does not exist.
var ErrSessionNotFound = errors.New("agent: 会话不存在")

// SessionStatus is the generic runtime session lifecycle.
type SessionStatus string

const (
	SessionActive    SessionStatus = "active"
	SessionFinished  SessionStatus = "finished"
	SessionFailed    SessionStatus = "failed"
	SessionAbandoned SessionStatus = "abandoned"
)

// Session is the persistence-neutral runtime session representation.
type Session struct {
	ID        uint          `json:"id"`
	UserID    uint          `json:"user_id"`
	AgentKey  string        `json:"agent_key"`
	Identity  string        `json:"identity,omitempty"`
	Status    SessionStatus `json:"status"`
	ModelName string        `json:"model_name,omitempty"`
	Metadata  string        `json:"metadata,omitempty"`
	Usage     Usage         `json:"usage"`
}

// SessionStore persists generic runtime sessions and their token usage.
type SessionStore interface {
	CreateSession(ctx context.Context, session Session) error
	GetSession(ctx context.Context, sessionID uint) (Session, error)
	UpdateSessionStatus(ctx context.Context, sessionID uint, status SessionStatus) error
	AddUsage(ctx context.Context, sessionID uint, usage Usage) error
}

// MessageStore persists ordered runtime messages.
type MessageStore interface {
	AppendMessages(ctx context.Context, sessionID uint, messages ...Message) error
	ListMessages(ctx context.Context, sessionID uint) ([]Message, error)
}

// Store is the complete persistence boundary used by agent services.
type Store interface {
	SessionStore
	MessageStore
}

// MemoryStore is a concurrency-safe Store implementation for tests and embedding.
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[uint]Session
	messages map[uint][]Message
}

// NewMemoryStore creates an empty in-memory store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		sessions: make(map[uint]Session),
		messages: make(map[uint][]Message),
	}
}

func (s *MemoryStore) CreateSession(_ context.Context, session Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[session.ID]; exists {
		return errors.New("agent: 会话已存在")
	}
	s.sessions[session.ID] = session
	return nil
}

func (s *MemoryStore) GetSession(_ context.Context, sessionID uint) (Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, exists := s.sessions[sessionID]
	if !exists {
		return Session{}, ErrSessionNotFound
	}
	return session, nil
}

func (s *MemoryStore) UpdateSessionStatus(_ context.Context, sessionID uint, status SessionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, exists := s.sessions[sessionID]
	if !exists {
		return ErrSessionNotFound
	}
	session.Status = status
	s.sessions[sessionID] = session
	return nil
}

func (s *MemoryStore) AddUsage(_ context.Context, sessionID uint, usage Usage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, exists := s.sessions[sessionID]
	if !exists {
		return ErrSessionNotFound
	}
	session.Usage.Add(usage)
	s.sessions[sessionID] = session
	return nil
}

func (s *MemoryStore) AppendMessages(_ context.Context, sessionID uint, messages ...Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[sessionID]; !exists {
		return ErrSessionNotFound
	}
	s.messages[sessionID] = append(s.messages[sessionID], cloneMessages(messages)...)
	return nil
}

func (s *MemoryStore) ListMessages(_ context.Context, sessionID uint) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, exists := s.sessions[sessionID]; !exists {
		return nil, ErrSessionNotFound
	}
	return cloneMessages(s.messages[sessionID]), nil
}

func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, len(messages))
	for i, message := range messages {
		cloned[i] = message
		cloned[i].Parts = make([]ContentPart, len(message.Parts))
		for j, part := range message.Parts {
			cloned[i].Parts[j] = part
			if part.ToolCall != nil {
				call := *part.ToolCall
				cloned[i].Parts[j].ToolCall = &call
			}
			if part.ToolResult != nil {
				result := *part.ToolResult
				cloned[i].Parts[j].ToolResult = &result
			}
			if part.Image != nil {
				image := *part.Image
				image.Data = append([]byte(nil), part.Image.Data...)
				cloned[i].Parts[j].Image = &image
			}
		}
	}
	return cloned
}
