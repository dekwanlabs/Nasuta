package app

import (
	"context"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestContractFromTaskContract(t *testing.T) {
	request := agent.InvestigationRequest{
		WorkflowRunID: "workflow-1",
		SeedEvidence: []tool.EvidenceUnit{{
			SourceKind: "code", Target: "service-a", Sections: []string{"L10-L20"},
			ContentHash: "seed-hash",
		}},
		Contract: agent.TaskContract{
			Objective: "where is the AI entrypoint?",
			Entities:  []agent.EntityRef{{ID: "hsas-aiot-service"}},
			EvidenceGoals: []agent.EvidenceGoal{
				{ID: "g1", Facet: "entrypoint", Required: true, HighRisk: true, MinimumCoverage: 2},
			},
		},
	}
	contract := contractFromTaskContract(request)
	if contract.ID != "workflow-1" || contract.Question != request.Contract.Objective {
		t.Fatalf("contract = %#v", contract)
	}
	if len(contract.Goals) != 1 || !contract.Goals[0].HighRisk || contract.Goals[0].MinimumCoverage != 2 {
		t.Fatalf("goals = %#v", contract.Goals)
	}
	if len(contract.SeedEvidence) != 1 ||
		contract.SeedEvidence[0].Target != "service-a" ||
		contract.SeedEvidence[0].Section != "L10-L20" {
		t.Fatalf("seed evidence = %#v", contract.SeedEvidence)
	}
}

func TestTerminalMapsDeliveryResult(t *testing.T) {
	run := investigation.InvestigationRun{
		ID:     "run-1",
		Status: investigation.RunDelivered,
		Delivery: &investigation.DeliveryResult{
			Status: investigation.DeliverySucceeded,
			Text:   "verified answer",
			Report: investigation.InvestigationReport{
				Evidence: []investigation.EvidenceUnit{{
					ID: "evidence-1", SourceKind: "code", Target: "service-a",
					Content: "the model client is called here", ContentHash: "hash",
				}},
				Claims: []investigation.VerifiedClaim{{
					ID: "claim-1", GoalID: "g1", Text: "the entrypoint is verified",
					Status:       investigation.ClaimSupported,
					EvidenceRefs: []investigation.EvidenceRef{{EvidenceID: "evidence-1", SourceKind: "code", Target: "service-a"}},
				}},
			},
		},
	}
	terminal, err := investigationTerminal(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != agent.InvestigationSucceeded || terminal.Output == nil {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal.Output.Answer != "verified answer" || len(terminal.Output.SupportedClaims) != 1 {
		t.Fatalf("output = %#v", terminal.Output)
	}
	if len(terminal.Output.EvidenceUnits) != 1 {
		t.Fatalf("evidence = %#v", terminal.Output.EvidenceUnits)
	}
}

func TestAwaitTerminalPollsDurableRunWithoutProcessLocalState(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	if err := store.Create(investigation.InvestigationRun{ID: "run-durable", Status: investigation.RunCreated}); err != nil {
		t.Fatal(err)
	}
	coordinator := investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store})
	runner := &qaInvestigator{platform: &Platform{}, coord: coordinator}

	go func() {
		time.Sleep(150 * time.Millisecond)
		if err := store.Fail("run-durable", investigation.RunFailure{
			Code: investigation.FailureExecution, Message: "worker stopped", Stage: string(investigation.StageExecution),
		}, investigation.RunFailed); err != nil {
			t.Errorf("persist terminal run: %v", err)
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	terminal, err := runner.AwaitTerminal(ctx, "run-durable")
	if err != nil {
		t.Fatal(err)
	}
	if terminal.WorkflowRunID != "run-durable" || terminal.Status != agent.InvestigationFailed {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal.ErrorCode != string(investigation.FailureExecution) {
		t.Fatalf("error code = %q, want %q", terminal.ErrorCode, investigation.FailureExecution)
	}
}
