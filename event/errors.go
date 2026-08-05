package event

import "errors"

var (
	ErrInvalidEnvelope     = errors.New("event: invalid envelope")
	ErrPayloadTooLarge     = errors.New("event: payload too large")
	ErrUnsupportedSchema   = errors.New("event: unsupported schema")
	ErrInvalidStream       = errors.New("event: invalid stream")
	ErrIdempotencyConflict = errors.New("event: idempotency conflict")
	ErrSequenceConflict    = errors.New("event: sequence conflict")
	ErrLeaseLost           = errors.New("event: lease lost")
	ErrNotFound            = errors.New("event: not found")
	ErrPublishRetryable    = errors.New("event: publish retryable")
	ErrPublishPermanent    = errors.New("event: publish permanent")
	ErrConsumptionConflict = errors.New("event: consumption conflict")
	ErrBusClosed           = errors.New("event: bus closed")
)
