package sessionhistory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/internal/semantic/contract"
	"github.com/dekwanlabs/nasuta/platform"
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

type recordingHistoryStore struct {
	*contract.Memory
	lastQuery semantic.Query
	upserts   []semantic.Record
	deletes   []semantic.DeleteQuery
}

func (store *recordingHistoryStore) Search(ctx context.Context, query semantic.Query) ([]semantic.Hit, error) {
	store.lastQuery = query
	return store.Memory.Search(ctx, query)
}

func (store *recordingHistoryStore) Upsert(ctx context.Context, records []semantic.Record) error {
	store.upserts = append(store.upserts, records...)
	return store.Memory.Upsert(ctx, records)
}

func (store *recordingHistoryStore) Delete(ctx context.Context, query semantic.DeleteQuery) error {
	store.deletes = append(store.deletes, query)
	return store.Memory.Delete(ctx, query)
}

func TestFindFusesDenseAndLexicalThenRevalidatesMySQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sem := &recordingHistoryStore{Memory: contract.NewMemory()}
	if err := sem.Upsert(t.Context(), []semantic.Record{
		{ID: "cmp-dense", DenseVector: []float32{1, 0}, Metadata: map[string]any{"kind": "session_turn", "ref": "cmp-dense", "user_id": int64(42), "session_id": "session-1"}},
		{ID: "cmp-stale", DenseVector: []float32{1, 0}, Metadata: map[string]any{"kind": "session_turn", "ref": "cmp-stale", "user_id": int64(42), "session_id": "session-1"}},
	}); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(`SELECT ref.*qa_session_history_terms`).
		WithArgs(int64(42), "session-1", "createcart", 64).
		WillReturnRows(sqlmock.NewRows([]string{"ref"}).AddRow("cmp-lexical"))
	mock.ExpectQuery(`SELECT t\.context_ref,t\.turn_no,t\.context_summary_text,t\.context_summary_tokens.*FROM qa_turns t.*JOIN qa_sessions s`).
		WithArgs(int64(42), "session-1", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"ref", "turn_number", "summary_text", "summary_tokens"}).
			AddRow("cmp-dense", 7, "semantic result", 3).
			AddRow("cmp-lexical", 9, "exact createCart result", 4))

	service := New(memory.NewSessionStore(db), sem, historyEmbedder{})
	if err := service.EnableBM25(filepath.Join(t.TempDir(), "history_bm25_vocab.json")); err != nil {
		t.Fatal(err)
	}
	service.bm25.AddDoc("createCart")
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
	if sem.lastQuery.SparseVector == nil || len(sem.lastQuery.SparseVector.Indices) == 0 {
		t.Fatalf("semantic query missing BM25 sparse vector: %+v", sem.lastQuery)
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
	limit := 5
	terms := extractTerms("ShopifyStorefrontClient createCart /api/cart trace_id=abc-123 ordinary words", limit)
	if len(terms) != limit {
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
	mock.ExpectQuery(`SELECT t\.context_ref,t\.turn_no,t\.context_summary_text,t\.context_summary_tokens.*FROM qa_turns t.*JOIN qa_sessions s`).
		WithArgs(int64(42), "session-1", "cmp-1").
		WillReturnRows(sqlmock.NewRows([]string{"ref", "turn_number", "summary_text", "summary_tokens"}).
			AddRow("cmp-1", 3, "archived decision", 4))
	mock.ExpectExec(`DELETE FROM qa_session_history_index_outbox WHERE id IN`).
		WithArgs(int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sem := &recordingHistoryStore{Memory: contract.NewMemory()}
	service := New(memory.NewSessionStore(db), sem, historyEmbedder{})
	vocabPath := filepath.Join(t.TempDir(), "history_bm25_vocab.json")
	if err := service.EnableBM25(vocabPath); err != nil {
		t.Fatal(err)
	}
	if err := service.SyncPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(sem.upserts) != 1 || sem.upserts[0].SparseVector == nil || len(sem.upserts[0].SparseVector.Indices) == 0 {
		t.Fatalf("upsert missing BM25 sparse vector: %+v", sem.upserts)
	}
	wantID := platform.UUIDFromString("session_history\x00cmp-1")
	if sem.upserts[0].ID != wantID || sem.upserts[0].Metadata["ref"] != "cmp-1" {
		t.Fatalf("upsert identity = (%q, %v), want (%q, cmp-1)", sem.upserts[0].ID, sem.upserts[0].Metadata["ref"], wantID)
	}
	reloaded, err := retrieval.LoadVocab(vocabPath)
	if err != nil {
		t.Fatal(err)
	}
	indices, _ := retrieval.SparseToSorted(reloaded.QuerySparse("archived decision"))
	if !slices.Equal(indices, sem.upserts[0].SparseVector.Indices) {
		t.Fatalf("reloaded sparse indices = %v, upsert indices = %v", indices, sem.upserts[0].SparseVector.Indices)
	}
	hits, err := sem.Search(t.Context(), semantic.Query{
		DenseVector: []float32{1, 0}, Limit: 1,
		Filter: semantic.Filter{Keywords: map[string]string{"session_id": "session-1"}},
	})
	if err != nil || len(hits) != 1 || hits[0].ID != wantID {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSyncPendingMapsDeleteRefToSemanticPointID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT id,operation,ref,session_id,user_id,attempts.*qa_session_history_index_outbox`).
		WithArgs(sqlmock.AnyArg(), 64).
		WillReturnRows(sqlmock.NewRows([]string{"id", "operation", "ref", "session_id", "user_id", "attempts"}).
			AddRow(12, "delete", "cmp-1", "session-1", 42, 0))
	mock.ExpectExec(`DELETE FROM qa_session_history_index_outbox WHERE id IN`).
		WithArgs(int64(12)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	sem := &recordingHistoryStore{Memory: contract.NewMemory()}
	service := New(memory.NewSessionStore(db), sem, historyEmbedder{})
	if err := service.SyncPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	wantID := platform.UUIDFromString("session_history\x00cmp-1")
	if len(sem.deletes) != 1 || !slices.Equal(sem.deletes[0].IDs, []string{wantID}) {
		t.Fatalf("delete queries = %+v, want point ID %q", sem.deletes, wantID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
