package event

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/iceymoss/agent-runtime-go"
)

type DispatcherConfig struct {
	TenantKey     agent.TenantKey
	Owner         string
	BatchSize     int
	LeaseDuration time.Duration
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	MaxAttempts   uint32
}

type DispatchStats struct {
	Claimed   int
	Delivered int
	Retried   int
	Dead      int
}

// Dispatcher performs one bounded poll at a time and starts no goroutines.
type Dispatcher struct {
	store     Store
	publisher Publisher
	config    DispatcherConfig
}

func NewDispatcher(store Store, publisher Publisher, config DispatcherConfig) (*Dispatcher, error) {
	if store == nil || publisher == nil || !config.TenantKey.Valid() || config.Owner == "" || config.BatchSize <= 0 || config.LeaseDuration <= 0 || config.BaseBackoff < 0 || config.MaxBackoff < 0 {
		return nil, fmt.Errorf("%w: invalid dispatcher configuration", ErrInvalidEnvelope)
	}
	if config.MaxBackoff > 0 && config.BaseBackoff > config.MaxBackoff {
		return nil, fmt.Errorf("%w: base backoff exceeds maximum", ErrInvalidEnvelope)
	}
	return &Dispatcher{store: store, publisher: publisher, config: config}, nil
}

// RunOnce claims and handles one batch. Publish errors are represented by
// retry/dead transitions; storage or context errors are returned.
func (d *Dispatcher) RunOnce(ctx context.Context) (DispatchStats, error) {
	claimed, err := d.store.Claim(ctx, ClaimCommand{
		TenantKey: d.config.TenantKey, Owner: d.config.Owner,
		Limit: d.config.BatchSize, LeaseDuration: d.config.LeaseDuration,
	})
	if err != nil {
		return DispatchStats{}, err
	}
	stats := DispatchStats{Claimed: len(claimed)}
	for _, item := range claimed {
		receipt := AckCommand{
			TenantKey: item.Envelope.TenantKey, EventID: item.Envelope.EventID,
			LeaseOwner: item.LeaseOwner, LeaseToken: item.LeaseToken, LeaseFence: item.LeaseFence,
		}
		publishErr := d.publisher.Publish(ctx, item.Envelope.Clone())
		if publishErr == nil {
			if err := d.store.Ack(ctx, receipt); err != nil {
				return stats, err
			}
			stats.Delivered++
			continue
		}
		dead := errors.Is(publishErr, ErrPublishPermanent) || d.config.MaxAttempts > 0 && item.Attempts >= d.config.MaxAttempts
		if err := d.store.Nack(ctx, NackCommand{
			AckCommand: receipt, RetryAfter: d.backoff(item.Attempts), MaxAttempts: d.config.MaxAttempts,
			Dead: dead, Reason: publishErr.Error(),
		}); err != nil {
			return stats, err
		}
		if dead {
			stats.Dead++
		} else {
			stats.Retried++
		}
	}
	return stats, nil
}

// DispatchBatch is an alias for RunOnce for outbox-oriented callers.
func (d *Dispatcher) DispatchBatch(ctx context.Context) (DispatchStats, error) {
	return d.RunOnce(ctx)
}

func (d *Dispatcher) backoff(attempt uint32) time.Duration {
	delay := d.config.BaseBackoff
	for current := uint32(1); current < attempt && delay > 0; current++ {
		if d.config.MaxBackoff > 0 && delay >= d.config.MaxBackoff/2 {
			return d.config.MaxBackoff
		}
		delay *= 2
	}
	if d.config.MaxBackoff > 0 && delay > d.config.MaxBackoff {
		return d.config.MaxBackoff
	}
	return delay
}
