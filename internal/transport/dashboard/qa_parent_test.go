package dashboard

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/auth"
)

func TestAPIQARunGetReturnsDurableParentTerminal(t *testing.T) {
	handler, mock, closeDB := newQAParentHandler(t)
	defer closeDB()
	startedAt := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	expectQAParentDetailRecord(mock, "parent-1", 42, run.StatusDone, startedAt)
	terminal := run.Terminal{
		RunID: "parent-1", Status: run.StatusDone,
		Answer: "durable answer", ErrorCode: "completed",
	}
	raw, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT detail_json FROM runtime_events").
		WithArgs("qa_parent", terminal.RunID, int64(1), "run_finished").
		WillReturnRows(sqlmock.NewRows([]string{"detail_json"}).AddRow(raw))

	response := serveQAParentRequest(
		t,
		handler.APIQARunGet,
		http.MethodGet,
		"/api/qa/runs/parent-1",
		"parent-1",
		42,
	)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			RunKind  run.Kind         `json:"run_kind"`
			Terminal *run.Terminal    `json:"terminal"`
			Steps    []run.StepRow    `json:"steps"`
			LLMCalls []run.LLMCallRow `json:"llm_calls"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.RunKind != run.KindQAParent ||
		payload.Data.Terminal == nil ||
		payload.Data.Terminal.Answer != terminal.Answer {
		t.Fatalf("response = %+v", payload.Data)
	}
	if payload.Data.Steps != nil || payload.Data.LLMCalls != nil {
		t.Fatalf("parent detail exposed child history: %+v", payload.Data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQARunEventsReturnsOwnedBoundedPage(t *testing.T) {
	handler, mock, closeDB := newQAParentHandler(t)
	defer closeDB()
	createdAt := time.Date(2026, 8, 13, 1, 2, 3, 0, time.UTC)
	terminal := run.Terminal{
		RunID: "parent-1", Status: run.StatusDone, Answer: "answer",
	}
	raw, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id FROM agent_runs.*LIMIT 1").
		WithArgs("parent-1", run.KindQAParent, int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("parent-1"))
	mock.ExpectQuery("SELECT stream_id,seq,kind,summary,detail_json,created_at.*ORDER BY seq LIMIT \\?").
		WithArgs("qa_parent", "parent-1", int64(0), 25).
		WillReturnRows(sqlmock.NewRows([]string{
			"stream_id", "seq", "kind", "summary", "detail_json", "created_at",
		}).AddRow("parent-1", int64(1), "run_finished", "completed", raw, createdAt))

	response := serveQAParentRequest(
		t,
		handler.APIQARunEvents,
		http.MethodGet,
		"/api/qa/runs/parent-1/events?after_seq=0&limit=25",
		"parent-1",
		42,
	)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Items        []run.QAParentEvent `json:"items"`
			NextAfterSeq int64               `json:"next_after_seq"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 1 ||
		payload.Data.Items[0].Detail.Answer != terminal.Answer ||
		payload.Data.NextAfterSeq != 1 {
		t.Fatalf("response = %+v", payload.Data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQARunEventsReturnsNotFoundForUnownedParent(t *testing.T) {
	handler, mock, closeDB := newQAParentHandler(t)
	defer closeDB()
	mock.ExpectQuery("SELECT id FROM agent_runs.*LIMIT 1").
		WithArgs("parent-1", run.KindQAParent, int64(42)).
		WillReturnError(sql.ErrNoRows)

	response := serveQAParentRequest(
		t,
		handler.APIQARunEvents,
		http.MethodGet,
		"/api/qa/runs/parent-1/events",
		"parent-1",
		42,
	)

	if response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), "run not found") {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQARunEventsRejectsInvalidBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		url  string
	}{
		{name: "negative cursor", url: "/api/qa/runs/parent-1/events?after_seq=-1"},
		{name: "invalid cursor", url: "/api/qa/runs/parent-1/events?after_seq=one"},
		{name: "zero limit", url: "/api/qa/runs/parent-1/events?limit=0"},
		{name: "oversized limit", url: "/api/qa/runs/parent-1/events?limit=201"},
		{name: "invalid limit", url: "/api/qa/runs/parent-1/events?limit=all"},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, _, closeDB := newQAParentHandler(t)
			defer closeDB()
			response := serveQAParentRequest(
				t,
				handler.APIQARunEvents,
				http.MethodGet,
				test.url,
				"parent-1",
				42,
			)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestAPIQARunEventsKeepsCursorOnEmptyPage(t *testing.T) {
	handler, mock, closeDB := newQAParentHandler(t)
	defer closeDB()
	mock.ExpectQuery("SELECT id FROM agent_runs.*LIMIT 1").
		WithArgs("parent-1", run.KindQAParent, int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("parent-1"))
	mock.ExpectQuery("SELECT stream_id,seq,kind,summary,detail_json,created_at.*ORDER BY seq LIMIT \\?").
		WithArgs("qa_parent", "parent-1", int64(7), 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"stream_id", "seq", "kind", "summary", "detail_json", "created_at",
		}))

	response := serveQAParentRequest(
		t,
		handler.APIQARunEvents,
		http.MethodGet,
		"/api/qa/runs/parent-1/events?after_seq=7",
		"parent-1",
		42,
	)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Data struct {
			Items        []run.QAParentEvent `json:"items"`
			NextAfterSeq int64               `json:"next_after_seq"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 0 || payload.Data.NextAfterSeq != 7 {
		t.Fatalf("response = %+v", payload.Data)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func newQAParentHandler(
	t *testing.T,
) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	handler := &Handler{persistentRunStore: run.Bind(db)}
	return handler, mock, func() {
		_ = db.Close()
	}
}

func expectQAParentDetailRecord(
	mock sqlmock.Sqlmock,
	runID string,
	userID int64,
	status run.Status,
	startedAt time.Time,
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
			runID, run.KindQAParent, userID, "session-1", "",
			int64(0), "", `{}`, "", int64(0), int64(0), "",
			"workflow-1", "", "question", status, "", "multi_agent",
			0, 0, 0, int64(0), int64(0), int64(0), int64(0), int64(0),
			0, 0, 0, run.EvidenceComplete, false, 1, 0, 0, 0, 0,
			startedAt, startedAt,
		))
}

func serveQAParentRequest(
	t *testing.T,
	serve func(http.ResponseWriter, *http.Request),
	method string,
	target string,
	runID string,
	userID int64,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	request.SetPathValue("id", runID)
	request = request.WithContext(auth.WithUser(
		request.Context(),
		&auth.User{ID: userID},
	))
	response := httptest.NewRecorder()
	serve(response, request)
	return response
}
