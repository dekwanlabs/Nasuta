package app

import (
	"encoding/json"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	platformagent "github.com/dekwanlabs/nasuta/internal/agent"
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

func (recorder *investigationEventRecorder) EmitEvent(
	eventType run.EventType,
	event run.ExecutionEvent,
) {
	recorder.projected <- investigationEventProjection{
		eventType: eventType,
		event:     event,
	}
}

func TestProjectInvestigationEvent(t *testing.T) {
	agentNodeIDs := map[string]struct{}{
		"investigate.code": {},
		"investigate.docs": {},
		"planner.runtime":  {},
		"synthesize":       {},
	}
	terminalDetail := func(detail workflow.TerminalEventDetail) json.RawMessage {
		payload, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	tests := []struct {
		name             string
		event            workflow.Event
		wantType         run.EventType
		wantStatus       string
		wantCompleteness string
		wantReason       string
		wantErrorCode    string
		want             bool
	}{
		{name: "workflow", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "workflow_started"}, wantType: run.EventWorkflowStarted, wantStatus: "running", want: true},
		{name: "agent start", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_started", NodeID: "investigate.code"}, wantType: run.EventAgentStarted, wantStatus: "running", want: true},
		{name: "agent complete", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_succeeded", NodeID: "synthesize"}, wantType: run.EventAgentCompleted, wantStatus: "completed", want: true},
		{name: "agent failed", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_failed", NodeID: "investigate.docs", Summary: "node failed"}, wantType: run.EventAgentCompleted, wantStatus: "failed", wantReason: "node failed", want: true},
		{name: "planner agent", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_started", NodeID: "planner.runtime"}, wantType: run.EventAgentStarted, wantStatus: "running", want: true},
		{name: "evidence joined", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "node_succeeded", NodeID: "evidence.join"}, wantType: run.EventEvidenceJoined, wantStatus: "completed", want: true},
		{
			name: "workflow complete", wantType: run.EventWorkflowCompleted,
			wantStatus: "succeeded", wantCompleteness: "complete",
			wantReason: "required_goals_covered", want: true,
			event: workflow.Event{
				WorkflowRunID: "workflow_1", Kind: "workflow_succeeded",
				Summary: "workflow succeeded",
				Detail: terminalDetail(workflow.TerminalEventDetail{
					RunStatus: workflow.RunSucceeded, Completeness: workflow.Complete,
					StopReason: workflow.StopRequiredGoalsCovered,
				}),
			},
		},
		{
			name: "workflow partial is still succeeded", wantType: run.EventWorkflowCompleted,
			wantStatus: "succeeded", wantCompleteness: "partial",
			wantReason: "capability_unavailable", want: true,
			event: workflow.Event{
				WorkflowRunID: "workflow_1", Kind: "workflow_succeeded",
				Summary: "workflow succeeded",
				Detail: terminalDetail(workflow.TerminalEventDetail{
					RunStatus: workflow.RunSucceeded, Completeness: workflow.Partial,
					StopReason: workflow.StopCapabilityUnavailable,
				}),
			},
		},
		{
			name: "workflow unavailable is not failed", wantType: run.EventWorkflowCompleted,
			wantStatus: "succeeded", wantCompleteness: "unavailable",
			wantReason: "no_affordable_task", want: true,
			event: workflow.Event{
				WorkflowRunID: "workflow_1", Kind: "workflow_succeeded",
				Summary: "workflow succeeded",
				Detail: terminalDetail(workflow.TerminalEventDetail{
					RunStatus: workflow.RunSucceeded, Completeness: workflow.Unavailable,
					StopReason: workflow.StopNoAffordableTask,
				}),
			},
		},
		{
			name: "workflow failed", wantType: run.EventWorkflowCompleted,
			wantStatus: "failed", wantReason: "verification_failed",
			wantErrorCode: "invalid_output", want: true,
			event: workflow.Event{
				WorkflowRunID: "workflow_1", Kind: "workflow_failed",
				Summary: "workflow failed",
				Detail: terminalDetail(workflow.TerminalEventDetail{
					RunStatus:  workflow.RunFailed,
					StopReason: workflow.StopVerificationFailed,
					ErrorCode:  "invalid_output",
				}),
			},
		},
		{name: "handoff ignored", event: workflow.Event{WorkflowRunID: "workflow_1", Kind: "handoff_created", NodeID: "synthesize"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			eventType, event, ok := projectInvestigationEvent(
				"qa_parent_1",
				test.event,
				agentNodeIDs,
			)
			if ok != test.want || eventType != test.wantType ||
				event.Status != test.wantStatus ||
				event.Completeness != test.wantCompleteness ||
				event.Reason != test.wantReason ||
				event.ErrorCode != test.wantErrorCode {
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
			map[string]struct{}{"synthesize": {}},
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

func TestInvestigationSubjectRequirementsPreserveComparisonEntities(t *testing.T) {
	contract := platformagent.TaskContract{
		Entities: []platformagent.EntityRef{
			{ID: "our_agent", Label: "Our Agent", Role: "first_party_agent", Aliases: []string{"our-agent"}},
			{ID: "google", Label: "Google", Role: "external_adapter"},
		},
		EvidenceGoals: []platformagent.EvidenceGoal{
			{
				ID: "core_flow", Facet: "core_flow", Required: true,
				RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal},
				MinimumCoverage: 2,
			},
			{
				ID: "entrypoint", Facet: "entrypoint", Required: true,
				MinimumCoverage: 1,
			},
		},
	}
	requirements := investigationSubjectRequirements(contract)
	if len(requirements) != 2 {
		t.Fatalf("subject requirements = %+v", requirements)
	}
	if requirements[0].EntityID != "our_agent" ||
		requirements[0].Role != "first_party_agent" ||
		len(requirements[0].Aliases) != 1 ||
		len(requirements[0].RequiredFacets) != 1 ||
		requirements[0].RequiredFacets[0] != "core_flow" ||
		len(requirements[0].RequiredSources) != 1 ||
		requirements[0].RequiredSources[0] != agentapi.EvidenceSourceInternal ||
		requirements[1].EntityID != "google" {
		t.Fatalf("subject requirements = %+v", requirements)
	}
}
