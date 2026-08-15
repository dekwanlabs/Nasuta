package run

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

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_steps").
		WithArgs(
			step.RunID, step.StepNo, step.Kind, step.TraceID, "", step.ToolCallID, step.Tool, step.Args,
			step.Content, step.PromptContent, step.AuthoritativeSHA256, step.PromptSHA256, step.SizeBytes,
			coverageJSON, contractJSON, false, "", 0, 0, step.DurationMs,
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

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO agent_steps").
		WithArgs(
			step.RunID, step.StepNo, step.Kind, step.TraceID, step.ArtifactID, step.ToolCallID, step.Tool, step.Args,
			nil, step.PromptContent, step.AuthoritativeSHA256, step.PromptSHA256, step.SizeBytes,
			coverageJSON, contractJSON, true, step.DeliveryError, 0, 0, 0,
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

func TestRunStoreGetQAParentUsesNarrowOwnedRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	startedAt := time.Date(2026, 8, 12, 1, 2, 3, 0, time.UTC)
	mock.ExpectQuery(
		"SELECT id,workflow_run_id,user_id,session_id,question,status,started_at,ended_at.*FROM agent_runs WHERE id=\\? AND run_kind=\\? AND user_id=\\?",
	).
		WithArgs("parent-1", KindQAParent, int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workflow_run_id", "user_id", "session_id", "question", "status", "started_at", "ended_at",
		}).AddRow(
			"parent-1", "workflow-1", int64(42), "session-1", "question",
			StatusRunning, startedAt, nil,
		))

	parent, err := store.GetParentForUser("parent-1", 42)
	if err != nil {
		t.Fatalf("GetParentForUser: %v", err)
	}
	if parent.ID != "parent-1" || parent.WorkflowRunID != "workflow-1" ||
		parent.Status != StatusRunning || parent.StartedAt == "" {
		t.Fatalf("parent = %+v", parent)
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
		WithArgs("parent-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_kind", "status", "workflow_run_id", "user_id",
		}).AddRow(
			"parent-1", KindQAParent, StatusRunning, "workflow-1", int64(42),
		))

	record, err := store.GetControlForUser("parent-1", 42)
	if err != nil {
		t.Fatalf("GetControlForUser: %v", err)
	}
	if record.ID != "parent-1" || record.RunKind != KindQAParent ||
		record.Status != StatusRunning || record.WorkflowRunID != "workflow-1" ||
		record.UserID != 42 {
		t.Fatalf("record = %+v", record)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreListActiveQAParentsUsesBoundedKeysetRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	startedBefore := time.Date(2026, 8, 12, 2, 0, 0, 0, time.UTC)
	cursor := QAParentCursor{StartedAt: "2026-08-12T01:02:03Z", ID: "parent-1"}
	mock.ExpectQuery(
		"SELECT id,workflow_run_id,user_id,session_id,question,status,started_at,ended_at.*"+
			"WHERE run_kind=\\? AND status IN \\(\\?,\\?\\) AND started_at<\\?.*"+
			"started_at>\\? OR \\(started_at=\\? AND id>\\?\\).*"+
			"ORDER BY started_at,id LIMIT \\?",
	).
		WithArgs(
			KindQAParent, StatusRunning, StatusPaused,
			sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), cursor.ID, 100,
		).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "workflow_run_id", "user_id", "session_id", "question", "status", "started_at", "ended_at",
		}))

	parents, err := store.ListActiveQAParents(startedBefore, cursor, 100)
	if err != nil {
		t.Fatalf("ListActiveQAParents: %v", err)
	}
	if len(parents) != 0 {
		t.Fatalf("parents = %+v", parents)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreListActiveQAParentsRejectsInvalidBounds(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	startedBefore := time.Date(2026, 8, 12, 2, 0, 0, 0, time.UTC)
	if _, err := store.ListActiveQAParents(time.Time{}, QAParentCursor{}, 10); err == nil {
		t.Fatal("ListActiveQAParents accepted a missing startup cutoff")
	}
	if _, err := store.ListActiveQAParents(startedBefore, QAParentCursor{}, 0); err == nil {
		t.Fatal("ListActiveQAParents accepted an unbounded read")
	}
	if _, err := store.ListActiveQAParents(
		startedBefore,
		QAParentCursor{ID: "parent-1"},
		10,
	); err == nil {
		t.Fatal("ListActiveQAParents accepted an incomplete cursor")
	}
}

func TestRunStoreRecoverInterruptedExcludesQAParents(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	mock.ExpectExec(
		"UPDATE agent_runs SET status=\\?,ended_at=\\? WHERE run_kind=\\? AND status IN \\(\\?,\\?\\)",
	).
		WithArgs(
			StatusAborted, sqlmock.AnyArg(), KindAgent,
			StatusRunning, StatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 3))

	recovered, err := store.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 3 {
		t.Fatalf("recovered = %d, want 3", recovered)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreCompleteQAParentCommitsTerminalEventAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	outcome := Outcome{
		Status: StatusDone, Answer: "persisted answer", ErrorCode: "completed",
		TokenUsed: 31, HitCount: 2,
		Evidence: EvidenceMetrics{
			Status: EvidenceComplete, ResultCount: 2, ToolCallCount: 3,
		},
	}

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE agent_runs").
		WithArgs(
			StatusDone, "completed", 0, 31, EvidenceComplete, false,
			2, 3, 0, 0, 0, sqlmock.AnyArg(), "parent-1", KindQAParent,
			StatusRunning, StatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO runtime_events").
		WithArgs(
			qaParentStreamKind, "parent-1", qaParentTerminalEventSeq,
			qaParentTerminalEventKind, "", string(StatusDone),
			sqlmock.AnyArg(), sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	persisted, err := store.CompleteQAParent(t.Context(), "parent-1", outcome)
	if err != nil {
		t.Fatalf("CompleteQAParent: %v", err)
	}
	if persisted.Status != outcome.Status || persisted.Answer != outcome.Answer ||
		persisted.ErrorCode != outcome.ErrorCode || persisted.TokenUsed != outcome.TokenUsed ||
		persisted.HitCount != outcome.HitCount {
		t.Fatalf("persisted = %+v", persisted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreCompleteQAParentRollsBackWhenEventInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE agent_runs").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO runtime_events").
		WillReturnError(errors.New("event storage unavailable"))
	mock.ExpectRollback()

	_, err = store.CompleteQAParent(t.Context(), "parent-1", Outcome{
		Status: StatusDone, Answer: "answer",
		Evidence: EvidenceMetrics{Status: EvidenceComplete},
	})
	if err == nil || !strings.Contains(err.Error(), "append QA parent") ||
		!strings.Contains(err.Error(), "event storage unavailable") {
		t.Fatalf("CompleteQAParent error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreCompleteQAParentReplayReturnsPersistedTerminal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	terminal := Terminal{
		RunID: "parent-replay", Status: StatusDone,
		Answer: "persisted answer", ErrorCode: "persisted_code", TokenUsed: 47,
		Evidence: EvidenceMetrics{Status: EvidencePartial, ResultCount: 3},
	}
	detail, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE agent_runs").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT r.status,e.detail_json").
		WithArgs(
			qaParentStreamKind, qaParentTerminalEventSeq,
			qaParentTerminalEventKind, terminal.RunID, KindQAParent,
		).
		WillReturnRows(sqlmock.NewRows([]string{"status", "detail_json"}).
			AddRow(StatusDone, detail))
	mock.ExpectRollback()

	persisted, err := store.CompleteQAParent(t.Context(), terminal.RunID, Outcome{
		Status: StatusDone, Answer: "caller answer",
		Evidence: EvidenceMetrics{Status: EvidenceComplete},
	})
	if err != nil {
		t.Fatalf("CompleteQAParent replay: %v", err)
	}
	if persisted.Answer != terminal.Answer ||
		persisted.ErrorCode != terminal.ErrorCode ||
		persisted.TokenUsed != terminal.TokenUsed ||
		persisted.Evidence.Status != terminal.Evidence.Status {
		t.Fatalf("persisted = %+v", persisted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreCompleteQAParentRejectsTerminalRowWithoutEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}

	mock.ExpectBegin()
	mock.ExpectExec("UPDATE agent_runs").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT r.status,e.detail_json").
		WillReturnRows(sqlmock.NewRows([]string{"status", "detail_json"}).
			AddRow(StatusDone, nil))
	mock.ExpectRollback()

	_, err = store.CompleteQAParent(t.Context(), "parent-missing-event", Outcome{
		Status:   StatusDone,
		Evidence: EvidenceMetrics{Status: EvidenceComplete},
	})
	if err == nil || !strings.Contains(err.Error(), "terminal without a durable terminal event") {
		t.Fatalf("CompleteQAParent error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreListQAParentEventsUsesOwnedBoundedRead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	terminal := Terminal{
		RunID: "parent-events", Status: StatusDone, Answer: "answer",
		Evidence: EvidenceMetrics{Status: EvidenceComplete},
	}
	detail, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)

	mock.ExpectQuery("SELECT id FROM agent_runs.*user_id=\\? LIMIT 1").
		WithArgs("parent-events", KindQAParent, int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("parent-events"))
	mock.ExpectQuery(
		"SELECT stream_id,seq,kind,summary,detail_json,created_at.*"+
			"seq>\\?.*ORDER BY seq LIMIT \\?",
	).
		WithArgs(qaParentStreamKind, "parent-events", int64(0), 25).
		WillReturnRows(sqlmock.NewRows([]string{
			"stream_id", "seq", "kind", "summary", "detail_json", "created_at",
		}).AddRow(
			"parent-events", int64(1), qaParentTerminalEventKind,
			string(StatusDone), detail, createdAt,
		))

	events, err := store.ListParentEvents(
		t.Context(), "parent-events", 42, 0, 25,
	)
	if err != nil {
		t.Fatalf("ListParentEvents: %v", err)
	}
	if len(events) != 1 || events[0].Seq != 1 ||
		events[0].Detail.Answer != terminal.Answer ||
		events[0].CreatedAt == "" {
		t.Fatalf("events = %+v", events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreGetQAParentDetailLoadsOnlyDurableTerminal(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &Store{db: db}
	createdAt := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	expectQAParentRunRecord(
		mock,
		"parent-detail",
		42,
		StatusDone,
		createdAt,
	)
	terminal := Terminal{
		RunID: "parent-detail", Status: StatusDone,
		Answer: "durable answer", ErrorCode: "completed",
		Evidence: EvidenceMetrics{Status: EvidenceComplete},
	}
	raw, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT detail_json FROM runtime_events").
		WithArgs(
			qaParentStreamKind, terminal.RunID,
			qaParentTerminalEventSeq, qaParentTerminalEventKind,
		).
		WillReturnRows(sqlmock.NewRows([]string{"detail_json"}).AddRow(raw))

	detail, err := store.GetForUser(terminal.RunID, 42)
	if err != nil {
		t.Fatalf("GetForUser: %v", err)
	}
	if detail.Terminal == nil || detail.Terminal.Answer != terminal.Answer ||
		detail.Terminal.ErrorCode != terminal.ErrorCode {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Steps != nil || detail.LLMCalls != nil {
		t.Fatalf("parent detail loaded agent history: %+v", detail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectQAParentRunRecord(
	mock sqlmock.Sqlmock,
	runID string,
	userID int64,
	status Status,
	createdAt time.Time,
) {
	mock.ExpectQuery("FROM agent_runs WHERE id=\\? AND user_id=\\?").
		WithArgs(runID, userID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_kind", "user_id", "session_id", "agent_id",
			"definition_version", "definition_hash", "selection_json", "tool_snapshot_id",
			"input_schema_version", "output_schema_version", "parent_run_id",
			"workflow_run_id", "workflow_node_id", "question", "status", "error_code",
			"mode", "max_steps", "step_count", "token_used", "input_tokens",
			"cached_input_tokens", "output_tokens", "reasoning_tokens", "total_tokens",
			"llm_call_count", "peak_input_tokens", "peak_reserved_tokens",
			"evidence_status", "forced_conclusion", "evidence_result_count",
			"tool_call_count", "tool_failure_count", "partial_result_count",
			"omitted_evidence_count", "started_at", "ended_at",
		}).AddRow(
			runID, KindQAParent, userID, "session-1", "",
			int64(0), "", `{}`, "", int64(0), int64(0), "",
			"workflow-1", "", "question", status, "", "multi_agent",
			0, 0, 0, int64(0), int64(0), int64(0), int64(0), int64(0),
			0, 0, 0, EvidenceComplete, false, 1, 0, 0, 0, 0,
			createdAt, createdAt,
		))
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
			"input_schema_version", "output_schema_version", "parent_run_id", "workflow_run_id", "workflow_node_id",
			"question", "status", "error_code", "mode", "max_steps", "step_count", "token_used",
			"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens", "total_tokens", "llm_call_count",
			"peak_input_tokens", "peak_reserved_tokens", "evidence_status", "forced_conclusion", "evidence_result_count",
			"tool_call_count", "tool_failure_count", "partial_result_count", "omitted_evidence_count", "started_at", "ended_at",
		}).AddRow(
			"run-1", KindAgent, int64(42), "session-1", "qa.answerer", int64(1), strings.Repeat("a", 64),
			`{"rule_version":2,"reason":"rollout_default"}`, "tools_test",
			int64(1), int64(1), "", "", "",
			"question", StatusDone, "", "", 2, 2, 10,
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
