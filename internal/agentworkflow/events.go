package agentworkflow

import "sync"

// EventHub broadcasts committed workflow events without blocking execution.
type EventHub struct {
	mu          sync.Mutex
	subscribers map[string]map[chan Event]struct{}
}

func NewEventHub() *EventHub {
	return &EventHub{subscribers: make(map[string]map[chan Event]struct{})}
}

func (hub *EventHub) Subscribe(runID string) (<-chan Event, func()) {
	channel := make(chan Event, 32)
	hub.mu.Lock()
	if hub.subscribers[runID] == nil {
		hub.subscribers[runID] = make(map[chan Event]struct{})
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

func (hub *EventHub) Publish(event Event) {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for channel := range hub.subscribers[event.WorkflowRunID] {
		select {
		case channel <- event:
		default:
		}
	}
}
