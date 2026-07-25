package memory

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestGetRecentSessionQueriesOnlyRequestedTail(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", "summary", 0, now, now, 3))
	mock.ExpectQuery(`SELECT m\.role, m\.content.*ORDER BY m\.seq DESC LIMIT \?`).
		WithArgs("session-1", int64(42), 6).
		WillReturnRows(sqlmock.NewRows([]string{"role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow("assistant", "nine", "", "", "").
			AddRow("user", "eight", "", "", "").
			AddRow("assistant", "seven", "", "", "").
			AddRow("user", "six", "", "", "").
			AddRow("assistant", "five", "", "", "").
			AddRow("user", "four", "", "", ""))

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
		WillReturnRows(sqlmock.NewRows([]string{"seq", "role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow(3, "assistant", "three", "", "", "").
			AddRow(2, "user", "two", "", "", "").
			AddRow(1, "assistant", "one", "", "", ""))

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

func TestListTurnsBeforeKeepsCompleteTurns(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectQuery(`SELECT t\.first_seq, t\.last_seq.*t\.last_seq < \?.*ORDER BY t\.turn_no DESC LIMIT \?`).
		WithArgs("session-1", int64(42), 8, 3).
		WillReturnRows(sqlmock.NewRows([]string{"first_seq", "last_seq"}).
			AddRow(5, 7).
			AddRow(2, 4).
			AddRow(0, 1))
	mock.ExpectQuery(`SELECT m\.seq, m\.role, m\.content.*m\.seq >= \?.*m\.seq < \?.*ORDER BY m\.seq ASC`).
		WithArgs("session-1", int64(42), 2, 8).
		WillReturnRows(sqlmock.NewRows([]string{"seq", "role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow(2, "user", "question two", "", "", "").
			AddRow(3, "assistant", "tool call", "", "", "").
			AddRow(4, "tool", "tool result", "", "call-1", "search").
			AddRow(5, "user", "question three", "", "", "").
			AddRow(6, "assistant", "tool call", "", "", "").
			AddRow(7, "assistant", "answer three", "", "", ""))

	page, err := store.ListTurnsBefore("session-1", 42, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || page.NextBeforeSeq != 2 || len(page.Messages) != 6 {
		t.Fatalf("page = %#v", page)
	}
	if page.Messages[0].Content != "question two" || page.Messages[5].Content != "answer three" {
		t.Fatalf("turn messages are incomplete or unordered: %#v", page.Messages)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetRecentSessionRestoresStructuredToolMessages(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", "summary", 0, now, now, 1))
	mock.ExpectQuery(`SELECT m\.role, m\.content.*ORDER BY m\.seq DESC LIMIT \?`).
		WithArgs("session-1", int64(42), 2).
		WillReturnRows(sqlmock.NewRows([]string{"role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow("tool", `{"count":42}`, "", "call-1", "observe_logs").
			AddRow("assistant", "", `[{"id":"call-1","type":"function","function":{"name":"observe_logs","arguments":"{\"trace_id\":\"abc123\"}"}}]`, "", ""))

	session, err := store.GetRecentSession("session-1", 42, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Messages) != 2 || len(session.Messages[0].ToolCalls) != 1 {
		t.Fatalf("messages = %#v", session.Messages)
	}
	call := session.Messages[0].ToolCalls[0]
	if call.ID != "call-1" || call.Function.Name != "observe_logs" || call.Function.Arguments != `{"trace_id":"abc123"}` {
		t.Fatalf("tool call = %#v", call)
	}
	result := session.Messages[1]
	if result.Role != "tool" || result.ToolCallID != "call-1" || result.Name != "observe_logs" {
		t.Fatalf("tool result = %#v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendTurnPersistsStructuredToolFields(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id FROM qa_sessions WHERE id=\? FOR UPDATE`).
		WithArgs("session-1").
		WillReturnRows(sqlmock.NewRows([]string{"user_id"}).AddRow(42))
	mock.ExpectQuery(`SELECT COALESCE\(\(SELECT MAX\(seq\).*SELECT MAX\(turn_no\)`).
		WithArgs("session-1", "session-1").
		WillReturnRows(sqlmock.NewRows([]string{"max_seq", "max_turn"}).AddRow(3, 2))
	mock.ExpectExec(`INSERT INTO qa_messages\(session_id,seq,turn_no,role,content,tool_calls_json,tool_call_id,tool_name,created_at\) VALUES`).
		WithArgs(
			"session-1", 4, 3, "assistant", "",
			`[{"id":"call-1","type":"function","function":{"name":"observe_logs","arguments":"{}"}}]`, "", "", sqlmock.AnyArg(),
			"session-1", 5, 3, "tool", "backend unavailable", "", "call-1", "observe_logs", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`INSERT INTO qa_turns\(session_id,turn_no,run_id,first_seq,last_seq,token_estimate,created_at\) VALUES`).
		WithArgs("session-1", 3, "run-1", 4, 5, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE qa_sessions SET updated_at = \? WHERE id = \? AND user_id=\?`).
		WithArgs(sqlmock.AnyArg(), "session-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	turnNo, err := store.AppendTurn("session-1", "run-1", 42, []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call-1", Type: "function", Function: llm.ToolFunction{Name: "observe_logs", Arguments: `{}`},
		}}},
		{Role: "tool", Content: "backend unavailable", ToolCallID: "call-1", Name: "observe_logs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if turnNo != 3 {
		t.Fatalf("turn = %d", turnNo)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAssignSessionTurnsKeepsToolProtocolInsideUserRound(t *testing.T) {
	messages := []llm.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-1", Function: llm.ToolFunction{Name: "observe"}}}},
		{Role: "tool", ToolCallID: "call-1", Name: "observe", Content: "evidence"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	turnNos, turns := assignSessionTurns(messages)
	want := []int{1, 1, 1, 1, 2, 2}
	for i := range want {
		if turnNos[i] != want[i] {
			t.Fatalf("turnNos = %v", turnNos)
		}
	}
	if len(turns) != 2 || turns[0].firstSeq != 0 || turns[0].lastSeq != 3 || turns[1].firstSeq != 4 || turns[1].lastSeq != 5 {
		t.Fatalf("turns = %#v", turns)
	}
}

func TestGetContextSessionLoadsOnlyTurnsAfterCompaction(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", "summary", 4, now, now, 6))
	mock.ExpectQuery(`SELECT m\.role.*m\.turn_no>\?.*ORDER BY m\.seq LIMIT \?`).
		WithArgs("session-1", int64(42), 4, maxContextSessionMessages+1).
		WillReturnRows(sqlmock.NewRows([]string{"role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow("user", "turn five", "", "", "").
			AddRow("assistant", "answer five", "", "", ""))

	session, err := store.GetContextSession("session-1", 42)
	if err != nil {
		t.Fatal(err)
	}
	if session.CompactedThroughTurn != 4 || len(session.Messages) != 2 {
		t.Fatalf("session = %#v", session)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionContextStatsTreatsNewSessionAsEmpty(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectQuery(`SELECT COALESCE\(CAST\(s\.summary AS CHAR\),''\),s\.compacted_through_turn`).
		WithArgs("new-session", int64(42)).
		WillReturnError(sql.ErrNoRows)

	stats, err := store.SessionContextStats("new-session", 42)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (SessionContextStats{}) {
		t.Fatalf("stats = %+v, want empty", stats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyCompactionRejectsStaleBoundary(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id,compacted_through_turn FROM qa_sessions WHERE id=\? FOR UPDATE`).
		WithArgs("session-1").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "compacted_through_turn"}).AddRow(42, 5))
	mock.ExpectRollback()

	applied, err := store.ApplyCompaction(CompactionCandidate{
		SessionID: "session-1", UserID: 42, PreviousThrough: 3, FromTurn: 4, ToTurn: 6,
	}, []TurnContextRecord{
		{Ref: "cmp-4", SessionID: "session-1", UserID: 42, TurnNumber: 4, DetailJSON: []byte(`{"turn":4}`)},
		{Ref: "cmp-5", SessionID: "session-1", UserID: 42, TurnNumber: 5, DetailJSON: []byte(`{"turn":5}`)},
		{Ref: "cmp-6", SessionID: "session-1", UserID: 42, TurnNumber: 6, DetailJSON: []byte(`{"turn":6}`)},
	}, `{"version":1}`)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("stale compaction was applied")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCompactionReadsOnlyNewTurnsAndKeepsThree(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", `{"version":1,"compactedThroughTurn":2,"items":[]}`, 2, now, now, 6))
	mock.ExpectQuery(`SELECT t\.turn_no,t\.token_estimate.*t\.turn_no BETWEEN \? AND \?.*ORDER BY t\.turn_no`).
		WithArgs("session-1", int64(42), 3, 3).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "token_estimate"}).AddRow(3, 120))
	mock.ExpectQuery(`SELECT m\.turn_no,t\.run_id,t\.token_estimate,m\.role.*m\.turn_no BETWEEN \? AND \?.*ORDER BY m\.turn_no,m\.seq`).
		WithArgs("session-1", int64(42), 3, 3).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "run_id", "token_estimate", "role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow(3, "run-3", 120, "user", "q3", "", "", "").
			AddRow(3, "run-3", 120, "assistant", "a3", "", "", ""))

	candidate, err := store.PrepareCompaction("session-1", 42, CompactionSelection{
		KeepRecentTurns: 3, TargetReductionTokens: 50, SummaryItemTokens: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.FromTurn != 3 || candidate.ToTurn != 3 || candidate.PreviousThrough != 2 {
		t.Fatalf("candidate = %#v", candidate)
	}
	if len(candidate.Turns) != 1 || candidate.Turns[0].SourceTokens != 120 || len(candidate.Turns[0].Messages) != 2 || candidate.PreviousSummary == "" {
		t.Fatalf("candidate content = %#v", candidate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCompactionSelectsOldestBatchToReductionTarget(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", "", 0, now, now, 7))
	mock.ExpectQuery(`SELECT t\.turn_no,t\.token_estimate.*t\.turn_no BETWEEN \? AND \?.*ORDER BY t\.turn_no`).
		WithArgs("session-1", int64(42), 1, 4).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "token_estimate"}).
			AddRow(1, 300).
			AddRow(2, 300).
			AddRow(3, 300).
			AddRow(4, 300))
	mock.ExpectQuery(`SELECT m\.turn_no,t\.run_id,t\.token_estimate,m\.role.*m\.turn_no BETWEEN \? AND \?.*ORDER BY m\.turn_no,m\.seq`).
		WithArgs("session-1", int64(42), 1, 2).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "run_id", "token_estimate", "role", "content", "tool_calls_json", "tool_call_id", "tool_name"}).
			AddRow(1, "run-1", 300, "user", "q1", "", "", "").
			AddRow(2, "run-2", 300, "user", "q2", "", "", ""))

	candidate, err := store.PrepareCompaction("session-1", 42, CompactionSelection{
		KeepRecentTurns: 3, TargetReductionTokens: 350, SummaryItemTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate.FromTurn != 1 || candidate.ToTurn != 2 || candidate.EligibleThrough != 4 {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.EstimatedReclaimedTokens != 400 || len(candidate.Turns) != 2 {
		t.Fatalf("candidate selection = %+v", candidate)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCompactionReportsMissingTurnNumbers(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	now := time.Now()
	mock.ExpectQuery(`SELECT s\.id.*compacted_through_turn.*qa_turns.*WHERE s\.id = \? AND s\.user_id = \?`).
		WithArgs("session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "title", "summary", "compacted_through_turn", "created_at", "updated_at", "latest_turn"}).
			AddRow("session-1", 42, "title", "", 0, now, now, 4))
	mock.ExpectQuery(`SELECT t\.turn_no,t\.token_estimate.*t\.turn_no BETWEEN \? AND \?.*ORDER BY t\.turn_no`).
		WithArgs("session-1", int64(42), 1, 3).
		WillReturnRows(sqlmock.NewRows([]string{"turn_no", "token_estimate"}).
			AddRow(1, 10).
			AddRow(3, 10))

	_, err := store.PrepareCompaction("session-1", 42, CompactionSelection{
		KeepRecentTurns: 1, TargetReductionTokens: 100, SummaryItemTokens: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "missing: 2") {
		t.Fatalf("err = %v, want missing turn 2", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyCompactionPublishesOneAtomicSnapshot(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT user_id,compacted_through_turn FROM qa_sessions WHERE id=\? FOR UPDATE`).
		WithArgs("session-1").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "compacted_through_turn"}).AddRow(42, 2))
	mock.ExpectExec(`INSERT INTO qa_turn_contexts.*VALUES`).
		WithArgs("cmp-3", "session-1", int64(42), "run-3", 3, []byte(`{"version":1,"turn":3}`), "short summary", 120, 30, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE qa_sessions SET summary=\?,compacted_through_turn=\?,updated_at=\?`).
		WithArgs(`{"version":1,"compactedThroughTurn":3,"items":[]}`, 3, sqlmock.AnyArg(), "session-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	applied, err := store.ApplyCompaction(CompactionCandidate{
		SessionID: "session-1", UserID: 42, PreviousThrough: 2,
		FromTurn: 3, ToTurn: 3,
	}, []TurnContextRecord{{
		Ref: "cmp-3", SessionID: "session-1", UserID: 42, RunID: "run-3",
		TurnNumber: 3, DetailJSON: []byte(`{"version":1,"turn":3}`), SummaryText: "short summary", SourceTokens: 120, RetainedTokens: 30,
	}}, `{"version":1,"compactedThroughTurn":3,"items":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("current compaction was not applied")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetTurnDetailBoundsReferenceToCurrentSession(t *testing.T) {
	store, mock, closeDB := newMockSessionStore(t)
	defer closeDB()
	mock.ExpectQuery(`SELECT ref,session_id,user_id,run_id,detail_json,turn_number,summary_text,source_tokens,retained_tokens.*FROM qa_turn_contexts`).
		WithArgs("cmp-1", "session-1", int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"ref", "session_id", "user_id", "run_id", "detail_json", "turn_number", "summary_text", "source_tokens", "retained_tokens"}).
			AddRow("cmp-1", "session-1", 42, "run-1", `{"version":1,"turn":1,"user":"q1"}`, 1, "summary", 90, 20))

	record, err := store.GetTurnDetail("session-1", 42, "cmp-1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Ref != "cmp-1" || record.TurnNumber != 1 || len(record.DetailJSON) == 0 {
		t.Fatalf("record = %#v", record)
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
	mock.ExpectExec(`DELETE FROM qa_turn_contexts WHERE session_id = \?`).
		WithArgs("session-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM qa_turns WHERE session_id = \?`).
		WithArgs("session-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
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
