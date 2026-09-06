package qa

import (
	"context"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func TestBuildHistoryRouteContextContainsMetadataAndRecentDialogue(t *testing.T) {
	conversation := execution.ConversationContext{
		SessionTitle: "runtime investigation",
		RecentTurns: []memory.TurnMetadata{{
			TurnNumber: 7, Question: "继续 trace-123", TopicKey: "trace-123",
			Entities: []string{"trace-123"}, QuestionTerms: []string{"trace-123"},
			EvidenceManifest: memory.EvidenceManifest{Status: "available", Items: []memory.EvidenceManifestItem{{
				Tool: "observe_logs", Source: "observe_logs", References: []string{"trace-123"}, Coverage: "partial",
			}}},
		}},
		RecentDialogue: []memory.RecentDialogueTurn{{
			TurnNumber: 7, User: "列出 UserController 选项",
			Assistant: "1. alpha\n2. hsas-backstage-user",
		}},
	}
	got := buildHistoryContext(conversation)
	for _, want := range []string{"runtime investigation", "继续 trace-123", "hsas-backstage-user", "recent_dialogue"} {
		if !strings.Contains(got, want) {
			t.Fatalf("route context missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "observe_logs") || strings.Contains(got, "partial") {
		t.Fatalf("route context included unbounded evidence details: %s", got)
	}
	for _, forbidden := range []string{"assistant_answer", "tool_payload", "request_body", "response_body", "SECRET_TOOL_PAYLOAD"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("route context leaked %q: %s", forbidden, got)
		}
	}
}

func TestBuildHistoryRouteContextBoundsDialogueAndEntities(t *testing.T) {
	conversation := execution.ConversationContext{
		SessionTitle: strings.Repeat("title ", 500),
		RecentTurns: []memory.TurnMetadata{{
			TurnNumber: 9, Question: strings.Repeat("question ", 500),
			TopicKey: strings.Repeat("topic ", 100),
			Entities: []string{strings.Repeat("entity ", 100), "second"},
		}},
		RecentDialogue: []memory.RecentDialogueTurn{
			{TurnNumber: 8, User: strings.Repeat("old ", 500), Assistant: strings.Repeat("old answer ", 500)},
			{TurnNumber: 9, User: strings.Repeat("current ", 500), Assistant: strings.Repeat("current answer ", 500)},
		},
	}
	got := buildHistoryContext(conversation)
	if len(got) > 12_000 {
		t.Fatalf("route context length = %d, want bounded context", len(got))
	}
	if strings.Contains(got, "old answer old answer") {
		t.Fatal("route context retained an older dialogue turn")
	}
}

func TestResolveHistoryRelationTrustsModelWhenValid(t *testing.T) {
	latest := turnMetadataForQuestion(8, "查 hs-user-service 的日志")
	relation, origin := resolveHistoryRelation(
		"再看 hs-user-service 的配置", []memory.TurnMetadata{latest},
		retrieval.HistoryRelation{TopicAffinity: 0.7, Confidence: 0.8}, true,
	)
	if origin != "model" || relation.TopicAffinity != 0.7 {
		t.Fatalf("relation = %+v origin=%q", relation, origin)
	}
	if relation.NeedsPriorEvidence {
		t.Fatalf("cross-source continuity replayed unrelated evidence: %+v", relation)
	}
}

func TestResolveHistoryRelationFallsBackToLexicalAffinity(t *testing.T) {
	latest := turnMetadataForQuestion(8, "查 hs-user-service 的日志")
	relation, origin := resolveHistoryRelation(
		"查 hs-user-service 的日志", []memory.TurnMetadata{latest}, retrieval.HistoryRelation{}, false,
	)
	if origin != "deterministic" || relation.Confidence != 0.5 || relation.TopicAffinity <= 0 {
		t.Fatalf("relation = %+v origin=%q", relation, origin)
	}
	if relation.NeedsPriorEntities || relation.NeedsPriorConclusion || relation.NeedsPriorEvidence ||
		len(relation.ExplicitTurnRefs) != 0 {
		t.Fatalf("deterministic fallback produced model-only fields: %+v", relation)
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
	svc := &Service{
		sessions: memory.NewSessionStore(db), contextWindow: 128000,
		outputReserve: 4000,
	}
	conversation, stats, err := svc.assembleActiveHistory(
		context.Background(), "继续看刚才的错误证据", 42,
		execution.ConversationContext{SessionID: "session-1", RecentTurns: []memory.TurnMetadata{metadata}},
		retrieval.HistoryRelation{NeedsPriorEntities: true, NeedsPriorConclusion: true, NeedsPriorEvidence: true},
		"model",
		128000, 4000,
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

func TestAssembleActiveHistoryUsesRecentAnswerWithoutReloadingToolTurn(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	metadata := turnMetadataForQuestion(8, "列出 UserController 选项")
	metadata.EvidenceManifest = memory.EvidenceManifest{
		Status: "available", Items: []memory.EvidenceManifestItem{{Tool: "code_search", Coverage: "full"}},
	}
	svc := &Service{
		sessions: memory.NewSessionStore(db), contextWindow: 128000,
		outputReserve: 4000,
	}
	conversation, stats, err := svc.assembleActiveHistory(
		context.Background(), "2", 42,
		execution.ConversationContext{
			SessionID: "session-1", RecentTurns: []memory.TurnMetadata{metadata},
			RecentDialogue: []memory.RecentDialogueTurn{{
				TurnNumber: 8, User: "列出 UserController 选项",
				Assistant: "1. alpha\n2. hsas-backstage-user",
			}},
		},
		retrieval.HistoryRelation{NeedsPriorEntities: true, NeedsPriorConclusion: true},
		"model",
		128000, 4000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Recent) != 0 || stats.DetailCount != 0 || stats.ReferenceCount != 1 {
		t.Fatalf("conversation=%#v stats=%+v", conversation, stats)
	}
	if !strings.Contains(conversation.HistoricalContext, `"representation":"reference"`) {
		t.Fatalf("historical context = %s", conversation.HistoricalContext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAssembleContextUsesDefinitionLimitsForActiveHistory(t *testing.T) {
	metadata := turnMetadataForQuestion(8, "列出 UserController 选项")
	svc := &Service{
		contextWindow: 128000,
		outputReserve: 4000,
	}

	output, err := svc.assembleContext(t.Context(), contextAssembleInput{
		Question: "2",
		UserID:   42,
		Conversation: execution.ConversationContext{
			RecentTurns: []memory.TurnMetadata{metadata},
			RecentDialogue: []memory.RecentDialogueTurn{{
				TurnNumber: 8,
				User:       "列出 UserController 选项",
				Assistant:  "1. alpha\n2. hsas-backstage-user",
			}},
		},
		Relation:      retrieval.HistoryRelation{NeedsPriorEntities: true, NeedsPriorConclusion: true},
		Origin:        "model",
		ContextWindow: 8192,
		OutputReserve: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Stats.HistoryBudgetTokens != 768 {
		t.Fatalf("history budget = %d, want definition-scoped budget 768",
			output.Stats.HistoryBudgetTokens)
	}
}

func turnMetadataForQuestion(turn int, question string) memory.TurnMetadata {
	topic, entities, terms := memory.CanonicalQuestionMetadata(question)
	return memory.TurnMetadata{
		TurnNumber: turn, Question: question, TopicKey: topic, Entities: entities, QuestionTerms: terms,
		EvidenceManifest: memory.EvidenceManifest{Status: "none", Items: []memory.EvidenceManifestItem{}},
	}
}
