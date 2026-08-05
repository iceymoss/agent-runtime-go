package event

import (
	"context"
	"fmt"
	"sync"
)

type ConsumeResult struct {
	Applied   bool
	Duplicate bool
}

type Handler func(context.Context, Envelope) error

// Inbox provides event-id idempotency. The handler and receipt are serialized
// as one memory transaction: a failed handler leaves no receipt.
type Inbox struct {
	mu       sync.Mutex
	receipts map[string]map[string]struct{}
}

func NewInbox() *Inbox {
	return &Inbox{receipts: make(map[string]map[string]struct{})}
}

func (i *Inbox) Consume(ctx context.Context, consumer string, envelope Envelope, handler Handler) (ConsumeResult, error) {
	if consumer == "" || !envelope.TenantKey.Valid() || envelope.EventID == "" || handler == nil {
		return ConsumeResult{}, fmt.Errorf("%w: consumer, tenant, event id, and handler are required", ErrInvalidEnvelope)
	}
	if err := contextError(ctx); err != nil {
		return ConsumeResult{}, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if _, ok := i.receipts[consumer][envelope.EventID]; ok {
		return ConsumeResult{Duplicate: true}, nil
	}
	if err := handler(ctx, envelope.Clone()); err != nil {
		return ConsumeResult{}, err
	}
	if i.receipts[consumer] == nil {
		i.receipts[consumer] = make(map[string]struct{})
	}
	i.receipts[consumer][envelope.EventID] = struct{}{}
	return ConsumeResult{Applied: true}, nil
}

func (i *Inbox) ConsumeWith(ctx context.Context, consumer Consumer, envelope Envelope) (ConsumeResult, error) {
	if consumer == nil {
		return ConsumeResult{}, fmt.Errorf("%w: consumer is required", ErrInvalidEnvelope)
	}
	return i.Consume(ctx, consumer.Name(), envelope, consumer.Handle)
}
