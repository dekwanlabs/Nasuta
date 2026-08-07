package agentcatalog

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStoreLoadRollouts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	createdAt := time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT\s+agent_id,rule_version,candidate_version,percentage_bps,salt,rule_hash,\s+active,created_by,created_at\s+FROM agent_definition_rollouts ORDER BY agent_id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"agent_id", "rule_version", "candidate_version", "percentage_bps",
			"salt", "rule_hash", "active", "created_by", "created_at",
		}).AddRow(
			"qa.answerer", int64(2), int64(3), 2500,
			"stable", "rule-hash", true, int64(7), createdAt,
		))

	rules, err := store.LoadRollouts(context.Background())
	if err != nil {
		t.Fatalf("LoadRollouts: %v", err)
	}
	if len(rules) != 1 || rules[0].AgentID != "qa.answerer" ||
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
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedRolloutRule(t, 1, true)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM agent_definition_rollouts WHERE agent_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.AgentID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}))
	mock.ExpectExec(`(?s)INSERT INTO agent_definition_rollouts\(`).
		WithArgs(
			rule.AgentID, rule.RuleVersion, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(9), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	expectRolloutAuditInsert(mock, rule, "rollout_enabled", 9).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.SetRollout(context.Background(), rule, 9); err != nil {
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
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedRolloutRule(t, 3, false)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM agent_definition_rollouts WHERE agent_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.AgentID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(2)))
	mock.ExpectExec(`(?s)UPDATE agent_definition_rollouts SET\s+rule_version=\?,candidate_version=\?,percentage_bps=\?,salt=\?,rule_hash=\?,\s+active=\?,created_by=\?,created_at=\? WHERE agent_id=\?`).
		WithArgs(
			rule.RuleVersion, rule.CandidateVersion, rule.PercentageBPS,
			rule.Salt, rule.RuleHash, rule.Active, int64(10),
			sqlmock.AnyArg(), rule.AgentID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectRolloutAuditInsert(mock, rule, "rollout_disabled", 10).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()

	if err := store.SetRollout(context.Background(), rule, 10); err != nil {
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
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedRolloutRule(t, 2, true)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM agent_definition_rollouts WHERE agent_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.AgentID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}).AddRow(int64(2)))
	mock.ExpectRollback()

	err = store.SetRollout(context.Background(), rule, 9)
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
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	rule := testPreparedRolloutRule(t, 1, true)
	auditErr := errors.New("audit unavailable")

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT rule_version\s+FROM agent_definition_rollouts WHERE agent_id=\? LIMIT 1 FOR UPDATE`).
		WithArgs(rule.AgentID).
		WillReturnRows(sqlmock.NewRows([]string{"rule_version"}))
	mock.ExpectExec(`(?s)INSERT INTO agent_definition_rollouts\(`).
		WithArgs(
			rule.AgentID, rule.RuleVersion, rule.CandidateVersion,
			rule.PercentageBPS, rule.Salt, rule.RuleHash, rule.Active,
			int64(9), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	expectRolloutAuditInsert(mock, rule, "rollout_enabled", 9).
		WillReturnError(auditErr)
	mock.ExpectRollback()

	err = store.SetRollout(context.Background(), rule, 9)
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
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	createdAt := time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT\s+seq,agent_id,rule_version,candidate_version,percentage_bps,rule_hash,\s+action,actor_user_id,created_at\s+FROM agent_definition_rollout_audit\s+WHERE agent_id=\? AND seq>\?\s+ORDER BY seq LIMIT \?`).
		WithArgs("qa.answerer", int64(20), 5).
		WillReturnRows(sqlmock.NewRows([]string{
			"seq", "agent_id", "rule_version", "candidate_version",
			"percentage_bps", "rule_hash", "action", "actor_user_id", "created_at",
		}).AddRow(
			int64(21), "qa.answerer", int64(3), int64(1),
			2500, "rule-hash", "rollout_enabled", int64(9), createdAt,
		))

	events, err := store.ListRolloutAudit(
		context.Background(), "qa.answerer", 20, 5,
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

func testPreparedRolloutRule(
	t *testing.T,
	version int64,
	active bool,
) RolloutRule {
	t.Helper()
	rule, err := prepareRolloutRule(RolloutRule{
		AgentID: "qa.answerer", RuleVersion: version, CandidateVersion: 1,
		PercentageBPS: 2500, Salt: "stable", Active: active,
		CreatedAt: time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("prepareRolloutRule: %v", err)
	}
	return rule
}

func expectRolloutAuditInsert(
	mock sqlmock.Sqlmock,
	rule RolloutRule,
	action string,
	actorUserID int64,
) *sqlmock.ExpectedExec {
	return mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO agent_definition_rollout_audit(
		agent_id,rule_version,candidate_version,percentage_bps,rule_hash,
		action,actor_user_id,created_at)
		VALUES(?,?,?,?,?,?,?,?)`)).
		WithArgs(
			rule.AgentID, rule.RuleVersion, rule.CandidateVersion,
			rule.PercentageBPS, rule.RuleHash, action, actorUserID,
			sqlmock.AnyArg(),
		)
}
