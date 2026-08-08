package qa

import (
	"context"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func TestBuildHistoryRouteContextContainsMetadataOnly(t *testing.T) {
	conversation := ConversationContext{
		SessionTitle: "runtime investigation",
		RecentTurns: []memory.TurnMetadata{{
			TurnNumber: 7, Question: "继续 trace-123", TopicKey: "trace-123",
			Entities: []string{"trace-123"}, QuestionTerms: []string{"trace-123"},
			EvidenceManifest: memory.EvidenceManifest{Status: "available", Items: []memory.EvidenceManifestItem{{
				Tool: "observe_logs", Source: "observe_logs", References: []string{"trace-123"}, Coverage: "partial",
			}}},
		}},
	}
	got := buildHistoryRouteContext(conversation)
	for _, want := range []string{"runtime investigation", "继续 trace-123", "observe_logs", "partial"} {
		if !strings.Contains(got, want) {
			t.Fatalf("route context missing %q: %s", want, got)
		}
	}
	for _, forbidden := range []string{"assistant_answer", "tool_payload", "request_body", "response_body"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("route context leaked %q: %s", forbidden, got)
		}
	}
}

func TestResolveHistoryRelationPreservesCrossSourceEntityContinuity(t *testing.T) {
	latest := turnMetadataForQuestion(8, "查 hs-user-service 的日志")
	relation, origin, _ := resolveHistoryRelation(
		"再看 hs-user-service 的配置", []memory.TurnMetadata{latest},
		retrieval.HistoryRelation{TopicAffinity: 0.7, Confidence: 0.8}, true,
	)
	if origin != "model" || relation.TopicAffinity <= 0 {
		t.Fatalf("relation = %+v origin=%q", relation, origin)
	}
	if relation.NeedsPriorEvidence {
		t.Fatalf("cross-source continuity replayed unrelated evidence: %+v", relation)
	}
}

func TestResolveHistoryRelationUpgradesUnresolvedEvidenceReference(t *testing.T) {
	latest := turnMetadataForQuestion(8, "查 trace-123 的日志")
	latest.EvidenceManifest = memory.EvidenceManifest{
		Status: "available", Items: []memory.EvidenceManifestItem{{Tool: "observe_logs", Coverage: "full"}},
	}
	relation, origin, upgrade := resolveHistoryRelation(
		"继续看刚才的错误证据", []memory.TurnMetadata{latest}, retrieval.HistoryRelation{Confidence: 0.2}, true,
	)
	if origin != "model" || upgrade != "reference_requires_evidence" ||
		!relation.NeedsPriorEntities || !relation.NeedsPriorConclusion || !relation.NeedsPriorEvidence {
		t.Fatalf("relation = %+v origin=%q upgrade=%q", relation, origin, upgrade)
	}
}

func TestSelectActiveTurnsKeepsMandatoryPreviousAndBoundsRelated(t *testing.T) {
	candidates := make([]memory.TurnMetadata, 0, 10)
	for turn := 10; turn >= 1; turn-- {
		candidates = append(candidates, turnMetadataForQuestion(turn, "查 hs-user-service 的日志"))
	}
	selected := selectActiveTurns("继续查 hs-user-service", candidates, retrieval.HistoryRelation{
		TopicAffinity: 0.9, NeedsPriorEntities: true,
	})
	if len(selected) != activeHistoryTopK {
		t.Fatalf("selected %d turns, want %d: %#v", len(selected), activeHistoryTopK, selected)
	}
	foundLatest := false
	for _, item := range selected {
		foundLatest = foundLatest || item.metadata.TurnNumber == 10
	}
	if !foundLatest {
		t.Fatalf("mandatory latest turn omitted: %#v", selected)
	}
}

func TestAssembleActiveHistoryLoadsOneCompleteAtomicTurn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`SELECT m\.turn_no,m\.role,m\.content.*m\.turn_no IN \(\?\).*ORDER BY m\.turn_no,m\.seq`).
		WithArgs("session-1", int64(42), 8).
		WillReturnRows(sqlmock.NewRows([]string{
			"turn_no", "role", "content", "tool_calls_json", "tool_call_id", "tool_name",
		}).
			AddRow(8, "user", "查 trace-123", "", "", "").
			AddRow(8, "assistant", "", `[{"id":"call-8","type":"function","function":{"name":"observe_logs","arguments":"{}"}}]`, "", "").
			AddRow(8, "tool", "evidence", "", "call-8", "observe_logs").
			AddRow(8, "assistant", "answer", "", "", ""))
	metadata := turnMetadataForQuestion(8, "查 trace-123")
	metadata.EvidenceManifest = memory.EvidenceManifest{Status: "available", Items: []memory.EvidenceManifestItem{{Tool: "observe_logs", Coverage: "full"}}}
	svc := &QA{
		sessions: memory.NewSessionStore(db), contextWindow: 128000,
		outputReserve: 4000,
	}
	conversation, stats, err := svc.assembleActiveHistory(
		context.Background(), "继续看刚才的错误证据", 42,
		ConversationContext{SessionID: "session-1", RecentTurns: []memory.TurnMetadata{metadata}},
		retrieval.HistoryRelation{NeedsPriorEntities: true, NeedsPriorConclusion: true, NeedsPriorEvidence: true},
		"model", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Recent) != 4 || stats.FullTurnCount != 1 || conversation.HistoricalContext != "" {
		t.Fatalf("conversation=%#v stats=%+v", conversation, stats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func turnMetadataForQuestion(turn int, question string) memory.TurnMetadata {
	topic, entities, terms := memory.CanonicalQuestionMetadata(question)
	return memory.TurnMetadata{
		TurnNumber: turn, Question: question, TopicKey: topic, Entities: entities, QuestionTerms: terms,
		EvidenceManifest: memory.EvidenceManifest{Status: "none", Items: []memory.EvidenceManifestItem{}},
	}
}
