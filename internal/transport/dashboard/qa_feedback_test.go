package dashboard

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

func TestAPIQAMessageFeedbackUpdatesOwnedAnswer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	handler := &Handler{qaSessions: memory.NewSessionStore(db)}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT m\.id, m\.feedback.*JOIN qa_turns.*m\.seq=\?.*FOR UPDATE`).
		WithArgs("session-1", int64(42), 7).
		WillReturnRows(sqlmock.NewRows([]string{"id", "feedback"}).AddRow(17, ""))
	mock.ExpectExec(`UPDATE qa_messages SET feedback=\? WHERE id=\?`).
		WithArgs("like", int64(17)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	request := httptest.NewRequest(http.MethodPut, "/api/qa/sessions/session-1/message-feedback", bytes.NewBufferString(
		`{"message_seq":7,"feedback":"like"}`,
	))
	request.SetPathValue("id", "session-1")
	request = request.WithContext(auth.WithUser(context.Background(), &auth.User{ID: 42}))
	response := httptest.NewRecorder()

	handler.APIQAMessageFeedback(response, request)

	if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"feedback":"like"`)) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQAMessageFeedbackRejectsInvalidValue(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	handler := &Handler{qaSessions: memory.NewSessionStore(db)}
	request := httptest.NewRequest(http.MethodPut, "/api/qa/sessions/session-1/message-feedback", bytes.NewBufferString(
		`{"message_seq":7,"feedback":"up"}`,
	))
	request.SetPathValue("id", "session-1")
	response := httptest.NewRecorder()

	handler.APIQAMessageFeedback(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
