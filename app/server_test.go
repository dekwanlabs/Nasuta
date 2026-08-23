package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
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

type qaRecoveryReconcilerStub struct {
	parentRunIDs []string
	errs         map[string]error
}

func (reconciler *qaRecoveryReconcilerStub) Reconcile(
	_ context.Context,
	parentRunID string,
) error {
	reconciler.parentRunIDs = append(reconciler.parentRunIDs, parentRunID)
	return reconciler.errs[parentRunID]
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
		wantQAParent   string
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
			qa := &qaRecoveryReconcilerStub{}
			reviews := &reviewRecoveryReconcilerStub{}

			err := reconcileRecoveredWorkflow(
				t.Context(),
				workflows,
				qa,
				reviews,
				test.record.ID,
				test.resumeErr,
			)
			if err != nil {
				t.Fatal(err)
			}
			if test.wantQAParent != "" {
				if len(qa.parentRunIDs) != 1 ||
					qa.parentRunIDs[0] != test.wantQAParent ||
					reviews.calls != 0 {
					t.Fatalf("QA calls = %v, review calls = %d", qa.parentRunIDs, reviews.calls)
				}
				return
			}
			if len(qa.parentRunIDs) != 0 ||
				reviews.calls != 1 ||
				reviews.runID != test.record.ID ||
				reviews.status != test.record.Status ||
				!errors.Is(reviews.cause, resumeErr) {
				t.Fatalf("QA calls = %v, review = %+v", qa.parentRunIDs, reviews)
			}
		})
	}
}

type activeQAParentStoreStub struct {
	pages   [][]run.QAParentRecord
	cursors []run.QAParentCursor
	limits  []int
}

func (store *activeQAParentStoreStub) ListActiveQAParents(
	_ time.Time,
	cursor run.QAParentCursor,
	limit int,
) ([]run.QAParentRecord, error) {
	store.cursors = append(store.cursors, cursor)
	store.limits = append(store.limits, limit)
	index := len(store.cursors) - 1
	if index >= len(store.pages) {
		return nil, nil
	}
	return store.pages[index], nil
}

func TestReconcileActiveQAParentsUsesStablePagesAndContinuesAfterErrors(t *testing.T) {
	parents := &activeQAParentStoreStub{
		pages: [][]run.QAParentRecord{
			{
				{ID: "parent-1", StartedAt: "2026-08-13T01:00:00Z"},
				{ID: "parent-2", StartedAt: "2026-08-13T01:01:00Z"},
			},
			{
				{ID: "parent-3", StartedAt: "2026-08-13T01:02:00Z"},
			},
		},
	}
	reconciler := &qaRecoveryReconcilerStub{
		errs: map[string]error{
			"parent-1": errors.New("terminal event unavailable"),
			"parent-2": workflow.ErrConflict,
		},
	}

	report, err := reconcileActiveQAParents(
		t.Context(),
		time.Date(2026, 8, 13, 2, 0, 0, 0, time.UTC),
		2,
		parents,
		reconciler,
	)

	if err == nil || !strings.Contains(err.Error(), "parent-1") {
		t.Fatalf("error = %v", err)
	}
	if report.Scanned != 3 || report.Converged != 1 ||
		report.Active != 1 || report.Errors != 1 {
		t.Fatalf("report = %+v", report)
	}
	if len(reconciler.parentRunIDs) != 3 {
		t.Fatalf("reconciled parents = %v", reconciler.parentRunIDs)
	}
	if len(parents.cursors) != 2 ||
		parents.cursors[0] != (run.QAParentCursor{}) ||
		parents.cursors[1] != (run.QAParentCursor{
			StartedAt: "2026-08-13T01:01:00Z",
			ID:        "parent-2",
		}) {
		t.Fatalf("cursors = %+v", parents.cursors)
	}
	if len(parents.limits) != 2 || parents.limits[0] != 2 || parents.limits[1] != 2 {
		t.Fatalf("limits = %v", parents.limits)
	}
}
