package definition

import (
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
)

func TestActiveRunEmitsDelegationAdoptionEvents(t *testing.T) {
	hub := agentrun.NewHub(nil)
	events := hub.Subscribe("parent-run")
	run := &activeRun{
		runtime: &Runtime{hub: hub},
	}
	run.emitDelegationAdoptions(
		"parent-run",
		[]agentapi.DelegationAdoption{{
			DelegationID:     "del-1",
			AdoptedReportIDs: []string{"report-1"},
			Status:           agentapi.DelegationAdopted,
		}},
	)

	select {
	case event := <-events:
		detail, ok := event.Data.(agentrun.ExecutionEvent)
		if event.Type != agentrun.EventDelegationAdoptionEvaluated ||
			!ok ||
			detail.RunID != "parent-run" ||
			detail.ParentRunID != "parent-run" ||
			detail.DelegationID != "del-1" ||
			detail.Status != string(agentapi.DelegationAdopted) ||
			detail.AdoptionStatus != string(agentapi.DelegationAdopted) ||
			len(detail.ReportIDs) != 1 ||
			detail.ReportIDs[0] != "report-1" {
			t.Fatalf("event = %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("delegation adoption event was not emitted")
	}
}

func TestMapResultMarksAdoptionsUnknownWhenOutputIsInvalid(t *testing.T) {
	schemas := agentapi.NewSchemaRegistry()
	outputSchema := agentapi.SchemaRef{ID: "answer.output", Version: 1}
	if err := schemas.Publish([]agentapi.SchemaDefinition{{
		ID: outputSchema.ID, Version: outputSchema.Version,
		Document: []byte(`{
			"type":"object",
			"required":["answer"],
			"properties":{"answer":{"type":"string"}},
			"additionalProperties":false
		}`),
	}}); err != nil {
		t.Fatal(err)
	}
	result := &execution.RunResult{
		RunID:  "parent-run",
		Answer: `{"unexpected":true}`,
		DelegationAdoptions: []agentapi.DelegationAdoption{{
			DelegationID:     "del-1",
			AdoptedReportIDs: []string{"report-1"},
			Status:           agentapi.DelegationAdopted,
		}},
	}

	publicResult, outcome := mapResult(
		"parent-run",
		result,
		nil,
		nil,
		agentapi.Usage{},
		nil,
		schemas,
		outputSchema,
	)

	if publicResult.Status != agentapi.RunFailed ||
		publicResult.Error == nil ||
		publicResult.Error.Code != "invalid_output" ||
		outcome.Status != agentrun.StatusFailed ||
		outcome.ErrorCode != "invalid_output" {
		t.Fatalf("public=%+v outcome=%+v", publicResult, outcome)
	}
	for name, adoptions := range map[string][]agentapi.DelegationAdoption{
		"public":  publicResult.DelegationAdoptions,
		"outcome": outcome.DelegationAdoptions,
	} {
		if len(adoptions) != 1 ||
			adoptions[0].DelegationID != "del-1" ||
			adoptions[0].Status != agentapi.DelegationUnknown ||
			adoptions[0].Reason != "invalid_output" ||
			len(adoptions[0].AdoptedReportIDs) != 0 {
			t.Fatalf("%s adoptions = %+v", name, adoptions)
		}
	}
}
