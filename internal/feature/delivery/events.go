package delivery

import "github.com/dekwanlabs/nasuta/internal/eventhub"

// EventHub broadcasts persisted events without blocking implementation workers.
type EventHub = eventhub.Hub[RunEvent]

func NewEventHub() *EventHub {
	return eventhub.New(func(event RunEvent) string { return event.RunID })
}
