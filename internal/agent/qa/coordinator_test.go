package qa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/investigation"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

type coordinatorParentStore struct {
	parent QAParentRecord
	err    error
}

func (store coordinatorParentStore) GetQAParent(runID string) (QAParentRecord, error) {
	if store.err != nil {
		return QAParentRecord{}, store.err
	}
	if store.parent.ID != runID {
		return QAParentRecord{}, fmt.Errorf("parent %q not found", runID)
	}
	return store.parent, nil
}

type coordinatorScenarioLifecycle struct {
	order    *[]string
	outcomes []RunOutcome
	err      error
}

type coordinatorPlannerStub struct {
	calls    int
	contract TaskContract
	result   InvestigationResult
	seed     []tool.EvidenceUnit
	proposal *agentapi.TaskGraphProposal
}

func (planner *coordinatorPlannerStub) PlanInvestigation(
	_ context.Context,
	contract TaskContract,
	result InvestigationResult,
	seed []tool.EvidenceUnit,
) (*agentapi.TaskGraphProposal, error) {
	planner.calls++
	planner.contract = contract
	planner.result = result
	planner.seed = cloneEvidenceUnits(seed)
	return planner.proposal, nil
}

func (*coordinatorScenarioLifecycle) Start(
	context.Context,
	ScenarioRunStart,
) (ScenarioRun, error) {
	panic("unexpected Start")
}

func (lifecycle *coordinatorScenarioLifecycle) Complete(
	_ context.Context,
	_ string,
	outcome RunOutcome,
) error {
	if lifecycle.order != nil {
		*lifecycle.order = append(*lifecycle.order, "parent")
	}
	if lifecycle.err != nil {
		return lifecycle.err
	}
	lifecycle.outcomes = append(lifecycle.outcomes, outcome)
	return nil
}

type coordinatorSessionStore struct {
	order      *[]string
	ensureErr  error
	appendErr  error
	turnByRun  map[string]int
	appendRuns []string
}

func (store *coordinatorSessionStore) EnsureSession(string, int64, string) error {
	if store.order != nil {
		*store.order = append(*store.order, "session")
	}
	return store.ensureErr
}

func (store *coordinatorSessionStore) AppendTurn(
	_ string,
	runID string,
	_ int64,
	_ []llm.Message,
) (int, error) {
	store.appendRuns = append(store.appendRuns, runID)
	if store.appendErr != nil {
		return 0, store.appendErr
	}
	if turn, exists := store.turnByRun[runID]; exists {
		return turn, nil
	}
	turn := len(store.turnByRun) + 1
	store.turnByRun[runID] = turn
	return turn, nil
}

func TestCoordinatorPersistsSessionBeforeParent(t *testing.T) {
	order := make([]string, 0, 2)
	parent := QAParentRecord{
		ID: "parent-1", WorkflowRunID: "workflow-1", UserID: 42,
		SessionID: "session-1", Question: "question", Status: RunStatusRunning,
	}
	sessions := &coordinatorSessionStore{
		order: &order, turnByRun: make(map[string]int),
	}
	scenarios := &coordinatorScenarioLifecycle{order: &order}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			result := InvestigationResult{Answer: "answer"}
			return InvestigationTerminal{
				WorkflowRunID: parent.WorkflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
		sessions:      sessions,
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if strings.Join(order, ",") != "session,parent" {
		t.Fatalf("order = %v, want session before parent", order)
	}
	if len(scenarios.outcomes) != 1 || scenarios.outcomes[0].Answer != "answer" {
		t.Fatalf("outcomes = %+v", scenarios.outcomes)
	}
}

func TestCoordinatorLeavesParentActiveWhenSessionFails(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-session-failed", WorkflowRunID: "workflow-1", UserID: 42,
		SessionID: "session-1", Question: "question", Status: RunStatusRunning,
	}
	scenarios := &coordinatorScenarioLifecycle{}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			result := InvestigationResult{Answer: "answer"}
			return InvestigationTerminal{
				WorkflowRunID: parent.WorkflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
		sessions: &coordinatorSessionStore{
			ensureErr: errors.New("session unavailable"),
			turnByRun: make(map[string]int),
		},
	}

	err := coordinator.Reconcile(t.Context(), parent.ID)
	if err == nil || !strings.Contains(err.Error(), "session unavailable") {
		t.Fatalf("Reconcile error = %v", err)
	}
	if len(scenarios.outcomes) != 0 {
		t.Fatalf("parent completed after session failure: %+v", scenarios.outcomes)
	}
}

