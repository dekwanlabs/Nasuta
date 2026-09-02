package run

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

type nonNilDatabaseTimestamp struct{}

func (nonNilDatabaseTimestamp) Match(value driver.Value) bool {
	timestamp, ok := value.(time.Time)
	return ok && !timestamp.IsZero()
}

func TestUpsertDelegationCheckpointFillsRequiredTimestamps(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}

	mock.ExpectExec("INSERT INTO agent_delegation_checkpoints").
		WithArgs(
			"parent-1", "delegation-1", 0, "", "", DelegationCheckpointPending,
			"", "", "", "", nonNilDatabaseTimestamp{}, nonNilDatabaseTimestamp{},
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.UpsertDelegationCheckpoint(context.Background(), DelegationCheckpoint{
		ParentRunID: "parent-1", DelegationID: "delegation-1", Status: DelegationCheckpointPending,
	}); err != nil {
		t.Fatalf("UpsertDelegationCheckpoint: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreAddStepPersistsInlineToolResultAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	step := StepRow{
		RunID: "run-1", StepNo: 2, Kind: StepKindToolResult,
		TraceID: "trace-1", ToolCallID: "call-1", Tool: "lookup", Args: `{}`,
		Content: `{"sn":"SN-1"}`, PromptContent: `{"sn":"SN-1"}`,
		AuthoritativeSHA256: "sha-authoritative", PromptSHA256: "sha-prompt", SizeBytes: 13,
		Coverage:       tool.EvidenceCoverage{Partial: true, OmittedItems: 2},
		AnswerContract: tool.AnswerContract{RequiredLiterals: []string{"SN-1"}},
		DurationMs:     12, CreatedAt: "2026-07-31T01:02:03Z",
	}
	coverageJSON, _ := json.Marshal(step.Coverage)
	contractJSON, _ := json.Marshal(step.AnswerContract)
	adoptionsJSON, _ := json.Marshal(step.DelegationAdoptions)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_steps").
		WithArgs(
			step.RunID, step.StepNo, step.Kind, step.TraceID, "", step.ToolCallID, step.Tool, step.Args,
			step.Content, step.PromptContent, step.AuthoritativeSHA256, step.PromptSHA256, step.SizeBytes,
			coverageJSON, contractJSON, adoptionsJSON, false, "", 0, 0, step.DurationMs,
			nil, nil, sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.AddStep(step); err != nil {
		t.Fatalf("AddStep: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreAddStepPersistsArtifactWithReferenceAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	step := StepRow{
		RunID: "run-large", StepNo: 4, Kind: StepKindToolResult,
		TraceID: "trace-large", ArtifactID: "artifact-large", ToolCallID: "call-large",
		Tool: "lookup", Args: `{"page_size":100}`,
		Content: `{"items":[{"sn":"SN-1"}]}`, PromptContent: `{"error":"tool_result_exceeds_context_budget"}`,
		AuthoritativeSHA256: "sha-authoritative", PromptSHA256: "sha-prompt", SizeBytes: 28,
		Coverage:       tool.EvidenceCoverage{Partial: true, OmittedItems: 4},
		AnswerContract: tool.AnswerContract{RequiredLiterals: []string{"SN-1"}},
		Failed:         true, DeliveryError: "tool_result_exceeds_context_budget",
		CreatedAt: "2026-07-31T01:02:03Z",
	}
	coverageJSON, _ := json.Marshal(step.Coverage)
	contractJSON, _ := json.Marshal(step.AnswerContract)
	adoptionsJSON, _ := json.Marshal(step.DelegationAdoptions)

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_steps").
		WithArgs(
			step.RunID, step.StepNo, step.Kind, step.TraceID, step.ArtifactID, step.ToolCallID, step.Tool, step.Args,
			nil, step.PromptContent, step.AuthoritativeSHA256, step.PromptSHA256, step.SizeBytes,
			coverageJSON, contractJSON, adoptionsJSON, true, step.DeliveryError, 0, 0, 0,
			[]byte(step.Content), "application/json", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := store.AddStep(step); err != nil {
		t.Fatalf("AddStep: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreAddStepRollsBackWhenMergedArtifactPersistenceFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	step := StepRow{
		RunID: "run-large", StepNo: 4, Kind: StepKindToolResult,
		ArtifactID: "artifact-large", ToolCallID: "call-large",
		Content: "complete authoritative result", SizeBytes: 29,
	}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_steps").
		WillReturnError(errors.New("artifact storage unavailable"))
	mock.ExpectRollback()

	err = store.AddStep(step)
	if err == nil || !strings.Contains(err.Error(), "persist agent step") {
		t.Fatalf("AddStep error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreGetToolArtifactUsesBoundedOwnedRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	createdAt := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	mock.ExpectQuery("SELECT s\\.artifact_id,r\\.session_id,s\\.run_id,s\\.tool_call_id,.*SUBSTRING\\(s\\.artifact_content,\\?,\\?\\)").
		WithArgs(int64(4), 4, "artifact-1", int64(42), "session-1", "session-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "session_id", "run_id", "tool_call_id", "content", "content_type", "sha256", "size_bytes", "coverage_json", "created_at",
		}).AddRow(
			"artifact-1", "session-1", "run-1", "call-1", []byte("defg"), "application/json", "sha", 12,
			`{"partial":true,"omitted_items":3}`, createdAt,
		))

	chunk, err := store.GetToolArtifact(42, "session-1", "artifact-1", 3, 4)
	if err != nil {
		t.Fatalf("GetToolArtifact: %v", err)
	}
	if chunk.Content != "defg" || chunk.Offset != 3 || chunk.NextOffset != 7 || !chunk.HasMore {
		t.Fatalf("chunk = %+v", chunk)
	}
	if !chunk.Coverage.Partial || chunk.Coverage.OmittedItems != 3 || chunk.CreatedAt == "" {
		t.Fatalf("chunk metadata = %+v", chunk)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreGetForUserConstrainsRunOwnershipBeforeLoadingSteps(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	mock.ExpectQuery("FROM agent_runs WHERE id=\\? AND user_id=\\?").
		WithArgs("run-1", int64(42)).
		WillReturnError(sql.ErrNoRows)

	_, err = store.GetForUser("run-1", 42)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetForUser error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreGetForUserDoesNotTreatZeroAsOwnershipBypass(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	mock.ExpectQuery("FROM agent_runs WHERE id=\\? AND user_id=\\?").
		WithArgs("run-1", int64(0)).
		WillReturnError(sql.ErrNoRows)

	_, err = store.GetForUser("run-1", 0)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetForUser error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreGetControlForUserUsesNarrowOwnedRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	mock.ExpectQuery(
		"SELECT id,run_kind,status,workflow_run_id,user_id.*"+
			"FROM agent_runs WHERE id=\\? AND user_id=\\? LIMIT 1",
	).
		WithArgs("run-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_kind", "status", "workflow_run_id", "user_id",
		}).AddRow(
			"run-1", KindAgent, StatusRunning, "workflow-1", int64(42),
		))

	record, err := store.GetControlForUser("run-1", 42)
	if err != nil {
		t.Fatalf("GetControlForUser: %v", err)
	}
	if record.ID != "run-1" || record.RunKind != KindAgent ||
		record.Status != StatusRunning || record.WorkflowRunID != "workflow-1" ||
		record.UserID != 42 {
		t.Fatalf("record = %+v", record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreRecoverInterruptedSettlesDelegationWithAuthoritativeUsage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	task := interruptedDelegationTask{
		ParentRunID:  "parent-1",
		DelegationID: "del-1",
		TaskIndex:    2,
		ChildRunID:   "child-2",
		CapabilityID: "knowledge.code.inspect",
		Usage: agentapi.Usage{
			InputTokens: 11, OutputTokens: 7, ReasoningTokens: 3,
			TotalTokens: 21, CostMicros: 250,
		},
		ToolCalls: 4,
	}
	artifact, err := interruptedDelegationReportArtifact(task)
	if err != nil {
		t.Fatalf("interruptedDelegationReportArtifact: %v", err)
	}
	var report agentapi.DelegationReport
	if err := json.Unmarshal(artifact.Content, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Status != agentapi.DelegationInterrupted ||
		report.Error == nil ||
		report.Error.Code != interruptedErrorCode ||
		report.Usage.TotalTokens != task.Usage.TotalTokens ||
		report.Usage.ToolCalls != task.ToolCalls ||
		artifact.ID != delegationReportArtifactID(stableDelegationReportID(task.ChildRunID)) {
		t.Fatalf("recovery report = %+v, artifact = %+v", report, artifact)
	}
	usageRaw, err := json.Marshal(task.Usage)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery("FROM agent_delegation_tasks t.*FOR UPDATE").
		WillReturnRows(interruptedDelegationTaskRows().AddRow(
			task.ParentRunID,
			task.DelegationID,
			task.TaskIndex,
			task.ChildRunID,
			task.CapabilityID,
			task.Usage.InputTokens,
			task.Usage.OutputTokens,
			task.Usage.ReasoningTokens,
			task.Usage.TotalTokens,
			task.Usage.CostMicros,
			task.ToolCalls,
		))
	mock.ExpectExec("UPDATE agent_runs SET status=\\?,error_code=\\?,ended_at=\\?").
		WithArgs(
			StatusAborted,
			interruptedErrorCode,
			sqlmock.AnyArg(),
			KindAgent,
			StatusRunning,
			StatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectRecoveryAttemptClose(mock)
	mock.ExpectExec("INSERT INTO agent_run_artifacts").
		WithArgs(
			artifact.ID,
			artifact.RunID,
			artifact.Kind,
			artifact.Schema.ID,
			artifact.Schema.Version,
			artifact.ContentHash,
			artifact.Content,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE agent_delegation_tasks").
		WithArgs(
			usageRaw,
			artifact.ID,
			sqlmock.AnyArg(),
			task.ParentRunID,
			task.DelegationID,
			task.TaskIndex,
			task.ChildRunID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectInterruptedCheckpoint(mock, task)
	mock.ExpectCommit()

	recovered, err := store.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreRecoverInterruptedSettlesMissingChildWithZeroUsage(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	task := interruptedDelegationTask{
		ParentRunID:  "parent-1",
		DelegationID: "del-1",
		TaskIndex:    0,
		ChildRunID:   "child-not-created",
		CapabilityID: "knowledge.code.inspect",
	}
	artifact, err := interruptedDelegationReportArtifact(task)
	if err != nil {
		t.Fatal(err)
	}
	usageRaw, _ := json.Marshal(agentapi.Usage{})

	mock.ExpectBegin()
	mock.ExpectQuery("FROM agent_delegation_tasks t.*FOR UPDATE").
		WillReturnRows(interruptedDelegationTaskRows().AddRow(
			task.ParentRunID,
			task.DelegationID,
			task.TaskIndex,
			task.ChildRunID,
			task.CapabilityID,
			int64(0),
			int64(0),
			int64(0),
			int64(0),
			int64(0),
			int64(0),
		))
	mock.ExpectExec("UPDATE agent_runs SET status=\\?,error_code=\\?,ended_at=\\?").
		WithArgs(
			StatusAborted,
			interruptedErrorCode,
			sqlmock.AnyArg(),
			KindAgent,
			StatusRunning,
			StatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectRecoveryAttemptClose(mock)
	mock.ExpectExec("INSERT INTO agent_run_artifacts").
		WithArgs(
			artifact.ID,
			artifact.RunID,
			artifact.Kind,
			artifact.Schema.ID,
			artifact.Schema.Version,
			artifact.ContentHash,
			artifact.Content,
			sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE agent_delegation_tasks").
		WithArgs(
			usageRaw,
			artifact.ID,
			sqlmock.AnyArg(),
			task.ParentRunID,
			task.DelegationID,
			task.TaskIndex,
			task.ChildRunID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectInterruptedCheckpoint(mock, task)
	mock.ExpectCommit()

	recovered, err := store.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recovered = %d, want 0", recovered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreRecoverInterruptedRollsBackOnArtifactFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery("FROM agent_delegation_tasks t.*FOR UPDATE").
		WillReturnRows(interruptedDelegationTaskRows().AddRow(
			"parent-1",
			"del-1",
			0,
			"child-0",
			"knowledge.code.inspect",
			int64(0),
			int64(0),
			int64(0),
			int64(0),
			int64(0),
			int64(0),
		))
	mock.ExpectExec("UPDATE agent_runs SET status=\\?,error_code=\\?,ended_at=\\?").
		WithArgs(
			StatusAborted,
			interruptedErrorCode,
			sqlmock.AnyArg(),
			KindAgent,
			StatusRunning,
			StatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectRecoveryAttemptClose(mock)
	mock.ExpectExec("INSERT INTO agent_run_artifacts").
		WillReturnError(errors.New("artifact unavailable"))
	mock.ExpectRollback()

	if _, err := store.RecoverInterrupted(); err == nil {
		t.Fatal("RecoverInterrupted succeeded after artifact failure")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectRecoveryAttemptClose(mock sqlmock.Sqlmock) {
	mock.ExpectExec("UPDATE agent_delegation_attempts a JOIN agent_delegation_tasks t").
		WithArgs(
			DelegationAttemptInterrupted,
			interruptedErrorCode,
			"delegation attempt was interrupted during process recovery",
			sqlmock.AnyArg(),
			DelegationAttemptRunning,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func expectInterruptedCheckpoint(mock sqlmock.Sqlmock, task interruptedDelegationTask) {
	mock.ExpectExec("INSERT INTO agent_delegation_checkpoints").
		WithArgs(
			task.ParentRunID, task.DelegationID, task.TaskIndex, "", "",
			DelegationCheckpointInterrupted, task.ChildRunID, sqlmock.AnyArg(),
			interruptedErrorCode,
			"delegation was interrupted during process recovery",
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func interruptedDelegationTaskRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"parent_run_id",
		"delegation_id",
		"task_index",
		"child_run_id",
		"capability_id",
		"input_tokens",
		"output_tokens",
		"reasoning_tokens",
		"total_tokens",
		"cost_micros",
		"tool_call_count",
	})
}

func TestRunStoreDeleteBySessionDeletesStepsBeforeRuns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}

	mock.ExpectBegin()
	mock.ExpectExec("DELETE c FROM agent_llm_calls").
		WithArgs("session-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE s FROM agent_steps").
		WithArgs("session-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM agent_runs").
		WithArgs("session-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.DeleteBySession("session-1", 42); err != nil {
		t.Fatalf("DeleteBySession: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreGetToolArtifactKeepsUTF8ChunkBoundaries(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	partial := []byte("你好")[:5]
	mock.ExpectQuery("SELECT s\\.artifact_id,r\\.session_id,s\\.run_id,s\\.tool_call_id,.*SUBSTRING\\(s\\.artifact_content,\\?,\\?\\)").
		WithArgs(int64(1), 5, "artifact-utf8", int64(42), "session-1", "session-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "session_id", "run_id", "tool_call_id", "content", "content_type", "sha256", "size_bytes", "coverage_json", "created_at",
		}).AddRow(
			"artifact-utf8", "session-1", "run-1", "call-1", partial, "text/plain; charset=utf-8", "sha", 6, `{}`,
			time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC),
		))

	chunk, err := store.GetToolArtifact(42, "session-1", "artifact-utf8", 0, 5)
	if err != nil {
		t.Fatalf("GetToolArtifact: %v", err)
	}
	if chunk.Content != "你" || chunk.NextOffset != 3 || !chunk.HasMore {
		t.Fatalf("chunk = %+v", chunk)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreGetReturnsFullInlineTraceAndBoundedArtifactPreview(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	createdAt := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	mock.ExpectQuery("FROM agent_runs WHERE id=\\? AND user_id=\\?").
		WithArgs("run-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_kind", "user_id", "session_id", "agent_id", "definition_version", "definition_hash", "selection_json", "tool_snapshot_id",
			"input_schema_version", "output_schema_version", "parent_run_id",
			"capability_id", "capability_version", "capability_content_hash",
			"delegation_id", "delegation_depth", "run_limits_json", "capability_registry_revision",
			"workflow_run_id", "workflow_node_id",
			"question", "status", "error_code", "mode", "max_steps", "step_count", "token_used",
			"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "cost_micros", "llm_call_count",
			"peak_input_tokens", "peak_reserved_tokens", "evidence_status", "forced_conclusion", "evidence_result_count",
			"tool_call_count", "tool_failure_count", "partial_result_count", "omitted_evidence_count", "started_at", "ended_at",
		}).AddRow(
			"run-1", KindAgent, int64(42), "session-1", "qa.answerer", int64(1), strings.Repeat("a", 64),
			`{"rule_version":2,"reason":"rollout_default"}`, "tools_test",
			int64(1), int64(1), "",
			"knowledge.code.inspect", int64(3), strings.Repeat("b", 64),
			"del-1", 1, `{"max_steps":2,"max_total_tokens":500}`, uint64(9), "", "",
			"question", StatusDone, "", "", 2, 2, 10,
			100, 20, 30, 5, 135, int64(77), 2, 100, 120, EvidencePartial, false, 1, 2, 1, 1, 3, createdAt, createdAt,
		))
	mock.ExpectQuery("FROM agent_steps s").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "step_no", "kind", "trace_id", "artifact_id", "tool_call_id", "tool", "args",
			"content", "prompt_content", "authoritative_sha256", "prompt_sha256", "content_bytes",
			"coverage_json", "answer_contract_json", "delegation_adoptions_json", "failed", "delivery_error", "token_delta",
			"reasoning_tokens", "duration_ms", "created_at", "artifact_preview",
		}).
			AddRow(
				int64(1), "run-1", 1, StepKindToolResult, "trace-inline", "", "call-inline", "lookup", `{}`,
				`{"sn":"SN-inline"}`, `{"sn":"SN-inline"}`, "same-sha", "same-sha", 18,
				`{}`, `{"required_literals":["SN-inline"]}`,
				`[{"delegation_id":"del-1","adopted_report_ids":["report-1"],"status":"adopted"}]`,
				false, "", 0, 0, 10, createdAt, nil,
			).
			AddRow(
				int64(2), "run-1", 2, StepKindToolResult, "trace-artifact", "artifact-1", "call-artifact", "lookup", `{}`,
				nil, `{"error":"tool_result_exceeds_context_budget"}`, "authoritative-sha", "prompt-sha", 900000,
				`{"partial":true,"omitted_items":3}`, `{}`, nil, true, "tool_result_exceeds_context_budget", 0, 0, 20, createdAt, "authoritative artifact preview",
			))
	mock.ExpectQuery("FROM agent_llm_calls WHERE run_id=\\?").
		WithArgs("run-1", 1000).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "call_seq", "phase", "provider", "model", "input_tokens", "cached_input_tokens",
			"output_tokens", "reasoning_tokens", "total_tokens", "max_output_tokens", "duration_ms", "status", "created_at",
		}))

	detail, err := store.GetForUser("run-1", 42)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	if detail.Selection.RuleVersion != 2 || detail.Selection.Reason != "rollout_default" {
		t.Fatalf("selection = %+v", detail.Selection)
	}
	if detail.CapabilityID != "knowledge.code.inspect" ||
		detail.CapabilityVersion != 3 ||
		detail.DelegationID != "del-1" ||
		detail.RunLimits.MaxTotalTokens != 500 ||
		detail.CostMicros != 77 {
		t.Fatalf("delegation snapshot = %+v", detail.Record)
	}
	if len(detail.Steps) != 2 {
		t.Fatalf("steps = %+v", detail.Steps)
	}
	inline := detail.Steps[0]
	if inline.Content != `{"sn":"SN-inline"}` || inline.PromptContent != inline.Content || inline.ResultPreview != inline.Content {
		t.Fatalf("inline step = %+v", inline)
	}
	if len(inline.AnswerContract.RequiredLiterals) != 1 || inline.AnswerContract.RequiredLiterals[0] != "SN-inline" {
		t.Fatalf("inline contract = %+v", inline.AnswerContract)
	}
	if len(inline.DelegationAdoptions) != 1 ||
		inline.DelegationAdoptions[0].DelegationID != "del-1" ||
		inline.DelegationAdoptions[0].Status != agentapi.DelegationAdopted ||
		len(inline.DelegationAdoptions[0].AdoptedReportIDs) != 1 ||
		inline.DelegationAdoptions[0].AdoptedReportIDs[0] != "report-1" {
		t.Fatalf("inline delegation adoptions = %+v", inline.DelegationAdoptions)
	}
	artifact := detail.Steps[1]
	if artifact.Content != "" || artifact.ArtifactID != "artifact-1" || artifact.ResultPreview != "authoritative artifact preview" {
		t.Fatalf("artifact step = %+v", artifact)
	}
	if artifact.PromptContent != `{"error":"tool_result_exceeds_context_budget"}` || !artifact.Failed || !artifact.Coverage.Partial {
		t.Fatalf("artifact delivery trace = %+v", artifact)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimWorkItemByKindClaimsOneItemWithFencingLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs := &Store{db: db}
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	available := now.Add(-time.Second)
	payload := []byte(`{"parent":"p1"}`)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT work_id,run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json,state,lease_owner,lease_fence,lease_expires_at,available_at,attempt_count,last_error FROM agent_work_items WHERE`).
		WithArgs(WorkReady, sqlmock.AnyArg(), WorkRunning, sqlmock.AnyArg(), "delegation_child").
		WillReturnRows(sqlmock.NewRows([]string{
			"work_id", "run_id", "parent_run_id", "delegation_id", "task_index", "attempt_no", "kind", "payload_json", "state", "lease_owner", "lease_fence", "lease_expires_at", "available_at", "attempt_count", "last_error",
		}).AddRow("work-1", "run-1", "parent-1", "delegation-1", 2, 1, "delegation_child", payload, WorkReady, "", int64(4), nil, available, 3, ""))
	mock.ExpectExec(`UPDATE agent_work_items SET state=\?,lease_owner=\?,lease_fence=\?,lease_expires_at=\?,attempt_count=\?,updated_at=\? WHERE work_id=\?`).
		WithArgs(WorkRunning, "worker-a", int64(5), sqlmock.AnyArg(), 4, sqlmock.AnyArg(), "work-1", WorkReady, WorkRunning, WorkReady, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	item, err := rs.ClaimWorkItemByKind(context.Background(), "delegation_child", "worker-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if item.WorkID != "work-1" || item.LeaseOwner != "worker-a" || item.LeaseFence != 5 || item.AttemptCount != 4 || item.State != WorkRunning {
		t.Fatalf("claimed item = %#v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimWorkItemByKindCanReclaimExpiredLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs := &Store{db: db}
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT work_id,run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json,state,lease_owner,lease_fence,lease_expires_at,available_at,attempt_count,last_error FROM agent_work_items WHERE`).
		WithArgs(WorkReady, sqlmock.AnyArg(), WorkRunning, sqlmock.AnyArg(), "delegation_child").
		WillReturnRows(sqlmock.NewRows([]string{
			"work_id", "run_id", "parent_run_id", "delegation_id", "task_index", "attempt_no", "kind", "payload_json", "state", "lease_owner", "lease_fence", "lease_expires_at", "available_at", "attempt_count", "last_error",
		}).AddRow("work-2", "run-2", "parent-2", "delegation-2", 0, 1, "delegation_child", []byte(`{}`), WorkRunning, "dead-worker", int64(9), now.Add(-time.Second), now.Add(-time.Minute), 1, "worker lease expired"))
	mock.ExpectExec(`UPDATE agent_work_items SET state=\?,lease_owner=\?,lease_fence=\?,lease_expires_at=\?,attempt_count=\?,updated_at=\? WHERE work_id=\?`).
		WithArgs(WorkRunning, "worker-b", int64(10), sqlmock.AnyArg(), 2, sqlmock.AnyArg(), "work-2", WorkReady, WorkRunning, WorkReady, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	item, err := rs.ClaimWorkItemByKind(context.Background(), "delegation_child", "worker-b", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if item.LeaseFence != 10 || item.LeaseOwner != "worker-b" {
		t.Fatalf("reclaimed item = %#v", item)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkLeaseRenewAndCompleteRejectStaleOwner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rs := &Store{db: db}
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)

	mock.ExpectExec(`UPDATE agent_work_items SET lease_expires_at=\?,updated_at=\? WHERE work_id=\? AND state=\? AND lease_owner=\? AND lease_fence=\?`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), "work-1", WorkRunning, "old-worker", int64(1), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := rs.RenewWorkItem(context.Background(), "work-1", "old-worker", 1, now, time.Minute); err == nil {
		t.Fatal("stale owner renewal succeeded")
	}

	mock.ExpectExec(`UPDATE agent_work_items SET state=\?,lease_owner='',lease_expires_at=NULL,last_error=\?,updated_at=\? WHERE work_id=\? AND state=\? AND lease_owner=\? AND lease_fence=\?`).
		WithArgs(WorkSucceeded, "", sqlmock.AnyArg(), "work-1", WorkRunning, "old-worker", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := rs.CompleteWorkItem(context.Background(), "work-1", "old-worker", 1, WorkSucceeded, ""); err == nil {
		t.Fatal("stale owner completion succeeded")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkLeaseRejectsInvalidTTL(t *testing.T) {
	rs := &Store{}
	if _, err := rs.ClaimWorkItem(context.Background(), "worker", time.Now(), 0); err == nil {
		t.Fatal("zero TTL claim accepted")
	}
	if err := rs.RenewWorkItem(context.Background(), "work", "worker", 1, time.Now(), -time.Second); err == nil {
		t.Fatal("negative TTL renewal accepted")
	}
}

func TestEnqueueWorkItemRejectsIdentityOrPayloadConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := &Store{db: db}
	item := WorkItem{
		WorkID: "work-1", RunID: "run-1", ParentRunID: "parent-1",
		DelegationID: "delegation-1", TaskIndex: 0, AttemptNo: 1,
		Kind: "delegation_child", Payload: []byte(`{"objective":"original"}`), State: WorkReady,
	}
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO agent_work_items`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json FROM agent_work_items WHERE work_id=\? FOR UPDATE`).
		WithArgs("work-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"run_id", "parent_run_id", "delegation_id", "task_index", "attempt_no", "kind", "payload_json",
		}).AddRow("run-1", "parent-1", "delegation-1", 0, 1, "delegation_child", []byte(`{"objective":"tampered"}`)))
	mock.ExpectRollback()
	if err := store.EnqueueWorkItem(context.Background(), item); !errors.Is(err, ErrWorkItemConflict) {
		t.Fatalf("error = %v, want ErrWorkItemConflict", err)
	}

	// A semantically identical JSON redelivery is idempotent and commits without
	// replacing any mutable queue state owned by the current worker. MySQL JSON
	// may normalize whitespace and object key order on storage.
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO agent_work_items`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT run_id,parent_run_id,delegation_id,task_index,attempt_no,kind,payload_json FROM agent_work_items WHERE work_id=\? FOR UPDATE`).
		WithArgs("work-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"run_id", "parent_run_id", "delegation_id", "task_index", "attempt_no", "kind", "payload_json",
		}).AddRow("run-1", "parent-1", "delegation-1", 0, 1, "delegation_child", []byte(`{ "objective" : "original" }`)))
	mock.ExpectCommit()
	if err := store.EnqueueWorkItem(context.Background(), item); err != nil {
		t.Fatalf("idempotent enqueue: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
