package app

import (
	"context"

	"github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/transport/dashboard"
)

// qaApplication adapts the QA service and its event hub into the narrow
// application port consumed by the dashboard transport. Dashboard no longer
// sees the QA service, hub, or stores directly.
type qaApplication struct {
	service *qa.Service
	hub     *run.Hub
}

// Start prepares, admits, and starts one QA run. The event subscription is
// bound before service.Ask runs so early preparation events are not dropped.
func (app *qaApplication) Start(ctx context.Context, request dashboard.QAStartRequest) (dashboard.QAStartedRun, error) {
	runID := qa.NewRunID()
	var events chan run.SSEEvent
	var unsubscribe func()
	if app.hub != nil {
		events = app.hub.Subscribe(runID)
		unsubscribe = func() {
			app.hub.Unsubscribe(runID, events)
		}
	}

	runCtx := context.WithoutCancel(ctx)
	if request.TraceEnabled && app.hub != nil {
		runCtx = runtrace.WithEvaluation(runCtx, func(event domain.EvaluationTrace) {
			app.hub.EmitTrace(runID, event)
		})
	}

	result, err := app.service.Ask(runCtx, qa.Request{
		Question:        request.Question,
		Conversation:    request.Conversation,
		UserID:          request.UserID,
		RolePrompt:      request.RolePrompt,
		RunID:           runID,
		EvidencePlan:    request.EvidencePlan,
		WriteAuthorized: request.WriteAuthorized,
		WriteRequested:  request.WriteRequested,
	})
	if err != nil {
		if unsubscribe != nil {
			unsubscribe()
		}
		return dashboard.QAStartedRun{}, err
	}
	var retrieved *retrieval.RetrievedContext
	if result != nil {
		retrieved = result.Context
	}
	return dashboard.QAStartedRun{
		RunID:   runID,
		Context: retrieved,
		Events:  events,
		Close:   unsubscribe,
	}, nil
}

func (app *qaApplication) CompactionStatus(runID string) run.SessionStatusEvent {
	if app.service == nil {
		return run.SessionStatusEvent{}
	}
	return app.service.CompactionStatus(runID)
}

func (app *qaApplication) ContextUsage(runID string) (run.ContextUsageEvent, bool) {
	if app.hub == nil {
		return run.ContextUsageEvent{}, false
	}
	return app.hub.ContextUsage(runID)
}