func TestCoordinatorReplayUsesIdempotentSessionRunKey(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-replay", WorkflowRunID: "workflow-1", UserID: 42,
		SessionID: "session-1", Question: "question", Status: RunStatusRunning,
	}
	sessions := &coordinatorSessionStore{turnByRun: make(map[string]int)}
	scenarios := &coordinatorScenarioLifecycle{}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			result := InvestigationResult{Answer: "answer"}
			return InvestigationTerminal{
				WorkflowRunID: parent.WorkflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
		sessions:      sessions,
	}

	for range 2 {
		if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	if len(sessions.turnByRun) != 1 || sessions.turnByRun[parent.ID] != 1 {
		t.Fatalf("turns = %+v", sessions.turnByRun)
	}
	if len(sessions.appendRuns) != 2 ||
		sessions.appendRuns[0] != parent.ID ||
		sessions.appendRuns[1] != parent.ID {
		t.Fatalf("append run keys = %v", sessions.appendRuns)
	}
}

func TestCoordinatorFailsParentWhenInvestigationRunIsMissing(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-missing", WorkflowRunID: "workflow-missing", Status: RunStatusRunning,
	}
	scenarios := &coordinatorScenarioLifecycle{}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			return InvestigationTerminal{}, fmt.Errorf("load snapshot: %w", investigation.ErrNotFound)
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(scenarios.outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want one terminal outcome", scenarios.outcomes)
	}
	outcome := scenarios.outcomes[0]
	if outcome.Status != RunStatusFailed || outcome.ErrorCode != "investigation_run_missing" {
		t.Fatalf("outcome = %+v, want failed investigation_run_missing", outcome)
	}
}

func TestCoordinatorAwaitFailsParentWhenInvestigationRunIsMissing(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-await-missing", WorkflowRunID: "workflow-await-missing", Status: RunStatusRunning,
	}
	scenarios := &coordinatorScenarioLifecycle{}
	runner := &investigationRunnerRecorder{
		await: func(context.Context, string) (InvestigationTerminal, error) {
			return InvestigationTerminal{}, fmt.Errorf("await snapshot: %w", investigation.ErrNotFound)
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Await(t.Context(), parent.ID, parent.WorkflowRunID); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if len(scenarios.outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want one terminal outcome", scenarios.outcomes)
	}
	outcome := scenarios.outcomes[0]
	if outcome.Status != RunStatusFailed || outcome.ErrorCode != "investigation_run_missing" {
		t.Fatalf("outcome = %+v, want failed investigation_run_missing", outcome)
	}
}

func TestCoordinatorPreservesActiveWorkflowConflict(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-active", WorkflowRunID: "workflow-active", Status: RunStatusRunning,
	}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			return InvestigationTerminal{}, workflow.ErrConflict
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     &coordinatorScenarioLifecycle{},
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	err := coordinator.Reconcile(t.Context(), parent.ID)
	if !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("Reconcile error = %v, want workflow.ErrConflict", err)
	}
}

func TestCoordinatorCancellationConvergesAfterWorkflow(t *testing.T) {
	order := make([]string, 0, 2)
	ctx, cancel := context.WithCancel(t.Context())
	parent := QAParentRecord{
		ID: "parent-cancel", WorkflowRunID: "workflow-cancel",
		UserID: 42, Status: RunStatusRunning,
	}
	runner := &investigationRunnerRecorder{
		cancel: func(_ context.Context, runID string, userID int64) error {
			order = append(order, "workflow")
			if runID != parent.WorkflowRunID || userID != parent.UserID {
				t.Fatalf("cancel workflow=%q user=%d", runID, userID)
			}
			cancel()
			return nil
		},
		load: func(loadCtx context.Context, _ string) (InvestigationTerminal, error) {
			if err := loadCtx.Err(); err != nil {
				t.Fatalf("load terminal received cancelled context: %v", err)
			}
			return InvestigationTerminal{
				WorkflowRunID: parent.WorkflowRunID,
				Status:        InvestigationCancelled,
				ErrorCode:     "workflow_cancelled",
			}, nil
		},
	}
	scenarios := &coordinatorScenarioLifecycle{order: &order}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Cancel(ctx, parent.ID, parent.UserID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if strings.Join(order, ",") != "workflow,parent" {
		t.Fatalf("order = %v, want workflow before parent", order)
	}
	if len(scenarios.outcomes) != 1 ||
		scenarios.outcomes[0].Status != RunStatusAborted ||
		scenarios.outcomes[0].ErrorCode != "workflow_cancelled" {
		t.Fatalf("outcomes = %+v", scenarios.outcomes)
	}
}

func TestCoordinatorCancellationStopsOnWorkflowFailure(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-cancel-failed", WorkflowRunID: "workflow-cancel",
		UserID: 42, Status: RunStatusRunning,
	}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &Coordinator{
		investigation: &investigationRunnerRecorder{
			cancel: func(context.Context, string, int64) error {
				return errors.New("cancel failed")
			},
		},
		scenarios:  scenarios,
		parentRuns: coordinatorParentStore{parent: parent},
	}

	err := coordinator.Cancel(t.Context(), parent.ID, parent.UserID)
	if err == nil || !strings.Contains(err.Error(), "cancel failed") {
		t.Fatalf("Cancel error = %v", err)
	}
	if len(scenarios.outcomes) != 0 {
		t.Fatalf("parent completed after cancellation failure: %+v", scenarios.outcomes)
	}
}

