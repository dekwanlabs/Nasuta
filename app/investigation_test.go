package app

import (
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
)

type investigationEventProjection struct {
	eventType run.EventType
	event     run.ExecutionEvent
}

type investigationEventRecorder struct {
	projected chan investigationEventProjection
}

func (recorder *investigationEventRecorder) EmitExecutionEvent(
	eventType run.EventType,
	event run.ExecutionEvent,
) {
	recorder.projected <- investigationEventProjection{
		eventType: eventType,
		event:     event,
	}
}

func TestProjectInvestigationEvent(t *testing.T) {
	tests := []struct {
		name       string
		event      workflow.Event
		wantType   run.EventType
		wantStatus string
		want       bool
	}{
		{name: "workflow", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "workflow_started"}, wantType: run.EventWorkflowStarted, wantStatus: "running", want: true},
		{name: "agent start", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_started", NodeID: "investigate.code"}, wantType: run.EventAgentStarted, wantStatus: "running", want: true},
		{name: "agent complete", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_succeeded", NodeID: "synthesize"}, wantType: run.EventAgentCompleted, wantStatus: "completed", want: true},
		{name: "agent failed", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_failed", NodeID: "investigate.docs", Summary: "node failed"}, wantType: run.EventAgentCompleted, wantStatus: "failed", want: true},
		{name: "evidence joined", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_succeeded", NodeID: "evidence.join"}, wantType: run.EventEvidenceJoined, wantStatus: "completed", want: true},
		{name: "handoff ignored", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "handoff_created", NodeID: "synthesize"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			eventType, event, ok := projectInvestigationEvent("qa_parent_1", test.event)
			if ok != test.want || eventType != test.wantType || event.Status != test.wantStatus {
				t.Fatalf("projection = (%q, %+v, %t)", eventType, event, ok)
			}
			if ok && (event.RunID != "qa_parent_1" || event.WorkflowRunID != "workflow_1" || event.Strategy != "multi_agent") {
				t.Fatalf("correlation = %+v", event)
			}
		})
	}
}

func TestBridgeInvestigationEventsStopsOnDurableCompletion(t *testing.T) {
	events := make(chan workflow.Event, 1)
	stop := make(chan struct{})
	completed := make(chan struct{})
	recorder := &investigationEventRecorder{
		projected: make(chan investigationEventProjection, 1),
	}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		bridgeInvestigationEvents(
			events,
			stop,
			completed,
			recorder,
			"qa_parent_1",
		)
	}()

	events <- workflow.Event{
		WorkflowRunID: "workflow_1",
		Kind:          "workflow_started",
	}
	select {
	case projected := <-recorder.projected:
		if projected.eventType != run.EventWorkflowStarted ||
			projected.event.RunID != "qa_parent_1" ||
			projected.event.WorkflowRunID != "workflow_1" {
			t.Fatalf("projection = %+v", projected)
		}
	case <-time.After(time.Second):
		t.Fatal("workflow event was not projected")
	}

	close(completed)
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("event bridge did not stop after durable workflow completion")
	}
}
