package mcp

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfig         = errors.New("agent/mcp: invalid configuration")
	ErrNotStarted            = errors.New("agent/mcp: scope not started")
	ErrNotReady              = errors.New("agent/mcp: scope not ready")
	ErrClosed                = errors.New("agent/mcp: closed")
	ErrServerNotFound        = errors.New("agent/mcp: server not found")
	ErrServerDisabled        = errors.New("agent/mcp: server disabled")
	ErrTransportDenied       = errors.New("agent/mcp: transport denied")
	ErrGenerationUnavailable = errors.New("agent/mcp: generation unavailable")
	ErrCapabilityInvalid     = errors.New("agent/mcp: capability invalid")
	ErrSchemaInvalid         = errors.New("agent/mcp: schema invalid")
	ErrReconnectFailed       = errors.New("agent/mcp: reconnect failed")
	ErrCallUnknown           = errors.New("agent/mcp: call outcome unknown")
	ErrResultTooLarge        = errors.New("agent/mcp: result too large")
	ErrUpstream              = errors.New("agent/mcp: upstream error")
)

// Error carries tenant-safe operation context. Detail must not contain URLs
// with query strings, header values, environment values, or protocol bodies.
type Error struct {
	Kind       error
	Cause      error
	Operation  string
	ServerID   ServerID
	Generation Generation
	Detail     string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	kind := e.Kind
	if kind == nil {
		kind = ErrUpstream
	}
	message := kind.Error()
	if e.Operation != "" {
		message += ": " + e.Operation
	}
	if e.ServerID != "" {
		message += fmt.Sprintf(" server=%q", e.ServerID)
	}
	if e.Generation != "" {
		message += fmt.Sprintf(" generation=%q", e.Generation)
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	if e.Cause != nil {
		return errors.Join(e.Kind, e.Cause)
	}
	return e.Kind
}

func mcpError(kind, cause error, operation string, serverID ServerID, generation Generation, detail string) error {
	return &Error{Kind: kind, Cause: cause, Operation: operation, ServerID: serverID, Generation: generation, Detail: detail}
}

// UpstreamError preserves protocol error classification without exposing a raw
// response body.
type UpstreamError struct {
	Code      int
	Message   string
	Retryable bool
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: code=%d message=%q", ErrUpstream, e.Code, e.Message)
}

func (e *UpstreamError) Unwrap() error { return ErrUpstream }