func TestCoordinatorRejectsInvalidParentBindings(t *testing.T) {
	tests := []struct {
		name        string
		coordinator *Coordinator
		runID       string
		userID      int64
		want        string
	}{
		{
			name: "store unavailable",
			coordinator: &Coordinator{
				investigation: &investigationRunnerRecorder{},
			},
			runID: "parent-1", want: "parent run store is unavailable",
		},
		{
			name: "workflow missing",
			coordinator: &Coordinator{
				investigation: &investigationRunnerRecorder{},
				parentRuns: coordinatorParentStore{
					parent: QAParentRecord{ID: "parent-1", UserID: 42},
				},
			},
			runID: "parent-1", want: "has no workflow run",
		},
		{
			name: "owner mismatch",
			coordinator: &Coordinator{
				investigation: &investigationRunnerRecorder{},
				parentRuns: coordinatorParentStore{
					parent: QAParentRecord{
						ID: "parent-1", WorkflowRunID: "workflow-1", UserID: 42,
					},
				},
			},
			runID: "parent-1", userID: 7, want: "is not owned by user 7",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.coordinator.Cancel(
				t.Context(),
				test.runID,
				test.userID,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Cancel error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCoordinatorRequiresSessionStoreForBoundSession(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-no-sessions", WorkflowRunID: "workflow-1", UserID: 42,
		SessionID: "session-1", Question: "question", Status: RunStatusRunning,
	}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			result := InvestigationResult{Answer: "answer"}
			return InvestigationTerminal{
				WorkflowRunID: parent.WorkflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     &coordinatorScenarioLifecycle{},
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	err := coordinator.Reconcile(t.Context(), parent.ID)
	if err == nil || !strings.Contains(err.Error(), "session store is unavailable") {
		t.Fatalf("Reconcile error = %v", err)
	}
}

func TestCoordinatorAllowsParentWithoutSession(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-no-session", WorkflowRunID: "workflow-1",
		Question: "question", Status: RunStatusRunning,
	}
	scenarios := &coordinatorScenarioLifecycle{}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			result := InvestigationResult{Answer: "answer"}
			return InvestigationTerminal{
				WorkflowRunID: parent.WorkflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	coordinator := &Coordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(scenarios.outcomes) != 1 {
		t.Fatalf("outcomes = %+v", scenarios.outcomes)
	}
}

type coordinatorContinuationRunner struct {
	*investigationRunnerRecorder
	snapshots      map[string]InvestigationRoundSnapshot
	startSnapshots map[string]InvestigationRoundSnapshot
	loadRoundErr   map[string]error
	loadRoundIDs   []string
	startRequests  []InvestigationContinuationRequest
	awaitIDs       []string
}

func (runner *coordinatorContinuationRunner) LoadRound(
	_ context.Context,
	workflowRunID string,
) (InvestigationRoundSnapshot, error) {
	runner.loadRoundIDs = append(runner.loadRoundIDs, workflowRunID)
	if snapshot, ok := runner.snapshots[workflowRunID]; ok {
		return snapshot, nil
	}
	if err, ok := runner.loadRoundErr[workflowRunID]; ok {
		return InvestigationRoundSnapshot{}, err
	}
	return InvestigationRoundSnapshot{}, workflow.ErrNotFound
}

func (runner *coordinatorContinuationRunner) StartNextRound(
	_ context.Context,
	request InvestigationContinuationRequest,
) error {
	runner.startRequests = append(runner.startRequests, request)
	if snapshot, ok := runner.startSnapshots[request.WorkflowRunID]; ok {
		runner.snapshots[request.WorkflowRunID] = snapshot
	}
	return nil
}

func (runner *coordinatorContinuationRunner) AwaitTerminal(
	_ context.Context,
	workflowRunID string,
) (InvestigationTerminal, error) {
	runner.awaitIDs = append(runner.awaitIDs, workflowRunID)
	snapshot, ok := runner.snapshots[workflowRunID]
	if !ok {
		return InvestigationTerminal{}, workflow.ErrNotFound
	}
	return snapshot.Terminal, nil
}

func coordinatorEvidenceUnit(
	sourceKind, target, section, contentHash string,
) tool.EvidenceUnit {
	return tool.EvidenceUnit{
		SourceKind: sourceKind, Target: target,
		Sections: []string{section}, ContentHash: contentHash,
	}
}

func coordinatorContinuationClaim() []InvestigationClaim {
	return []InvestigationClaim{{
		ProducerNodeID: "investigate.docs.1", FindingIndex: 1,
		Claim: "bounded finding", EvidenceGoalIDs: []string{"core_flow"},
	}}
}

func coordinatorContinuationContract() TaskContract {
	return TaskContract{
		TaskID: "parent-dynamic", Objective: "investigate the system",
		EvidenceGoals: []EvidenceGoal{
			{ID: "business_domain", Facet: "business_domain", Required: true},
			{ID: "core_flow", Facet: "core_flow", Required: true},
		},
		InvestigationGoals: []InvestigationGoal{{
			ID: "core_flow", Objective: "trace the core flow",
			IndependentlyUseful: true, DependsOn: []string{"business_domain"},
		}},
		TaskEvidenceAssignments: []TaskEvidenceAssignment{{
			TaskID: "investigator.code",
			InputRefs: []agentapi.EvidenceRef{{
				SourceKind: "code", Target: "seed.Service", Section: "overview",
			}},
		}},
		Context: TaskContext{SeedMaterial: []agentapi.ContextBlock{{
			Source: "qa.seed", Content: "seed context",
		}}},
	}
}

func TestCoordinatorContinuationStartsStableBoundedRound(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-dynamic", WorkflowRunID: "workflow-1", UserID: 42,
		Status: RunStatusRunning,
	}
	seed := coordinatorEvidenceUnit("code", "seed.Service", "overview", "seed-v1")
	first := coordinatorEvidenceUnit("code", "first.Service", "implementation", "first-v1")
	second := coordinatorEvidenceUnit("runtime", "first-service", "logs", "second-v1")
	firstTerminal := InvestigationTerminal{
		WorkflowRunID: parent.WorkflowRunID, Status: InvestigationSucceeded,
		Completeness: InvestigationPartial, Round: 1, BaseDepth: 2,
		Usage: InvestigationUsage{InputTokens: 30, OutputTokens: 10, TotalTokens: 40, ToolCalls: 2},
		Output: &InvestigationResult{
			Answer: "round one", Round: 1,
			EvidenceUnits:           []tool.EvidenceUnit{first},
			PartialClaims:           coordinatorContinuationClaim(),
			PartialEvidenceGoals:    []string{"core_flow"},
			UnresolvedEvidenceGoals: []string{"core_flow"},
		},
	}
	childID := StableRoundWorkflowID(parent.ID, 2)
	childTerminal := InvestigationTerminal{
		WorkflowRunID: childID, Status: InvestigationSucceeded,
		Completeness: InvestigationComplete, Round: 2, BaseDepth: 2,
		Output: &InvestigationResult{
			Answer: "round two", Round: 2,
			EvidenceUnits: []tool.EvidenceUnit{second},
		},
	}
	runner := &coordinatorContinuationRunner{
		investigationRunnerRecorder: &investigationRunnerRecorder{
			load: func(context.Context, string) (InvestigationTerminal, error) {
				return firstTerminal, nil
			},
		},
		snapshots: map[string]InvestigationRoundSnapshot{
			parent.WorkflowRunID: {
				Terminal:     firstTerminal,
				Contract:     coordinatorContinuationContract(),
				SeedEvidence: []tool.EvidenceUnit{seed},
				Actor:        agentapi.Actor{UserID: parent.UserID, TenantID: "tenant-1"},
				BudgetLimit:  InvestigationBudget{InputTokens: 80, OutputTokens: 20, TotalTokens: 100, ToolCalls: 5},
			},
		},
		loadRoundErr: make(map[string]error),
		startSnapshots: map[string]InvestigationRoundSnapshot{
			childID: {
				Terminal:     childTerminal,
				Contract:     coordinatorContinuationContract(),
				SeedEvidence: []tool.EvidenceUnit{seed, first},
				Actor:        agentapi.Actor{UserID: parent.UserID, TenantID: "tenant-1"},
			},
		},
	}
	scenarios := &coordinatorScenarioLifecycle{}
	planner := &coordinatorPlannerStub{
		proposal: &agentapi.TaskGraphProposal{
			Tasks: []agentapi.TaskSpec{{
				ID: "dynamic.task", Capability: "knowledge.code.inspect",
				OutputSchema: agentapi.InvestigationReportSchemaRef(),
			}},
		},
	}
	coordinator := &Coordinator{
		investigation: runner, scenarios: scenarios,
		parentRuns: coordinatorParentStore{parent: parent},
		planner:    planner,
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(runner.startRequests) != 1 {
		t.Fatalf("start requests = %d, want 1", len(runner.startRequests))
	}
	request := runner.startRequests[0]
	if request.ParentRunID != parent.ID ||
		request.PreviousWorkflowRunID != parent.WorkflowRunID ||
		request.WorkflowRunID != childID || request.Round != 2 ||
		request.Actor.TenantID != "tenant-1" {
		t.Fatalf("continuation request = %+v", request)
	}
	if len(request.Contract.EvidenceGoals) != 1 ||
		request.Contract.EvidenceGoals[0].ID != "core_flow" {
		t.Fatalf("next evidence goals = %+v", request.Contract.EvidenceGoals)
	}
	if len(request.Contract.InvestigationGoals) != 1 ||
		request.Contract.InvestigationGoals[0].ID != "core_flow" ||
		len(request.Contract.InvestigationGoals[0].DependsOn) != 0 {
		t.Fatalf("next investigation goals = %+v", request.Contract.InvestigationGoals)
	}
	if request.Contract.TaskEvidenceAssignments != nil {
		t.Fatalf("next task evidence assignments = %+v, want nil", request.Contract.TaskEvidenceAssignments)
	}
	if request.Proposal == nil || request.Proposal.Stop.MaxInputTokens != 50 ||
		request.Proposal.Stop.MaxOutputTokens != 10 || request.Proposal.Stop.MaxTotalTokens != 60 ||
		request.Proposal.Stop.MaxToolCalls != 3 {
		t.Fatalf("continuation budget = %+v", request.Proposal)
	}
	if len(request.SeedEvidence) != 2 ||
		request.SeedEvidence[0].Target != seed.Target ||
		request.SeedEvidence[1].Target != first.Target {
		t.Fatalf("next seed evidence = %+v", request.SeedEvidence)
	}
	if planner.calls != 1 || planner.contract.EvidenceGoals[0].ID != "core_flow" ||
		planner.result.UnresolvedEvidenceGoals[0] != "core_flow" || len(planner.seed) != 2 {
		t.Fatalf("planner input = calls:%d contract:%+v result:%+v seed:%+v", planner.calls, planner.contract, planner.result, planner.seed)
	}
	if request.Proposal == nil || len(request.Proposal.Tasks) != 1 ||
		request.Proposal.Tasks[0].ID != "dynamic.task" {
		t.Fatalf("continuation proposal = %+v", request.Proposal)
	}
	if len(scenarios.outcomes) != 1 ||
		scenarios.outcomes[0].Status != RunStatusDone ||
		scenarios.outcomes[0].Answer != "round two" {
		t.Fatalf("parent outcomes = %+v", scenarios.outcomes)
	}
}

func TestCoordinatorContinuationStopsWithoutNewEvidence(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-no-progress", WorkflowRunID: "workflow-1", UserID: 42,
		Status: RunStatusRunning,
	}
	unit := coordinatorEvidenceUnit("code", "same.Service", "implementation", "v1")
	terminal := InvestigationTerminal{
		WorkflowRunID: parent.WorkflowRunID, Status: InvestigationSucceeded,
		Completeness: InvestigationPartial, Round: 1,
		Output: &InvestigationResult{
			Answer: "partial", Round: 1,
			EvidenceUnits:           []tool.EvidenceUnit{unit},
			PartialClaims:           coordinatorContinuationClaim(),
			UnresolvedEvidenceGoals: []string{"core_flow"},
		},
	}
	runner := &coordinatorContinuationRunner{
		investigationRunnerRecorder: &investigationRunnerRecorder{
			load: func(context.Context, string) (InvestigationTerminal, error) {
				return terminal, nil
			},
		},
		snapshots: map[string]InvestigationRoundSnapshot{
			parent.WorkflowRunID: {
				Terminal: terminal, Contract: coordinatorContinuationContract(),
				SeedEvidence: []tool.EvidenceUnit{unit},
			},
		},
		loadRoundErr: make(map[string]error),
	}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &Coordinator{
		investigation: runner, scenarios: scenarios,
		parentRuns: coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(runner.startRequests) != 0 {
		t.Fatalf("unexpected continuation starts = %+v", runner.startRequests)
	}
	if len(scenarios.outcomes) != 1 || scenarios.outcomes[0].Answer != "partial" {
		t.Fatalf("parent outcomes = %+v", scenarios.outcomes)
	}
}

func TestCoordinatorContinuationReusesExistingStableChild(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-replay-round", WorkflowRunID: "workflow-1", UserID: 42,
		Status: RunStatusRunning,
	}
	seed := coordinatorEvidenceUnit("code", "seed.Service", "overview", "seed-v1")
	first := coordinatorEvidenceUnit("code", "first.Service", "implementation", "v1")
	second := coordinatorEvidenceUnit("runtime", "first-service", "logs", "v2")
	firstTerminal := InvestigationTerminal{
		WorkflowRunID: parent.WorkflowRunID, Status: InvestigationSucceeded,
		Completeness: InvestigationPartial, Round: 1,
		Output: &InvestigationResult{
			Answer: "round one", Round: 1, EvidenceUnits: []tool.EvidenceUnit{first},
			PartialClaims:           coordinatorContinuationClaim(),
			UnresolvedEvidenceGoals: []string{"core_flow"},
		},
	}
	childID := StableRoundWorkflowID(parent.ID, 2)
	childTerminal := InvestigationTerminal{
		WorkflowRunID: childID, Status: InvestigationSucceeded,
		Completeness: InvestigationComplete, Round: 2,
		Output: &InvestigationResult{
			Answer: "replayed round two", Round: 2,
			EvidenceUnits: []tool.EvidenceUnit{second},
		},
	}
	runner := &coordinatorContinuationRunner{
		investigationRunnerRecorder: &investigationRunnerRecorder{
			load: func(context.Context, string) (InvestigationTerminal, error) {
				return firstTerminal, nil
			},
		},
		snapshots: map[string]InvestigationRoundSnapshot{
			parent.WorkflowRunID: {
				Terminal: firstTerminal, Contract: coordinatorContinuationContract(),
				SeedEvidence: []tool.EvidenceUnit{seed},
			},
			childID: {
				Terminal: childTerminal, Contract: coordinatorContinuationContract(),
				SeedEvidence: []tool.EvidenceUnit{seed, first},
			},
		},
		loadRoundErr: make(map[string]error),
	}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &Coordinator{
		investigation: runner, scenarios: scenarios,
		parentRuns: coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(runner.startRequests) != 0 {
		t.Fatalf("stable child was started again: %+v", runner.startRequests)
	}
	if len(scenarios.outcomes) != 1 ||
		scenarios.outcomes[0].Answer != "replayed round two" {
		t.Fatalf("parent outcomes = %+v", scenarios.outcomes)
	}
}

func TestCoordinatorContinuationStopsAtRoundLimit(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-round-limit", WorkflowRunID: "workflow-1", UserID: 42,
		Status: RunStatusRunning,
	}
	terminal := InvestigationTerminal{
		WorkflowRunID: parent.WorkflowRunID, Status: InvestigationSucceeded,
		Completeness: InvestigationPartial, Round: defaultInvestigationMaxRounds,
		Output: &InvestigationResult{
			Answer: "bounded answer", Round: defaultInvestigationMaxRounds,
			EvidenceUnits: []tool.EvidenceUnit{
				coordinatorEvidenceUnit("code", "new.Service", "implementation", "v1"),
			},
			UnresolvedEvidenceGoals: []string{"core_flow"},
		},
	}
	runner := &coordinatorContinuationRunner{
		investigationRunnerRecorder: &investigationRunnerRecorder{
			load: func(context.Context, string) (InvestigationTerminal, error) {
				return terminal, nil
			},
		},
		snapshots: map[string]InvestigationRoundSnapshot{
			parent.WorkflowRunID: {
				Terminal: terminal, Contract: coordinatorContinuationContract(),
			},
		},
		loadRoundErr: make(map[string]error),
	}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &Coordinator{
		investigation: runner, scenarios: scenarios,
		parentRuns: coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(runner.startRequests) != 0 {
		t.Fatalf("continuation started past round limit: %+v", runner.startRequests)
	}
}

func TestLoadInvestigationTerminalRetriesTransientMissingSnapshot(t *testing.T) {
	calls := 0
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			calls++
			if calls < 3 {
				return InvestigationTerminal{}, fmt.Errorf("replica lag: %w", investigation.ErrNotFound)
			}
			return InvestigationTerminal{WorkflowRunID: "workflow-retry", Status: InvestigationFailed}, nil
		},
	}
	terminal, err := loadInvestigationTerminalWithRetry(t.Context(), runner, "workflow-retry", "reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || terminal.WorkflowRunID != "workflow-retry" {
		t.Fatalf("calls=%d terminal=%#v", calls, terminal)
	}
}

func TestLoadInvestigationTerminalStopsAfterMissingRetryBudget(t *testing.T) {
	calls := 0
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			calls++
			return InvestigationTerminal{}, fmt.Errorf("missing: %w", investigation.ErrNotFound)
		},
	}
	_, err := loadInvestigationTerminalWithRetry(t.Context(), runner, "workflow-missing", "reconcile")
	if !errors.Is(err, investigation.ErrNotFound) || calls != investigationMissingRetryAttempts {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestLoadInvestigationTerminalCancellationStopsRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			calls++
			cancel()
			return InvestigationTerminal{}, fmt.Errorf("missing: %w", investigation.ErrNotFound)
		},
	}
	_, err := loadInvestigationTerminalWithRetry(ctx, runner, "workflow-cancel", "reconcile")
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestLoadInvestigationTerminalDoesNotRetryOtherFailures(t *testing.T) {
	calls := 0
	want := errors.New("database unavailable")
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			calls++
			return InvestigationTerminal{}, want
		},
	}
	_, err := loadInvestigationTerminalWithRetry(t.Context(), runner, "workflow-error", "reconcile")
	if !errors.Is(err, want) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestCoordinatorTerminalParentSkipsWorkflowAndSessionSideEffects(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-terminal", WorkflowRunID: "workflow-terminal", Status: RunStatusDone,
		SessionID: "session-terminal", UserID: 42,
	}
	sessions := &coordinatorSessionStore{turnByRun: make(map[string]int)}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &Coordinator{
		investigation: &investigationRunnerRecorder{},
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
		sessions:      sessions,
	}
	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Await(t.Context(), parent.ID, parent.WorkflowRunID); err != nil {
		t.Fatal(err)
	}
	if len(sessions.appendRuns) != 0 || len(scenarios.outcomes) != 0 {
		t.Fatalf("terminal replay produced side effects: sessions=%v outcomes=%v", sessions.appendRuns, scenarios.outcomes)
	}
}

