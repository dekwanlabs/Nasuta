package app

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
)

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
