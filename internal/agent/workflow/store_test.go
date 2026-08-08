package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestStartWorkflowCommitsRunInputAndEventsAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run := WorkflowRunRecord{
		ID: "run_1", ParentRunID: "qa_parent_1",
		WorkflowID: "delivery.review", WorkflowVersion: 2,
		WorkflowHash: "workflow_hash",
		Selection: DefinitionSelection{
			RuleVersion: 3, RuleHash: "rule_hash", CandidateVersion: 2,
			BucketBasisPoints: 1200, PercentageBasisPoints: 2500,
			StableKeyHash: "stable_key_hash", Reason: "rollout_candidate",
		},
		InputHash:   "input_hash",
		ActorUserID: 7, ActorTenantID: "tenant-a",
		ActorPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Scenario: "delivery.review",
		ScenarioPermissions: agentapi.PermissionPolicy{
			Scopes: []string{"knowledge.read"},
		},
		Budget: WorkflowBudget{
			MaxNodes: 4, MaxParallelism: 2, Timeout: time.Minute, MaxHandoffBytes: 4096,
		},
		Usage: WorkflowUsage{
			InputTokens: 11, OutputTokens: 12, ReasoningTokens: 13, TotalTokens: 36,
			ToolCalls: 14, CostMicros: 15, Retries: 16,
		},
		StartedAt: now,
	}
	input := Handoff{
		ID: "handoff_input", WorkflowRunID: run.ID, ProducerNodeID: "workflow.input",
		Schema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		Payload: json.RawMessage(`{"subject":"x"}`), Completeness: Complete,
		ContentHash: "input_handoff_hash", CreatedAt: now,
	}
	budget, err := json.Marshal(run.Budget)
	if err != nil {
		t.Fatal(err)
	}
	actorPermissions, err := json.Marshal(run.ActorPermissions)
	if err != nil {
		t.Fatal(err)
	}
	scenarioPermissions, err := json.Marshal(run.ScenarioPermissions)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := json.Marshal(run.Selection)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO workflow_runs(
		id,parent_run_id,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			"run_1", "qa_parent_1", "delivery.review", int64(2), "workflow_hash", selection, "input_hash",
			int64(7), "tenant-a", actorPermissions, "delivery.review", scenarioPermissions,
			RunRunning, budget,
			int64(11), int64(12), int64(13), int64(36), int64(14), int64(15), int64(16),
			"", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			"handoff_input", "run_1", "workflow.input", "", "review.subject", int64(1),
			input.Payload, []byte("null"), Complete, "input_handoff_hash", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
	)).WithArgs("run_1").WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(1))
	for _, expected := range []struct {
		seq    int64
		kind   string
		nodeID string
	}{
		{seq: 1, kind: "workflow_started"},
		{seq: 2, kind: "handoff_created", nodeID: "workflow.input"},
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
			VALUES('workflow',?,?,?,?,?,?,?)`)).
			WithArgs(
				"run_1", expected.seq, expected.kind, expected.nodeID,
				sqlmock.AnyArg(), nil, sqlmock.AnyArg(),
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
	if err := workflowStore.StartWorkflow(context.Background(), run, input); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListEventsBoundsReadAtStorageBoundary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		stream_id,seq,kind,node_id,summary,detail_json,created_at
		FROM runtime_events
		WHERE stream_kind='workflow' AND stream_id=? AND seq>?
		ORDER BY seq LIMIT ?`)).
		WithArgs("run_1", int64(4), 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"stream_id", "seq", "kind", "node_id", "summary", "detail_json", "created_at",
		}))
	if _, err := workflowStore.ListEvents(context.Background(), "run_1", 4, 1000); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListActiveRunsUsesStartupCutoffAndKeysetPagination(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC()
	firstStartedAt := cutoff.Add(-2 * time.Minute)
	secondStartedAt := cutoff.Add(-time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id,started_at
			FROM workflow_runs
			WHERE status=? AND started_at<?
			ORDER BY started_at,id LIMIT ?`)).
		WithArgs(RunRunning, sqlmock.AnyArg(), 100).
		WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}).
			AddRow("workflow_a", firstStartedAt).
			AddRow("workflow_b", secondStartedAt))

	runs, err := workflowStore.ListActiveRuns(
		context.Background(),
		cutoff,
		ActiveRunCursor{},
		1000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "workflow_a" || runs[1].ID != "workflow_b" {
		t.Fatalf("active runs = %+v", runs)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id,started_at
			FROM workflow_runs
			WHERE status=? AND started_at<?
			AND (started_at>? OR (started_at=? AND id>?))
			ORDER BY started_at,id LIMIT ?`)).
		WithArgs(
			RunRunning,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"workflow_b",
			25,
		).
		WillReturnRows(sqlmock.NewRows([]string{"id", "started_at"}))

	runs, err = workflowStore.ListActiveRuns(
		context.Background(),
		cutoff,
		ActiveRunCursor{StartedAt: secondStartedAt, ID: "workflow_b"},
		25,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("second page active runs = %+v", runs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSucceedNodeCommitsHandoffTransitionAndEventsAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	handoff := Handoff{
		ID: "handoff_1", WorkflowRunID: "run_1", ProducerNodeID: "review.a",
		ProducerRunID: "agent_run_1",
		Schema:        agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload:       json.RawMessage(`{"node":"review.a"}`),
		Completeness:  Complete, ContentHash: "handoff_hash", CreatedAt: now,
	}
	usage := WorkflowUsage{
		InputTokens: 101, OutputTokens: 23, ReasoningTokens: 7, TotalTokens: 131,
		ToolCalls: 4, CostMicros: 29, Retries: 1,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunRunning),
	)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			"handoff_1", "run_1", "review.a", "agent_run_1", "review.report", int64(1),
			handoff.Payload, []byte("null"), Complete, "handoff_hash", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_node_runs
		SET agent_run_id=?,output_handoff_id=?,status=?,error_code='',
			input_tokens=?,output_tokens=?,reasoning_tokens=?,total_tokens=?,
			tool_call_count=?,cost_micros=?,retry_count=?,
			gate_decision_id=?,gate_id=?,gate_subject_hash=?,gate_decision=?,
			gate_reason_codes_json=?,gate_finding_ids_json=?,gate_evaluated_at=?,
			ended_at=?
		WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`)).
		WithArgs(
			"agent_run_1", "handoff_1", RunSucceeded,
			int64(101), int64(23), int64(7), int64(131), int64(4), int64(29), int64(1),
			nil, nil, nil, nil, nil, nil, nil,
			sqlmock.AnyArg(),
			"run_1", "review.a", 1, RunRunning,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_runs
		SET input_tokens=input_tokens+?,output_tokens=output_tokens+?,
			reasoning_tokens=reasoning_tokens+?,total_tokens=total_tokens+?,
			tool_call_count=tool_call_count+?,cost_micros=cost_micros+?,
			retry_count=retry_count+?
		WHERE id=? AND status=?`)).
		WithArgs(
			int64(101), int64(23), int64(7), int64(131), int64(4), int64(29), int64(1),
			"run_1", RunRunning,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
	)).WithArgs("run_1").WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(3))
	for _, expected := range []struct {
		seq  int64
		kind string
	}{
		{seq: 3, kind: "handoff_created"},
		{seq: 4, kind: "node_succeeded"},
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
			VALUES('workflow',?,?,?,?,?,?,?)`)).
			WithArgs(
				"run_1", expected.seq, expected.kind, "review.a", sqlmock.AnyArg(), nil, sqlmock.AnyArg(),
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
	if err := workflowStore.SucceedNode(
		context.Background(), "run_1", "review.a", 1, "agent_run_1",
		handoff, nil, usage, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStartNodePersistsSecondAttempt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunRunning),
	)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO workflow_node_runs(
		workflow_run_id,node_id,attempt,kind,agent_run_id,input_handoff_ids_json,
		output_handoff_id,status,error_code,started_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			"run_1", "review.a", 2, NodeAgent, "", []byte(`["handoff_input"]`), "",
			RunRunning, "", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
	)).WithArgs("run_1").WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(5))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_events(
		stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
		VALUES('workflow',?,?,?,?,?,?,?)`)).
		WithArgs(
			"run_1", int64(5), "node_started", "review.a",
			"workflow node started", nil, sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := workflowStore.StartNode(context.Background(), NodeRunRecord{
		WorkflowRunID:   "run_1",
		NodeID:          "review.a",
		Attempt:         2,
		Kind:            NodeAgent,
		InputHandoffIDs: []string{"handoff_input"},
		Status:          RunRunning,
		StartedAt:       startedAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFailNodePersistsWaitingHumanTransition(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	usage := WorkflowUsage{
		InputTokens: 41, OutputTokens: 9, ReasoningTokens: 2, TotalTokens: 52,
		ToolCalls: 3, CostMicros: 17, Retries: 1,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunRunning),
	)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_node_runs
		SET agent_run_id=?,status=?,error_code=?,
			input_tokens=?,output_tokens=?,reasoning_tokens=?,total_tokens=?,
			tool_call_count=?,cost_micros=?,retry_count=?,ended_at=?
		WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`)).
		WithArgs(
			"", RunWaitingHuman, "human_approval_required",
			int64(41), int64(9), int64(2), int64(52), int64(3), int64(17), int64(1),
			sqlmock.AnyArg(),
			"run_1", "approve", 1, RunRunning,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_runs
		SET input_tokens=input_tokens+?,output_tokens=output_tokens+?,
			reasoning_tokens=reasoning_tokens+?,total_tokens=total_tokens+?,
			tool_call_count=tool_call_count+?,cost_micros=cost_micros+?,
			retry_count=retry_count+?
		WHERE id=? AND status=?`)).
		WithArgs(
			int64(41), int64(9), int64(2), int64(52), int64(3), int64(17), int64(1),
			"run_1", RunRunning,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
	)).WithArgs("run_1").WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(4))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_events(
		stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
		VALUES('workflow',?,?,?,?,?,?,?)`)).
		WithArgs(
			"run_1", int64(4), "human_review_required", "approve",
			"workflow node requires human approval", nil, sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := workflowStore.FailNode(
		context.Background(), "run_1", "approve", 1, "", RunWaitingHuman,
		"human_approval_required", usage, time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFinishWorkflowCommitsOutputAndTerminalTransition(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	output := Handoff{
		ID: "handoff_output", WorkflowRunID: "run_1", ProducerNodeID: "workflow.output",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"result":"ok"}`), Completeness: Complete,
		ContentHash: "output_hash", CreatedAt: now,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunRunning),
	)
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			"handoff_output", "run_1", "workflow.output", "", "review.report", int64(1),
			output.Payload, []byte("null"), Complete, "output_hash", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_runs
		SET status=?,error_code=?,ended_at=? WHERE id=? AND status=?`)).
		WithArgs(RunSucceeded, "", sqlmock.AnyArg(), "run_1", RunRunning).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
	)).WithArgs("run_1").WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(7))
	for _, expected := range []struct {
		seq    int64
		kind   string
		nodeID string
	}{
		{seq: 7, kind: "handoff_created", nodeID: "workflow.output"},
		{seq: 8, kind: "workflow_succeeded"},
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
			VALUES('workflow',?,?,?,?,?,?,?)`)).
			WithArgs(
				"run_1", expected.seq, expected.kind, expected.nodeID,
				sqlmock.AnyArg(), nil, sqlmock.AnyArg(),
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()
	if err := workflowStore.FinishWorkflow(
		context.Background(), "run_1", RunSucceeded, "", &output, now,
	); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetRunUsesBoundedReadAndDecodesSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-time.Minute)
	endedAt := time.Now().UTC()
	budget := WorkflowBudget{
		MaxNodes: 6, MaxParallelism: 3, Timeout: 2 * time.Minute, MaxHandoffBytes: 8192,
	}
	usage := WorkflowUsage{
		InputTokens: 120, OutputTokens: 30, ReasoningTokens: 5, TotalTokens: 155,
		ToolCalls: 6, CostMicros: 44, Retries: 2,
	}
	budgetJSON, err := json.Marshal(budget)
	if err != nil {
		t.Fatal(err)
	}
	actorPermissions := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	scenarioPermissions := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	actorPermissionsJSON, err := json.Marshal(actorPermissions)
	if err != nil {
		t.Fatal(err)
	}
	scenarioPermissionsJSON, err := json.Marshal(scenarioPermissions)
	if err != nil {
		t.Fatal(err)
	}
	selection := DefinitionSelection{
		RuleVersion: 4, RuleHash: "rule_hash", CandidateVersion: 2,
		BucketBasisPoints: 2400, PercentageBasisPoints: 3000,
		StableKeyHash: "stable_key_hash", Reason: "rollout_candidate",
	}
	selectionJSON, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		id,parent_run_id,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,started_at,ended_at
		FROM workflow_runs WHERE id=? LIMIT 1`)).
		WithArgs("run_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "parent_run_id", "workflow_id", "workflow_version", "workflow_hash", "selection_json", "input_hash",
			"actor_user_id", "actor_tenant_id", "actor_permissions_json", "scenario",
			"scenario_permissions_json", "status", "budget_json", "input_tokens",
			"output_tokens", "reasoning_tokens", "total_tokens", "tool_call_count",
			"cost_micros", "retry_count", "error_code", "started_at", "ended_at",
		}).AddRow(
			"run_1", "qa_parent_1", "delivery.review", int64(2), "workflow_hash", selectionJSON, "input_hash",
			int64(7), "tenant-a", actorPermissionsJSON, "delivery.review",
			scenarioPermissionsJSON, RunSucceeded, budgetJSON,
			usage.InputTokens, usage.OutputTokens, usage.ReasoningTokens, usage.TotalTokens,
			usage.ToolCalls, usage.CostMicros, usage.Retries, "", startedAt, endedAt,
		))
	run, err := workflowStore.GetRun(context.Background(), "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "run_1" || run.ParentRunID != "qa_parent_1" ||
		run.Status != RunSucceeded || run.Budget != budget ||
		run.Selection != selection ||
		run.Usage != usage ||
		run.EndedAt == nil || !run.EndedAt.Equal(endedAt) ||
		len(run.ActorPermissions.Scopes) != 1 ||
		run.ActorPermissions.Scopes[0] != "knowledge.read" ||
		len(run.ScenarioPermissions.Scopes) != 1 ||
		run.ScenarioPermissions.Scopes[0] != "knowledge.read" {
		t.Fatalf("run = %+v", run)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDecideHumanApprovalCommitsApprovalAndResumeAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	decidedAt := time.Now().UTC()
	approval := WorkflowApproval{
		WorkflowRunID: "run_1", NodeID: "approve",
		Decision: ApprovalApproved, ApproverUserID: 99,
		ApproverTenantID: "tenant-a", Comment: "approved",
		DecidedAt: decidedAt,
	}
	handoff := Handoff{
		ID: "handoff_approved", WorkflowRunID: "run_1", ProducerNodeID: "approve",
		Schema:  agentapi.SchemaRef{ID: "review.report", Version: 1},
		Payload: json.RawMessage(`{"result":"approved"}`),
		References: []agentapi.Reference{{
			Type: "artifact", Target: "report_1",
		}},
		Completeness: Complete, ContentHash: "approved_hash", CreatedAt: decidedAt,
	}
	references, err := json.Marshal(handoff.References)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunWaitingHuman),
	)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		workflow_run_id,node_id,approval_decision,approver_user_id,approver_tenant_id,
		approval_comment,approval_decided_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=? AND approval_decision IS NOT NULL
		ORDER BY attempt DESC LIMIT 1`)).
		WithArgs("run_1", "approve").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT attempt,kind,status
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=?
		ORDER BY attempt DESC LIMIT 1 FOR UPDATE`)).
		WithArgs("run_1", "approve").
		WillReturnRows(sqlmock.NewRows([]string{"attempt", "kind", "status"}).
			AddRow(2, NodeHumanApproval, RunWaitingHuman))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO handoff_artifacts(
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			"handoff_approved", "run_1", "approve", "", "review.report", int64(1),
			handoff.Payload, references, Complete, "approved_hash", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_node_runs
			SET output_handoff_id=?,approval_decision=?,approver_user_id=?,
				approver_tenant_id=?,approval_comment=?,approval_decided_at=?,
				status=?,error_code='',ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`)).
		WithArgs(
			"handoff_approved", ApprovalApproved, int64(99), "tenant-a",
			"approved", sqlmock.AnyArg(), RunSucceeded, sqlmock.AnyArg(),
			"run_1", "approve", 2, RunWaitingHuman,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_runs
			SET status=?,error_code='',ended_at=NULL WHERE id=? AND status=?`)).
		WithArgs(RunRunning, "run_1", RunWaitingHuman).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
	)).WithArgs("run_1").WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(8))
	for _, expected := range []struct {
		seq     int64
		kind    string
		nodeID  string
		summary string
	}{
		{seq: 8, kind: "human_approved", nodeID: "approve", summary: "workflow node approved"},
		{seq: 9, kind: "handoff_created", nodeID: "approve", summary: "approved handoff created"},
		{seq: 10, kind: "node_succeeded", nodeID: "approve", summary: "workflow node succeeded"},
		{seq: 11, kind: "workflow_resumed", summary: "workflow resumed"},
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
			VALUES('workflow',?,?,?,?,?,?,?)`)).
			WithArgs(
				"run_1", expected.seq, expected.kind, expected.nodeID,
				expected.summary, nil, sqlmock.AnyArg(),
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()

	transition, err := workflowStore.DecideHumanApproval(
		context.Background(), approval, &handoff,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !transition.Applied || transition.RunStatus != RunRunning ||
		transition.Approval != approval {
		t.Fatalf("approval transition = %+v", transition)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDecideHumanApprovalCommitsRejectionAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	decidedAt := time.Now().UTC()
	approval := WorkflowApproval{
		WorkflowRunID: "run_1", NodeID: "approve",
		Decision: ApprovalRejected, ApproverUserID: 99,
		ApproverTenantID: "tenant-a", Comment: "rejected",
		DecidedAt: decidedAt,
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunWaitingHuman),
	)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		workflow_run_id,node_id,approval_decision,approver_user_id,approver_tenant_id,
		approval_comment,approval_decided_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=? AND approval_decision IS NOT NULL
		ORDER BY attempt DESC LIMIT 1`)).
		WithArgs("run_1", "approve").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT attempt,kind,status
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=?
		ORDER BY attempt DESC LIMIT 1 FOR UPDATE`)).
		WithArgs("run_1", "approve").
		WillReturnRows(sqlmock.NewRows([]string{"attempt", "kind", "status"}).
			AddRow(1, NodeHumanApproval, RunWaitingHuman))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_node_runs
			SET approval_decision=?,approver_user_id=?,approver_tenant_id=?,
				approval_comment=?,approval_decided_at=?,
				status=?,error_code=?,ended_at=?
			WHERE workflow_run_id=? AND node_id=? AND attempt=? AND status=?`)).
		WithArgs(
			ApprovalRejected, int64(99), "tenant-a", "rejected", sqlmock.AnyArg(),
			RunFailed, "human_approval_rejected", sqlmock.AnyArg(),
			"run_1", "approve", 1, RunWaitingHuman,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workflow_runs
			SET status=?,error_code=?,ended_at=? WHERE id=? AND status=?`)).
		WithArgs(
			RunFailed, "human_approval_rejected", sqlmock.AnyArg(),
			"run_1", RunWaitingHuman,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT COALESCE(MAX(seq),0)+1 FROM runtime_events
		 WHERE stream_kind='workflow' AND stream_id=?`,
	)).WithArgs("run_1").WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(8))
	for _, expected := range []struct {
		seq     int64
		kind    string
		nodeID  string
		summary string
	}{
		{seq: 8, kind: "human_rejected", nodeID: "approve", summary: "workflow node rejected"},
		{seq: 9, kind: "node_failed", nodeID: "approve", summary: "workflow node failed"},
		{seq: 10, kind: "workflow_failed", summary: "workflow failed"},
	} {
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_events(
			stream_kind,stream_id,seq,kind,node_id,summary,detail_json,created_at)
			VALUES('workflow',?,?,?,?,?,?,?)`)).
			WithArgs(
				"run_1", expected.seq, expected.kind, expected.nodeID,
				expected.summary, nil, sqlmock.AnyArg(),
			).
			WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()

	transition, err := workflowStore.DecideHumanApproval(
		context.Background(), approval, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !transition.Applied || transition.RunStatus != RunFailed ||
		transition.Approval != approval {
		t.Fatalf("approval transition = %+v", transition)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDecideHumanApprovalReturnsExistingFactForSameDecision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	existing := WorkflowApproval{
		WorkflowRunID: "run_1", NodeID: "approve",
		Decision: ApprovalApproved, ApproverUserID: 42,
		ApproverTenantID: "tenant-a", Comment: "original",
		DecidedAt: time.Now().UTC().Add(-time.Minute),
	}
	requested := existing
	requested.ApproverUserID = 99
	requested.Comment = "retry"
	requested.DecidedAt = time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunSucceeded),
	)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		workflow_run_id,node_id,approval_decision,approver_user_id,approver_tenant_id,
		approval_comment,approval_decided_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=? AND approval_decision IS NOT NULL
		ORDER BY attempt DESC LIMIT 1`)).
		WithArgs("run_1", "approve").
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_run_id", "node_id", "decision", "approver_user_id",
			"approver_tenant_id", "comment", "decided_at",
		}).AddRow(
			existing.WorkflowRunID, existing.NodeID, existing.Decision,
			existing.ApproverUserID, existing.ApproverTenantID,
			existing.Comment, existing.DecidedAt,
		))
	mock.ExpectCommit()

	transition, err := workflowStore.DecideHumanApproval(
		context.Background(), requested, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if transition.Applied || transition.RunStatus != RunSucceeded ||
		transition.Approval != existing {
		t.Fatalf("approval transition = %+v", transition)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDecideHumanApprovalRejectsConflictingDecision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	decidedAt := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
	)).WithArgs("run_1").WillReturnRows(
		sqlmock.NewRows([]string{"status"}).AddRow(RunFailed),
	)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		workflow_run_id,node_id,approval_decision,approver_user_id,approver_tenant_id,
		approval_comment,approval_decided_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND node_id=? AND approval_decision IS NOT NULL
		ORDER BY attempt DESC LIMIT 1`)).
		WithArgs("run_1", "approve").
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_run_id", "node_id", "decision", "approver_user_id",
			"approver_tenant_id", "comment", "decided_at",
		}).AddRow(
			"run_1", "approve", ApprovalRejected, int64(42),
			"tenant-a", "original", decidedAt,
		))
	mock.ExpectRollback()

	_, err = workflowStore.DecideHumanApproval(context.Background(), WorkflowApproval{
		WorkflowRunID: "run_1", NodeID: "approve",
		Decision: ApprovalApproved, ApproverUserID: 99,
		ApproverTenantID: "tenant-a", DecidedAt: time.Now().UTC(),
	}, nil)
	if !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("approval conflict error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDecideHumanApprovalClassifiesLockedStateErrors(t *testing.T) {
	tests := []struct {
		name       string
		runStatus  RunStatus
		nodeRows   *sqlmock.Rows
		nodeErr    error
		want       error
		expectNode bool
	}{
		{
			name:      "run not waiting",
			runStatus: RunRunning,
			want:      ErrConflict,
		},
		{
			name:       "node missing",
			runStatus:  RunWaitingHuman,
			nodeErr:    sql.ErrNoRows,
			want:       ErrNotFound,
			expectNode: true,
		},
		{
			name:      "node kind does not require approval",
			runStatus: RunWaitingHuman,
			nodeRows: sqlmock.NewRows([]string{"attempt", "kind", "status"}).
				AddRow(1, NodeAgent, RunWaitingHuman),
			want:       ErrConflict,
			expectNode: true,
		},
		{
			name:      "node not waiting",
			runStatus: RunWaitingHuman,
			nodeRows: sqlmock.NewRows([]string{"attempt", "kind", "status"}).
				AddRow(1, NodeHumanApproval, RunRunning),
			want:       ErrConflict,
			expectNode: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			workflowStore, err := NewStore(db)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(
				`SELECT status FROM workflow_runs WHERE id=? LIMIT 1 FOR UPDATE`,
			)).WithArgs("run_1").WillReturnRows(
				sqlmock.NewRows([]string{"status"}).AddRow(test.runStatus),
			)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT
				workflow_run_id,node_id,approval_decision,approver_user_id,approver_tenant_id,
				approval_comment,approval_decided_at
				FROM workflow_node_runs
				WHERE workflow_run_id=? AND node_id=? AND approval_decision IS NOT NULL
				ORDER BY attempt DESC LIMIT 1`)).
				WithArgs("run_1", "approve").
				WillReturnError(sql.ErrNoRows)
			if test.expectNode {
				query := mock.ExpectQuery(regexp.QuoteMeta(`SELECT attempt,kind,status
					FROM workflow_node_runs
					WHERE workflow_run_id=? AND node_id=?
					ORDER BY attempt DESC LIMIT 1 FOR UPDATE`)).
					WithArgs("run_1", "approve")
				if test.nodeErr != nil {
					query.WillReturnError(test.nodeErr)
				} else {
					query.WillReturnRows(test.nodeRows)
				}
			}
			mock.ExpectRollback()

			_, err = workflowStore.DecideHumanApproval(
				context.Background(),
				WorkflowApproval{
					WorkflowRunID:  "run_1",
					NodeID:         "approve",
					Decision:       ApprovalRejected,
					ApproverUserID: 41,
					DecidedAt:      time.Now().UTC(),
				},
				nil,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("DecideHumanApproval error = %v, want %v", err, test.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLoadFullRunStateRestoresDurableCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now().UTC().Add(-2 * time.Minute)
	succeededAt := startedAt.Add(30 * time.Second)
	approvedAt := startedAt.Add(time.Minute)
	budget := WorkflowBudget{
		MaxNodes: 3, MaxParallelism: 1,
		Timeout: 5 * time.Minute, MaxHandoffBytes: 4096,
	}
	runUsage := WorkflowUsage{
		InputTokens: 88, OutputTokens: 21, ReasoningTokens: 4, TotalTokens: 113,
		ToolCalls: 5, CostMicros: 37, Retries: 1,
	}
	agentUsage := runUsage
	budgetJSON, err := json.Marshal(budget)
	if err != nil {
		t.Fatal(err)
	}
	actorPermissions := agentapi.PermissionPolicy{
		Scopes: []string{"knowledge.read", "knowledge.write"},
	}
	scenarioPermissions := agentapi.PermissionPolicy{
		Scopes: []string{"knowledge.read"},
	}
	actorPermissionsJSON, err := json.Marshal(actorPermissions)
	if err != nil {
		t.Fatal(err)
	}
	scenarioPermissionsJSON, err := json.Marshal(scenarioPermissions)
	if err != nil {
		t.Fatal(err)
	}
	selection := DefinitionSelection{
		RuleVersion: 5, RuleHash: "rule_hash", CandidateVersion: 3,
		BucketBasisPoints: 9100, PercentageBasisPoints: 4000,
		StableKeyHash: "stable_key_hash", Reason: "rollout_default",
	}
	selectionJSON, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		id,parent_run_id,workflow_id,workflow_version,workflow_hash,selection_json,input_hash,actor_user_id,
		actor_tenant_id,actor_permissions_json,scenario,scenario_permissions_json,
		status,budget_json,input_tokens,output_tokens,reasoning_tokens,total_tokens,
		tool_call_count,cost_micros,retry_count,error_code,started_at,ended_at
		FROM workflow_runs WHERE id=? LIMIT 1`)).
		WithArgs("run_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "parent_run_id", "workflow_id", "workflow_version", "workflow_hash", "selection_json", "input_hash",
			"actor_user_id", "actor_tenant_id", "actor_permissions_json", "scenario",
			"scenario_permissions_json", "status", "budget_json", "input_tokens",
			"output_tokens", "reasoning_tokens", "total_tokens", "tool_call_count",
			"cost_micros", "retry_count", "error_code", "started_at", "ended_at",
		}).AddRow(
			"run_1", "qa_parent_1", "delivery.approval", int64(3), "workflow_hash", selectionJSON, "input_hash",
			int64(41), "tenant-a", actorPermissionsJSON, "approval.test",
			scenarioPermissionsJSON, RunRunning, budgetJSON,
			runUsage.InputTokens, runUsage.OutputTokens, runUsage.ReasoningTokens,
			runUsage.TotalTokens, runUsage.ToolCalls, runUsage.CostMicros, runUsage.Retries,
			"", startedAt, nil,
		))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		current.workflow_run_id,current.node_id,current.attempt,current.kind,
		current.agent_run_id,current.input_handoff_ids_json,current.output_handoff_id,
		current.status,current.error_code,current.input_tokens,current.output_tokens,
		current.reasoning_tokens,current.total_tokens,current.tool_call_count,
		current.cost_micros,current.retry_count,current.started_at,
		(SELECT MIN(first.started_at) FROM workflow_node_runs first
			WHERE first.workflow_run_id=current.workflow_run_id
			AND first.node_id=current.node_id),
		current.ended_at
		FROM workflow_node_runs current
		WHERE current.workflow_run_id=? AND current.attempt=(
			SELECT MAX(latest.attempt) FROM workflow_node_runs latest
			WHERE latest.workflow_run_id=current.workflow_run_id
			AND latest.node_id=current.node_id)
		ORDER BY current.node_id`)).
		WithArgs("run_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_run_id", "node_id", "attempt", "kind", "agent_run_id",
			"input_handoff_ids_json", "output_handoff_id", "status",
			"error_code", "input_tokens", "output_tokens", "reasoning_tokens",
			"total_tokens", "tool_call_count", "cost_micros", "retry_count",
			"started_at", "first_started_at", "ended_at",
		}).
			AddRow(
				"run_1", "approve", 1, NodeHumanApproval, "",
				[]byte(`["handoff_gate"]`), "handoff_approved", RunSucceeded,
				"", int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0),
				succeededAt, succeededAt, approvedAt,
			).
			AddRow(
				"run_1", "gate.check", 1, NodeGate, "",
				[]byte(`["handoff_review"]`), "handoff_gate", RunSucceeded,
				"", int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0),
				startedAt.Add(20*time.Second), startedAt.Add(20*time.Second), succeededAt,
			).
			AddRow(
				"run_1", "review.before", 1, NodeAgent, "agent_run_1",
				[]byte(`["handoff_input"]`), "handoff_review", RunSucceeded,
				"", agentUsage.InputTokens, agentUsage.OutputTokens,
				agentUsage.ReasoningTokens, agentUsage.TotalTokens, agentUsage.ToolCalls,
				agentUsage.CostMicros, agentUsage.Retries,
				startedAt, startedAt, startedAt.Add(20*time.Second),
			))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		id,workflow_run_id,producer_node_id,producer_run_id,schema_id,schema_version,
		payload_json,references_json,completeness,content_hash,created_at
		FROM handoff_artifacts
		WHERE workflow_run_id=?
		ORDER BY created_at,id`)).
		WithArgs("run_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workflow_run_id", "producer_node_id", "producer_run_id",
			"schema_id", "schema_version", "payload_json", "references_json",
			"completeness", "content_hash", "created_at",
		}).
			AddRow(
				"handoff_input", "run_1", "workflow.input", "",
				"review.subject", int64(1), []byte(`{"subject":"x"}`), []byte(`[]`),
				Complete, "input_hash", startedAt,
			).
			AddRow(
				"handoff_review", "run_1", "review.before", "agent_run_1",
				"review.report", int64(1), []byte(`{"result":"reviewed"}`), []byte(`[]`),
				Complete, "review_hash", startedAt.Add(20*time.Second),
			).
			AddRow(
				"handoff_gate", "run_1", "gate.check", "",
				"review.report", int64(1), []byte(`{"decision":"pass"}`), []byte(`[]`),
				Complete, "gate_hash", succeededAt,
			).
			AddRow(
				"handoff_approved", "run_1", "approve", "",
				"review.report", int64(1), []byte(`{"decision":"pass"}`), []byte(`[]`),
				Complete, "approved_hash", approvedAt,
			))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		node_id,gate_id,gate_subject_hash,gate_decision,gate_reason_codes_json,
		gate_finding_ids_json,gate_evaluated_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND gate_decision_id IS NOT NULL
		ORDER BY node_id,attempt`)).
		WithArgs("run_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"node_id", "gate_id", "subject_hash", "decision", "reason_codes_json",
			"finding_ids_json", "evaluated_at",
		}).AddRow(
			"gate.check", "quality_gate", "subject_hash", "pass",
			[]byte(`["quality_ok"]`), []byte(`["finding_1"]`), succeededAt,
		))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		workflow_run_id,node_id,approval_decision,approver_user_id,
		approver_tenant_id,approval_comment,approval_decided_at
		FROM workflow_node_runs
		WHERE workflow_run_id=? AND approval_decision IS NOT NULL
		ORDER BY node_id,attempt`)).
		WithArgs("run_1").
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_run_id", "node_id", "decision", "approver_user_id",
			"approver_tenant_id", "comment", "decided_at",
		}).AddRow(
			"run_1", "approve", ApprovalApproved, int64(99),
			"tenant-a", "approved", approvedAt,
		))

	state, err := workflowStore.LoadFullRunState(context.Background(), "run_1")
	if err != nil {
		t.Fatal(err)
	}
	if state.Run.ParentRunID != "qa_parent_1" || state.Run.ActorUserID != 41 ||
		state.Run.ActorTenantID != "tenant-a" ||
		state.Run.Selection != selection ||
		state.Run.Usage != runUsage ||
		len(state.Run.ActorPermissions.Scopes) != 2 ||
		state.Run.ActorPermissions.Scopes[0] != "knowledge.read" ||
		state.Run.ActorPermissions.Scopes[1] != "knowledge.write" ||
		len(state.Run.ScenarioPermissions.Scopes) != 1 ||
		state.Run.ScenarioPermissions.Scopes[0] != "knowledge.read" ||
		!state.Run.StartedAt.Equal(startedAt) {
		t.Fatalf("run snapshot = %+v", state.Run)
	}
	if state.Input.ID != "handoff_input" ||
		string(state.Input.Payload) != `{"subject":"x"}` {
		t.Fatalf("input = %+v", state.Input)
	}
	for _, expected := range []struct {
		nodeID    string
		handoffID string
	}{
		{nodeID: "review.before", handoffID: "handoff_review"},
		{nodeID: "gate.check", handoffID: "handoff_gate"},
		{nodeID: "approve", handoffID: "handoff_approved"},
	} {
		if state.NodeOutputs[expected.nodeID].ID != expected.handoffID {
			t.Fatalf("node outputs = %+v", state.NodeOutputs)
		}
	}
	approvalNode := state.Nodes["approve"]
	gate := state.Gates["gate.check"]
	if len(approvalNode.InputHandoffIDs) != 1 ||
		approvalNode.InputHandoffIDs[0] != "handoff_gate" ||
		!approvalNode.FirstStartedAt.Equal(succeededAt) ||
		len(gate.ReasonCodes) != 1 || gate.ReasonCodes[0] != "quality_ok" ||
		len(gate.FindingIDs) != 1 || gate.FindingIDs[0] != "finding_1" ||
		state.Approvals["approve"].ApproverUserID != 99 ||
		!state.Approvals["approve"].DecidedAt.Equal(approvedAt) {
		t.Fatalf(
			"checkpoint nodes=%+v gates=%+v approvals=%+v",
			state.Nodes, state.Gates, state.Approvals,
		)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
