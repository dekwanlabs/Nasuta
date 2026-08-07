package agent

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestRunStoreAddStepPersistsInlineToolResultAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
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

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_steps").
		WithArgs(
			step.RunID, step.StepNo, step.Kind, step.TraceID, "", step.ToolCallID, step.Tool, step.Args,
			step.Content, step.PromptContent, step.AuthoritativeSHA256, step.PromptSHA256, step.SizeBytes,
			coverageJSON, contractJSON, false, "", 0, 0, step.DurationMs, sqlmock.AnyArg(),
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

func TestRunStoreAddStepPersistsArtifactBeforeReference(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
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

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_tool_result_artifacts").
		WithArgs(
			step.ArtifactID, step.ToolCallID, []byte(step.Content), "application/json",
			step.AuthoritativeSHA256, step.SizeBytes, coverageJSON, sqlmock.AnyArg(), step.RunID,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO agent_steps").
		WithArgs(
			step.RunID, step.StepNo, step.Kind, step.TraceID, step.ArtifactID, step.ToolCallID, step.Tool, step.Args,
			nil, step.PromptContent, step.AuthoritativeSHA256, step.PromptSHA256, step.SizeBytes,
			coverageJSON, contractJSON, true, step.DeliveryError, 0, 0, 0, sqlmock.AnyArg(),
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

func TestRunStoreAddStepRollsBackWhenArtifactPersistenceFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	step := StepRow{
		RunID: "run-large", StepNo: 4, Kind: StepKindToolResult,
		ArtifactID: "artifact-large", ToolCallID: "call-large",
		Content: "complete authoritative result", SizeBytes: 29,
	}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_tool_result_artifacts").
		WillReturnError(errors.New("artifact storage unavailable"))
	mock.ExpectRollback()

	err = store.AddStep(step)
	if err == nil || !strings.Contains(err.Error(), "persist tool result artifact") {
		t.Fatalf("AddStep error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreGetToolResultArtifactUsesBoundedOwnedRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	createdAt := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	mock.ExpectQuery("SELECT id,session_id,run_id,tool_call_id,SUBSTRING\\(content,\\?,\\?\\)").
		WithArgs(int64(4), 4, "artifact-1", int64(42), "session-1", "session-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "session_id", "run_id", "tool_call_id", "content", "content_type", "sha256", "size_bytes", "coverage_json", "created_at",
		}).AddRow(
			"artifact-1", "session-1", "run-1", "call-1", []byte("defg"), "application/json", "sha", 12,
			`{"partial":true,"omitted_items":3}`, createdAt,
		))

	chunk, err := store.GetToolResultArtifact(42, "session-1", "artifact-1", 3, 4)
	if err != nil {
		t.Fatalf("GetToolResultArtifact: %v", err)
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
	store := &RunStore{db: db}
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
	store := &RunStore{db: db}
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

func TestRunStoreDeleteBySessionDeletesArtifactsBeforeRuns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}

	mock.ExpectBegin()
	mock.ExpectExec("DELETE c FROM agent_llm_calls").
		WithArgs("session-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE a FROM agent_tool_result_artifacts").
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

func TestRunStoreGetToolResultArtifactKeepsUTF8ChunkBoundaries(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	partial := []byte("你好")[:5]
	mock.ExpectQuery("SELECT id,session_id,run_id,tool_call_id,SUBSTRING\\(content,\\?,\\?\\)").
		WithArgs(int64(1), 5, "artifact-utf8", int64(42), "session-1", "session-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "session_id", "run_id", "tool_call_id", "content", "content_type", "sha256", "size_bytes", "coverage_json", "created_at",
		}).AddRow(
			"artifact-utf8", "session-1", "run-1", "call-1", partial, "text/plain; charset=utf-8", "sha", 6, `{}`,
			time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC),
		))

	chunk, err := store.GetToolResultArtifact(42, "session-1", "artifact-utf8", 0, 5)
	if err != nil {
		t.Fatalf("GetToolResultArtifact: %v", err)
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
	store := &RunStore{db: db}
	createdAt := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	mock.ExpectQuery("FROM agent_runs WHERE id=\\? AND user_id=\\?").
		WithArgs("run-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "user_id", "session_id", "agent_id", "definition_version", "definition_hash", "selection_json", "tool_snapshot_id",
			"input_schema_version", "output_schema_version", "parent_run_id", "workflow_run_id", "workflow_node_id",
			"question", "status", "error_code", "mode", "max_steps", "step_count", "token_used",
			"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "llm_call_count",
			"peak_input_tokens", "peak_reserved_tokens", "evidence_status", "forced_conclusion", "evidence_result_count",
			"tool_call_count", "tool_failure_count", "partial_result_count", "omitted_evidence_count", "started_at", "ended_at",
		}).AddRow(
			"run-1", int64(42), "session-1", "qa.answerer", int64(1), strings.Repeat("a", 64),
			`{"rule_version":2,"reason":"rollout_default"}`, "tools_test",
			int64(1), int64(1), "", "", "",
			"question", RunStatusDone, "", "", 2, 2, 10,
			100, 20, 30, 5, 135, 2, 100, 120, EvidencePartial, false, 1, 2, 1, 1, 3, createdAt, createdAt,
		))
	mock.ExpectQuery("FROM agent_steps s").
		WithArgs("run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "step_no", "kind", "trace_id", "artifact_id", "tool_call_id", "tool", "args",
			"content", "prompt_content", "authoritative_sha256", "prompt_sha256", "content_bytes",
			"coverage_json", "answer_contract_json", "failed", "delivery_error", "token_delta",
			"reasoning_tokens", "duration_ms", "created_at", "artifact_preview",
		}).
			AddRow(
				int64(1), "run-1", 1, StepKindToolResult, "trace-inline", "", "call-inline", "lookup", `{}`,
				`{"sn":"SN-inline"}`, `{"sn":"SN-inline"}`, "same-sha", "same-sha", 18,
				`{}`, `{"required_literals":["SN-inline"]}`, false, "", 0, 0, 10, createdAt, nil,
			).
			AddRow(
				int64(2), "run-1", 2, StepKindToolResult, "trace-artifact", "artifact-1", "call-artifact", "lookup", `{}`,
				nil, `{"error":"tool_result_exceeds_context_budget"}`, "authoritative-sha", "prompt-sha", 900000,
				`{"partial":true,"omitted_items":3}`, `{}`, true, "tool_result_exceeds_context_budget", 0, 0, 20, createdAt, "authoritative artifact preview",
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