func TestCoordinatorTreatsConcurrentParentCompletionAsReplay(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-concurrent-complete", WorkflowRunID: "workflow-concurrent-complete", Status: RunStatusRunning,
	}
	scenarios := &coordinatorScenarioLifecycle{err: agentrun.ErrNotActive}
	coordinator := &Coordinator{scenarios: scenarios}
	if err := coordinator.completeOutcome(t.Context(), parent, RunOutcome{Status: RunStatusFailed}); err != nil {
		t.Fatalf("concurrent completion was not replay-safe: %v", err)
	}
}

func TestCoordinatorDoesNotContinueBudgetExhaustedPartialEvidence(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-budget", WorkflowRunID: "workflow-budget", UserID: 42,
		Status: RunStatusRunning,
	}
	terminal := InvestigationTerminal{
		WorkflowRunID: parent.WorkflowRunID, Status: InvestigationFailed,
		ErrorCode: "budget_exhausted", StopReason: string(workflow.StopBudgetExhausted),
		Completeness: InvestigationPartial, Round: 2,
		Output: &InvestigationResult{
			Answer: "partial answer", UnresolvedEvidenceGoals: []string{"core_flow"},
			EvidenceUnits: []tool.EvidenceUnit{coordinatorEvidenceUnit("code", "service-a", "implementation", "v1")},
		},
	}
	runner := &coordinatorContinuationRunner{
		investigationRunnerRecorder: &investigationRunnerRecorder{load: func(context.Context, string) (InvestigationTerminal, error) {
			return terminal, nil
		}},
		snapshots: map[string]InvestigationRoundSnapshot{
			parent.WorkflowRunID: {Terminal: terminal, Contract: coordinatorContinuationContract()},
		},
		loadRoundErr: make(map[string]error),
	}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &Coordinator{
		investigation: runner, scenarios: scenarios,
		parentRuns: coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(runner.startRequests) != 0 {
		t.Fatalf("continuation started after budget exhaustion: %+v", runner.startRequests)
	}
	if len(scenarios.outcomes) != 1 || scenarios.outcomes[0].Status != RunStatusDone ||
		scenarios.outcomes[0].ErrorCode != "budget_exhausted" ||
		scenarios.outcomes[0].Answer != "partial answer" {
		t.Fatalf("outcomes = %+v", scenarios.outcomes)
	}
}

