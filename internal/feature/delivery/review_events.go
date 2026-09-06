package delivery

import "github.com/dekwanlabs/nasuta/internal/eventhub"

// ReviewEventHub broadcasts events only after durable persistence succeeds.
type ReviewEventHub = eventhub.Hub[ReviewEvent]

func NewReviewEventHub() *ReviewEventHub {
	return eventhub.New(func(event ReviewEvent) string { return event.RoundID })
}
