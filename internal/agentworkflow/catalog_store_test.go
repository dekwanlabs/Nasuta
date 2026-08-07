package agentworkflow

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestWorkflowStoreLoadDefinitionRollouts(t *testing.T) {
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
	mock.ExpectQuery(`(?s)SELECT\s+workflow_id,rule_version,candidate_version,percentage_bps,salt,rule_hash,\s+active,created_by,created_at\s+FROM workflow_definition_rollouts ORDER BY workflow_id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_id", "rule_version", "candidate_version", "percentage_bps",
			"salt", "rule_hash", "active", "created_by", "created_at",
		}).AddRow(
			"delivery.review", int64(2), int64(3), 2500,
			"stable", "rule-hash", true, int64(7), createdAt,
		))

	rules, err := workflowStore.LoadDefinitionRollouts(context.Background())
	if err != nil {
		t.Fatalf("LoadDefinitionRollouts: %v", err)
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

func TestWorkflowStoreSetDefinitionRolloutInsertsInitialRuleAndAudit(t *testing.T) {
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
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM workflow_definition_rollouts\s+WHERE workflow_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}))
	mock.ExpectExec(`(?s)INSERT INTO workflow_definition_rollouts\(`).
		WithArgs(
			rule.WorkflowID, rule.RuleVersion, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(9), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	expectWorkflowRolloutAuditInsert(mock, rule, "rollout_enabled", 9).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := workflowStore.SetDefinitionRollout(
		context.Background(), rule, 9,
	); err != nil {
		t.Fatalf("SetDefinitionRollout: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestWorkflowStoreSetDefinitionRolloutUpdatesNextRuleAndAudit(t *testing.T) {
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
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM workflow_definition_rollouts\s+WHERE workflow_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(2)))
	mock.ExpectExec(`(?s)UPDATE workflow_definition_rollouts SET\s+rule_version=\?,candidate_version=\?,percentage_bps=\?,salt=\?,rule_hash=\?,\s+active=\?,created_by=\?,created_at=\? WHERE workflow_id=\?`).
		WithArgs(
			rule.RuleVersion, rule.CandidateVersion, rule.PercentageBPS,
			rule.Salt, rule.RuleHash, rule.Active, int64(10),
			sqlmock.AnyArg(), rule.WorkflowID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectWorkflowRolloutAuditInsert(mock, rule, "rollout_disabled", 10).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()

	if err := workflowStore.SetDefinitionRollout(
		context.Background(), rule, 10,
	); err != nil {
		t.Fatalf("SetDefinitionRollout: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestWorkflowStoreSetDefinitionRolloutRejectsStaleRuleVersion(t *testing.T) {
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
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM workflow_definition_rollouts\s+WHERE workflow_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(2)))
	mock.ExpectRollback()

	err = workflowStore.SetDefinitionRollout(context.Background(), rule, 9)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SetDefinitionRollout error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestWorkflowStoreSetDefinitionRolloutRollsBackWhenAuditFails(t *testing.T) {
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
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM workflow_definition_rollouts\s+WHERE workflow_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.WorkflowID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}))
	mock.ExpectExec(`(?s)INSERT INTO workflow_definition_rollouts\(`).
		WithArgs(
			rule.WorkflowID, rule.RuleVersion, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(9), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	expectWorkflowRolloutAuditInsert(mock, rule, "rollout_enabled", 9).
		WillReturnError(auditErr)
	mock.ExpectRollback()

	err = workflowStore.SetDefinitionRollout(context.Background(), rule, 9)
	if !errors.Is(err, auditErr) {
		t.Fatalf("SetDefinitionRollout error = %v, want audit error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestWorkflowStoreListDefinitionRolloutAuditUsesBoundedCursor(t *testing.T) {
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
	mock.ExpectQuery(`(?s)SELECT\s+seq,workflow_id,rule_version,candidate_version,percentage_bps,rule_hash,\s+action,actor_user_id,created_at\s+FROM workflow_definition_rollout_audit\s+WHERE workflow_id=\? AND seq>\?\s+ORDER BY seq LIMIT \?`).
		WithArgs("delivery.review", int64(20), 5).
		WillReturnRows(sqlmock.NewRows([]string{
			"seq", "workflow_id", "rule_version", "candidate_version",
			"percentage_bps", "rule_hash", "action", "actor_user_id", "created_at",
		}).AddRow(
			int64(21), "delivery.review", int64(3), int64(1),
			2500, "rule-hash", "rollout_enabled", int64(9), createdAt,
		))

	events, err := workflowStore.ListDefinitionRolloutAudit(
		context.Background(), "delivery.review", 20, 5,
	)
	if err != nil {
		t.Fatalf("ListDefinitionRolloutAudit: %v", err)
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
	return mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO workflow_definition_rollout_audit(
		workflow_id,rule_version,candidate_version,percentage_bps,rule_hash,
		action,actor_user_id,created_at)
		VALUES(?,?,?,?,?,?,?,?)`)).
		WithArgs(
			rule.WorkflowID, rule.RuleVersion, rule.CandidateVersion,
			rule.PercentageBPS, rule.RuleHash, action, actorUserID,
			sqlmock.AnyArg(),
		)
}
