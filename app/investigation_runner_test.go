package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
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
				{ID: "g1", Facet: "entrypoint", Facets: []string{"entrypoint"}, Required: true, HighRisk: true, MinimumCoverage: 2},
			},
		},
	}
	contract := contractFromTaskContract(request)
	if contract.ID != "workflow-1" ||
		contract.Version != investigation.InvestigationContractVersion ||
		contract.Question != request.Contract.Objective {
		t.Fatalf("contract = %#v", contract)
	}
	if len(contract.EvidenceGoals) != 1 || !contract.EvidenceGoals[0].HighRisk || contract.EvidenceGoals[0].MinimumCoverage != 2 {
		t.Fatalf("goals = %#v", contract.EvidenceGoals)
	}
	if len(contract.SeedEvidence) != 1 ||
		contract.SeedEvidence[0].Target != "service-a" ||
		contract.SeedEvidence[0].Section != "L10-L20" {
		t.Fatalf("seed evidence = %#v", contract.SeedEvidence)
	}
}

func TestTerminalMapsDeliveryResult(t *testing.T) {
	run := investigation.InvestigationRun{
		ID: "run-1",
		Contract: investigation.InvestigationContract{
			ID: "run-1", Version: investigation.InvestigationContractVersion,
		},
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

func TestLoadTerminalRejectsUnsupportedContractVersion(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	if err := store.Create(investigation.InvestigationRun{
		ID: "run-old-contract", Status: investigation.RunCreated,
		Contract: investigation.InvestigationContract{ID: "run-old-contract", Version: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Fail("run-old-contract", investigation.RunFailure{
		Code: investigation.FailurePlan, Message: "old contract", Stage: string(investigation.StagePlanning),
	}, investigation.RunFailed); err != nil {
		t.Fatal(err)
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
	}

	_, err := runner.LoadTerminal(t.Context(), "run-old-contract")
	if !errors.Is(err, investigation.ErrPlanInvalid) {
		t.Fatalf("LoadTerminal error = %v", err)
	}
}

func TestAwaitTerminalPollsDurableRunWithoutProcessLocalState(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	if err := store.Create(investigation.InvestigationRun{
		ID: "run-durable", Status: investigation.RunCreated,
		Contract: investigation.InvestigationContract{
			ID: "run-durable", Version: investigation.InvestigationContractVersion,
		},
	}); err != nil {
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

func TestInvestigationChildRecoveryPaginatesAndConvergesRuns(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	for _, id := range []string{"workflow-created-a", "workflow-created-b"} {
		if err := store.Create(investigation.InvestigationRun{
			ID: id, Status: investigation.RunCreated,
			Contract: investigation.InvestigationContract{
				ID: id, Version: investigation.InvestigationContractVersion,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seedComposingRun(t, store, "workflow-composing")
	coordinator := investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store})
	runner := &qaInvestigator{platform: &Platform{}, coord: coordinator}

	if err := runner.RecoverActive(t.Context(), time.Now().UTC().Add(time.Hour), 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"workflow-created-a", "workflow-created-b"} {
		run, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != investigation.RunFailed || run.Failure == nil || run.Failure.Stage != "initialization" {
			t.Fatalf("run %s = %#v", id, run)
		}
	}
	resumed, err := store.Get("workflow-composing")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Status != investigation.RunDelivered || resumed.Delivery == nil {
		t.Fatalf("resumed run = %#v", resumed)
	}
}

type failingRecoveryRunStore struct {
	*investigation.MemoryRunStore
	failID string
}

func (store *failingRecoveryRunStore) Fail(id string, failure investigation.RunFailure, status investigation.RunStatus) error {
	if id == store.failID {
		return fmt.Errorf("injected fail persistence")
	}
	return store.MemoryRunStore.Fail(id, failure, status)
}

func TestInvestigationChildRecoveryContinuesAfterOneFailure(t *testing.T) {
	base := investigation.NewMemoryRunStore()
	store := &failingRecoveryRunStore{MemoryRunStore: base, failID: "workflow-fail-a"}
	for _, id := range []string{"workflow-fail-a", "workflow-fail-b"} {
		if err := base.Create(investigation.InvestigationRun{
			ID: id, Status: investigation.RunCreated,
			Contract: investigation.InvestigationContract{
				ID: id, Version: investigation.InvestigationContractVersion,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
	}
	if err := runner.RecoverActive(t.Context(), time.Now().UTC().Add(time.Hour), 1); err == nil {
		t.Fatal("recovery returned nil after injected failure")
	}
	converged, err := base.Get("workflow-fail-b")
	if err != nil {
		t.Fatal(err)
	}
	if converged.Status != investigation.RunFailed {
		t.Fatalf("later run was not recovered: %#v", converged)
	}
}

func TestInvestigationChildRecoverySkipsRunOwnedByAnotherWorker(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	seedComposingRun(t, store, "workflow-owned")
	leases := investigation.NewMemoryLeaseStore()
	if err := leases.AcquireLease(t.Context(), "workflow-owned", "other-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord: investigation.NewCoordinator(investigation.CoordinatorOptions{
			Store: store,
			Lease: leases,
		}),
	}
	if err := runner.RecoverActive(t.Context(), time.Now().UTC().Add(time.Hour), 10); err != nil {
		t.Fatalf("lease conflict should be skipped: %v", err)
	}
	run, err := store.Get("workflow-owned")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != investigation.RunComposing {
		t.Fatalf("owned run changed: %#v", run)
	}
}

func seedComposingRun(t *testing.T, store investigation.RunStore, id string) {
	t.Helper()
	contract := investigation.InvestigationContract{
		Version: investigation.InvestigationContractVersion,
		ID:      id, Question: "question",
		EvidenceGoals: []investigation.EvidenceGoal{{ID: "goal", Kind: "flow", Required: true}},
	}
	if err := store.Create(investigation.InvestigationRun{ID: id, Status: investigation.RunCreated, Contract: contract}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(id, investigation.RunAnalyzing); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition(id, investigation.RunPlanned); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePlan(id, investigation.PlanRevision{
		Revision: 1, ContractID: id,
		Tasks: []investigation.ExecutableTask{{ID: "task", EvidenceGoalIDs: []string{"goal"}, Status: investigation.TaskPending}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []investigation.RunStatus{investigation.RunExecuting, investigation.RunVerifying, investigation.RunComposing} {
		if err := store.Transition(id, status); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInvestigationRunnerRejectsConcurrentTrackedStart(t *testing.T) {
	runner := &qaInvestigator{}
	first := &investigationState{done: make(chan struct{})}
	if err := runner.track("workflow-track", first); err != nil {
		t.Fatal(err)
	}
	if err := runner.track("workflow-track", &investigationState{done: make(chan struct{})}); !errors.Is(err, investigation.ErrInvalidTransition) {
		t.Fatalf("duplicate track error = %v", err)
	}
	close(first.done)
	second := &investigationState{done: make(chan struct{})}
	if err := runner.track("workflow-track", second); err != nil {
		t.Fatalf("completed entry was not replaceable: %v", err)
	}
}

func TestInvestigationRunnerStaleCompletionCannotOverwriteReplacement(t *testing.T) {
	runner := &qaInvestigator{}
	old := &investigationState{done: make(chan struct{})}
	close(old.done)
	if err := runner.track("workflow-stale", old); err != nil {
		t.Fatal(err)
	}
	current := &investigationState{done: make(chan struct{})}
	if err := runner.track("workflow-stale", current); err != nil {
		t.Fatal(err)
	}
	runner.complete("workflow-stale", old, agent.InvestigationTerminal{Status: agent.InvestigationFailed}, errors.New("stale"))
	if current.err != nil || current.terminal.Status != "" {
		t.Fatalf("current state was overwritten: %#v", current)
	}
	runner.remove("workflow-stale", old)
	if got, ok := runner.state("workflow-stale"); !ok || got != current {
		t.Fatalf("stale remove deleted current state: got=%p ok=%t", got, ok)
	}
}

func TestLoadTerminalNeverUsesProcessLocalTerminalWithoutSnapshot(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
		runs: map[string]*investigationState{
			"workflow-deleted": {
				done:     closedChannel(),
				terminal: agent.InvestigationTerminal{WorkflowRunID: "workflow-deleted", Status: agent.InvestigationSucceeded},
			},
		},
	}
	_, err := runner.LoadTerminal(t.Context(), "workflow-deleted")
	if !errors.Is(err, investigation.ErrNotFound) {
		t.Fatalf("LoadTerminal error = %v, want durable ErrNotFound", err)
	}
}

func TestAwaitTerminalKeepsPollingAfterLocalWorkerStopsBeforeTerminalPersistence(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	if err := store.Create(investigation.InvestigationRun{
		ID: "workflow-lag", Status: investigation.RunCreated,
		Contract: investigation.InvestigationContract{
			ID: "workflow-lag", Version: investigation.InvestigationContractVersion,
		},
	}); err != nil {
		t.Fatal(err)
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
		runs: map[string]*investigationState{
			"workflow-lag": {done: closedChannel()},
		},
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = store.Fail("workflow-lag", investigation.RunFailure{Code: investigation.FailureExecution, Message: "failed"}, investigation.RunFailed)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	terminal, err := runner.AwaitTerminal(ctx, "workflow-lag")
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != agent.InvestigationFailed {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func closedChannel() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func TestLoadTerminalReportsActiveSnapshotAsConflict(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	if err := store.Create(investigation.InvestigationRun{
		ID: "workflow-active", Status: investigation.RunCreated,
		Contract: investigation.InvestigationContract{
			ID: "workflow-active", Version: investigation.InvestigationContractVersion,
		},
	}); err != nil {
		t.Fatal(err)
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
	}
	_, err := runner.LoadTerminal(t.Context(), "workflow-active")
	if !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("LoadTerminal error = %v, want workflow.ErrConflict", err)
	}
}

func TestSameInvestigationContractIgnoresCreationTime(t *testing.T) {
	goal := investigation.EvidenceGoal{ID: "goal", Kind: "flow", Required: true}
	left := investigation.InvestigationContract{
		Version: investigation.InvestigationContractVersion,
		ID:      "workflow-contract", Question: "question", EvidenceGoals: []investigation.EvidenceGoal{goal},
		CreatedAt: time.Now().UTC(),
	}
	right := investigation.InvestigationContract{
		Version: investigation.InvestigationContractVersion,
		ID:      "workflow-contract", Question: "question", EvidenceGoals: []investigation.EvidenceGoal{goal},
		CreatedAt: time.Now().UTC().Add(time.Minute),
	}
	if !sameInvestigationContract(left, right) {
		t.Fatal("equivalent contracts did not match")
	}
	right.Question = "different"
	if sameInvestigationContract(left, right) {
		t.Fatal("different contracts matched")
	}
}

func TestLoadRoundReconstructsDurableContinuationMetadata(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	runID := "workflow-round-metadata"
	contract := investigation.InvestigationContract{
		Version: investigation.InvestigationContractVersion,
		ID:      runID, ParentRunID: "parent-round", TaskID: "parent-round", Round: 2, BaseDepth: 3,
		Actor: agentapi.Actor{UserID: 42}, Question: "question", Entities: []string{"service-a"},
		EntityDetails: []investigation.InvestigationEntity{{ID: "service-a", Label: "Service A", Aliases: []string{"svc-a"}}},
		Context: investigation.InvestigationContext{
			ConversationRefs: []investigation.InvestigationConversationRef{{SessionID: "session-a", RunID: "run-a", Turn: 2}},
			TimeRange:        &investigation.InvestigationTimeRange{From: time.Now().UTC().Add(-time.Hour), To: time.Now().UTC(), ToExclusive: true},
			SeedMaterial:     []agentapi.ContextBlock{{Source: "history", Content: "context"}},
		},
		InvestigationGoals: []investigation.InvestigationGoal{{ID: "deliverable", Objective: "explain flow"}},
		EvidenceGoals: []investigation.EvidenceGoal{{
			ID: "flow", Kind: "core_flow", Facets: []string{"core_flow", "data_and_state"}, Required: true,
		}},
		SeedEvidence: []investigation.EvidenceUnit{{SourceKind: "code", Target: "service-a", ContentHash: "hash"}},
	}
	if err := store.Create(investigation.InvestigationRun{ID: runID, Status: investigation.RunCreated, Contract: contract}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []investigation.RunStatus{investigation.RunAnalyzing, investigation.RunPlanned} {
		if err := store.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SavePlan(runID, investigation.PlanRevision{
		Revision: 1, ContractID: runID,
		Tasks: []investigation.ExecutableTask{{ID: "task", EvidenceGoalIDs: []string{"flow"}, Status: investigation.TaskPending}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []investigation.RunStatus{investigation.RunExecuting, investigation.RunVerifying, investigation.RunComposing} {
		if err := store.Transition(runID, status); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveDelivery(runID, investigation.DeliveryResult{Status: investigation.DeliveryPartial, Text: "partial"}); err != nil {
		t.Fatal(err)
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
	}
	snapshot, err := runner.LoadRound(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Terminal.Round != 2 || snapshot.Terminal.BaseDepth != 3 || snapshot.Actor.UserID != 42 {
		t.Fatalf("snapshot metadata = %#v", snapshot)
	}
	if snapshot.Contract.TaskID != "parent-round" || len(snapshot.Contract.InvestigationGoals) != 1 ||
		len(snapshot.Contract.EvidenceGoals) != 1 || len(snapshot.SeedEvidence) != 1 ||
		len(snapshot.Contract.EvidenceGoals[0].Facets) != 2 || snapshot.Contract.EvidenceGoals[0].Facets[1] != "data_and_state" ||
		snapshot.Contract.Entities[0].Label != "Service A" || len(snapshot.Contract.Context.ConversationRefs) != 1 ||
		snapshot.Contract.Context.TimeRange == nil || len(snapshot.Contract.Context.SeedMaterial) != 1 {
		t.Fatalf("snapshot contract = %#v", snapshot)
	}
}

var _ agent.InvestigationContinuationRunner = (*qaInvestigator)(nil)

func TestLoadRoundMapsMissingSnapshotToWorkflowNotFound(t *testing.T) {
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: investigation.NewMemoryRunStore()}),
	}
	_, err := runner.LoadRound(t.Context(), "workflow-round-missing")
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("LoadRound error = %v, want workflow.ErrNotFound", err)
	}
}

func TestInvestigationChildRecoveryTerminalizesSnapshotsWithoutDurablePlan(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	for _, item := range []struct {
		id     string
		status investigation.RunStatus
	}{
		{id: "workflow-analyzing", status: investigation.RunAnalyzing},
		{id: "workflow-planned-empty", status: investigation.RunPlanned},
	} {
		if err := store.Create(investigation.InvestigationRun{
			ID: item.id, Status: investigation.RunCreated,
			Contract: investigation.InvestigationContract{
				ID: item.id, Version: investigation.InvestigationContractVersion,
			},
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Transition(item.id, investigation.RunAnalyzing); err != nil {
			t.Fatal(err)
		}
		if item.status == investigation.RunPlanned {
			if err := store.Transition(item.id, investigation.RunPlanned); err != nil {
				t.Fatal(err)
			}
		}
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
	}
	if err := runner.RecoverActive(t.Context(), time.Now().UTC().Add(time.Hour), 10); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"workflow-analyzing", "workflow-planned-empty"} {
		run, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != investigation.RunFailed || run.Failure == nil || run.Failure.Code != investigation.FailurePlan {
			t.Fatalf("run %s = %#v", id, run)
		}
	}
}

func TestSameInvestigationContractTreatsNilAndEmptySlicesAsEquivalent(t *testing.T) {
	left := investigation.InvestigationContract{
		ID: "workflow-empty", Version: investigation.InvestigationContractVersion,
		Question: "question",
	}
	right := investigation.InvestigationContract{
		Version: investigation.InvestigationContractVersion,
		ID:      "workflow-empty", Question: "question",
		Entities: []string{}, InvestigationGoals: []investigation.InvestigationGoal{},
		EvidenceGoals: []investigation.EvidenceGoal{}, SeedEvidence: []investigation.EvidenceUnit{},
	}
	if !sameInvestigationContract(left, right) {
		t.Fatal("nil and empty canonical slices did not match")
	}
}

func TestInvestigationRunnerCancelWaitsForDurableTerminal(t *testing.T) {
	store := investigation.NewMemoryRunStore()
	if err := store.Create(investigation.InvestigationRun{
		ID: "workflow-cancel", Status: investigation.RunCreated,
		Contract: investigation.InvestigationContract{
			ID: "workflow-cancel", Version: investigation.InvestigationContractVersion,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Transition("workflow-cancel", investigation.RunAnalyzing); err != nil {
		t.Fatal(err)
	}
	runner := &qaInvestigator{
		platform: &Platform{},
		coord:    investigation.NewCoordinator(investigation.CoordinatorOptions{Store: store}),
	}
	if err := runner.Cancel(t.Context(), "workflow-cancel", 42); err != nil {
		t.Fatal(err)
	}
	terminal, err := runner.LoadTerminal(t.Context(), "workflow-cancel")
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != agent.InvestigationCancelled {
		t.Fatalf("terminal = %#v", terminal)
	}
}

func TestTerminalMapsEvidenceInsufficientDeliveryToSuccessfulPartialResult(t *testing.T) {
	run := investigation.InvestigationRun{
		ID: "run-evidence-insufficient",
		Contract: investigation.InvestigationContract{
			ID: "run-evidence-insufficient", Version: investigation.InvestigationContractVersion,
		},
		Status: investigation.RunDelivered,
		Delivery: &investigation.DeliveryResult{
			Status: investigation.DeliveryEvidenceInsufficient,
			Text:   "The investigation produced no admissible evidence.",
			Report: investigation.InvestigationReport{
				Gaps: []investigation.EvidenceGap{{
					GoalID: "core_flow", Reason: "no verified claim covers this goal",
				}},
			},
		},
	}

	terminal, err := investigationTerminal(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != agent.InvestigationSucceeded || terminal.ErrorCode != "" ||
		terminal.StopReason != "evidence_insufficient" {
		t.Fatalf("terminal = %#v, want successful user-visible result", terminal)
	}
	if terminal.Output == nil || terminal.Output.Answer != run.Delivery.Text ||
		terminal.Completeness != agent.InvestigationPartial {
		t.Fatalf("terminal output = %#v, want partial answer", terminal)
	}
}

func TestTerminalMapsPartialDeliveryToSuccessfulPartialResult(t *testing.T) {
	run := investigation.InvestigationRun{
		ID: "run-partial",
		Contract: investigation.InvestigationContract{
			ID: "run-partial", Version: investigation.InvestigationContractVersion,
		},
		Status: investigation.RunDelivered,
		Delivery: &investigation.DeliveryResult{
			Status: investigation.DeliveryPartial,
			Text:   "partial answer",
			Report: investigation.InvestigationReport{
				Evidence: []investigation.EvidenceUnit{{
					ID: "evidence-1", SourceKind: "code", Target: "service-a",
					Content: "limited evidence", ContentHash: "hash",
				}},
			},
		},
	}
	terminal, err := investigationTerminal(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != agent.InvestigationSucceeded || terminal.ErrorCode != "" {
		t.Fatalf("terminal = %#v", terminal)
	}
	if terminal.Output == nil || terminal.Completeness != agent.InvestigationPartial {
		t.Fatalf("terminal output = %#v", terminal)
	}
}
