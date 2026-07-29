package featuredelivery

import "sync"

// EventHub broadcasts persisted events without blocking implementation workers.
type EventHub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan RunEvent]struct{}
}

func NewEventHub() *EventHub {
	return &EventHub{subscribers: make(map[string]map[chan RunEvent]struct{})}
}

func (hub *EventHub) Subscribe(runID string) (<-chan RunEvent, func()) {
	channel := make(chan RunEvent, 32)
	hub.mu.Lock()
	if hub.subscribers[runID] == nil {
		hub.subscribers[runID] = make(map[chan RunEvent]struct{})
	}
	hub.subscribers[runID][channel] = struct{}{}
	hub.mu.Unlock()
	return channel, func() {
		hub.mu.Lock()
		if subscribers := hub.subscribers[runID]; subscribers != nil {
			delete(subscribers, channel)
			if len(subscribers) == 0 {
				delete(hub.subscribers, runID)
			}
		}
		hub.mu.Unlock()
	}
}

func (hub *EventHub) Publish(event RunEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for channel := range hub.subscribers[event.RunID] {
		select {
		case channel <- event:
		default:
		}
	}
}
