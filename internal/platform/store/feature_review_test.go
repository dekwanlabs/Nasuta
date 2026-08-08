package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

func TestGetReviewPolicyUsesBoundedDefinitionProjection(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	policy := storedReviewPolicy(t)
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT definition_json FROM review_policies WHERE id=\? AND version=\? LIMIT 1`).
		WithArgs(policy.ID, policy.Version).
		WillReturnRows(sqlmock.NewRows([]string{"definition_json"}).AddRow(raw))

	store := NewFeatureDeliveryStore(db)
	got, err := store.GetReviewPolicy(context.Background(), policy.ID, policy.Version)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != policy.ContentHash || got.CreatedAt != policy.CreatedAt {
		t.Fatalf("policy = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetReviewPolicyRolloutReturnsStoredRule(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT
		subject_id,rule_version,candidate_id,candidate_version,
		percentage_bps,salt,rule_hash,active,created_by,created_at
		FROM catalog_rollouts
		WHERE catalog_kind='review_policy' AND subject_id=? LIMIT 1`).
		WithArgs(delivery.SubjectSystemDesign).
		WillReturnRows(sqlmock.NewRows([]string{
			"subject_kind", "rule_version", "candidate_policy_id",
			"candidate_policy_version", "percentage_bps", "salt", "rule_hash",
			"active", "created_by", "created_at",
		}).AddRow(
			delivery.SubjectSystemDesign, int64(3), "candidate-policy",
			int64(2), 2500, "rollout-2026-08", "rule-hash", true, int64(7), createdAt,
		))

	rule, found, err := NewFeatureDeliveryStore(db).GetReviewPolicyRollout(
		context.Background(), delivery.SubjectSystemDesign,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !found || rule.RuleVersion != 3 || rule.CandidatePolicyID != "candidate-policy" ||
		rule.CandidatePolicyVersion != 2 || rule.PercentageBPS != 2500 ||
		rule.Salt != "rollout-2026-08" || rule.RuleHash != "rule-hash" ||
		!rule.Active || rule.CreatedBy != 7 || !rule.CreatedAt.Equal(createdAt) {
		t.Fatalf("rule = %+v, found = %t", rule, found)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetReviewPolicyRolloutInsertsInitialRuleAndAudit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rule := storedReviewPolicyRolloutRule(1)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT rule_version
		FROM catalog_rollouts
		WHERE catalog_kind='review_policy' AND subject_id=? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.SubjectKind).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}))
	mock.ExpectExec(`INSERT INTO catalog_rollouts(
			catalog_kind,subject_id,rule_version,candidate_id,candidate_version,
			percentage_bps,salt,rule_hash,active,created_by,created_at)
			VALUES('review_policy',?,?,?,?,?,?,?,?,?,?)`).
		WithArgs(
			rule.SubjectKind, rule.RuleVersion, rule.CandidatePolicyID,
			rule.CandidatePolicyVersion, rule.PercentageBPS, rule.Salt,
			rule.RuleHash, rule.Active, int64(7), rule.CreatedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO catalog_audit(
		catalog_kind,event_kind,subject_id,version,candidate_id,candidate_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at)
		VALUES('review_policy','rollout',?,?,?,?,?,?,?,?,?)`).
		WithArgs(
			rule.SubjectKind, rule.RuleVersion, rule.CandidatePolicyID,
			rule.CandidatePolicyVersion, rule.PercentageBPS, rule.RuleHash,
			"rollout_enabled", int64(7), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := NewFeatureDeliveryStore(db).SetReviewPolicyRollout(
		context.Background(), rule, 7,
	); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetReviewPolicyRolloutUpdatesNextRuleVersion(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rule := storedReviewPolicyRolloutRule(4)
	rule.Active = false
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT rule_version
		FROM catalog_rollouts
		WHERE catalog_kind='review_policy' AND subject_id=? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.SubjectKind).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(3)))
	mock.ExpectExec(`UPDATE catalog_rollouts SET
			rule_version=?,candidate_id=?,candidate_version=?,
			percentage_bps=?,salt=?,rule_hash=?,active=?,created_by=?,created_at=?
			WHERE catalog_kind='review_policy' AND subject_id=?`).
		WithArgs(
			rule.RuleVersion, rule.CandidatePolicyID, rule.CandidatePolicyVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(8), rule.CreatedAt, rule.SubjectKind,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO catalog_audit(
		catalog_kind,event_kind,subject_id,version,candidate_id,candidate_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at)
		VALUES('review_policy','rollout',?,?,?,?,?,?,?,?,?)`).
		WithArgs(
			rule.SubjectKind, rule.RuleVersion, rule.CandidatePolicyID,
			rule.CandidatePolicyVersion, rule.PercentageBPS, rule.RuleHash,
			"rollout_disabled", int64(8), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := NewFeatureDeliveryStore(db).SetReviewPolicyRollout(
		context.Background(), rule, 8,
	); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSetReviewPolicyRolloutRejectsSkippedRuleVersion(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rule := storedReviewPolicyRolloutRule(5)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT rule_version
		FROM catalog_rollouts
		WHERE catalog_kind='review_policy' AND subject_id=? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.SubjectKind).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(3)))
	mock.ExpectRollback()

	err = NewFeatureDeliveryStore(db).SetReviewPolicyRollout(
		context.Background(), rule, 8,
	)
	if !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListReviewPolicyRolloutAuditUsesStableSequenceAndLimit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT
		seq,subject_id,version,candidate_id,candidate_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at
		FROM catalog_audit
		WHERE catalog_kind='review_policy' AND event_kind='rollout'
			AND subject_id=? AND seq>?
		ORDER BY seq LIMIT ?`).
		WithArgs(delivery.SubjectSystemDesign, int64(11), 2).
		WillReturnRows(sqlmock.NewRows([]string{
			"seq", "subject_kind", "rule_version", "candidate_policy_id",
			"candidate_policy_version", "percentage_bps", "rule_hash", "action",
			"actor_user_id", "created_at",
		}).AddRow(
			int64(12), delivery.SubjectSystemDesign, int64(3),
			"candidate-policy", int64(2), 2500, "rule-hash",
			"rollout_enabled", int64(7), createdAt,
		))

	events, err := NewFeatureDeliveryStore(db).ListReviewPolicyRolloutAudit(
		context.Background(), delivery.SubjectSystemDesign, 11, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 12 ||
		events[0].SubjectKind != delivery.SubjectSystemDesign ||
		events[0].RuleVersion != 3 || events[0].Action != "rollout_enabled" {
		t.Fatalf("events = %+v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateReviewRoundPersistsPolicySelectionSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	round, assignments := storedReviewRoundWithSelection()
	subjectJSON, err := json.Marshal(round.Subject)
	if err != nil {
		t.Fatal(err)
	}
	selectionJSON, err := json.Marshal(round.PolicySelection)
	if err != nil {
		t.Fatal(err)
	}
	riskFactsJSON, err := json.Marshal(round.RiskFacts)
	if err != nil {
		t.Fatal(err)
	}
	reviewersJSON, err := json.Marshal(round.Reviewers)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT content_hash FROM review_policies`).
		WithArgs(round.PolicyID, round.PolicyVersion).
		WillReturnRows(sqlmock.NewRows([]string{"content_hash"}).AddRow(round.PolicyHash))
	mock.ExpectExec(`INSERT INTO review_rounds`).
		WithArgs(
			round.ID, round.WorkflowRunID, round.Subject.Kind, round.Subject.ID,
			round.Subject.Version, round.Subject.ContentHash, subjectJSON,
			round.PolicyID, round.PolicyVersion, round.PolicyHash, selectionJSON,
			riskFactsJSON, round.RiskHash, round.RuleVersion, reviewersJSON,
			round.PanelHash, round.Status, round.CreatedBy, round.CreatedAt,
			round.CompletedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	for _, assignment := range assignments {
		categoriesJSON, err := json.Marshal(assignment.Categories)
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectExec(`INSERT INTO review_assignments`).
			WithArgs(
				assignment.ID, assignment.RoundID, assignment.ReviewerID,
				assignment.Agent.ID, assignment.Agent.Version,
				assignment.DefinitionHash, categoriesJSON, assignment.Required,
				assignment.Status, assignment.Attempt, assignment.AgentRunID,
				assignment.ErrorCode, assignment.CreatedAt, assignment.StartedAt,
				assignment.CompletedAt,
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()

	if err := NewFeatureDeliveryStore(db).CreateReviewRound(
		context.Background(), round, assignments,
	); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateReviewRoundWithReusesStoresAuditOnReport(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	round, assignments := storedReviewRoundWithSelection()
	completedAt := round.CreatedAt.Add(time.Minute)
	assignments[0].Status = delivery.AssignmentReused
	assignments[0].CompletedAt = &completedAt
	reason := "The immutable review inputs are unchanged."
	report := delivery.ReviewReport{
		ID: "report-reused", RoundID: round.ID, AssignmentID: assignments[0].ID,
		ReviewerID: assignments[0].ReviewerID, SubjectHash: round.Subject.ContentHash,
		Summary: "Reused review.", ReportHash: "report-hash", ContentHash: "content-hash",
		Reuse: &delivery.ReviewReportReuseRef{
			SourceReportID: "source-report", SourceRoundID: "source-round",
			SourceAssignmentID: "source-assignment", Reason: reason,
		},
		CompletedAt: completedAt,
	}
	reuse := delivery.ReviewReportReuse{
		ID: "reuse-1", RoundID: round.ID, AssignmentID: assignments[0].ID,
		ReportID: report.ID, ReviewerID: report.ReviewerID,
		SourceRoundID: "source-round", SourceAssignmentID: "source-assignment",
		SourceReportID: "source-report", SubjectHash: round.Subject.ContentHash,
		PolicyHash: round.PolicyHash, DefinitionHash: assignments[0].DefinitionHash,
		ReportHash: report.ReportHash, Reason: reason, ActorID: 7, CreatedAt: completedAt,
	}
	subjectJSON, err := json.Marshal(round.Subject)
	if err != nil {
		t.Fatal(err)
	}
	selectionJSON, err := json.Marshal(round.PolicySelection)
	if err != nil {
		t.Fatal(err)
	}
	riskFactsJSON, err := json.Marshal(round.RiskFacts)
	if err != nil {
		t.Fatal(err)
	}
	reviewersJSON, err := json.Marshal(round.Reviewers)
	if err != nil {
		t.Fatal(err)
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT content_hash FROM review_policies`).
		WithArgs(round.PolicyID, round.PolicyVersion).
		WillReturnRows(sqlmock.NewRows([]string{"content_hash"}).AddRow(round.PolicyHash))
	mock.ExpectExec(`INSERT INTO review_rounds`).
		WithArgs(
			round.ID, round.WorkflowRunID, round.Subject.Kind, round.Subject.ID,
			round.Subject.Version, round.Subject.ContentHash, subjectJSON,
			round.PolicyID, round.PolicyVersion, round.PolicyHash, selectionJSON,
			riskFactsJSON, round.RiskHash, round.RuleVersion, reviewersJSON,
			round.PanelHash, round.Status, round.CreatedBy, round.CreatedAt,
			round.CompletedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	for _, assignment := range assignments {
		categoriesJSON, err := json.Marshal(assignment.Categories)
		if err != nil {
			t.Fatal(err)
		}
		mock.ExpectExec(`INSERT INTO review_assignments`).
			WithArgs(
				assignment.ID, assignment.RoundID, assignment.ReviewerID,
				assignment.Agent.ID, assignment.Agent.Version,
				assignment.DefinitionHash, categoriesJSON, assignment.Required,
				assignment.Status, assignment.Attempt, assignment.AgentRunID,
				assignment.ErrorCode, assignment.CreatedAt, assignment.StartedAt,
				assignment.CompletedAt,
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectExec(`INSERT INTO review_reports`).
		WithArgs(
			report.ID, report.RoundID, report.AssignmentID, report.ReviewerID,
			report.SubjectHash, reportJSON, report.ReportHash,
			report.ContentHash, report.CompletedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)UPDATE review_reports.*SET reuse_id=\?.*reuse_created_at=\?.*WHERE id=\?.*reuse_id IS NULL`).
		WithArgs(
			reuse.ID, reuse.SourceRoundID, reuse.SourceAssignmentID,
			reuse.SourceReportID, reuse.PolicyHash, reuse.DefinitionHash,
			reuse.Reason, reuse.ActorID, reuse.CreatedAt,
			reuse.ReportID, reuse.RoundID, reuse.AssignmentID, reuse.ReviewerID,
			reuse.SubjectHash, reuse.ReportHash,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := NewFeatureDeliveryStore(db).CreateReviewRoundWithReuses(
		context.Background(), round, assignments,
		[]delivery.ReviewReport{report},
		[]delivery.ReviewReportReuse{reuse},
	); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetReviewRoundRestoresPolicySelectionSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	round, _ := storedReviewRoundWithSelection()
	subjectJSON, err := json.Marshal(round.Subject)
	if err != nil {
		t.Fatal(err)
	}
	selectionJSON, err := json.Marshal(round.PolicySelection)
	if err != nil {
		t.Fatal(err)
	}
	riskFactsJSON, err := json.Marshal(round.RiskFacts)
	if err != nil {
		t.Fatal(err)
	}
	reviewersJSON, err := json.Marshal(round.Reviewers)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT id,workflow_run_id,subject_json,policy_id,policy_version,policy_hash`).
		WithArgs(round.ID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workflow_run_id", "subject_json", "policy_id", "policy_version",
			"policy_hash", "policy_selection_json", "risk_facts_json", "risk_hash",
			"selection_rule_version", "selected_reviewers_json", "panel_hash",
			"status", "created_by", "created_at", "completed_at",
		}).AddRow(
			round.ID, round.WorkflowRunID, subjectJSON, round.PolicyID,
			round.PolicyVersion, round.PolicyHash, selectionJSON, riskFactsJSON,
			round.RiskHash, round.RuleVersion, reviewersJSON, round.PanelHash,
			round.Status, round.CreatedBy, round.CreatedAt, nil,
		))

	got, err := NewFeatureDeliveryStore(db).GetReviewRound(
		context.Background(), round.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicySelection != round.PolicySelection ||
		got.PolicyID != round.PolicyID || got.PolicyVersion != round.PolicyVersion ||
		got.Subject != round.Subject {
		t.Fatalf("round = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListReviewRoundSummariesFiltersByFeatureOwnershipAndBoundsPage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cursorAt := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	columns := []string{
		"id", "workflow_run_id", "feature_id", "subject_kind", "subject_id",
		"subject_version", "subject_hash", "policy_id", "policy_version",
		"policy_hash", "risk_hash", "selection_rule_version", "panel_hash",
		"reviewer_count", "status", "created_by", "created_at", "completed_at",
	}
	rows := sqlmock.NewRows(columns)
	for index := 0; index < 3; index++ {
		rows.AddRow(
			"round-"+string(rune('a'+index)),
			"workflow-"+string(rune('a'+index)),
			"feature-1",
			delivery.SubjectSystemDesign,
			"artifact-"+string(rune('a'+index)),
			1,
			"subject-hash",
			"system-design-review",
			int64(2),
			"policy-hash",
			"risk-hash",
			"risk-v1",
			"panel-hash",
			2,
			delivery.RoundCompleted,
			int64(7),
			cursorAt.Add(-time.Duration(index+1)*time.Minute),
			nil,
		)
	}
	mock.ExpectQuery(
		`(?s)SELECT r.id,r.workflow_run_id,f.id.*`+
			`LEFT JOIN feature_artifacts a.*`+
			`LEFT JOIN feature_implementation_runs i.*`+
			`JOIN feature_requests f.*`+
			`f.created_by=\?.*f.id=\?.*r.subject_kind=\?.*r.status=\?.*`+
			`r.created_at<\?.*r.created_at=\?.*r.id<\?.*`+
			`ORDER BY r.created_at DESC,r.id DESC LIMIT \?`,
	).WithArgs(
		int64(7),
		"feature-1",
		delivery.SubjectSystemDesign,
		delivery.RoundCompleted,
		cursorAt,
		cursorAt,
		"round-before",
		3,
	).WillReturnRows(rows)

	items, hasMore, err := NewFeatureDeliveryStore(db).ListReviewRoundSummaries(
		context.Background(),
		delivery.ReviewRoundFilter{
			FeatureID: "feature-1", SubjectKind: delivery.SubjectSystemDesign,
			Status: delivery.RoundCompleted,
		},
		delivery.ReviewRoundCursor{CreatedAt: cursorAt, ID: "round-before"},
		2,
		7,
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore || len(items) != 2 ||
		items[0].FeatureID != "feature-1" ||
		items[1].ID != "round-b" {
		t.Fatalf("items = %+v, has_more = %t", items, hasMore)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveReviewPoliciesIsIdempotentOnlyForMatchingContent(t *testing.T) {
	policy := storedReviewPolicy(t)
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		existingHash string
		wantConflict bool
	}{
		{name: "matching content", existingHash: policy.ContentHash},
		{name: "changed content", existingHash: "different-hash", wantConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectExec(`INSERT INTO review_policies`).
				WithArgs(
					policy.ID, policy.Version, policy.SubjectKind, raw,
					policy.ContentHash, policy.CreatedAt,
				).
				WillReturnError(&mysqlDriver.MySQLError{Number: 1062, Message: "duplicate"})
			mock.ExpectQuery(`SELECT content_hash FROM review_policies WHERE id=\? AND version=\? LIMIT 1`).
				WithArgs(policy.ID, policy.Version).
				WillReturnRows(sqlmock.NewRows([]string{"content_hash"}).AddRow(test.existingHash))
			if test.wantConflict {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}

			err = NewFeatureDeliveryStore(db).SaveReviewPolicies(
				context.Background(), []delivery.ReviewPolicy{policy},
			)
			if test.wantConflict != errors.Is(err, delivery.ErrConflict) {
				t.Fatalf("error = %v, want conflict=%t", err, test.wantConflict)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSaveReviewPoliciesRollsBackWholeBatchOnConflict(t *testing.T) {
	first := storedReviewPolicy(t)
	second := first
	second.ID = "second-system-design-review"
	second.ContentHash = ""
	var err error
	second, err = delivery.PrepareReviewPolicy(second)
	if err != nil {
		t.Fatal(err)
	}
	firstRaw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO review_policies`).
		WithArgs(
			first.ID, first.Version, first.SubjectKind, firstRaw,
			first.ContentHash, first.CreatedAt,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO review_policies`).
		WithArgs(
			second.ID, second.Version, second.SubjectKind, secondRaw,
			second.ContentHash, second.CreatedAt,
		).
		WillReturnError(&mysqlDriver.MySQLError{Number: 1062, Message: "duplicate"})
	mock.ExpectQuery(`SELECT content_hash FROM review_policies WHERE id=\? AND version=\? LIMIT 1`).
		WithArgs(second.ID, second.Version).
		WillReturnRows(sqlmock.NewRows([]string{"content_hash"}).AddRow("different-hash"))
	mock.ExpectRollback()

	err = NewFeatureDeliveryStore(db).SaveReviewPolicies(
		context.Background(), []delivery.ReviewPolicy{first, second},
	)
	if !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveReviewAdjudicationsIsIdempotentOnlyForMatchingContent(t *testing.T) {
	adjudication := storedReviewAdjudication(t)
	raw, err := json.Marshal(adjudication)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name         string
		existingHash string
		wantConflict bool
	}{
		{name: "matching content", existingHash: adjudication.ContentHash},
		{name: "changed content", existingHash: "different-hash", wantConflict: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectExec(`INSERT INTO review_adjudications`).
				WithArgs(
					adjudication.ID, adjudication.RoundID, adjudication.SubjectHash,
					adjudication.PolicyHash, adjudication.Fingerprint,
					adjudication.Agent.ID, adjudication.Agent.Version,
					adjudication.DefinitionHash, adjudication.Decision,
					adjudication.ErrorCode, raw, adjudication.ContentHash,
					adjudication.CreatedAt, adjudication.RoundID,
					adjudication.SubjectHash, adjudication.PolicyHash,
				).
				WillReturnError(&mysqlDriver.MySQLError{Number: 1062, Message: "duplicate"})
			mock.ExpectQuery(`SELECT content_hash FROM review_adjudications`).
				WithArgs(adjudication.RoundID, adjudication.Fingerprint).
				WillReturnRows(sqlmock.NewRows([]string{"content_hash"}).
					AddRow(test.existingHash))
			if test.wantConflict {
				mock.ExpectRollback()
			} else {
				mock.ExpectCommit()
			}

			err = NewFeatureDeliveryStore(db).SaveReviewAdjudications(
				context.Background(),
				[]delivery.ReviewAdjudication{adjudication},
			)
			if test.wantConflict != errors.Is(err, delivery.ErrConflict) {
				t.Fatalf("error = %v, want conflict=%t", err, test.wantConflict)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestListReviewAdjudicationsUsesBoundedLimitAndRevalidatesArtifact(t *testing.T) {
	adjudication := storedReviewAdjudication(t)
	raw, err := json.Marshal(adjudication)
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cursor := delivery.ReviewAdjudicationCursor{
		Fingerprint: "fingerprint-0",
		ID:          "adjudication-0",
	}
	query := `SELECT adjudication_json FROM review_adjudications
		 WHERE round_id=? AND (fingerprint>? OR (fingerprint=? AND id>?)) ORDER BY fingerprint,id LIMIT ?`
	mock.ExpectQuery(query).
		WithArgs(
			adjudication.RoundID,
			cursor.Fingerprint,
			cursor.Fingerprint,
			cursor.ID,
			1600,
		).
		WillReturnRows(sqlmock.NewRows([]string{"adjudication_json"}).AddRow(raw))

	items, err := NewFeatureDeliveryStore(db).ListReviewAdjudications(
		context.Background(),
		adjudication.RoundID,
		cursor,
		9999,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ContentHash != adjudication.ContentHash ||
		items[0].ID != adjudication.ID {
		t.Fatalf("adjudications = %+v", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListReviewAdjudicationsRejectsInvalidStoredArtifact(t *testing.T) {
	adjudication := storedReviewAdjudication(t)
	adjudication.Rationale = "Tampered after hashing."
	raw, err := json.Marshal(adjudication)
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT adjudication_json FROM review_adjudications`).
		WithArgs(adjudication.RoundID, 25).
		WillReturnRows(sqlmock.NewRows([]string{"adjudication_json"}).AddRow(raw))

	_, err = NewFeatureDeliveryStore(db).ListReviewAdjudications(
		context.Background(),
		adjudication.RoundID,
		delivery.ReviewAdjudicationCursor{},
		25,
	)
	if !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListReviewFindingsBoundsQueryAndProjectsSummary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT id,report_id,round_id,category,severity,claim,impact,recommendation,.*FROM review_findings WHERE round_id=\? AND severity=\? AND id>\? ORDER BY id LIMIT \?`).
		WithArgs("round-1", delivery.SeverityHigh, "finding-0", 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "report_id", "round_id", "category", "severity", "claim", "impact",
			"recommendation", "confidence", "fingerprint", "location_json", "content_hash", "created_at",
		}).AddRow(
			"finding-1", "report-1", "round-1", "security", "high", "unsafe input",
			"privilege escalation", "validate at ingress", 0.9, "fingerprint",
			[]byte(`{"path":"handler.go","start_line":42}`), "content-hash", now,
		))

	store := NewFeatureDeliveryStore(db)
	items, err := store.ListReviewFindings(
		context.Background(), "round-1", delivery.SeverityHigh,
		delivery.FindingCursor{ID: "finding-0"}, 500,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Location == nil ||
		items[0].Location.Path != "handler.go" || items[0].Location.StartLine != 42 {
		t.Fatalf("findings = %+v", items)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetReviewFindingReadsEmbeddedEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT id,report_id,round_id,category,severity,claim,impact,recommendation,.*evidence_json.*FROM review_findings WHERE id=\? LIMIT 1`).
		WithArgs("finding-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "report_id", "round_id", "category", "severity", "claim", "impact",
			"recommendation", "confidence", "fingerprint", "location_json", "evidence_json",
			"content_hash", "created_at",
		}).AddRow(
			"finding-1", "report-1", "round-1", "security", "high", "unsafe input",
			"privilege escalation", "validate at ingress", 0.9, "fingerprint",
			[]byte(`{"path":"handler.go","start_line":42}`),
			[]byte(`[{"kind":"code","ref":"handler.go:42","hash":"source-hash","summary":"unvalidated input"}]`),
			"content-hash", now,
		))

	finding, err := NewFeatureDeliveryStore(db).GetReviewFinding(context.Background(), "finding-1")
	if err != nil {
		t.Fatal(err)
	}
	if finding.Location == nil || finding.Location.Path != "handler.go" ||
		len(finding.Evidence) != 1 || finding.Evidence[0].Ref != "handler.go:42" {
		t.Fatalf("finding = %+v", finding)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetReviewReportByAssignmentUsesBoundedLookupAndChecksIdentity(t *testing.T) {
	report := delivery.ReviewReport{
		ID: "report-1", RoundID: "round-1", AssignmentID: "assignment-1",
		ReviewerID: "architecture", SubjectHash: "subject-hash",
		Summary: "No blocking findings.", ReportHash: "semantic-hash",
		ContentHash: "report-hash",
		CompletedAt: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	query := `SELECT id,round_id,assignment_id,reviewer_id,subject_hash,
		        report_json,report_hash,content_hash
		 FROM review_reports WHERE round_id=? AND assignment_id=? LIMIT 1`
	mock.ExpectQuery(query).
		WithArgs(report.RoundID, report.AssignmentID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "round_id", "assignment_id", "reviewer_id",
			"subject_hash", "report_json", "report_hash", "content_hash",
		}).AddRow(
			report.ID, report.RoundID, report.AssignmentID, report.ReviewerID,
			report.SubjectHash, raw, report.ReportHash, report.ContentHash,
		))

	got, err := NewFeatureDeliveryStore(db).GetReviewReportByAssignment(
		context.Background(), report.RoundID, report.AssignmentID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != report.ID || got.ContentHash != report.ContentHash {
		t.Fatalf("report = %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetReviewReportByAssignmentRejectsInconsistentStoredIdentity(t *testing.T) {
	report := delivery.ReviewReport{
		ID: "report-1", RoundID: "round-1", AssignmentID: "assignment-1",
		ReviewerID: "architecture", SubjectHash: "subject-hash",
		ReportHash: "semantic-hash", ContentHash: "report-hash",
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT id,round_id,assignment_id,reviewer_id,subject_hash,\s*report_json,report_hash,content_hash`).
		WithArgs(report.RoundID, report.AssignmentID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "round_id", "assignment_id", "reviewer_id",
			"subject_hash", "report_json", "report_hash", "content_hash",
		}).AddRow(
			report.ID, report.RoundID, report.AssignmentID, "security",
			report.SubjectHash, raw, report.ReportHash, report.ContentHash,
		))

	_, err = NewFeatureDeliveryStore(db).GetReviewReportByAssignment(
		context.Background(), report.RoundID, report.AssignmentID,
	)
	if !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetReviewReportReuseSourcesUsesBoundedExactLookup(t *testing.T) {
	assignment := delivery.ReviewAssignment{
		ID: "assignment-1", RoundID: "round-1", ReviewerID: "architecture",
		Agent: agentapi.DefinitionRef{
			ID: "review.architecture", Version: 1,
		},
		DefinitionHash: "definition-hash",
		Status:         delivery.AssignmentRunning,
	}
	report, err := delivery.PrepareReviewReport(delivery.ReviewReport{
		RoundID: assignment.RoundID, AssignmentID: assignment.ID,
		ReviewerID: assignment.ReviewerID, SubjectHash: "subject-hash",
		Coverage: []delivery.CoverageItem{{
			Category: "architecture", Covered: true,
		}},
		Summary:     "The review is complete.",
		CompletedAt: time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
	}, assignment, "subject-hash")
	if err != nil {
		t.Fatal(err)
	}
	assignment.Status = delivery.AssignmentSucceeded
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(
		`SELECT report.id,report.round_id,report.assignment_id,report.reviewer_id,
		        report.subject_hash,report.report_json,report.report_hash,report.content_hash,
		        assignment.agent_id,assignment.agent_version,assignment.definition_hash,
		        assignment.status,round.policy_id,round.policy_version,round.policy_hash
		 FROM review_reports report
		 JOIN review_assignments assignment ON assignment.id=report.assignment_id
		 JOIN review_rounds round ON round.id=report.round_id
		 WHERE report.id IN (?)
		   AND assignment.status IN ('succeeded','reused')
		 ORDER BY report.id LIMIT ?`,
	).WithArgs(report.ID, 1).WillReturnRows(sqlmock.NewRows([]string{
		"id", "round_id", "assignment_id", "reviewer_id",
		"subject_hash", "report_json", "report_hash", "content_hash",
		"agent_id", "agent_version", "definition_hash", "status",
		"policy_id", "policy_version", "policy_hash",
	}).AddRow(
		report.ID, report.RoundID, report.AssignmentID, report.ReviewerID,
		report.SubjectHash, raw, report.ReportHash, report.ContentHash,
		assignment.Agent.ID, assignment.Agent.Version, assignment.DefinitionHash,
		assignment.Status, "policy-1", int64(1), "policy-hash",
	))

	sources, err := NewFeatureDeliveryStore(db).GetReviewReportReuseSources(
		context.Background(),
		[]string{report.ID},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 ||
		sources[0].Report.ID != report.ID ||
		sources[0].Report.ReportHash != report.ReportHash ||
		sources[0].Assignment.DefinitionHash != assignment.DefinitionHash ||
		sources[0].PolicyHash != "policy-hash" {
		t.Fatalf("sources = %+v", sources)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListReviewEventsUsesSequenceCursorAndBoundedLimit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	query := `SELECT stream_id,seq,kind,summary,detail_json,created_at
		 FROM runtime_events
		 WHERE stream_kind='review_round' AND stream_id=? AND seq>?
		 ORDER BY seq LIMIT ?`
	mock.ExpectQuery(query).
		WithArgs("round-1", int64(7), 500).
		WillReturnRows(sqlmock.NewRows([]string{
			"round_id", "seq", "kind", "summary", "detail_json", "created_at",
		}).AddRow(
			"round-1", int64(8), delivery.ReviewEventAssignmentSucceeded,
			"review assignment succeeded", []byte(`{"assignment_id":"assignment-1"}`), now,
		))

	events, err := NewFeatureDeliveryStore(db).ListReviewEvents(
		context.Background(), "round-1", 7, 999,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Seq != 8 ||
		string(events[0].Detail) != `{"assignment_id":"assignment-1"}` {
		t.Fatalf("events = %+v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendReviewEventAllocatesSequenceUnderRoundLock(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	event := delivery.ReviewEvent{
		RoundID: "round-1", Kind: delivery.ReviewEventAssignmentStarted,
		Summary:   "review assignment started",
		Detail:    json.RawMessage(`{"assignment_id":"assignment-1"}`),
		CreatedAt: now,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id,status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`).
		WithArgs(event.RoundID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).
			AddRow(event.RoundID, delivery.RoundRunning))
	mock.ExpectQuery(`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='review_round' AND stream_id=?`).
		WithArgs(event.RoundID).
		WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(int64(4)))
	mock.ExpectExec(`INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
		 VALUES('review_round',?,?,?,?,?,?,?)`).
		WithArgs(event.RoundID, int64(4), event.Kind, "", event.Summary, []byte(event.Detail), event.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	persisted, err := NewFeatureDeliveryStore(db).AppendReviewEvent(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Seq != 4 {
		t.Fatalf("sequence = %d, want 4", persisted.Seq)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendReviewEventRejectsLifecycleMismatch(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id,status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`).
		WithArgs("round-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "status"}).
			AddRow("round-1", delivery.RoundCancelled))
	mock.ExpectRollback()

	_, err = NewFeatureDeliveryStore(db).AppendReviewEvent(
		context.Background(),
		delivery.ReviewEvent{
			RoundID: "round-1",
			Kind:    delivery.ReviewEventAssignmentSucceeded,
		},
	)
	if !errors.Is(err, delivery.ErrConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestReviewRoundCancelIsAtomicAndIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	for _, status := range []delivery.ReviewRoundStatus{
		delivery.RoundCreated,
		delivery.RoundRunning,
		delivery.RoundEvaluating,
	} {
		t.Run(string(status), func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`).
				WithArgs("round-1").
				WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(status))
			mock.ExpectExec(`UPDATE review_assignments
		 SET status='cancelled',error_code='review_cancelled',completed_at=?
		 WHERE round_id=? AND status IN ('queued','running')`).
				WithArgs(now, "round-1").
				WillReturnResult(sqlmock.NewResult(0, 2))
			mock.ExpectExec(`UPDATE review_rounds SET status='cancelled',completed_at=?
		 WHERE id=? AND status=?`).
				WithArgs(now, "round-1", status).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()

			changed, err := NewFeatureDeliveryStore(db).RequestReviewRoundCancel(
				context.Background(), "round-1", now,
			)
			if err != nil || !changed {
				t.Fatalf("changed = %t, error = %v", changed, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}

	for _, test := range []struct {
		name    string
		status  delivery.ReviewRoundStatus
		wantErr error
	}{
		{name: "duplicate", status: delivery.RoundCancelled},
		{name: "completed", status: delivery.RoundCompleted, wantErr: delivery.ErrConflict},
		{name: "failed", status: delivery.RoundFailed, wantErr: delivery.ErrConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			mock.ExpectQuery(`SELECT status FROM review_rounds WHERE id=? LIMIT 1 FOR UPDATE`).
				WithArgs("round-1").
				WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(test.status))
			mock.ExpectRollback()

			changed, err := NewFeatureDeliveryStore(db).RequestReviewRoundCancel(
				context.Background(), "round-1", now,
			)
			if changed || !errors.Is(err, test.wantErr) {
				t.Fatalf("changed = %t, error = %v", changed, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateFindingResolutionRequiresMatchingFinding(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	resolution := delivery.FindingResolution{
		ID: "resolution-1", FindingID: "finding-1",
		Resolution:  delivery.ResolutionWaived,
		SubjectHash: "subject-hash", Rationale: "Accepted risk",
		ActorID: 7, CreatedAt: now,
	}
	mock.ExpectExec(`INSERT INTO finding_resolutions`).
		WithArgs(
			resolution.ID, resolution.FindingID, resolution.Resolution,
			resolution.SubjectHash, resolution.ReplacementHash, resolution.Rationale,
			resolution.ActorID, resolution.ExpiresAt, resolution.CreatedAt,
			resolution.FindingID, resolution.SubjectHash,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))

	store := NewFeatureDeliveryStore(db)
	err = store.CreateFindingResolution(context.Background(), resolution)
	if !errors.Is(err, delivery.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetLatestCompletedReviewRoundBySubjectHashUsesCompletedGate(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	subject := delivery.ReviewSubject{
		Kind: delivery.SubjectSystemDesign, ID: "artifact-2",
		Version: 2, SourceContentHash: "artifact-hash",
		ContentHash: "replacement-subject-hash",
	}
	subjectJSON, err := json.Marshal(subject)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	completedAt := createdAt.Add(time.Minute)
	riskFactsJSON := []byte(`[]`)
	reviewersJSON := []byte(`[]`)
	mock.ExpectQuery(`SELECT r.id,r.workflow_run_id,r.subject_json,r.policy_id,r.policy_version,
		        r.policy_hash,r.policy_selection_json,r.risk_facts_json,r.risk_hash,
		        r.selection_rule_version,r.selected_reviewers_json,r.panel_hash,
		        r.status,r.created_by,r.created_at,r.completed_at
		 FROM review_rounds r
		 WHERE r.subject_hash=? AND r.status='completed' AND r.gate_result_id IS NOT NULL
		 ORDER BY r.gate_created_at DESC,r.gate_result_id DESC LIMIT 1`).
		WithArgs(subject.ContentHash).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workflow_run_id", "subject_json", "policy_id", "policy_version",
			"policy_hash", "policy_selection_json", "risk_facts_json", "risk_hash",
			"selection_rule_version",
			"selected_reviewers_json", "panel_hash", "status", "created_by",
			"created_at", "completed_at",
		}).AddRow(
			"round-2", "workflow-2", subjectJSON, "policy-1", int64(1),
			"policy-hash", []byte(`{}`), riskFactsJSON, "risk-hash", "", reviewersJSON,
			"panel-hash", delivery.RoundCompleted, int64(7),
			createdAt, completedAt,
		))

	round, err := NewFeatureDeliveryStore(db).GetLatestCompletedReviewRoundBySubjectHash(
		context.Background(), subject.ContentHash,
	)
	if err != nil {
		t.Fatal(err)
	}
	if round.ID != "round-2" || round.Status != delivery.RoundCompleted ||
		round.Subject != subject || round.CompletedAt == nil ||
		!round.CompletedAt.Equal(completedAt) {
		t.Fatalf("round = %+v", round)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteReviewRoundStoresGateOnRound(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC)
	completedAt := createdAt.Add(time.Second)
	result := delivery.ReviewGateResult{
		ID: "gate-1", RoundID: "round-1", SubjectHash: "subject-hash",
		Decision: delivery.GatePass, PolicyHash: "policy-hash",
		ContentHash: "content-hash", CreatedAt: createdAt,
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT round.subject_hash,round.policy_hash,round.status,
		        COALESCE(round.gate_content_hash,'')
		 FROM review_rounds round
		 WHERE round.id=? LIMIT 1 FOR UPDATE`).
		WithArgs(result.RoundID).
		WillReturnRows(sqlmock.NewRows([]string{
			"subject_hash", "policy_hash", "status", "gate_content_hash",
		}).AddRow(result.SubjectHash, result.PolicyHash, delivery.RoundEvaluating, ""))
	mock.ExpectExec(`UPDATE review_rounds
		 SET gate_result_id=?,gate_decision=?,gate_result_json=?,gate_content_hash=?,
		     gate_created_at=?,status='completed',completed_at=?
		 WHERE id=? AND status='evaluating' AND gate_result_id IS NULL`).
		WithArgs(
			result.ID, result.Decision, resultJSON, result.ContentHash,
			result.CreatedAt, completedAt, result.RoundID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = NewFeatureDeliveryStore(db).CompleteReviewRound(context.Background(), result, completedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetReviewGateResultReadsRoundGate(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	result := delivery.ReviewGateResult{
		ID: "gate-1", RoundID: "round-1", SubjectHash: "subject-hash",
		Decision: delivery.GatePass, PolicyHash: "policy-hash",
		ContentHash: "content-hash",
		CreatedAt:   time.Date(2026, 8, 8, 14, 0, 0, 0, time.UTC),
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT gate_result_json FROM review_rounds
		 WHERE gate_result_id=? AND gate_result_json IS NOT NULL LIMIT 1`).
		WithArgs(result.ID).
		WillReturnRows(sqlmock.NewRows([]string{"gate_result_json"}).AddRow(resultJSON))
	mock.ExpectQuery(`SELECT gate_result_json FROM review_rounds
		 WHERE id=? AND gate_result_json IS NOT NULL LIMIT 1`).
		WithArgs(result.RoundID).
		WillReturnRows(sqlmock.NewRows([]string{"gate_result_json"}).AddRow(resultJSON))

	store := NewFeatureDeliveryStore(db)
	byID, err := store.GetReviewGateResult(context.Background(), result.ID)
	if err != nil {
		t.Fatal(err)
	}
	byRound, err := store.GetReviewGateResultByRound(context.Background(), result.RoundID)
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != result.ID || byRound.RoundID != result.RoundID {
		t.Fatalf("byID=%+v byRound=%+v", byID, byRound)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListFindingResolutionsUsesStableCursorAndLimit(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cursorTime := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	createdAt := cursorTime.Add(-time.Minute)
	expiresAt := createdAt.Add(time.Hour)
	mock.ExpectQuery(`SELECT id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at
		FROM finding_resolutions WHERE finding_id=? AND subject_hash=? AND (created_at<? OR (created_at=? AND id<?)) ORDER BY created_at DESC,id DESC LIMIT ?`).
		WithArgs(
			"finding-1", "subject-hash", cursorTime, cursorTime, "resolution-before", 3,
		).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "finding_id", "resolution", "subject_hash", "replacement_hash",
			"rationale", "actor_id", "expires_at", "created_at",
		}).AddRow(
			"resolution-1", "finding-1", delivery.ResolutionWaived,
			"subject-hash", "", "Accepted risk", int64(7), expiresAt, createdAt,
		))

	resolutions, err := NewFeatureDeliveryStore(db).ListFindingResolutions(
		context.Background(),
		"finding-1",
		"subject-hash",
		delivery.FindingResolutionCursor{
			CreatedAt: cursorTime, ID: "resolution-before",
		},
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolutions) != 1 || resolutions[0].ID != "resolution-1" ||
		resolutions[0].ExpiresAt == nil || !resolutions[0].ExpiresAt.Equal(expiresAt) {
		t.Fatalf("resolutions = %+v", resolutions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListFindingResolutionsByIDsKeepsLatestFactPerFinding(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at
		FROM (
			SELECT id,finding_id,resolution,subject_hash,replacement_hash,rationale,actor_id,expires_at,created_at,
			       ROW_NUMBER() OVER(PARTITION BY finding_id ORDER BY created_at DESC,id DESC) AS resolution_rank
			FROM finding_resolutions WHERE subject_hash=? AND finding_id IN (?,?)
		) ranked WHERE resolution_rank=1 ORDER BY finding_id LIMIT ?`).
		WithArgs("subject-hash", "finding-1", "finding-2", 2).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "finding_id", "resolution", "subject_hash", "replacement_hash",
			"rationale", "actor_id", "expires_at", "created_at",
		}).
			AddRow(
				"resolution-2", "finding-1", delivery.ResolutionInvalidated,
				"subject-hash", "", "No longer applicable", int64(7), nil,
				time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC),
			).
			AddRow(
				"resolution-3", "finding-2", delivery.ResolutionWaived,
				"subject-hash", "", "Accepted risk", int64(7), nil,
				time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC),
			))

	resolutions, err := NewFeatureDeliveryStore(db).ListFindingResolutionsByIDs(
		context.Background(), []string{"finding-1", "finding-2"}, "subject-hash",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolutions) != 2 ||
		resolutions[0].ID != "resolution-2" ||
		resolutions[1].ID != "resolution-3" {
		t.Fatalf("resolutions = %+v", resolutions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func storedReviewPolicy(t *testing.T) delivery.ReviewPolicy {
	t.Helper()
	policy, err := delivery.PrepareReviewPolicy(delivery.ReviewPolicy{
		ID: "system-design-review", Version: 1,
		SubjectKind: delivery.SubjectSystemDesign,
		Reviewers: []delivery.ReviewerSpec{
			{
				ID:             "architecture",
				Agent:          agentapi.DefinitionRef{ID: "review.architecture", Version: 1},
				DefinitionHash: "agent-hash-1", Categories: []string{"architecture"},
				Required: true, ReadOnly: true,
			},
			{
				ID:             "security",
				Agent:          agentapi.DefinitionRef{ID: "review.security", Version: 1},
				DefinitionHash: "agent-hash-2", Categories: []string{"security"},
				Required: true, ReadOnly: true,
			},
		},
		BlockingSeverities: []delivery.Severity{
			delivery.SeverityCritical, delivery.SeverityHigh,
		},
		RequiredCategories:     []string{"architecture", "security"},
		MaxParallelism:         2,
		MaxInputTokens:         2,
		MaxOutputTokens:        2,
		MaxTotalTokens:         2,
		MaxToolCalls:           2,
		MaxCostMicros:          2,
		MaxRetries:             1,
		Timeout:                time.Minute,
		OptionalReviewerAction: delivery.OptionalReviewerContinue,
		CreatedAt:              time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func storedReviewPolicyRolloutRule(version int64) delivery.ReviewPolicyRolloutRule {
	return delivery.ReviewPolicyRolloutRule{
		SubjectKind:            delivery.SubjectSystemDesign,
		RuleVersion:            version,
		CandidatePolicyID:      "candidate-policy",
		CandidatePolicyVersion: 2,
		PercentageBPS:          2500,
		Salt:                   "rollout-2026-08",
		RuleHash:               "rule-hash",
		Active:                 true,
		CreatedBy:              7,
		CreatedAt:              time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC),
	}
}

func storedReviewRoundWithSelection() (
	delivery.ReviewRound,
	[]delivery.ReviewAssignment,
) {
	createdAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	reviewers := []delivery.ReviewerSpec{
		{
			ID: "architecture",
			Agent: agentapi.DefinitionRef{
				ID: "review.architecture", Version: 1,
			},
			DefinitionHash: "agent-hash-1", Categories: []string{"architecture"},
			Required: true, ReadOnly: true,
		},
		{
			ID: "security",
			Agent: agentapi.DefinitionRef{
				ID: "review.security", Version: 1,
			},
			DefinitionHash: "agent-hash-2", Categories: []string{"security"},
			Required: true, ReadOnly: true,
		},
	}
	round := delivery.ReviewRound{
		ID: "round-rollout", WorkflowRunID: "workflow-rollout",
		Subject: delivery.ReviewSubject{
			Kind: delivery.SubjectSystemDesign, ID: "artifact-1", Version: 2,
			SourceContentHash: "artifact-hash", ContentHash: "subject-hash",
		},
		PolicyID: "candidate-policy", PolicyVersion: 2, PolicyHash: "policy-hash",
		PolicySelection: delivery.ReviewPolicySelection{
			RuleVersion: 3, RuleHash: "rule-hash",
			CandidatePolicyID: "candidate-policy", CandidatePolicyVersion: 2,
			BucketBasisPoints: 421, PercentageBasisPoints: 2500,
			StableKeyHash: "stable-key-hash", Reason: "rollout_candidate",
		},
		RiskFacts: []delivery.ReviewRiskFact{}, RiskHash: "risk-hash",
		Reviewers: reviewers, PanelHash: "panel-hash", Status: delivery.RoundCreated,
		CreatedBy: 7, CreatedAt: createdAt,
	}
	assignments := make([]delivery.ReviewAssignment, 0, len(reviewers))
	for index, reviewer := range reviewers {
		assignments = append(assignments, delivery.ReviewAssignment{
			ID: "assignment-" + string(rune('1'+index)), RoundID: round.ID,
			ReviewerID: reviewer.ID, Agent: reviewer.Agent,
			DefinitionHash: reviewer.DefinitionHash,
			Categories:     append([]string(nil), reviewer.Categories...),
			Required:       reviewer.Required,
			Status:         delivery.AssignmentQueued,
			Attempt:        1,
			CreatedAt:      createdAt,
		})
	}
	return round, assignments
}

func storedReviewAdjudication(t *testing.T) delivery.ReviewAdjudication {
	t.Helper()
	adjudication, err := delivery.PrepareReviewAdjudication(
		delivery.ReviewAdjudication{
			RoundID: "round-1", SubjectHash: "subject-hash",
			PolicyHash: "policy-hash", Fingerprint: "finding-fingerprint",
			FindingIDs: []string{"finding-high", "finding-medium"},
			Agent: agentapi.DefinitionRef{
				ID: "review.adjudicator", Version: 1,
			},
			DefinitionHash: "adjudicator-hash",
			Decision:       delivery.AdjudicationConfirmed,
			Rationale:      "The high-severity evidence is confirmed.",
			CreatedAt:      time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return adjudication
}
