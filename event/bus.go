package event

import "sync"

// Bus is a bounded, best-effort observation queue. It is intentionally
// separate from Store and never persists or retries observations.
type Bus struct {
	mu     sync.RWMutex
	events chan Envelope
	closed bool
	once   sync.Once
}

func NewBus(capacity int) *Bus {
	if capacity <= 0 {
		capacity = 1
	}
	return &Bus{events: make(chan Envelope, capacity)}
}

// TryPublish is nonblocking and returns false when closed or full.
func (b *Bus) TryPublish(envelope Envelope) bool {
	if b == nil || envelope.Reliability != ReliabilityObservation || !envelope.TenantKey.Valid() {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed {
		return false
	}
	select {
	case b.events <- envelope.Clone():
		return true
	default:
		return false
	}
}

func (b *Bus) Events() <-chan Envelope {
	if b == nil {
		return nil
	}
	return b.events
}

func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		close(b.events)
		b.mu.Unlock()
	})
}
