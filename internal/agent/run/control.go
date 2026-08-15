package run

import (
	"context"
	"fmt"

	"github.com/dekwanlabs/nasuta/log"
)

func (hub *Hub) Send(runID string, signal ControlSignal) {
	hub.mu.Lock()
	hub.signals[runID] = append(hub.signals[runID], signal)
	var paused chan struct{}
	if signal.Kind == CtrlAbort {
		paused = hub.paused[runID]
		delete(hub.paused, runID)
	}
	hub.mu.Unlock()
	if paused != nil {
		close(paused)
	}
}

func (hub *Hub) Resume(runID string) error {
	hub.mu.Lock()
	resume := hub.paused[runID]
	if resume == nil {
		queue := hub.signals[runID]
		out := queue[:0]
		for _, signal := range queue {
			if signal.Kind != CtrlPause {
				out = append(out, signal)
			}
		}
		hub.signals[runID] = out
		hub.mu.Unlock()
		return nil
	}
	hub.mu.Unlock()
	if hub.control != nil {
		if err := hub.control.TransitionControl(runID, StatusPaused, StatusRunning); err != nil {
			return fmt.Errorf("resume run: %w", err)
		}
	}
	hub.mu.Lock()
	if hub.paused[runID] == resume {
		delete(hub.paused, runID)
		hub.mu.Unlock()
		close(resume)
		return nil
	}
	hub.mu.Unlock()
	return nil
}

func (hub *Hub) Poll(runID string) ControlSignal {
	hub.mu.Lock()
	queue := hub.signals[runID]
	if len(queue) == 0 {
		hub.mu.Unlock()
		return ControlSignal{Kind: CtrlNone}
	}
	signal := queue[0]
	hub.signals[runID] = queue[1:]
	if signal.Kind == CtrlPause && hub.paused[runID] == nil {
		hub.paused[runID] = make(chan struct{})
	}
	hub.mu.Unlock()

	if signal.Kind == CtrlPause && hub.control != nil {
		if err := hub.control.TransitionControl(runID, StatusRunning, StatusPaused); err != nil {
			log.WarnfCtx(ctxWithRunID(runID), "[hub] pause transition rejected: %v", err)
			hub.mu.Lock()
			paused := hub.paused[runID]
			delete(hub.paused, runID)
			hub.mu.Unlock()
			if paused != nil {
				close(paused)
			}
			return ControlSignal{Kind: CtrlAbort}
		}
	}
	return signal
}

func (hub *Hub) WaitResume(ctx context.Context, runID string) error {
	hub.mu.Lock()
	resume := hub.paused[runID]
	hub.mu.Unlock()
	if resume == nil {
		return nil
	}
	select {
	case <-resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Observer is the agent loop's boundary for live events and persisted steps.
type Observer interface {
	OnStep(ctx context.Context, runID string, step StepRecord) error
	OnToken(ctx context.Context, runID, token string)
	OnReasoning(ctx context.Context, runID, token string)
}

// ContextUsageObserver is optional so existing observer implementations remain
// source-compatible while context-budget telemetry evolves independently.
type ContextUsageObserver interface {
	OnContextUsage(ctx context.Context, runID string, event ContextUsageEvent)
}

// Controller delivers out-of-band control signals to a running loop.
type Controller interface {
	Poll(runID string) ControlSignal
	WaitResume(ctx context.Context, runID string) error
}

type noopObserver struct{}

func (noopObserver) OnStep(context.Context, string, StepRecord) error {
	return nil
}

func (noopObserver) OnToken(context.Context, string, string) {}

func (noopObserver) OnReasoning(context.Context, string, string) {}

func NoopObserver() Observer {
	return noopObserver{}
}

func ctxWithRunID(runID string) context.Context {
	return log.WithTraceID(context.Background(), runID)
}
