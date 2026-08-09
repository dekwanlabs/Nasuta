package app

import (
	"testing"

	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
)

func TestProjectInvestigationEvent(t *testing.T) {
	tests := []struct {
		name       string
		event      workflow.Event
		wantType   agentrun.EventType
		wantStatus string
		want       bool
	}{
		{name: "workflow", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "workflow_started"}, wantType: agentrun.EventWorkflowStarted, wantStatus: "running", want: true},
		{name: "agent start", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_started", NodeID: "investigate.code"}, wantType: agentrun.EventAgentStarted, wantStatus: "running", want: true},
		{name: "agent complete", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_succeeded", NodeID: "synthesize"}, wantType: agentrun.EventAgentCompleted, wantStatus: "completed", want: true},
		{name: "agent failed", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_failed", NodeID: "investigate.docs", Summary: "node failed"}, wantType: agentrun.EventAgentCompleted, wantStatus: "failed", want: true},
		{name: "evidence joined", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_succeeded", NodeID: "evidence.join"}, wantType: agentrun.EventEvidenceJoined, wantStatus: "completed", want: true},
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
