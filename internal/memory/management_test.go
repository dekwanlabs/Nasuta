package memory

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestListUsesBoundedUserScopedCursorPage(t *testing.T) {
	memory, _, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	rows := sqlmock.NewRows(memoryColumns()).
		AddRow(memoryRow("c", 42, "user:role:app", KindProfile, "Owns App", SourceUserStated, StatusActive, nil, nil, now)...).
		AddRow(memoryRow("b", 42, "user:role:iot", KindProfile, "Owns IoT", SourceUserStated, StatusActive, nil, nil, now.Add(-time.Minute))...).
		AddRow(memoryRow("a", 42, "user:role:cloud", KindProfile, "Owns Cloud", SourceUserStated, StatusActive, nil, nil, now.Add(-2*time.Minute))...)
	mock.ExpectQuery(`(?s)SELECT .*FROM qa_memories.*WHERE user_id=\? AND kind=\? AND status=\?.*ORDER BY created_at DESC,id DESC.*LIMIT \?`).
		WithArgs(int64(42), KindProfile, StatusActive, 3).
		WillReturnRows(rows)

	page, err := memory.List(context.Background(), 42, ListOptions{
		Limit: 2, Kind: KindProfile, Status: StatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !page.HasMore || len(page.Records) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %#v", page)
	}
	cursor, err := decodeMemoryCursor(page.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.ID != "b" || !cursor.CreatedAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("cursor = %#v", cursor)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteScopesByUserAndRemovesVector(t *testing.T) {
	memory, semantic, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	mock.ExpectExec(`DELETE FROM qa_memories WHERE id=\? AND user_id=\?`).
		WithArgs("memory-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	deleted, err := memory.Delete(context.Background(), 42, "memory-1")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted || len(semantic.deleted) != 1 || semantic.deleted[0] != "memory-1" {
		t.Fatalf("deleted=%t vectors=%v", deleted, semantic.deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteBySessionBatchesOwnedVectorCleanup(t *testing.T) {
	memory, semantic, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM qa_memories WHERE user_id=\? AND source_session=\? FOR UPDATE`).
		WithArgs(int64(42), "session-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("memory-1").AddRow("memory-2"))
	mock.ExpectExec(`DELETE FROM qa_memories WHERE user_id=\? AND source_session=\?`).
		WithArgs(int64(42), "session-1").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	deleted, err := memory.DeleteBySession(context.Background(), 42, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 || len(semantic.deleted) != 2 {
		t.Fatalf("deleted=%d vectors=%v", deleted, semantic.deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
