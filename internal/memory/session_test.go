package memory

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestGetRecentSessionQueriesOnlyRequestedTail(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT s.id, s.user_id, s.title, COALESCE(s.summary,''), s.created_at, s.updated_at
         FROM qa_sessions s WHERE s.id = ? AND s.user_id = ?`)).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "created_at", "updated_at"}).
			AddRow("session-1", 42, "title", "summary", now, now))
	mock.ExpectQuery(`SELECT m\.role, m\.content.*ORDER BY m\.seq DESC LIMIT \?`).
		WithArgs("session-1", int64(42), 6).
		WillReturnRows(sqlmock.NewRows([]string{"role", "content"}).
			AddRow("assistant", "nine").
			AddRow("user", "eight").
			AddRow("assistant", "seven").
			AddRow("user", "six").
			AddRow("assistant", "five").
			AddRow("user", "four"))

	session, err := store.GetRecentSession("session-1", 42, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Messages) != 6 {
		t.Fatalf("session = %#v", session)
	}
	if session.Messages[0].Content != "four" || session.Messages[5].Content != "nine" {
		t.Fatalf("messages are not chronological: %#v", session.Messages)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListMessagesBeforeUsesExclusiveCursor(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectQuery(`SELECT m\.seq, m\.role, m\.content.*m\.seq < \?.*ORDER BY m\.seq DESC LIMIT \?`).
		WithArgs("session-1", int64(42), 4, 3).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "role", "content"}).
			AddRow(3, "assistant", "three").
			AddRow(2, "user", "two").
			AddRow(1, "assistant", "one"))

	page, err := store.ListMessagesBefore("session-1", 42, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || page.NextBeforeSeq != 2 || len(page.Messages) != 2 {
		t.Fatalf("page = %#v", page)
	}
	if page.Messages[0].Content != "two" || page.Messages[1].Content != "three" {
		t.Fatalf("messages are not chronological: %#v", page.Messages)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListSessionsReturnsMetadataWithoutLoadingBodies(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.id, s\.title.*SELECT COUNT\(\*\).*LIMIT 50`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "summary", "user_id", "created_at", "updated_at", "message_count"}).
			AddRow("session-1", "title", "summary", 42, now, now, 37))

	sessions, err := store.List(42)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].MessageCount != 37 || len(sessions[0].Messages) != 0 {
		t.Fatalf("sessions = %#v", sessions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSessionRequiresOwner(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM qa_sessions WHERE id=\? AND user_id=\? FOR UPDATE`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"owned"}).AddRow(1))
	mock.ExpectExec(`DELETE FROM qa_messages WHERE session_id = \?`).
		WithArgs("session-1").
		WillReturnResult(sqlmock.NewResult(0, 4))
	mock.ExpectExec(`DELETE FROM qa_sessions WHERE id = \? AND user_id=\?`).
		WithArgs("session-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	deleted, err := store.Delete("session-1", 42)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("owned session was not deleted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSessionRejectsDifferentOwner(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM qa_sessions WHERE id=\? AND user_id=\? FOR UPDATE`).
		WithArgs("session-1", int64(99)).
		WillReturnRows(sqlmock.NewRows([]string{"owned"}))
	mock.ExpectRollback()

	deleted, err := store.Delete("session-1", 99)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("cross-user session delete succeeded")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSessionRejectsExistingDifferentOwner(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id FROM qa_sessions WHERE id=\? FOR UPDATE`).
		WithArgs("session-1").
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(99))
	mock.ExpectRollback()

	err := store.EnsureSession("session-1", 42, "title")
	if !errors.Is(err, ErrSessionOwnership) {
		t.Fatalf("error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func newMockSessionStore(t *testing.T) (*SessionStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	return &SessionStore{db: db}, mock, func() { db.Close() }
}
