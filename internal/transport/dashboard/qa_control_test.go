package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/auth"
)

type investigationCancellerRecorder struct {
	runID  string
	userID int64
	calls  int
	err    error
}

func (recorder *investigationCancellerRecorder) Cancel(
	_ context.Context,
	runID string,
	userID int64,
) error {
	recorder.runID = runID
	recorder.userID = userID
	recorder.calls++
	return recorder.err
}

func TestAPIQARunControlAbortsParentWithoutAgentHub(t *testing.T) {
	handler, mock, closeDB := newRunControlHandler(t)
	defer closeDB()
	canceller := &investigationCancellerRecorder{}
	handler.qaRuntimeFn = func() QARuntime {
		return QARuntime{
			RunStore:               handler.persistentRunStore,
			InvestigationCanceller: canceller,
		}
	}
	expectRunControlRecord(
		mock,
		"parent-1",
		42,
		agentrun.KindQAParent,
		agentrun.StatusRunning,
		"workflow-1",
	)

	response := serveRunControl(t, handler, "parent-1", 42, `{"action":"abort"}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if canceller.calls != 1 || canceller.runID != "parent-1" || canceller.userID != 42 {
		t.Fatalf("canceller = %+v", canceller)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQARunControlRejectsUnsupportedParentAction(t *testing.T) {
	handler, mock, closeDB := newRunControlHandler(t)
	defer closeDB()
	canceller := &investigationCancellerRecorder{}
	handler.qaRuntimeFn = func() QARuntime {
		return QARuntime{
			RunStore:               handler.persistentRunStore,
			InvestigationCanceller: canceller,
		}
	}
	expectRunControlRecord(
		mock,
		"parent-1",
		42,
		agentrun.KindQAParent,
		agentrun.StatusRunning,
		"workflow-1",
	)

	response := serveRunControl(t, handler, "parent-1", 42, `{"action":"pause"}`)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if canceller.calls != 0 {
		t.Fatalf("canceller calls = %d, want 0", canceller.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQARunControlRoutesAgentAbortToHub(t *testing.T) {
	handler, mock, closeDB := newRunControlHandler(t)
	defer closeDB()
	hub := agentrun.NewHub(nil)
	canceller := &investigationCancellerRecorder{}
	handler.qaRuntimeFn = func() QARuntime {
		return QARuntime{
			RunStore:               handler.persistentRunStore,
			Hub:                    hub,
			InvestigationCanceller: canceller,
		}
	}
	expectRunControlRecord(
		mock,
		"agent-1",
		42,
		agentrun.KindAgent,
		agentrun.StatusRunning,
		"",
	)

	response := serveRunControl(t, handler, "agent-1", 42, `{"action":"abort"}`)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if signal := hub.Poll("agent-1"); signal.Kind != agentrun.CtrlAbort {
		t.Fatalf("signal = %+v, want abort", signal)
	}
	if canceller.calls != 0 {
		t.Fatalf("canceller calls = %d, want 0", canceller.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQARunControlRejectsTerminalParentAbort(t *testing.T) {
	handler, mock, closeDB := newRunControlHandler(t)
	defer closeDB()
	canceller := &investigationCancellerRecorder{}
	handler.qaRuntimeFn = func() QARuntime {
		return QARuntime{
			RunStore:               handler.persistentRunStore,
			InvestigationCanceller: canceller,
		}
	}
	expectRunControlRecord(
		mock,
		"parent-1",
		42,
		agentrun.KindQAParent,
		agentrun.StatusDone,
		"workflow-1",
	)

	response := serveRunControl(t, handler, "parent-1", 42, `{"action":"abort"}`)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if canceller.calls != 0 {
		t.Fatalf("canceller calls = %d, want 0", canceller.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQARunControlRequiresRunStore(t *testing.T) {
	handler := &Handler{
		qaRuntimeFn: func() QARuntime {
			return QARuntime{}
		},
	}

	response := serveRunControl(t, handler, "parent-1", 42, `{"action":"abort"}`)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func newRunControlHandler(
	t *testing.T,
) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	handler := &Handler{persistentRunStore: agentrun.Bind(db)}
	return handler, mock, func() {
		_ = db.Close()
	}
}

func expectRunControlRecord(
	mock sqlmock.Sqlmock,
	runID string,
	userID int64,
	kind agentrun.Kind,
	status agentrun.Status,
	workflowRunID string,
) {
	mock.ExpectQuery(
		"SELECT id,run_kind,status,workflow_run_id,user_id.*"+
			"FROM agent_runs WHERE id=\\? AND user_id=\\? LIMIT 1",
	).
		WithArgs(runID, userID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_kind", "status", "workflow_run_id", "user_id",
		}).AddRow(runID, kind, status, workflowRunID, userID))
}

func serveRunControl(
	t *testing.T,
	handler *Handler,
	runID string,
	userID int64,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/qa/runs/"+runID, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.SetPathValue("id", runID)
	request = request.WithContext(auth.WithUser(
		request.Context(),
		&auth.User{ID: userID},
	))
	response := httptest.NewRecorder()
	handler.APIQARunControl(response, request)
	return response
}
