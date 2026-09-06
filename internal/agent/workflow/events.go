package workflow

import "github.com/dekwanlabs/nasuta/internal/eventhub"

// EventHub broadcasts committed workflow events without blocking execution.
type EventHub = eventhub.Hub[Event]

func NewEventHub() *EventHub {
	return eventhub.New(func(event Event) string { return event.WorkflowRunID })
}
