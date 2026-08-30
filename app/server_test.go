package app

import (
	"context"
	"errors"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/feature/reviewworkflow"
)

type recoveredWorkflowStoreStub struct {
	records map[string]workflow.RunRecord
}

func (store *recoveredWorkflowStoreStub) GetRun(
	_ context.Context,
	runID string,
	_ int64,
	_ bool,
) (*workflow.RunRecord, error) {
	record, ok := store.records[runID]
	if !ok {
		return nil, workflow.ErrNotFound
	}
	return &record, nil
}

type reviewRecoveryReconcilerStub struct {
	runID  string
	status workflow.RunStatus
	cause  error
	calls  int
}

func (reconciler *reviewRecoveryReconcilerStub) ReconcileRecoveredRun(
	_ context.Context,
	runID string,
	status workflow.RunStatus,
	cause error,
) error {
	reconciler.runID = runID
	reconciler.status = status
	reconciler.cause = cause
	reconciler.calls++
	return nil
}

func TestReconcileRecoveredWorkflowDispatchesByPersistedScenario(t *testing.T) {
	resumeErr := errors.New("resume failed")
	for _, test := range []struct {
		name           string
		record         workflow.RunRecord
		resumeErr      error
		wantReviewCall bool
	}{
		{
			name: "review",
			record: workflow.RunRecord{
				ID:       "review.round-1",
				Scenario: reviewworkflow.ScenarioID,
				Status:   workflow.RunFailed,
			},
			resumeErr:      resumeErr,
			wantReviewCall: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflows := &recoveredWorkflowStoreStub{
				records: map[string]workflow.RunRecord{
					test.record.ID: test.record,
				},
			}
			reviews := &reviewRecoveryReconcilerStub{}

			err := reconcileRecoveredWorkflow(
				t.Context(),
				workflows,
				reviews,
				test.record.ID,
				test.resumeErr,
			)
			if err != nil {
				t.Fatal(err)
			}
			if reviews.calls != 1 ||
				reviews.runID != test.record.ID ||
				reviews.status != test.record.Status ||
				!errors.Is(reviews.cause, resumeErr) {
				t.Fatalf("review = %+v", reviews)
			}
		})
	}
}
