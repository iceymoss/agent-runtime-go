package tool

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidConfiguration = errors.New("agent/tool: invalid configuration")
	ErrToolNotFound         = errors.New("agent/tool: tool not found")
	ErrToolInputInvalid     = errors.New("agent/tool: tool input invalid")
	ErrRewriteInvalid       = errors.New("agent/tool: interceptor rewrite invalid")
	ErrRewriteLoop          = errors.New("agent/tool: interceptor rewrite loop")
	ErrInterceptorFailed    = errors.New("agent/tool: interceptor failed")
	ErrAuthorizationFailed  = errors.New("agent/tool: authorization failed")
	ErrPermissionDenied     = errors.New("agent/tool: permission denied")
	ErrApprovalPending      = errors.New("agent/tool: approval pending")
	ErrExecutionConflict    = errors.New("agent/tool: execution conflict")
	ErrExecutionInProgress  = errors.New("agent/tool: execution in progress")
	ErrStaleFence           = errors.New("agent/tool: stale execution fence")
	ErrExecutionUnknown     = errors.New("agent/tool: execution outcome unknown")
	ErrResultInvariant      = errors.New("agent/tool: result invariant violated")
	ErrToolFatal            = errors.New("agent/tool: tool execution failed")
)

// Error adds safe execution context while retaining the original cause for
// errors.Is/errors.As. Detail must never contain raw tool input or output.
type Error struct {
	Kind         error
	Cause        error
	Operation    string
	ExecutionKey string
	Detail       string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	kind := e.Kind
	if kind == nil {
		kind = ErrToolFatal
	}
	message := kind.Error()
	if e.Operation != "" {
		message += ": " + e.Operation
	}
	if e.ExecutionKey != "" {
		message += fmt.Sprintf(" execution=%q", e.ExecutionKey)
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

func lifecycleError(kind, cause error, operation, key, detail string) error {
	return &Error{Kind: kind, Cause: cause, Operation: operation, ExecutionKey: key, Detail: detail}
}
