package agent

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ModelErrorKind classifies a provider-neutral model operation failure.
type ModelErrorKind string

const (
	ModelErrorKindTransport   ModelErrorKind = "transport"
	ModelErrorKindRateLimit   ModelErrorKind = "rate_limit"
	ModelErrorKindAuth        ModelErrorKind = "auth"
	ModelErrorKindRejected    ModelErrorKind = "rejected"
	ModelErrorKindProtocol    ModelErrorKind = "protocol"
	ModelErrorKindUnsupported ModelErrorKind = "unsupported"
)

// ModelError preserves safe provider metadata and the original cause.
// Error never includes Cause because provider errors may contain secrets.
type ModelError struct {
	Kind       ModelErrorKind
	Retryable  bool
	HTTPStatus int
	RetryAfter time.Duration
	SafeDetail string
	Cause      error
}

// NewModelError constructs a classified model error. Context termination is
// returned unchanged so callers retain the standard cancellation contract.
func NewModelError(kind ModelErrorKind, retryable bool, httpStatus int, retryAfter time.Duration, safeDetail string, cause error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	if !validModelErrorKind(kind) {
		return fmt.Errorf("agent: invalid model error kind %q", kind)
	}
	if httpStatus != 0 && (httpStatus < 100 || httpStatus > 599) {
		return fmt.Errorf("agent: invalid model error HTTP status %d", httpStatus)
	}
	if retryAfter < 0 {
		return fmt.Errorf("agent: invalid model error retry-after %s", retryAfter)
	}
	return &ModelError{
		Kind:       kind,
		Retryable:  retryable,
		HTTPStatus: httpStatus,
		RetryAfter: retryAfter,
		SafeDetail: safeDetail,
		Cause:      cause,
	}
}

func (e *ModelError) Error() string {
	if e == nil {
		return "agent: model error"
	}
	message := "agent: model error"
	if e.Kind != "" {
		message = fmt.Sprintf("agent: model %s error", e.Kind)
	}
	if e.HTTPStatus != 0 {
		message = fmt.Sprintf("%s (HTTP %d)", message, e.HTTPStatus)
	}
	if e.SafeDetail != "" {
		message = fmt.Sprintf("%s: %s", message, e.SafeDetail)
	}
	return message
}

func (e *ModelError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func validModelErrorKind(kind ModelErrorKind) bool {
	switch kind {
	case ModelErrorKindTransport,
		ModelErrorKindRateLimit,
		ModelErrorKindAuth,
		ModelErrorKindRejected,
		ModelErrorKindProtocol,
		ModelErrorKindUnsupported:
		return true
	default:
		return false
	}
}

func normalizeModelError(err error, detail string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var modelErr *ModelError
	if errors.As(err, &modelErr) {
		return err
	}
	return NewModelError(ModelErrorKindTransport, true, 0, 0, detail, err)
}

func newProtocolError(detail string, cause error) error {
	return NewModelError(ModelErrorKindProtocol, false, 0, 0, detail, cause)
}

func retryableModelError(err error) bool {
	var modelErr *ModelError
	return errors.As(err, &modelErr) && modelErr.Retryable
}
