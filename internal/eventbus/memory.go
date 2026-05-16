// In-memory Bus implementation. Holds the active subscription
// set per topic in a map; Publish iterates the matching slice
// and invokes each handler synchronously. Unsubscribe marks the
// subscription closed (so a concurrent Publish that already
// snapshotted the slice skips it) and removes it from the map
// under the write lock.

package eventbus

import (
	"context"
	"sync"
	"sync/atomic"
)

// NewMemoryBus returns a fresh in-memory Bus with no
// subscriptions. The zero state delivers nothing on Publish
// (no-op) and is safe to use from multiple goroutines without
// further initialization.
func NewMemoryBus() Bus {
	return &memoryBus{
		subs: make(map[Topic][]*memorySubscription),
	}
}

type memoryBus struct {
	mu   sync.RWMutex
	subs map[Topic][]*memorySubscription
}

type memorySubscription struct {
	bus     *memoryBus
	topic   Topic
	handler Handler
	closed  atomic.Bool
}

func (b *memoryBus) Subscribe(topic Topic, handler Handler) Subscription {
	if handler == nil {
		// Nil handler would panic on Publish; rejecting at
		// Subscribe time gives the caller the error at the
		// point of mistake. A pre-closed Subscription is
		// returned so Unsubscribe is still safe (idempotent).
		sub := &memorySubscription{}
		sub.closed.Store(true)
		return sub
	}
	sub := &memorySubscription{
		bus:     b,
		topic:   topic,
		handler: handler,
	}
	b.mu.Lock()
	b.subs[topic] = append(b.subs[topic], sub)
	b.mu.Unlock()
	return sub
}

func (b *memoryBus) Publish(ctx context.Context, e Event) {
	if e == nil {
		return
	}
	topic := e.Topic()

	// Snapshot the subscription slice under the read lock so
	// Publish doesn't hold the lock across handler calls
	// (which would deadlock if a handler tried to Subscribe).
	// Append into a freshly-allocated slice rather than
	// aliasing the map's slice — a concurrent Subscribe under
	// the write lock can grow the underlying array, but our
	// snapshot is already a separate backing array.
	b.mu.RLock()
	snapshot := append([]*memorySubscription(nil), b.subs[topic]...)
	b.mu.RUnlock()

	for _, sub := range snapshot {
		// Skip subscriptions that were Unsubscribed between
		// snapshot and dispatch — Unsubscribe sets closed
		// before removing from the map, so the closed flag
		// is the authoritative gate even when the snapshot
		// is stale.
		if sub.closed.Load() {
			continue
		}
		sub.handler(ctx, e)
	}
}

func (s *memorySubscription) Unsubscribe() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	if s.bus == nil {
		// Nil-handler path from Subscribe — nothing to remove
		// from the map.
		return
	}
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	subs := s.bus.subs[s.topic]
	for i, candidate := range subs {
		if candidate == s {
			s.bus.subs[s.topic] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(s.bus.subs[s.topic]) == 0 {
		delete(s.bus.subs, s.topic)
	}
}

