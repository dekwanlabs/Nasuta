package dashboard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

func TestQAHistoryPageRestoresEvidenceOnFinalAnswer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(`UPDATE agent_runs SET status=\?,ended_at=\? WHERE status IN \(\?,\?\)`).
		WithArgs(agent.RunStatusAborted, sqlmock.AnyArg(), agent.RunStatusRunning, agent.RunStatusPaused).
		WillReturnResult(sqlmock.NewResult(0, 0))
	runStore, err := agent.NewRunStore(db)
	if err != nil {
		t.Fatalf("NewRunStore: %v", err)
	}
	handler := &Handler{persistentRunStore: runStore}
	mock.ExpectQuery(`SELECT id,evidence_status.*FROM agent_runs WHERE user_id=\? AND session_id=\? AND id IN \(\?\)`).
		WithArgs(int64(42), "session-1", "run-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "evidence_status", "forced_conclusion", "evidence_result_count", "tool_call_count",
			"tool_failure_count", "partial_result_count", "omitted_evidence_count",
		}).AddRow("run-1", agent.EvidencePartial, true, 2, 3, 1, 1, 4))

	page, err := handler.qaHistoryPage(context.Background(), 42, "session-1", &memory.MessagePage{
		Messages: []memory.SessionMessage{
			{Message: llm.Message{Role: "user", Content: "question"}},
			{Message: llm.Message{Role: "assistant", Content: "answer"}, RunID: "run-1"},
		},
		NextBeforeSeq: 3,
		HasMore:       true,
	})
	if err != nil {
		t.Fatalf("qaHistoryPage: %v", err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("marshal history page: %v", err)
	}
	jsonText := string(raw)
	for _, want := range []string{`"status":"partial"`, `"forced_conclusion":true`, `"omitted_item_count":4`} {
		if !strings.Contains(jsonText, want) {
			t.Fatalf("history JSON %s missing %s", jsonText, want)
		}
	}
	if strings.Contains(jsonText, "run-1") {
		t.Fatalf("history JSON leaked internal run id: %s", jsonText)
	}
	if page.Messages[0].Evidence != nil || page.Messages[1].Evidence == nil {
		t.Fatalf("evidence attached to wrong messages: %#v", page.Messages)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
