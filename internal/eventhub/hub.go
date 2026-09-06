// Package eventhub provides a bounded, best-effort fan-out hub for keyed events.
package eventhub

import "sync"

// Hub broadcasts events to subscribers registered for the event's key.
// A full subscriber queue drops the event instead of blocking the publisher.
type Hub[T any] struct {
	mu          sync.Mutex
	subscribers map[string]map[chan T]struct{}
	key         func(T) string
}

// New creates a hub that uses key to select subscribers for each event.
func New[T any](key func(T) string) *Hub[T] {
	return &Hub[T]{
		subscribers: make(map[string]map[chan T]struct{}),
		key:         key,
	}
}

// Subscribe registers a buffered subscriber for key and returns its cancel function.
func (hub *Hub[T]) Subscribe(key string) (<-chan T, func()) {
	channel := make(chan T, 32)
	hub.mu.Lock()
	if hub.subscribers[key] == nil {
		hub.subscribers[key] = make(map[chan T]struct{})
	}
	hub.subscribers[key][channel] = struct{}{}
	hub.mu.Unlock()
	return channel, func() {
		hub.mu.Lock()
		if subscribers := hub.subscribers[key]; subscribers != nil {
			delete(subscribers, channel)
			if len(subscribers) == 0 {
				delete(hub.subscribers, key)
			}
		}
		hub.mu.Unlock()
	}
}

// Publish sends event to all subscribers for its key without blocking.
func (hub *Hub[T]) Publish(event T) {
	key := hub.key(event)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for channel := range hub.subscribers[key] {
		select {
		case channel <- event:
		default:
		}
	}
}
