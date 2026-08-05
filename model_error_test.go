package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewModelError(t *testing.T) {
	cause := errors.New("provider secret detail")
	tests := []struct {
		name        string
		kind        ModelErrorKind
		retryable   bool
		httpStatus  int
		retryAfter  time.Duration
		safeDetail  string
		wantMessage string
	}{
		{name: "transport", kind: ModelErrorKindTransport, retryable: true, wantMessage: "agent: model transport error"},
		{name: "rate limit", kind: ModelErrorKindRateLimit, retryable: true, httpStatus: http.StatusTooManyRequests, retryAfter: 15 * time.Second, safeDetail: "provider capacity exhausted", wantMessage: "agent: model rate_limit error (HTTP 429): provider capacity exhausted"},
		{name: "auth", kind: ModelErrorKindAuth, httpStatus: http.StatusUnauthorized, safeDetail: "provider authentication failed", wantMessage: "agent: model auth error (HTTP 401): provider authentication failed"},
		{name: "rejected", kind: ModelErrorKindRejected, httpStatus: http.StatusBadRequest, safeDetail: "request rejected", wantMessage: "agent: model rejected error (HTTP 400): request rejected"},
		{name: "protocol", kind: ModelErrorKindProtocol, safeDetail: "stream closed before terminal response", wantMessage: "agent: model protocol error: stream closed before terminal response"},
		{name: "unsupported", kind: ModelErrorKindUnsupported, safeDetail: "structured output is unsupported", wantMessage: "agent: model unsupported error: structured output is unsupported"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewModelError(tt.kind, tt.retryable, tt.httpStatus, tt.retryAfter, tt.safeDetail, cause)
			var modelErr *ModelError
			if !errors.As(err, &modelErr) {
				t.Fatalf("errors.As(%v) = false", err)
			}
			if modelErr.Kind != tt.kind || modelErr.Retryable != tt.retryable || modelErr.HTTPStatus != tt.httpStatus || modelErr.RetryAfter != tt.retryAfter || modelErr.SafeDetail != tt.safeDetail || modelErr.Cause != cause {
				t.Fatalf("ModelError = %+v", modelErr)
			}
			if err.Error() != tt.wantMessage {
				t.Fatalf("Error() = %q, want %q", err, tt.wantMessage)
			}
			if !errors.Is(err, cause) {
				t.Fatal("errors.Is(error, cause) = false")
			}
			if strings.Contains(err.Error(), cause.Error()) {
				t.Fatalf("Error() exposed unsafe cause: %q", err)
			}
		})
	}
}

func TestNewModelErrorPreservesContextErrors(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		want  error
	}{
		{name: "canceled", cause: context.Canceled, want: context.Canceled},
		{name: "wrapped canceled", cause: fmt.Errorf("provider stopped: %w", context.Canceled), want: context.Canceled},
		{name: "deadline exceeded", cause: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "wrapped deadline exceeded", cause: fmt.Errorf("provider timed out: %w", context.DeadlineExceeded), want: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewModelError(ModelErrorKindTransport, true, 0, 0, "", tt.cause)
			if err != tt.cause {
				t.Fatalf("error = %v, want original cause %v", err, tt.cause)
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("errors.Is(%v, %v) = false", err, tt.want)
			}
			var modelErr *ModelError
			if errors.As(err, &modelErr) {
				t.Fatalf("context error classified as ModelError: %+v", modelErr)
			}
		})
	}
}

func TestNewModelErrorRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name       string
		kind       ModelErrorKind
		httpStatus int
		retryAfter time.Duration
	}{
		{name: "empty kind"},
		{name: "unknown kind", kind: "unknown"},
		{name: "status below range", kind: ModelErrorKindTransport, httpStatus: 99},
		{name: "status above range", kind: ModelErrorKindTransport, httpStatus: 600},
		{name: "negative retry after", kind: ModelErrorKindRateLimit, retryAfter: -time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := NewModelError(tt.kind, false, tt.httpStatus, tt.retryAfter, "", nil)
			if err == nil {
				t.Fatal("NewModelError() = nil")
			}
			var modelErr *ModelError
			if errors.As(err, &modelErr) {
				t.Fatalf("invalid metadata produced ModelError: %+v", modelErr)
			}
		})
	}
}

func TestModelErrorZeroValueAndWrapping(t *testing.T) {
	var zero ModelError
	if zero.Error() != "agent: model error" || zero.Unwrap() != nil {
		t.Fatalf("zero ModelError = %q, %v", zero.Error(), zero.Unwrap())
	}
	var nilError *ModelError
	if nilError.Error() != "agent: model error" || nilError.Unwrap() != nil {
		t.Fatalf("nil ModelError = %q, %v", nilError.Error(), nilError.Unwrap())
	}

	err := NewModelError(ModelErrorKindRejected, false, 599, time.Second, "50% unavailable", nil)
	wrapped := fmt.Errorf("generate: %w", err)
	var modelErr *ModelError
	if !errors.As(wrapped, &modelErr) {
		t.Fatalf("errors.As(%v) = false", wrapped)
	}
	if modelErr.Error() != "agent: model rejected error (HTTP 599): 50% unavailable" {
		t.Fatalf("Error() = %q", modelErr.Error())
	}
}
