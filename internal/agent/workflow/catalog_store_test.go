package workflow

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStoreLoadDefinitionRollouts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	createdAt := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT\s+subject_id,rule_version,candidate_version,percentage_bps,salt,rule_hash,\s+active,created_by,created_at\s+FROM catalog_rollouts\s+WHERE catalog_kind='workflow'\s+ORDER BY subject_id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_id", "rule_version", "candidate_version", "percentage_bps",
			"salt", "rule_hash", "active", "created_by", "created_at",
		}).AddRow(
			"delivery.review", int64(2), int64(3), 2500,
			"stable", "rule-hash", true, int64(7), createdAt,
		))

	rules, err := workflowStore.LoadRollouts(context.Background())
	if err != nil {
		t.Fatalf("LoadRollouts: %v", err)
	}
	if len(rules) != 1 || rules[0].WorkflowID != "delivery.review" ||
		rules[0].RuleVersion != 2 || rules[0].CandidateVersion != 3 ||
		rules[0].PercentageBPS != 2500 || rules[0].CreatedAt != createdAt {
		t.Fatalf("rules = %+v", rules)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestStoreSetRolloutInsertsInitialRuleAndAudit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedWorkflowRolloutRule(t, 1, true)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM catalog_rollouts\s+WHERE catalog_kind='workflow' AND subject_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}))
	mock.ExpectExec(`(?s)INSERT INTO catalog_rollouts\(`).
		WithArgs(
			rule.WorkflowID, rule.RuleVersion, rule.WorkflowID, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(9), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	expectWorkflowRolloutAuditInsert(mock, rule, "rollout_enabled", 9).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := workflowStore.SetRollout(
		context.Background(), rule, 9,
	); err != nil {
		t.Fatalf("SetRollout: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestStoreSetRolloutUpdatesNextRuleAndAudit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedWorkflowRolloutRule(t, 3, false)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM catalog_rollouts\s+WHERE catalog_kind='workflow' AND subject_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(2)))
	mock.ExpectExec(`(?s)UPDATE catalog_rollouts SET\s+rule_version=\?,candidate_id=\?,candidate_version=\?,percentage_bps=\?,salt=\?,rule_hash=\?,\s+active=\?,created_by=\?,created_at=\?\s+WHERE catalog_kind='workflow' AND subject_id=\?`).
		WithArgs(
			rule.RuleVersion, rule.WorkflowID, rule.CandidateVersion, rule.PercentageBPS,
			rule.Salt, rule.RuleHash, rule.Active, int64(10),
			sqlmock.AnyArg(), rule.WorkflowID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowRolloutAuditInsert(mock, rule, "rollout_disabled", 10).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()

	if err := workflowStore.SetRollout(
		context.Background(), rule, 10,
	); err != nil {
		t.Fatalf("SetRollout: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestStoreSetRolloutRejectsStaleRuleVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedWorkflowRolloutRule(t, 2, true)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM catalog_rollouts\s+WHERE catalog_kind='workflow' AND subject_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(2)))
	mock.ExpectRollback()

	err = workflowStore.SetRollout(context.Background(), rule, 9)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SetRollout error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestStoreSetRolloutRollsBackWhenAuditFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedWorkflowRolloutRule(t, 1, true)
	auditErr := errors.New("audit unavailable")

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM catalog_rollouts\s+WHERE catalog_kind='workflow' AND subject_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}))
	mock.ExpectExec(`(?s)INSERT INTO catalog_rollouts\(`).
		WithArgs(
			rule.WorkflowID, rule.RuleVersion, rule.WorkflowID, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(9), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	expectWorkflowRolloutAuditInsert(mock, rule, "rollout_enabled", 9).
		WillReturnError(auditErr)
	mock.ExpectRollback()

	err = workflowStore.SetRollout(context.Background(), rule, 9)
	if !errors.Is(err, auditErr) {
		t.Fatalf("SetRollout error = %v, want audit error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestStoreListRolloutAuditUsesBoundedCursor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	createdAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT\s+seq,subject_id,version,candidate_version,percentage_bps,rule_hash,\s+action,actor_user_id,created_at\s+FROM catalog_audit\s+WHERE catalog_kind='workflow' AND event_kind='rollout'\s+AND subject_id=\? AND seq>\?\s+ORDER BY seq LIMIT \?`).
		WithArgs("delivery.review", int64(20), 5).
		WillReturnRows(sqlmock.NewRows([]string{
			"seq", "workflow_id", "rule_version", "candidate_version",
			"percentage_bps", "rule_hash", "action", "actor_user_id", "created_at",
		}).AddRow(
			int64(21), "delivery.review", int64(3), int64(1),
			2500, "rule-hash", "rollout_enabled", int64(9), createdAt,
		))

	events, err := workflowStore.ListRolloutAudit(
		context.Background(), "delivery.review", 20, 5,
	)
	if err != nil {
		t.Fatalf("ListRolloutAudit: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 21 ||
		events[0].RuleVersion != 3 || events[0].ActorUserID != 9 {
		t.Fatalf("events = %+v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func testPreparedWorkflowRolloutRule(
	t *testing.T,
	version int64,
	active bool,
) RolloutRule {
	t.Helper()
	rule, err := prepareRolloutRule(RolloutRule{
		WorkflowID: "delivery.review", RuleVersion: version, CandidateVersion: 1,
		PercentageBPS: 2500, Salt: "stable", Active: active,
		CreatedAt: time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("prepareRolloutRule: %v", err)
	}
	return rule
}

func expectWorkflowRolloutAuditInsert(
	mock sqlmock.Sqlmock,
	rule RolloutRule,
	action string,
	actorUserID int64,
) *sqlmock.ExpectedExec {
	return mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO catalog_audit(
		catalog_kind,event_kind,subject_id,version,candidate_id,candidate_version,
		percentage_bps,rule_hash,action,actor_user_id,created_at)
		VALUES('workflow','rollout',?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			rule.WorkflowID, rule.RuleVersion, rule.WorkflowID, rule.CandidateVersion,
			rule.PercentageBPS, rule.RuleHash, action, actorUserID,
			sqlmock.AnyArg(),
		)
}