func TestContinuationEligibilityRejectsBudgetExhaustion(t *testing.T) {
	for _, terminal := range []InvestigationTerminal{
		{Status: InvestigationSucceeded, StopReason: string(workflow.StopBudgetExhausted)},
		{Status: InvestigationFailed, ErrorCode: "budget_exhausted"},
	} {
		if continuationEligible(terminal) {
			t.Fatalf("budget terminal was eligible for continuation: %+v", terminal)
		}
	}
}

func TestCoordinatorDoesNotContinueNonEvidenceFailure(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-composer-failure", WorkflowRunID: "workflow-composer-failure", Status: RunStatusRunning,
	}
	terminal := InvestigationTerminal{
		WorkflowRunID: parent.WorkflowRunID, Status: InvestigationFailed, ErrorCode: "composer_failed",
		Completeness: InvestigationPartial, Round: 1,
		Output: &InvestigationResult{
			Answer: "fallback answer", EvidenceUnits: []tool.EvidenceUnit{
				coordinatorEvidenceUnit("code", "service-a", "implementation", "v1"),
			},
			UnresolvedEvidenceGoals: []string{"core_flow"},
		},
	}
	runner := &coordinatorContinuationRunner{
		investigationRunnerRecorder: &investigationRunnerRecorder{load: func(context.Context, string) (InvestigationTerminal, error) {
			return terminal, nil
		}},
		snapshots: map[string]InvestigationRoundSnapshot{
			parent.WorkflowRunID: {Terminal: terminal, Contract: coordinatorContinuationContract()},
		},
		loadRoundErr: make(map[string]error),
	}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &Coordinator{
		investigation: runner, scenarios: scenarios, parentRuns: coordinatorParentStore{parent: parent},
	}

	if err := coordinator.Reconcile(t.Context(), parent.ID); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(runner.startRequests) != 0 {
		t.Fatalf("continuation started after non-evidence failure: %+v", runner.startRequests)
	}
	if len(scenarios.outcomes) != 1 || scenarios.outcomes[0].Status != RunStatusDone ||
		scenarios.outcomes[0].ErrorCode != "composer_failed" ||
		scenarios.outcomes[0].Answer != "fallback answer" {
		t.Fatalf("outcomes = %+v", scenarios.outcomes)
	}
}

func TestCoordinatorUsesConfiguredContinuationRoundLimit(t *testing.T) {
	coordinator := &Coordinator{}
	if coordinator.investigationMaxRounds() != defaultInvestigationMaxRounds {
		t.Fatalf("default max rounds = %d", coordinator.investigationMaxRounds())
	}
	coordinator.SetInvestigationMaxRounds(6)
	if coordinator.investigationMaxRounds() != 6 {
		t.Fatalf("configured max rounds = %d", coordinator.investigationMaxRounds())
	}
}

func TestContinuationEligibilityRejectsWorkflowBudgetExhaustion(t *testing.T) {
	terminal := InvestigationTerminal{
		Status:    InvestigationFailed,
		ErrorCode: "workflow_budget_exhausted",
	}
	if continuationEligible(terminal) {
		t.Fatal("workflow budget exhaustion was eligible for continuation")
	}
}
