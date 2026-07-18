package memory

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestWriteSupersedesLowerAuthorityFact(t *testing.T) {
	memory, semantic, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	oldID := "11111111-1111-1111-1111-111111111111"
	newID := "22222222-2222-2222-2222-222222222222"

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .*WHERE user_id=\? AND fact_key=\? AND status='active'.*FOR UPDATE`).
		WithArgs(int64(42), "user:response-language").
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(oldID, 42, "user:response-language", KindAssistantInference, "Use English", SourceAssistantInference, StatusActive, nil, nil, now)...))
	mock.ExpectExec(`(?s)UPDATE qa_memories.*SET status='superseded',superseded_by=\?,updated_at=\?.*WHERE id=\? AND user_id=\? AND status='active'`).
		WithArgs(newID, sqlmock.AnyArg(), oldID, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMemoryInsert(mock, newID, StatusActive, nil)
	mock.ExpectCommit()

	result, err := memory.Write(context.Background(), MemoryRecord{
		ID: newID, UserID: 42, FactKey: "user:response-language",
		Kind: KindPreference, Content: "Use Chinese", SourceType: SourceExplicitUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != WriteSuperseded || result.SupersededRecord != oldID || !result.VectorSynced {
		t.Fatalf("result = %#v", result)
	}
	if len(semantic.points) != 2 {
		t.Fatalf("vector points = %#v", semantic.points)
	}
	if semantic.points[0].Payload["status"] != string(StatusSuperseded) {
		t.Fatalf("old payload = %#v", semantic.points[0].Payload)
	}
	if semantic.points[1].Payload["source_type"] != string(SourceExplicitUser) {
		t.Fatalf("new payload = %#v", semantic.points[1].Payload)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRejectsLowerAuthorityReplacement(t *testing.T) {
	memory, semantic, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	activeID := "11111111-1111-1111-1111-111111111111"
	incomingID := "22222222-2222-2222-2222-222222222222"

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .*WHERE user_id=\? AND fact_key=\? AND status='active'.*FOR UPDATE`).
		WithArgs(int64(42), "workspace:user-center:owner").
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(activeID, 42, "workspace:user-center:owner", KindProfile, "Owns user center", SourceExplicitUser, StatusActive, nil, nil, now)...))
	expectMemoryInsert(mock, incomingID, StatusSuperseded, activeID)
	mock.ExpectCommit()

	result, err := memory.Write(context.Background(), MemoryRecord{
		ID: incomingID, UserID: 42, FactKey: "workspace:user-center:owner",
		Kind: KindAssistantInference, Content: "Possibly owns another service", SourceType: SourceAssistantInference,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != WriteRejected || result.SupersededRecord != activeID {
		t.Fatalf("result = %#v", result)
	}
	if len(semantic.points) != 1 || semantic.points[0].Payload["status"] != string(StatusSuperseded) {
		t.Fatalf("points = %#v", semantic.points)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRefreshPromotesConfirmedContent(t *testing.T) {
	memory, semantic, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	activeID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .*WHERE user_id=\? AND fact_key=\? AND status='active'.*FOR UPDATE`).
		WithArgs(int64(42), "user:response-style").
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(activeID, 42, "user:response-style", KindAssistantInference, "Lead with conclusion", SourceAssistantInference, StatusActive, nil, nil, now)...))
	mock.ExpectExec(`(?s)UPDATE qa_memories.*SET kind=\?,source_type=\?,authority=\?`).
		WithArgs(
			KindPreference, SourceExplicitUser, AuthorityExplicitUser, "", float32(1),
			nil, sqlmock.AnyArg(), activeID, int64(42),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := memory.Write(context.Background(), MemoryRecord{
		UserID: 42, FactKey: "user:response-style",
		Kind: KindPreference, Content: "Lead with conclusion", SourceType: SourceExplicitUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != WriteRefreshed || result.ID != activeID {
		t.Fatalf("result = %#v", result)
	}
	if len(semantic.points) != 1 || semantic.points[0].Payload["source_type"] != string(SourceExplicitUser) {
		t.Fatalf("points = %#v", semantic.points)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRefreshKeepsHigherAuthorityMetadata(t *testing.T) {
	memory, semantic, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	expiresAt := now.Add(12 * time.Hour)
	activeID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .*WHERE user_id=\? AND fact_key=\? AND status='active'.*FOR UPDATE`).
		WithArgs(int64(42), "user:current-focus").
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(activeID, 42, "user:current-focus", KindWorkContext, "Refactor user center", SourceExplicitUser, StatusActive, nil, &expiresAt, now)...))
	mock.ExpectExec(`(?s)UPDATE qa_memories.*SET kind=\?,source_type=\?,authority=\?,source_session=\?,confidence=\?`).
		WithArgs(
			KindWorkContext, SourceExplicitUser, AuthorityExplicitUser, "", float32(1),
			expiresAt, sqlmock.AnyArg(), activeID, int64(42),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := memory.Write(context.Background(), MemoryRecord{
		UserID: 42, FactKey: "user:current-focus",
		Kind: KindAssistantInference, Content: "Refactor user center", SourceType: SourceAssistantInference,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != WriteRefreshed {
		t.Fatalf("result = %#v", result)
	}
	if len(semantic.points) != 1 || semantic.points[0].Payload["source_type"] != string(SourceExplicitUser) {
		t.Fatalf("points = %#v", semantic.points)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalizeRecordAppliesTTLAndRejectsSecrets(t *testing.T) {
	now := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
	rec, err := canonicalizeRecord(MemoryRecord{
		UserID: 42, FactKey: "user:current-focus", Kind: KindWorkContext,
		Content: "Refactor user center", SourceType: SourceUserStated,
	}, 48*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExpiresAt == nil || !rec.ExpiresAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("expires_at = %v", rec.ExpiresAt)
	}
	_, err = canonicalizeRecord(MemoryRecord{
		UserID: 42, FactKey: "user:current-focus", Kind: KindWorkContext,
		Content: "access_token=secret-value", SourceType: SourceUserStated,
	}, 48*time.Hour, now)
	if err == nil {
		t.Fatal("secret content was accepted")
	}
}

func expectMemoryInsert(mock sqlmock.Sqlmock, id string, status MemoryStatus, supersededBy any) {
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO qa_memories(
			id,user_id,fact_key,kind,content,source_type,authority,status,superseded_by,
			source_session,confidence,expires_at,created_at,updated_at,last_used,use_count
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)).
		WithArgs(
			id, int64(42), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), status, supersededBy, "", float32(1), nil,
			sqlmock.AnyArg(), sqlmock.AnyArg(), nil, 0,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
}
