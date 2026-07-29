package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/featuredelivery"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestAppendRunEventsUsesOneSequenceAllocationAndInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	createdAt := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM feature_implementation_runs WHERE id=\? LIMIT 1 FOR UPDATE`).
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-1"))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(seq\),0\)\+1 FROM feature_run_events WHERE run_id=\?`).
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(7))
	mock.ExpectExec(`INSERT INTO feature_run_events.*VALUES\(\?,\?,\?,\?,\?,\?\),\(\?,\?,\?,\?,\?,\?\)`).
		WithArgs(
			"run-1", int64(7), featuredelivery.EventProviderMessage, "first", nil, createdAt,
			"run-1", int64(8), featuredelivery.EventCommandStarted, "second", []byte(`{"command":"go test"}`), createdAt,
		).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	store := NewFeatureDeliveryStore(db)
	events, err := store.AppendRunEvents(context.Background(), []featuredelivery.RunEvent{
		{RunID: "run-1", Kind: featuredelivery.EventProviderMessage, Summary: "first", CreatedAt: createdAt},
		{RunID: "run-1", Kind: featuredelivery.EventCommandStarted, Summary: "second", Detail: json.RawMessage(`{"command":"go test"}`), CreatedAt: createdAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Seq != 7 || events[1].Seq != 8 {
		t.Fatalf("persisted events = %+v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptActiveImplementationsOnlyClaimsExpiredLeases(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	retainUntil := now.Add(72 * time.Hour)
	mock.ExpectQuery(`(?s)SELECT id FROM feature_implementation_runs.*status IN \('preparing','running','validating'\).*lease_expires_at IS NULL OR lease_expires_at<=\?.*LIMIT 100`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-expired").AddRow("run-raced"))
	mock.ExpectExec(`(?s)UPDATE feature_implementation_runs.*error_summary='worker lease expired'.*lease_expires_at IS NULL OR lease_expires_at<=\?`).
		WithArgs(now, retainUntil, "run-expired", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE feature_implementation_runs.*error_summary='worker lease expired'.*lease_expires_at IS NULL OR lease_expires_at<=\?`).
		WithArgs(now, retainUntil, "run-raced", now).
		WillReturnResult(sqlmock.NewResult(0, 0))

	store := NewFeatureDeliveryStore(db)
	ids, err := store.InterruptActiveImplementations(context.Background(), now, retainUntil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "run-expired" {
		t.Fatalf("interrupted IDs = %v", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentParentArtifactIDUsesApprovedLineage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT id FROM feature_artifacts.*kind=\?.*ORDER BY version DESC LIMIT 1`).
		WithArgs("feat-1", featuredelivery.KindRequirement).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-2"))
	mock.ExpectQuery(`(?s)SELECT a.id FROM feature_artifacts a.*r.decision='approved'.*a.parent_artifact_id=\?.*ORDER BY a.version DESC LIMIT 1`).
		WithArgs("feat-1", featuredelivery.KindRequirementAnalysis, "req-2").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("analysis-3"))
	mock.ExpectQuery(`(?s)SELECT a.id FROM feature_artifacts a.*r.decision='approved'.*a.parent_artifact_id=\?.*ORDER BY a.version DESC LIMIT 1`).
		WithArgs("feat-1", featuredelivery.KindTechnicalProposal, "analysis-3").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("proposal-4"))

	parentID, err := currentParentArtifactID(context.Background(), db, "feat-1", featuredelivery.KindSystemDesign)
	if err != nil {
		t.Fatal(err)
	}
	if parentID != "proposal-4" {
		t.Fatalf("parent ID = %q", parentID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCurrentParentArtifactIDRejectsIncompleteLineage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT id FROM feature_artifacts.*kind=\?.*ORDER BY version DESC LIMIT 1`).
		WithArgs("feat-1", featuredelivery.KindRequirement).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-2"))
	mock.ExpectQuery(`(?s)SELECT a.id FROM feature_artifacts a.*r.decision='approved'.*a.parent_artifact_id=\?.*ORDER BY a.version DESC LIMIT 1`).
		WithArgs("feat-1", featuredelivery.KindRequirementAnalysis, "req-2").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	_, err = currentParentArtifactID(context.Background(), db, "feat-1", featuredelivery.KindTechnicalProposal)
	if !errors.Is(err, featuredelivery.ErrConflict) {
		t.Fatalf("expected lineage conflict, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewArtifactLocksFeatureBeforeArtifact(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	reviewedAt := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT request_id FROM feature_artifacts WHERE id=\? LIMIT 1`).
		WithArgs("analysis-1").
		WillReturnRows(sqlmock.NewRows([]string{"request_id"}).AddRow("feat-1"))
	mock.ExpectQuery(`SELECT id FROM feature_requests WHERE id=\? LIMIT 1 FOR UPDATE`).
		WithArgs("feat-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("feat-1"))
	mock.ExpectQuery(`SELECT kind,parent_artifact_id FROM feature_artifacts WHERE id=\? AND request_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs("analysis-1", "feat-1").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "parent_artifact_id"}).
			AddRow(featuredelivery.KindRequirementAnalysis, "req-1"))
	mock.ExpectQuery(`(?s)SELECT id FROM feature_artifacts.*kind=\?.*ORDER BY version DESC LIMIT 1`).
		WithArgs("feat-1", featuredelivery.KindRequirement).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-1"))
	mock.ExpectExec(`INSERT INTO feature_artifact_reviews`).
		WithArgs("analysis-1", featuredelivery.DecisionApproved, "approved", int64(42), reviewedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=\?`).
		WithArgs("feat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	store := NewFeatureDeliveryStore(db)
	err = store.ReviewArtifact(context.Background(), featuredelivery.ArtifactReview{
		ArtifactID: "analysis-1",
		Decision:   featuredelivery.DecisionApproved,
		Comment:    "approved",
		Reviewer:   42,
		CreatedAt:  reviewedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListArtifactsUsesSummaryColumnsAndCursor(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	query := artifactSummarySelect + ` WHERE a.request_id=? AND a.kind=? AND a.version<? ORDER BY a.version DESC LIMIT ?`
	mock.ExpectQuery(query).
		WithArgs("feat-1", featuredelivery.KindRequirementAnalysis, 3, 20).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "request_id", "kind", "version", "parent_artifact_id", "origin",
			"content_hash", "created_by", "created_at", "review_id", "decision", "comment", "reviewer", "reviewed_at",
		}).AddRow(
			"analysis-2", "feat-1", featuredelivery.KindRequirementAnalysis, 2, "req-1", featuredelivery.OriginAgent,
			"hash", int64(7), createdAt, nil, nil, nil, nil, nil,
		))

	items, err := NewFeatureDeliveryStore(db).ListArtifacts(
		context.Background(), "feat-1", featuredelivery.KindRequirementAnalysis,
		featuredelivery.ArtifactCursor{Kind: featuredelivery.KindRequirementAnalysis, Version: 3}, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "analysis-2" {
		t.Fatalf("artifact summaries = %+v", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetCurrentLineageUsesOneBoundedQuery(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	columns := []string{
		"id", "request_id", "kind", "version", "parent_artifact_id", "origin", "document_json",
		"rendered_markdown", "evidence_json", "content_hash", "created_by", "created_at",
		"review_id", "decision", "comment", "reviewer", "reviewed_at",
	}
	rows := sqlmock.NewRows(columns).
		AddRow("req-2", "feat-1", featuredelivery.KindRequirement, 2, "", featuredelivery.OriginUser,
			[]byte(`{"description":"request"}`), "request", []byte(`[]`), "req-hash", int64(7), createdAt,
			nil, nil, nil, nil, nil).
		AddRow("analysis-3", "feat-1", featuredelivery.KindRequirementAnalysis, 3, "req-2", featuredelivery.OriginAgent,
			[]byte(`{"background":"analysis"}`), "analysis", []byte(`[]`), "analysis-hash", int64(7), createdAt,
			"analysis-3", featuredelivery.DecisionApproved, "approved", int64(9), createdAt)
	mock.ExpectQuery(currentLineageSelect).WithArgs("feat-1", "feat-1").WillReturnRows(rows)

	lineage, err := NewFeatureDeliveryStore(db).GetCurrentLineage(context.Background(), "feat-1")
	if err != nil {
		t.Fatal(err)
	}
	if lineage.Requirement == nil || lineage.Requirement.ID != "req-2" ||
		lineage.RequirementAnalysis == nil || lineage.RequirementAnalysis.ID != "analysis-3" {
		t.Fatalf("lineage = %+v", lineage)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteGenerationCommitsArtifactAndRunTogether(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)
	artifact := featuredelivery.Artifact{
		ID: "analysis-1", RequestID: "feat-1", Kind: featuredelivery.KindRequirementAnalysis,
		ParentArtifactID: "req-1", Origin: featuredelivery.OriginAgent,
		DocumentJSON: []byte(`{}`), RenderedMarkdown: "analysis", Evidence: []featuredelivery.EvidenceRef{},
		ContentHash: "hash", CreatedBy: 7, CreatedAt: createdAt,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM feature_requests WHERE id=\? LIMIT 1 FOR UPDATE`).
		WithArgs("feat-1").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("feat-1"))
	mock.ExpectQuery(`(?s)SELECT request_id,artifact_kind,parent_artifact_id,status.*WHERE id=\? LIMIT 1 FOR UPDATE`).
		WithArgs("gen-1").WillReturnRows(sqlmock.NewRows([]string{"request_id", "artifact_kind", "parent_artifact_id", "status"}).
		AddRow("feat-1", featuredelivery.KindRequirementAnalysis, "req-1", "running"))
	mock.ExpectQuery(`(?s)SELECT id FROM feature_artifacts.*kind=\?.*ORDER BY version DESC LIMIT 1`).
		WithArgs("feat-1", featuredelivery.KindRequirement).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("req-1"))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version\),0\)\+1 FROM feature_artifacts WHERE request_id=\? AND kind=\?`).
		WithArgs("feat-1", featuredelivery.KindRequirementAnalysis).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(4))
	mock.ExpectExec(`INSERT INTO feature_artifacts`).
		WithArgs(
			"analysis-1", "feat-1", featuredelivery.KindRequirementAnalysis, 4, "req-1",
			featuredelivery.OriginAgent, []byte(`{}`), "analysis", []byte(`[]`), "hash", int64(7), createdAt,
		).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)UPDATE feature_generation_runs.*status='succeeded'.*WHERE id=\? AND status='running'`).
		WithArgs(int64(12), int64(34), "gen-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE feature_requests SET updated_at=CURRENT_TIMESTAMP WHERE id=\?`).
		WithArgs("feat-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	saved, err := NewFeatureDeliveryStore(db).CompleteGeneration(context.Background(), "gen-1", artifact, 12, 34)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 4 {
		t.Fatalf("artifact version = %d", saved.Version)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListGenerationRunsUsesStableCursorAndLimit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	startedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	query := generationRunSelect + ` WHERE request_id=? AND (started_at<? OR (started_at=? AND id<?)) ORDER BY started_at DESC,id DESC LIMIT ?`
	mock.ExpectQuery(query).
		WithArgs("feat-1", startedAt, startedAt, "gen-3", 20).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "request_id", "artifact_kind", "parent_artifact_id", "status", "provider", "model",
			"requested_by", "input_tokens", "output_tokens", "error_summary", "started_at", "ended_at",
		}).AddRow(
			"gen-2", "feat-1", featuredelivery.KindTechnicalProposal, "analysis-1", "succeeded", "openai", "model-1",
			int64(7), int64(100), int64(50), "", startedAt.Add(-time.Minute), startedAt,
		))

	runs, err := NewFeatureDeliveryStore(db).ListGenerationRuns(
		context.Background(), "feat-1", featuredelivery.GenerationCursor{StartedAt: startedAt, ID: "gen-3"}, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "gen-2" || runs[0].InputTokens != 100 {
		t.Fatalf("generation runs = %+v", runs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTransitionImplementationRejectsInvalidLifecycle(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewFeatureDeliveryStore(db)

	err = store.TransitionImplementation(
		context.Background(), "run-1", "worker-1", featuredelivery.RunPreparing,
		featuredelivery.RunSucceeded, featuredelivery.RunUpdate{},
	)
	if !errors.Is(err, featuredelivery.ErrInvalid) {
		t.Fatalf("invalid transition error = %v", err)
	}
	err = store.TransitionImplementation(
		context.Background(), "run-1", "worker-1", featuredelivery.RunRunning,
		featuredelivery.RunFailed, featuredelivery.RunUpdate{},
	)
	if !errors.Is(err, featuredelivery.ErrInvalid) {
		t.Fatalf("missing retention error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateImplementationIsIdempotentByRequesterAndClientRequest(t *testing.T) {
	tests := []struct {
		name         string
		existingHash string
		wantConflict bool
	}{
		{name: "same request returns existing run", existingHash: "request-hash"},
		{name: "different request conflicts", existingHash: "different-hash", wantConflict: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			createdAt := time.Date(2026, 7, 29, 12, 30, 0, 0, time.UTC)
			run := featuredelivery.ImplementationRun{
				ID: "run-new", RequestID: "feat-1", ClientRequestID: "client-1", RequestHash: "request-hash",
				DesignArtifactID: "design-1", PlanArtifactID: "plan-1", Repo: "team/service",
				BaseRef: "main", BaseCommit: "abc", WorkspaceUserID: 7, WorkspaceUsername: "developer",
				Provider: "codex", Status: featuredelivery.RunQueued, RequestedBy: 42, CreatedAt: createdAt,
			}
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT created_by,archived_at FROM feature_requests WHERE id=\? LIMIT 1 FOR UPDATE`).
				WithArgs("feat-1").
				WillReturnRows(sqlmock.NewRows([]string{"created_by", "archived_at"}).AddRow(int64(7), nil))
			mock.ExpectExec(`INSERT INTO feature_implementation_runs`).
				WillReturnError(&mysqlDriver.MySQLError{Number: 1062, Message: "duplicate"})
			mock.ExpectRollback()
			mock.ExpectQuery(`(?s)FROM feature_implementation_runs r.*WHERE r.requested_by=\? AND r.client_request_id=\? LIMIT 1`).
				WithArgs(int64(42), "client-1").
				WillReturnRows(implementationRunRows(featuredelivery.ImplementationRun{
					ID: "run-existing", RequestID: "feat-1", ClientRequestID: "client-1", RequestHash: test.existingHash,
					DesignArtifactID: "design-1", PlanArtifactID: "plan-1", Repo: "team/service",
					BaseRef: "main", BaseCommit: "abc", WorkspaceUserID: 7, WorkspaceUsername: "developer",
					Provider: "codex", Status: featuredelivery.RunQueued, RequestedBy: 42, CreatedAt: createdAt,
				}))

			saved, created, err := NewFeatureDeliveryStore(db).CreateImplementation(context.Background(), run)
			if test.wantConflict {
				if !errors.Is(err, featuredelivery.ErrConflict) || saved != nil || created {
					t.Fatalf("saved=%+v created=%t err=%v", saved, created, err)
				}
			} else if err != nil || created || saved == nil || saved.ID != "run-existing" {
				t.Fatalf("saved=%+v created=%t err=%v", saved, created, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMutableFeatureWritesRejectArchivedRequests(t *testing.T) {
	tests := []struct {
		name string
		run  func(*FeatureDeliveryStore) error
	}{
		{
			name: "artifact",
			run: func(store *FeatureDeliveryStore) error {
				_, err := store.CreateArtifact(context.Background(), featuredelivery.Artifact{
					ID: "artifact-1", RequestID: "feat-1", Kind: featuredelivery.KindRequirement,
				})
				return err
			},
		},
		{
			name: "generation",
			run: func(store *FeatureDeliveryStore) error {
				return store.CreateGenerationRun(context.Background(), featuredelivery.GenerationRun{
					ID: "gen-1", RequestID: "feat-1", Status: "running",
				})
			},
		},
		{
			name: "implementation",
			run: func(store *FeatureDeliveryStore) error {
				_, _, err := store.CreateImplementation(context.Background(), featuredelivery.ImplementationRun{
					ID: "run-1", RequestID: "feat-1", Status: featuredelivery.RunQueued,
				})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			archivedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT created_by,archived_at FROM feature_requests WHERE id=\? LIMIT 1 FOR UPDATE`).
				WithArgs("feat-1").
				WillReturnRows(sqlmock.NewRows([]string{"created_by", "archived_at"}).AddRow(int64(7), archivedAt))
			mock.ExpectRollback()

			if err := test.run(NewFeatureDeliveryStore(db)); !errors.Is(err, featuredelivery.ErrConflict) {
				t.Fatalf("archived write error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListImplementationsUsesSummaryColumnsAndStableCursor(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 7, 29, 12, 45, 0, 0, time.UTC)
	query := implementationSummarySelect + ` WHERE r.request_id=? AND (r.created_at < ? OR (r.created_at = ? AND r.id < ?)) ORDER BY r.created_at DESC,r.id DESC LIMIT ?`
	mock.ExpectQuery(query).
		WithArgs("feat-1", createdAt, createdAt, "run-3", 20).
		WillReturnRows(implementationSummaryRows(featuredelivery.ImplementationRun{
			ID: "run-2", RequestID: "feat-1", ClientRequestID: "client-2", RequestHash: "hash",
			DesignArtifactID: "design-1", PlanArtifactID: "plan-1", Repo: "team/service",
			BaseRef: "main", BaseCommit: "abc", WorkspaceUserID: 7, WorkspaceUsername: "developer",
			Provider: "codex", Status: featuredelivery.RunSucceeded, RequestedBy: 42,
			CreatedAt: createdAt.Add(-time.Minute),
			Review: &featuredelivery.ChangeReview{
				RunID: "run-2", Decision: featuredelivery.DecisionRejected, Comment: "revise", Reviewer: 9, CreatedAt: createdAt,
			},
		}))

	runs, err := NewFeatureDeliveryStore(db).ListImplementations(
		context.Background(), "feat-1", featuredelivery.RunCursor{CreatedAt: createdAt, ID: "run-3"}, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "run-2" || runs[0].ChangeSet != nil ||
		runs[0].Review == nil || runs[0].Review.Decision != featuredelivery.DecisionRejected {
		t.Fatalf("implementation summaries = %+v", runs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func implementationRunRows(run featuredelivery.ImplementationRun) *sqlmock.Rows {
	columns := []string{
		"id", "request_id", "client_request_id", "request_hash", "design_artifact_id", "plan_artifact_id",
		"parent_run_id", "repo", "base_ref", "base_commit", "workspace_user_id", "workspace_username",
		"provider", "model", "provider_version", "network_enabled", "status", "worker_id", "lease_expires_at",
		"cancel_requested_at", "provider_session_id", "exit_code", "error_summary", "requested_by",
		"started_at", "ended_at", "retain_until", "worktree_cleaned_at", "cleanup_error", "created_at",
		"change_run_id", "worktree_head", "patch_rel_path", "patch_sha256", "patch_bytes", "files_changed",
		"additions", "deletions", "files_json", "plan_deviations_json", "validation_results_json", "provider_summary", "change_created_at",
		"review_run_id", "decision", "comment", "reviewer", "review_created_at",
	}
	return sqlmock.NewRows(columns).AddRow(
		run.ID, run.RequestID, run.ClientRequestID, run.RequestHash, run.DesignArtifactID, run.PlanArtifactID,
		run.ParentRunID, run.Repo, run.BaseRef, run.BaseCommit, run.WorkspaceUserID, run.WorkspaceUsername,
		run.Provider, run.Model, run.ProviderVersion, run.NetworkEnabled, run.Status, run.WorkerID, nil,
		nil, run.ProviderSessionID, nil, run.ErrorSummary, run.RequestedBy,
		nil, nil, nil, nil, run.CleanupError, run.CreatedAt,
		nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil,
	)
}

func implementationSummaryRows(run featuredelivery.ImplementationRun) *sqlmock.Rows {
	columns := []string{
		"id", "request_id", "client_request_id", "request_hash", "design_artifact_id", "plan_artifact_id",
		"parent_run_id", "repo", "base_ref", "base_commit", "workspace_user_id", "workspace_username",
		"provider", "model", "provider_version", "network_enabled", "status", "worker_id", "lease_expires_at",
		"cancel_requested_at", "provider_session_id", "exit_code", "error_summary", "requested_by",
		"started_at", "ended_at", "retain_until", "worktree_cleaned_at", "cleanup_error", "created_at",
		"review_run_id", "decision", "comment", "reviewer", "review_created_at",
	}
	var reviewRunID, decision, comment any
	var reviewer, reviewCreated any
	if run.Review != nil {
		reviewRunID = run.Review.RunID
		decision = run.Review.Decision
		comment = run.Review.Comment
		reviewer = run.Review.Reviewer
		reviewCreated = run.Review.CreatedAt
	}
	return sqlmock.NewRows(columns).AddRow(
		run.ID, run.RequestID, run.ClientRequestID, run.RequestHash, run.DesignArtifactID, run.PlanArtifactID,
		run.ParentRunID, run.Repo, run.BaseRef, run.BaseCommit, run.WorkspaceUserID, run.WorkspaceUsername,
		run.Provider, run.Model, run.ProviderVersion, run.NetworkEnabled, run.Status, run.WorkerID, nil,
		nil, run.ProviderSessionID, nil, run.ErrorSummary, run.RequestedBy,
		nil, nil, nil, nil, run.CleanupError, run.CreatedAt,
		reviewRunID, decision, comment, reviewer, reviewCreated,
	)
}

func TestSaveChangeSetAndFinishCommitsChangeAndTerminalStateTogether(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	retainUntil := createdAt.Add(72 * time.Hour)
	change := featuredelivery.ChangeSet{
		RunID: "run-1", WorktreeHead: "abc", PatchRelPath: "run-1/changes.patch",
		PatchSHA256: "hash", PatchBytes: 42, FilesChanged: 1, Additions: 2, Deletions: 1,
		Files:             []featuredelivery.ChangedFile{{Path: "main.go", Status: "M", Additions: 2, Deletions: 1}},
		PlanDeviations:    []featuredelivery.PlanDeviation{},
		ValidationResults: []featuredelivery.ValidationResult{{Sequence: 1, Status: "passed"}},
		ProviderSummary:   "implemented", CreatedAt: createdAt,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM feature_implementation_runs WHERE id=\? LIMIT 1 FOR UPDATE`).
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-1"))
	mock.ExpectQuery(`SELECT status FROM feature_implementation_runs WHERE id=\? LIMIT 1`).
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(featuredelivery.RunValidating))
	mock.ExpectExec(`INSERT INTO feature_change_sets`).
		WithArgs(
			"run-1", "abc", "run-1/changes.patch", "hash", int64(42), 1, 2, 1,
			[]byte(`[{"path":"main.go","status":"M","additions":2,"deletions":1,"binary":false}]`),
			[]byte(`[]`),
			[]byte(`[{"sequence":1,"argv":null,"status":"passed","exit_code":0,"duration_ms":0,"output_summary":"","output_bytes":0,"timed_out":false}]`),
			"implemented", createdAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)UPDATE feature_implementation_runs.*status=\?.*WHERE id=\? AND status='validating'`).
		WithArgs(featuredelivery.RunSucceeded, "", retainUntil, "run-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = NewFeatureDeliveryStore(db).SaveChangeSetAndFinish(
		context.Background(), change, featuredelivery.RunSucceeded, "", retainUntil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestCancelDistinguishesQueuedActiveAndTerminalRuns(t *testing.T) {
	tests := []struct {
		name       string
		status     featuredelivery.RunStatus
		wantStatus featuredelivery.RunStatus
		wantErr    error
		update     string
	}{
		{name: "queued becomes terminal", status: featuredelivery.RunQueued, wantStatus: featuredelivery.RunCancelled, update: `status='cancelled'`},
		{name: "active records intent", status: featuredelivery.RunRunning, wantStatus: featuredelivery.RunRunning, update: `cancel_requested_at=COALESCE`},
		{name: "terminal conflicts", status: featuredelivery.RunSucceeded, wantStatus: featuredelivery.RunSucceeded, wantErr: featuredelivery.ErrConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT status FROM feature_implementation_runs WHERE id=\? LIMIT 1 FOR UPDATE`).
				WithArgs("run-1").
				WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(test.status))
			if test.update != "" {
				mock.ExpectExec(test.update).WithArgs("run-1").WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit()
			} else {
				mock.ExpectRollback()
			}

			status, err := NewFeatureDeliveryStore(db).RequestCancel(context.Background(), "run-1")
			if !errors.Is(err, test.wantErr) || status != test.wantStatus {
				t.Fatalf("status=%s err=%v", status, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListRunEventsUsesCursorAndBoundedLimit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	query := `SELECT run_id,seq,kind,summary,detail_json,created_at
		 FROM feature_run_events WHERE run_id=? AND seq>? ORDER BY seq LIMIT ?`
	mock.ExpectQuery(query).
		WithArgs("run-1", int64(41), 500).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "seq", "kind", "summary", "detail_json", "created_at"}).
			AddRow("run-1", int64(42), featuredelivery.EventProviderMessage, "progress", []byte(`{"step":1}`), createdAt))

	events, err := NewFeatureDeliveryStore(db).ListRunEvents(context.Background(), "run-1", 41, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 42 || string(events[0].Detail) != `{"step":1}` {
		t.Fatalf("events = %+v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
