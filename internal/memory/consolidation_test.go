package memory

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/semantic"
)

func TestRecallForConsolidationAdmitsExactFactKeyWithoutDenseMatch(t *testing.T) {
	memory, semanticStore, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	activeID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectQuery(`(?s)SELECT .*FROM qa_memories.*WHERE user_id=\? AND status='active' AND fact_key IN \(\?\).*LIMIT \?`).
		WithArgs(int64(42), "workspace:billing-service:owner", 1).
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(
				activeID, 42, "workspace:billing-service:owner", KindProfile,
				"Owned by the platform team", SourceUserStated, StatusActive, nil, nil, now,
			)...))

	result, err := memory.RecallForConsolidation(t.Context(), 42, []MemoryProbe{{
		Query: "billing service ownership", FactKeyHint: "workspace:billing-service:owner",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Record.ID != activeID ||
		!result.Matches[0].ExactFactKey {
		t.Fatalf("matches = %#v", result.Matches)
	}
	if result.Stats.ExactFactKeys != 1 || result.Stats.Admitted != 1 {
		t.Fatalf("stats = %#v", result.Stats)
	}
	if got := semanticStore.query.Filter.AnyInteger["user_id"]; len(got) != 1 || got[0] != 42 {
		t.Fatalf("user filter = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecallForConsolidationDropsLowDenseScores(t *testing.T) {
	memory, semanticStore, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	semanticStore.hits = []semantic.Hit{
		{
			Score: 0.91, DenseScore: 0.91, ScoreKind: semantic.ScoreDense,
			Metadata: map[string]any{"memory_id": "high", "user_id": int64(42)},
		},
		{
			Score: 0.77, DenseScore: 0.77, ScoreKind: semantic.ScoreDense,
			Metadata: map[string]any{"memory_id": "low", "user_id": int64(42)},
		},
		{
			Score: 0.99, DenseScore: 0.99, ScoreKind: semantic.ScoreDense,
			Metadata: map[string]any{"memory_id": "global", "user_id": int64(0)},
		},
	}
	mock.ExpectQuery(`(?s)SELECT .*FROM qa_memories.*WHERE id IN \(\?\) AND \(user_id=\? OR user_id=0\)`).
		WithArgs("high", int64(42)).
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(
				"high", 42, "user:response-style", KindPreference,
				"Lead with the conclusion", SourceUserStated, StatusActive, nil, nil, now,
			)...))

	result, err := memory.RecallForConsolidation(t.Context(), 42, []MemoryProbe{{
		Query: "preferred response style",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || result.Matches[0].Record.ID != "high" {
		t.Fatalf("matches = %#v", result.Matches)
	}
	if result.Stats.BelowScore != 1 || result.Stats.InvalidPayload != 1 {
		t.Fatalf("stats = %#v", result.Stats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecallForConsolidationRejectsFusionScores(t *testing.T) {
	memory, semanticStore, _, closeDB := newMemoryTestStore(t)
	defer closeDB()
	semanticStore.hits = []semantic.Hit{{
		Score: 0.03, FusionScore: 0.03, ScoreKind: semantic.ScoreFusion,
		Metadata: map[string]any{"memory_id": "memory", "user_id": int64(42)},
	}}

	_, err := memory.RecallForConsolidation(t.Context(), 42, []MemoryProbe{{Query: "preference"}})
	if err == nil {
		t.Fatal("expected fusion score rejection")
	}
}

func TestApplyDecisionRefreshPreservesExistingContent(t *testing.T) {
	memory, semanticStore, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	activeID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .*WHERE user_id=\? AND fact_key=\? AND status='active'.*FOR UPDATE`).
		WithArgs(int64(42), "user:response-style").
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(
				activeID, 42, "user:response-style", KindPreference,
				"Lead with the conclusion", SourceUserStated, StatusActive, nil, nil, now,
			)...))
	mock.ExpectExec(`(?s)UPDATE qa_memories.*SET kind=\?,source_type=\?,authority=\?,source_session=\?,confidence=\?.*WHERE id=\? AND user_id=\? AND fact_key=\? AND status='active'`).
		WithArgs(
			KindPreference, SourceExplicitUser, AuthorityExplicitUser, "session-1", float32(1),
			nil, sqlmock.AnyArg(), activeID, int64(42), "user:response-style",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := memory.ApplyDecision(t.Context(), MemoryDecision{
		Action:   ConsolidationRefresh,
		TargetID: activeID,
		Record: MemoryRecord{
			UserID: 42, FactKey: "user:response-style", Kind: KindPreference,
			Content: "Start every answer with the main point", SourceType: SourceExplicitUser,
			SourceSession: "session-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != WriteRefreshed || result.ID != activeID || !result.VectorSynced {
		t.Fatalf("result = %#v", result)
	}
	if len(semanticStore.points) != 1 || semanticStore.points[0].ID != activeID {
		t.Fatalf("points = %#v", semanticStore.points)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDecisionAddRejectsConcurrentActiveFact(t *testing.T) {
	memory, _, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .*WHERE user_id=\? AND fact_key=\? AND status='active'.*FOR UPDATE`).
		WithArgs(int64(42), "workspace:billing-service:owner").
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(
				"active", 42, "workspace:billing-service:owner", KindProfile,
				"Owned by the platform team", SourceUserStated, StatusActive, nil, nil, now,
			)...))
	mock.ExpectRollback()

	_, err := memory.ApplyDecision(context.Background(), MemoryDecision{
		Action: ConsolidationAdd,
		Record: MemoryRecord{
			UserID: 42, FactKey: "workspace:billing-service:owner", Kind: KindProfile,
			Content: "Owned by the commerce team", SourceType: SourceExplicitUser,
		},
	})
	if !errors.Is(err, ErrStaleMemoryDecision) {
		t.Fatalf("error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDecisionReplaceCannotOverrideHigherAuthority(t *testing.T) {
	memory, _, mock, closeDB := newMemoryTestStore(t)
	defer closeDB()
	now := memory.now()
	activeID := "11111111-1111-1111-1111-111111111111"

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT .*WHERE user_id=\? AND fact_key=\? AND status='active'.*FOR UPDATE`).
		WithArgs(int64(42), "workspace:billing-service:owner").
		WillReturnRows(sqlmock.NewRows(memoryColumns()).
			AddRow(memoryRow(
				activeID, 42, "workspace:billing-service:owner", KindProfile,
				"Owned by the platform team", SourceExplicitUser, StatusActive, nil, nil, now,
			)...))
	mock.ExpectRollback()

	_, err := memory.ApplyDecision(t.Context(), MemoryDecision{
		Action:   ConsolidationReplace,
		TargetID: activeID,
		Record: MemoryRecord{
			UserID: 42, FactKey: "workspace:billing-service:owner", Kind: KindProfile,
			Content: "Owned by the commerce team", SourceType: SourceUserStated,
		},
	})
	if !errors.Is(err, ErrRejectedMemoryDecision) {
		t.Fatalf("error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
