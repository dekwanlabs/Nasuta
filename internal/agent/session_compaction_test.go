package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	mock.ExpectQuery(`SELECT COALESCE\(CAST\(s\.summary AS CHAR\),''\),s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"summary", "compacted_through_turn", "tokens", "latest_turn"}).AddRow("", 0, 79, 6))

	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42, SessionCompactionUsage{ContextWindow: 100}, "",
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

func TestSessionCompactionBatchesOldestTurnsToLowWater(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []llm.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		content := `[{"item":1,"text":"summary one"},{"item":2,"text":"summary two"},{"item":3,"text":"summary three"}]`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(`SELECT COALESCE\(CAST\(s\.summary AS CHAR\),''\),s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"summary", "compacted_through_turn", "tokens", "latest_turn"}).
			AddRow("", 0, 1700, 6))
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", "", 0, now, now, 6))
	mock.ExpectQuery(`SELECT t\.turn_no,t\.token_estimate.*t\.turn_no BETWEEN \? AND \?.*ORDER BY t\.turn_no`).
		WithArgs("session-1", int64(42), 1, 3).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "token_estimate"}).
			AddRow(1, 400).
			AddRow(2, 400).
			AddRow(3, 400))
	mock.ExpectQuery(`SELECT m\.turn_no,t\.run_id,t\.token_estimate,m\.role.*m\.turn_no BETWEEN \? AND \?.*ORDER BY m\.turn_no,m\.seq`).
		WithArgs("session-1", int64(42), 1, 3).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "run_id", "token_estimate", "role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow(1, "run-1", 400, "user", "q1", "", "", "").
			AddRow(1, "run-1", 400, "assistant", "a1", "", "", "").
			AddRow(2, "run-2", 400, "user", "q2", "", "", "").
			AddRow(2, "run-2", 400, "assistant", "a2", "", "", "").
			AddRow(3, "run-3", 400, "user", "q3", "", "", "").
			AddRow(3, "run-3", 400, "assistant", "a3", "", "", ""))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id,compacted_through_turn FROM qa_sessions WHERE id=\? FOR UPDATE`).
		WithArgs("session-1").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "compacted_through_turn"}).AddRow(42, 0))
	mock.ExpectExec(`INSERT INTO qa_turn_contexts.*detail_json`).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`UPDATE qa_sessions SET summary=\?,compacted_through_turn=\?,updated_at=\?`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	startedFrom, startedTo := 0, 0
	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP(server.URL, "key", "model", 100, server.Client()),
		memory.NewSessionStore(db), "session-1", 42,
		SessionCompactionUsage{ContextWindow: 2000}, "",
		func(fromTurn, toTurn int) { startedFrom, startedTo = fromTurn, toTurn },
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.FromTurn != 1 || result.ToTurn != 3 || startedFrom != 1 || startedTo != 3 {
		t.Fatalf("result = %+v, callback=%d-%d", result, startedFrom, startedTo)
	}
	if result.ProjectedAfterTokens > int(2000*sessionLowWaterRatio) {
		t.Fatalf("projected after = %d, want <= low water", result.ProjectedAfterTokens)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactionDoesNotStayActiveBelowHighWater(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT COALESCE\(CAST\(s\.summary AS CHAR\),''\),s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"summary", "compacted_through_turn", "tokens", "latest_turn"}).
			AddRow(`{"version":1,"compactedThroughTurn":2,"items":[{"turn":1,"ref":"cmp-1","summary":"one"},{"turn":2,"ref":"cmp-2","summary":"two"}]}`, 2, 10, 6))

	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42, SessionCompactionUsage{ContextWindow: 1000}, "",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied {
		t.Fatalf("below-high-water session compacted again: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactionRecommendsNewSessionAtCriticalWater(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(`SELECT COALESCE\(CAST\(s\.summary AS CHAR\),''\),s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"summary", "compacted_through_turn", "tokens", "latest_turn"}).
			AddRow(`{}`, 21, 30399, 24))
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", `{}`, 21, now, now, 24))

	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42, SessionCompactionUsage{ContextWindow: 32000}, "",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.NewSessionRecommended || result.SummaryItemCount != 21 || result.SummaryItemThreshold != 53 {
		t.Fatalf("result = %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNewSessionRecommendationTriggersAtEitherThreshold(t *testing.T) {
	if threshold := restartSummaryItemThreshold(128000); threshold != 209 {
		t.Fatalf("128K restart item threshold = %d, want 209", threshold)
	}
	criticalWater := 950
	if !shouldRecommendNewSession(949, criticalWater, 210, 209) {
		t.Fatal("recommendation did not trigger above the summary item threshold")
	}
	if !shouldRecommendNewSession(950, criticalWater, 209, 209) {
		t.Fatal("recommendation did not trigger at critical water")
	}
	if shouldRecommendNewSession(949, criticalWater, 209, 209) {
		t.Fatal("recommendation triggered below both thresholds")
	}
}
