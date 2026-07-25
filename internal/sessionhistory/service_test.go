package sessionhistory

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/internal/semantic/contract"
)

type historyEmbedder struct{}

func (historyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}
func (historyEmbedder) Dim() int      { return 2 }
func (historyEmbedder) Enabled() bool { return true }

func TestFindFusesDenseAndLexicalThenRevalidatesMySQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sem := contract.NewMemory()
	if err := sem.Upsert(t.Context(), []semantic.Record{
		{ID: "cmp-dense", DenseVector: []float32{1, 0}, Metadata: map[string]any{"kind": "session_turn", "ref": "cmp-dense", "user_id": int64(42), "session_id": "session-1"}},
		{ID: "cmp-stale", DenseVector: []float32{1, 0}, Metadata: map[string]any{"kind": "session_turn", "ref": "cmp-stale", "user_id": int64(42), "session_id": "session-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT ref.*qa_session_history_terms`).
		WithArgs(int64(42), "session-1", "createcart", 64).
		WillReturnRows(sqlmock.NewRows([]string{"ref"}).AddRow("cmp-lexical"))
	mock.ExpectQuery(`SELECT ref,turn_number,summary_text,summary_tokens.*qa_turn_contexts`).
		WithArgs(int64(42), "session-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"ref", "turn_number", "summary_text", "summary_tokens"}).
			AddRow("cmp-dense", 7, "semantic result", 3).
			AddRow("cmp-lexical", 9, "exact createCart result", 4))

	service := New(memory.NewSessionStore(db), sem, historyEmbedder{})
	result, err := service.find(t.Context(), 42, "session-1", "createCart", 8, 256, false)
	if err != nil {
		t.Fatal(err)
	}
	var payload historyPayload
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Mode != "hybrid" || len(payload.Turns) != 2 {
		t.Fatalf("payload = %+v", payload)
	}
	for _, turn := range payload.Turns {
		if turn.Ref == "cmp-stale" {
			t.Fatal("stale vector record survived MySQL revalidation")
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectPayloadUsesSerializedTokenBudget(t *testing.T) {
	candidates := []memory.HistorySummary{
		{Ref: "cmp-1", TurnNumber: 1, Summary: "short"},
		{Ref: "cmp-2", TurnNumber: 2, Summary: string(make([]byte, 2000))},
	}
	result, err := selectPayload("hybrid", candidates, 24, 64)
	if err != nil {
		t.Fatal(err)
	}
	var payload historyPayload
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Turns) != 1 || payload.Turns[0].Ref != "cmp-1" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestExtractTermsPrioritizesTechnicalIdentifiersAndCapsOutput(t *testing.T) {
	terms := extractTerms("ShopifyStorefrontClient createCart /api/cart trace_id=abc-123 ordinary words", 4)
	if len(terms) != 4 {
		t.Fatalf("terms = %+v", terms)
	}
	for _, term := range terms {
		if term.weight != 4 {
			t.Fatalf("technical term was not prioritized: %+v", terms)
		}
	}
}

func TestSyncPendingIndexesCommittedSummaryBeforeCompletingOutbox(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT id,operation,ref,session_id,user_id,attempts.*qa_session_history_index_outbox`).
		WithArgs(sqlmock.AnyArg(), 64).
		WillReturnRows(sqlmock.NewRows([]string{"id", "operation", "ref", "session_id", "user_id", "attempts"}).
			AddRow(11, "upsert", "cmp-1", "session-1", 42, 0))
	mock.ExpectQuery(`SELECT ref,turn_number,summary_text,summary_tokens.*qa_turn_contexts`).
		WithArgs(int64(42), "session-1", "cmp-1").
		WillReturnRows(sqlmock.NewRows([]string{"ref", "turn_number", "summary_text", "summary_tokens"}).
			AddRow("cmp-1", 3, "archived decision", 4))
	mock.ExpectExec(`DELETE FROM qa_session_history_index_outbox WHERE id IN`).
		WithArgs(int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sem := contract.NewMemory()
	service := New(memory.NewSessionStore(db), sem, historyEmbedder{})
	if err := service.SyncPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	hits, err := sem.Search(t.Context(), semantic.Query{
		DenseVector: []float32{1, 0}, Limit: 1,
		Filter: semantic.Filter{Keywords: map[string]string{"session_id": "session-1"}},
	})
	if err != nil || len(hits) != 1 || hits[0].ID != "cmp-1" {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
