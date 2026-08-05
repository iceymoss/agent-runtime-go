package permission

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidRequest     = errors.New("agent/permission: invalid request")
	ErrPolicyInvalid      = errors.New("agent/permission: invalid policy")
	ErrPolicyUnavailable  = errors.New("agent/permission: policy unavailable")
	ErrPermissionDenied   = errors.New("agent/permission: permission denied")
	ErrApprovalRequired   = errors.New("agent/permission: approval required")
	ErrRequestNotFound    = errors.New("agent/permission: request not found")
	ErrRequestConflict    = errors.New("agent/permission: immutable request conflict")
	ErrAlreadyResolved    = errors.New("agent/permission: approval already resolved")
	ErrRequestExpired     = errors.New("agent/permission: request expired")
	ErrRequestCanceled    = errors.New("agent/permission: request canceled")
	ErrGrantNotFound      = errors.New("agent/permission: grant not found")
	ErrGrantNotApplicable = errors.New("agent/permission: grant not applicable")
	ErrGrantExpired       = errors.New("agent/permission: grant expired")
	ErrGrantRevoked       = errors.New("agent/permission: grant revoked")
	ErrGrantExhausted     = errors.New("agent/permission: grant exhausted")
	ErrStaleFence         = errors.New("agent/permission: stale attempt fence")
	ErrResumeRevalidation = errors.New("agent/permission: resume revalidation failed")
	ErrRevisionConflict   = errors.New("agent/permission: revision conflict")
)

type Error struct {
	Kind       error
	Operation  string
	RequestKey RequestKey
	Detail     string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Kind == nil {
		return "agent/permission: unknown error"
	}
	s := e.Kind.Error()
	if e.Operation != "" {
		s += ": " + e.Operation
	}
	if e.RequestKey != "" {
		s += fmt.Sprintf(" request=%q", e.RequestKey)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}
func (e *Error) Unwrap() error { return e.Kind }
func permissionError(kind error, operation string, key RequestKey, detail string) error {
	return &Error{Kind: kind, Operation: operation, RequestKey: key, Detail: detail}
}
