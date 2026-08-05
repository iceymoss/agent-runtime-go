package durable

import (
	"errors"
	"fmt"
)

var (
	ErrSnapshotSchema           = errors.New("durable: unsupported snapshot schema")
	ErrSnapshotIncoherent       = errors.New("durable: snapshot incoherent")
	ErrRunNotFound              = errors.New("durable: run not found")
	ErrRunConflict              = errors.New("durable: immutable run conflict")
	ErrLeaseHeld                = errors.New("durable: lease held")
	ErrLeaseLost                = errors.New("durable: lease lost")
	ErrInvalidTransition        = errors.New("durable: invalid transition")
	ErrTerminal                 = errors.New("durable: terminal run is immutable")
	ErrEffectNotFound           = errors.New("durable: effect not found")
	ErrEffectConflict           = errors.New("durable: immutable effect conflict")
	ErrToolEffectUnknown        = errors.New("durable: tool effect unknown")
	ErrUsageNotFound            = errors.New("durable: usage fact not found")
	ErrUsageConflict            = errors.New("durable: immutable usage conflict")
	ErrReconciliationIncomplete = errors.New("durable: reconciliation incomplete")
	ErrLimitExceeded            = errors.New("durable: operation limit exceeded")
)

// Error preserves a machine-testable category while adding safe operation and
// run context. It deliberately excludes checkpoint, tool input, and result data.
type Error struct {
	Kind      error
	Operation string
	RunKey    RunKey
	Detail    string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.Kind.Error()
	if e.Operation != "" {
		message += ": " + e.Operation
	}
	if e.RunKey != "" {
		message += fmt.Sprintf(" run=%q", e.RunKey)
	}
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

func (e *Error) Unwrap() error { return e.Kind }

func durableError(kind error, operation string, runKey RunKey, detail string) error {
	return &Error{Kind: kind, Operation: operation, RunKey: runKey, Detail: detail}
}
