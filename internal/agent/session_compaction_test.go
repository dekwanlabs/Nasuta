package agent

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

func TestSessionCompactionStartsAtEightyPercent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(CASE WHEN t\.turn_no>s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"tokens", "compacted_through_turn"}).AddRow(79, 0))

	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42, 100, 79, "",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.Stale {
		t.Fatalf("result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactionKeepsRollingThreeAfterActivation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(`SELECT COALESCE\(SUM\(CASE WHEN t\.turn_no>s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"tokens", "compacted_through_turn"}).AddRow(10, 2))
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", "summary", 2, now, now, 5))

	_, err = CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42, 100, 10, "",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
