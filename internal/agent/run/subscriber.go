package run

import "sync"

const (
	subscriberDiagnosticLimit = 512
	subscriberEventBuffer     = 1
	subscriberTextMergeLimit  = 8 * 1024
)

type subscriber struct {
	events       chan SSEEvent
	wake         chan struct{}
	stop         chan struct{}
	once         sync.Once
	mu           sync.Mutex
	queue        []SSEEvent
	closed       bool
	dropReported bool
}

func newSubscriber() *subscriber {
	sub := &subscriber{
		events: make(chan SSEEvent, subscriberEventBuffer),
		wake:   make(chan struct{}, 1),
		stop:   make(chan struct{}),
	}
	go sub.deliver()
	return sub
}

func (sub *subscriber) enqueue(event SSEEvent) bool {
	sub.mu.Lock()
	if sub.closed || !sub.enqueueLocked(event) {
		sub.mu.Unlock()
		return false
	}
	sub.mu.Unlock()
	select {
	case sub.wake <- struct{}{}:
	default:
	}
	return true
}

func (sub *subscriber) enqueueLocked(event SSEEvent) bool {
	if sub.mergeAdjacentLocked(event) {
		return true
	}
	if len(sub.queue) < subscriberDiagnosticLimit {
		sub.queue = append(sub.queue, event)
		return true
	}
	if isBestEffortEvent(event.Type) {
		return false
	}
	if index := sub.findDroppableLocked(); index >= 0 {
		sub.removeAtLocked(index)
		sub.queue = append(sub.queue, event)
		return true
	}
	if event.Type != EventRunFinished {
		return false
	}
	if index := sub.findNonTerminalLocked(); index >= 0 {
		sub.removeAtLocked(index)
		sub.queue = append(sub.queue, event)
		return true
	}
	return false
}

func (sub *subscriber) mergeAdjacentLocked(event SSEEvent) bool {
	if len(sub.queue) == 0 {
		return false
	}
	last := &sub.queue[len(sub.queue)-1]
	if last.Type != event.Type {
		return false
	}
	switch event.Type {
	case EventAnswerDelta, EventReasoningDelta:
		previous, previousOK := last.Data.(TextEvent)
		current, currentOK := event.Data.(TextEvent)
		if !previousOK || !currentOK {
			return false
		}
		if len(previous.Text)+len(current.Text) > subscriberTextMergeLimit {
			return false
		}
		previous.Text += current.Text
		last.Data = previous
		return true
	case EventStatus:
		last.Data = event.Data
		return true
	default:
		return false
	}
}

func (sub *subscriber) findDroppableLocked() int {
	for index, event := range sub.queue {
		if isBestEffortEvent(event.Type) {
			return index
		}
	}
	return -1
}

func (sub *subscriber) findNonTerminalLocked() int {
	for index, event := range sub.queue {
		if event.Type != EventRunFinished {
			return index
		}
	}
	return -1
}

func (sub *subscriber) removeAtLocked(index int) {
	copy(sub.queue[index:], sub.queue[index+1:])
	sub.queue[len(sub.queue)-1] = SSEEvent{}
	sub.queue = sub.queue[:len(sub.queue)-1]
}

func (sub *subscriber) deliver() {
	for {
		sub.mu.Lock()
		if len(sub.queue) == 0 {
			sub.mu.Unlock()
			select {
			case <-sub.wake:
				continue
			case <-sub.stop:
				return
			}
		}
		event := sub.queue[0]
		sub.queue[0] = SSEEvent{}
		sub.queue = sub.queue[1:]
		if len(sub.queue) < subscriberDiagnosticLimit/2 {
			sub.dropReported = false
		}
		sub.mu.Unlock()
		select {
		case sub.events <- event:
			if event.Type == EventRunFinished {
				return
			}
		case <-sub.stop:
			return
		}
	}
}

func (sub *subscriber) reportDrop() bool {
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if sub.closed || sub.dropReported {
		return false
	}
	sub.dropReported = true
	return true
}

func (sub *subscriber) close() {
	sub.once.Do(func() {
		sub.mu.Lock()
		sub.closed = true
		sub.queue = nil
		sub.mu.Unlock()
		close(sub.stop)
	})
}

func isBestEffortEvent(event EventType) bool {
	switch event {
	case EventAnswerDelta, EventReasoningDelta, EventTrace, EventStatus, EventLLMCall,
		EventExecutionRouted, EventExecutionDegraded, EventWorkflowStarted,
		EventAgentStarted, EventAgentCompleted, EventEvidenceJoined,
		EventDelegationCreated, EventDelegationStarted, EventDelegationDone,
		EventDelegationFailed, EventDelegationCancelled, EventDelegationRejected,
		EventDelegationValidated,
		EventDelegationVerificationStarted, EventDelegationVerificationDone,
		EventDelegationVerificationFailed, EventDelegationVerificationRejected,
		EventDelegationAdoptionEvaluated:
		return true
	default:
		return false
	}
}
