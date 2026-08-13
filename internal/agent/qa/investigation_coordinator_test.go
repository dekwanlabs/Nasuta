package qa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/llm"
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

func (*coordinatorScenarioLifecycle) BeginScenario(
	context.Context,
	ScenarioRunStart,
) (ScenarioRun, error) {
	panic("unexpected BeginScenario")
}

func (lifecycle *coordinatorScenarioLifecycle) CompleteScenario(
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

func TestInvestigationCoordinatorPersistsSessionBeforeParent(t *testing.T) {
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
				Output:        &result,
			}, nil
		},
	}
	coordinator := &InvestigationCoordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
		sessions:      sessions,
	}

	if err := coordinator.ReconcileInvestigation(t.Context(), parent.ID); err != nil {
		t.Fatalf("ReconcileInvestigation: %v", err)
	}
	if strings.Join(order, ",") != "session,parent" {
		t.Fatalf("order = %v, want session before parent", order)
	}
	if len(scenarios.outcomes) != 1 || scenarios.outcomes[0].Answer != "answer" {
		t.Fatalf("outcomes = %+v", scenarios.outcomes)
	}
}

func TestInvestigationCoordinatorLeavesParentActiveWhenSessionFails(t *testing.T) {
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
				Output:        &result,
			}, nil
		},
	}
	coordinator := &InvestigationCoordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
		sessions: &coordinatorSessionStore{
			ensureErr: errors.New("session unavailable"),
			turnByRun: make(map[string]int),
		},
	}

	err := coordinator.ReconcileInvestigation(t.Context(), parent.ID)
	if err == nil || !strings.Contains(err.Error(), "session unavailable") {
		t.Fatalf("ReconcileInvestigation error = %v", err)
	}
	if len(scenarios.outcomes) != 0 {
		t.Fatalf("parent completed after session failure: %+v", scenarios.outcomes)
	}
}

func TestInvestigationCoordinatorReplayUsesIdempotentSessionRunKey(t *testing.T) {
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
				Output:        &result,
			}, nil
		},
	}
	coordinator := &InvestigationCoordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
		sessions:      sessions,
	}

	for range 2 {
		if err := coordinator.ReconcileInvestigation(t.Context(), parent.ID); err != nil {
			t.Fatalf("ReconcileInvestigation: %v", err)
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

func TestInvestigationCoordinatorPreservesActiveWorkflowConflict(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-active", WorkflowRunID: "workflow-active", Status: RunStatusRunning,
	}
	runner := &investigationRunnerRecorder{
		load: func(context.Context, string) (InvestigationTerminal, error) {
			return InvestigationTerminal{}, workflow.ErrConflict
		},
	}
	coordinator := &InvestigationCoordinator{
		investigation: runner,
		scenarios:     &coordinatorScenarioLifecycle{},
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	err := coordinator.ReconcileInvestigation(t.Context(), parent.ID)
	if !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("ReconcileInvestigation error = %v, want workflow.ErrConflict", err)
	}
}

func TestInvestigationCoordinatorCancellationConvergesAfterWorkflow(t *testing.T) {
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
	coordinator := &InvestigationCoordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	if err := coordinator.CancelInvestigation(ctx, parent.ID, parent.UserID); err != nil {
		t.Fatalf("CancelInvestigation: %v", err)
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

func TestInvestigationCoordinatorCancellationStopsOnWorkflowFailure(t *testing.T) {
	parent := QAParentRecord{
		ID: "parent-cancel-failed", WorkflowRunID: "workflow-cancel",
		UserID: 42, Status: RunStatusRunning,
	}
	scenarios := &coordinatorScenarioLifecycle{}
	coordinator := &InvestigationCoordinator{
		investigation: &investigationRunnerRecorder{
			cancel: func(context.Context, string, int64) error {
				return errors.New("cancel failed")
			},
		},
		scenarios:  scenarios,
		parentRuns: coordinatorParentStore{parent: parent},
	}

	err := coordinator.CancelInvestigation(t.Context(), parent.ID, parent.UserID)
	if err == nil || !strings.Contains(err.Error(), "cancel failed") {
		t.Fatalf("CancelInvestigation error = %v", err)
	}
	if len(scenarios.outcomes) != 0 {
		t.Fatalf("parent completed after cancellation failure: %+v", scenarios.outcomes)
	}
}

func TestInvestigationCoordinatorRejectsInvalidParentBindings(t *testing.T) {
	tests := []struct {
		name        string
		coordinator *InvestigationCoordinator
		runID       string
		userID      int64
		want        string
	}{
		{
			name: "store unavailable",
			coordinator: &InvestigationCoordinator{
				investigation: &investigationRunnerRecorder{},
			},
			runID: "parent-1", want: "parent run store is unavailable",
		},
		{
			name: "workflow missing",
			coordinator: &InvestigationCoordinator{
				investigation: &investigationRunnerRecorder{},
				parentRuns: coordinatorParentStore{
					parent: QAParentRecord{ID: "parent-1", UserID: 42},
				},
			},
			runID: "parent-1", want: "has no workflow run",
		},
		{
			name: "owner mismatch",
			coordinator: &InvestigationCoordinator{
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
			err := test.coordinator.CancelInvestigation(
				t.Context(),
				test.runID,
				test.userID,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("CancelInvestigation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInvestigationCoordinatorRequiresSessionStoreForBoundSession(t *testing.T) {
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
				Output:        &result,
			}, nil
		},
	}
	coordinator := &InvestigationCoordinator{
		investigation: runner,
		scenarios:     &coordinatorScenarioLifecycle{},
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	err := coordinator.ReconcileInvestigation(t.Context(), parent.ID)
	if err == nil || !strings.Contains(err.Error(), "session store is unavailable") {
		t.Fatalf("ReconcileInvestigation error = %v", err)
	}
}

func TestInvestigationCoordinatorAllowsParentWithoutSession(t *testing.T) {
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
				Output:        &result,
			}, nil
		},
	}
	coordinator := &InvestigationCoordinator{
		investigation: runner,
		scenarios:     scenarios,
		parentRuns:    coordinatorParentStore{parent: parent},
	}

	if err := coordinator.ReconcileInvestigation(t.Context(), parent.ID); err != nil {
		t.Fatalf("ReconcileInvestigation: %v", err)
	}
	if len(scenarios.outcomes) != 1 {
		t.Fatalf("outcomes = %+v", scenarios.outcomes)
	}
}
