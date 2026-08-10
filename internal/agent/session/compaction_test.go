package session

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

func TestSessionCompactionDoesNotStartBelowEightyPercent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(0, 0, 79, 6))

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

func TestSessionCompactionUsesMeasuredIncomingTokens(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(0, 0, 60, 3))
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "archived_summary_tokens", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", 0, 0, now, now, 3))

	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42,
		SessionCompactionUsage{ContextWindow: 100, IncomingTokens: 15, OutputReserveTokens: 5},
		"short question",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectedBeforeTokens != 80 || result.Applied || result.Stale {
		t.Fatalf("result = %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionCompactionUsesProjectedRequestTokens(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(0, 0, 90, 6))

	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42,
		SessionCompactionUsage{ContextWindow: 100, ProjectedTokens: 10},
		"",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectedBeforeTokens != 10 || result.Triggered || result.Applied {
		t.Fatalf("result = %+v, want the complete request projection to drive the decision", result)
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
		content := `{"items":[{"item":1,"text":"summary one"},{"item":2,"text":"summary two"}]}`
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
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(0, 0, 1700, 6))
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "archived_summary_tokens", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", 0, 0, now, now, 6))
	mock.ExpectQuery(`SELECT t\.turn_no,t\.token_estimate.*t\.turn_no BETWEEN \? AND \?.*ORDER BY t\.turn_no`).
		WithArgs("session-1", int64(42), 1, 3).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "token_estimate"}).
			AddRow(1, 400).
			AddRow(2, 400).
			AddRow(3, 400))
	mock.ExpectQuery(`SELECT m\.turn_no,t\.run_id,t\.token_estimate,m\.role.*m\.turn_no BETWEEN \? AND \?.*ORDER BY m\.turn_no,m\.seq`).
		WithArgs("session-1", int64(42), 1, 2).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "run_id", "token_estimate", "role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow(1, "run-1", 400, "user", "q1", "", "", "").
			AddRow(1, "run-1", 400, "assistant", "a1", "", "", "").
			AddRow(2, "run-2", 400, "user", "q2", "", "", "").
			AddRow(2, "run-2", 400, "assistant", "a2", "", "", ""))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id,compacted_through_turn FROM qa_sessions WHERE id=\? FOR UPDATE`).
		WithArgs("session-1").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "compacted_through_turn"}).AddRow(42, 0))
	mock.ExpectExec(`UPDATE qa_turns target.*SET target\.context_ref=context\.ref`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`INSERT INTO qa_session_history_index_outbox.*VALUES`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE qa_sessions SET archived_summary_tokens=archived_summary_tokens\+\?,compacted_through_turn=\?,updated_at=\?`).
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
	if !result.Applied || result.NewSessionRecommended ||
		result.FromTurn != 1 || result.ToTurn != 2 || startedFrom != 1 || startedTo != 2 {
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
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(20, 2, 10, 6))

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

func TestPostTurnArchiveDoesNotStartBelowHighWater(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(0, 0, 20000, 4))

	result, err := ArchiveSessionHistoryIfNeededWithStatus(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil),
		memory.NewSessionStore(db), "session-1", 42,
		SessionCompactionUsage{ContextWindow: 128000, OutputReserveTokens: 8000},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.Stale || result.ProjectedBeforeTokens != 28000 {
		t.Fatalf("result = %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostTurnArchiveUsesSameHighWaterThreshold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(0, 0, 1500, 3))
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "archived_summary_tokens", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", 0, 0, now, now, 3))

	result, err := ArchiveSessionHistoryIfNeededWithStatus(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil),
		memory.NewSessionStore(db), "session-1", 42,
		SessionCompactionUsage{ContextWindow: 2000, OutputReserveTokens: 100},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Applied || result.Stale || result.ProjectedBeforeTokens != 1600 {
		t.Fatalf("result = %+v", result)
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
	mock.ExpectQuery(`SELECT s\.archived_summary_tokens,s\.compacted_through_turn`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(compactionStatsRow(3864, 21, 30400, 24))
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "archived_summary_tokens", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", 3864, 21, now, now, 24))

	result, err := CompactSessionIfNeeded(
		t.Context(), llm.NewLLMClientWithHTTP("", "", "", 0, nil), memory.NewSessionStore(db),
		"session-1", 42, SessionCompactionUsage{ContextWindow: 32000}, "",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.NewSessionRecommended || result.ArchivedTurnCount != 21 || result.RestartTurnThreshold != 53 {
		t.Fatalf("result = %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func compactionStatsRow(archivedTokens int64, compactedThrough, uncompactedTokens, latestTurn int) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"archived_summary_tokens", "compacted_through_turn", "tokens", "latest_turn"}).
		AddRow(archivedTokens, compactedThrough, uncompactedTokens, latestTurn)
}

func TestNewSessionRecommendationTriggersAtEitherThreshold(t *testing.T) {
	if threshold := restartTurnThreshold(128000); threshold != 209 {
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
