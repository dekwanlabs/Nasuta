package delivery

import "sync"

// ReviewEventHub broadcasts events only after durable persistence succeeds.
type ReviewEventHub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan ReviewEvent]struct{}
}

func NewReviewEventHub() *ReviewEventHub {
	return &ReviewEventHub{subscribers: make(map[string]map[chan ReviewEvent]struct{})}
}

func (hub *ReviewEventHub) Subscribe(roundID string) (<-chan ReviewEvent, func()) {
	channel := make(chan ReviewEvent, 32)
	hub.mu.Lock()
	if hub.subscribers[roundID] == nil {
		hub.subscribers[roundID] = make(map[chan ReviewEvent]struct{})
	}
	hub.subscribers[roundID][channel] = struct{}{}
	hub.mu.Unlock()
	return channel, func() {
		hub.mu.Lock()
		if subscribers := hub.subscribers[roundID]; subscribers != nil {
			delete(subscribers, channel)
			if len(subscribers) == 0 {
				delete(hub.subscribers, roundID)
			}
		}
		hub.mu.Unlock()
	}
}

func (hub *ReviewEventHub) Publish(event ReviewEvent) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for channel := range hub.subscribers[event.RoundID] {
		select {
		case channel <- event:
		default:
		}
	}
}
