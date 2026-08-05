package context

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeInvalidRequest         ErrorCode = "invalid_request"
	CodeHistoryInvalid         ErrorCode = "history_invalid"
	CodeToolPairing            ErrorCode = "tool_pairing"
	CodeHistoryInterrupted     ErrorCode = "history_interrupted"
	CodeRevisionNotFound       ErrorCode = "revision_not_found"
	CodePivotInvalid           ErrorCode = "pivot_invalid"
	CodeProtectedFactConflict  ErrorCode = "protected_fact_conflict"
	CodeProtectedFactsTooLarge ErrorCode = "protected_facts_too_large"
	CodeCompactionRequired     ErrorCode = "compaction_required"
	CodeCompactionNoProgress   ErrorCode = "compaction_no_progress"
	CodeArtifactConflict       ErrorCode = "artifact_conflict"
	CodeArtifactNotFound       ErrorCode = "artifact_not_found"
	CodePivotConflict          ErrorCode = "pivot_conflict"
	CodeGenerationUnavailable  ErrorCode = "generation_unavailable"
	CodePlanNotFound           ErrorCode = "plan_not_found"
	CodePlanDrift              ErrorCode = "plan_drift"
	CodeContextTooLarge        ErrorCode = "context_too_large"
)

var (
	ErrInvalidRequest         = errors.New("agent context: invalid request")
	ErrHistoryInvalid         = errors.New("agent context: history invalid")
	ErrToolPairing            = errors.New("agent context: tool pairing invalid")
	ErrHistoryInterrupted     = errors.New("agent context: history interrupted")
	ErrRevisionNotFound       = errors.New("agent context: revision not found")
	ErrPivotInvalid           = errors.New("agent context: pivot invalid")
	ErrProtectedFactConflict  = errors.New("agent context: protected fact conflict")
	ErrProtectedFactsTooLarge = errors.New("agent context: protected facts exceed budget")
	ErrCompactionRequired     = errors.New("agent context: compaction required")
	ErrCompactionNoProgress   = errors.New("agent context: compaction made no progress")
	ErrArtifactConflict       = errors.New("agent context: immutable artifact conflict")
	ErrArtifactNotFound       = errors.New("agent context: artifact not found")
	ErrPivotConflict          = errors.New("agent context: pivot conflict")
	ErrGenerationUnavailable  = errors.New("agent context: generation unavailable")
	ErrPlanNotFound           = errors.New("agent context: plan not found")
	ErrPlanDrift              = errors.New("agent context: plan drift")
	ErrContextTooLarge        = errors.New("agent context: context too large")
)

// Error carries safe context identity without embedding message or artifact content.
type Error struct {
	Code      ErrorCode
	Operation string
	Key       string
	Cause     error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	text := "agent context"
	if e.Operation != "" {
		text += ": " + e.Operation
	}
	if e.Key != "" {
		text += fmt.Sprintf(" key=%q", e.Key)
	}
	if e.Cause != nil {
		text += ": " + e.Cause.Error()
	}
	return text
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func contextError(code ErrorCode, operation, key string, sentinel, cause error) error {
	if cause == nil {
		cause = sentinel
	} else {
		cause = errors.Join(sentinel, cause)
	}
	return &Error{Code: code, Operation: operation, Key: key, Cause: cause}
}
