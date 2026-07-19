package agent

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/llm"
)

func TestBuildAgentMessagesUsesCanonicalSummaryAndRecentTail(t *testing.T) {
	agent := &Agent{cfg: AgentConfig{HistoryLimit: 2}}
	conversation := ConversationContext{
		Summary:      "canonical session summary",
		Instructions: []llm.Message{{Role: "system", Content: "role instruction"}},
		Recent: []llm.Message{
			{Role: "user", Content: "old turn"},
			{Role: "assistant", Content: "recent answer"},
			{Role: "user", Content: "recent question"},
		},
	}

	got := agent.buildAgentMessages("current question", conversation, &retrieval.RetrievedContext{}, domain.DirectPlan())
	joined := ""
	for _, message := range got {
		joined += "\n" + message.Content
	}
	for _, want := range []string{"canonical session summary", "role instruction", "recent answer", "recent question", "current question"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("messages missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "old turn") {
		t.Fatalf("messages retained old turn: %s", joined)
	}
}

func TestBuildAgentMessagesTreatsReferenceCountAsCandidates(t *testing.T) {
	agent := &Agent{cfg: AgentConfig{HistoryLimit: 2}}
	messages := agent.buildAgentMessages(
		"how does the complete flow work",
		ConversationContext{},
		&retrieval.RetrievedContext{Text: "seed evidence", HitCount: 22},
		domain.EvidencePlan{Sources: domain.Internal},
	)
	joined := ""
	for _, message := range messages {
		joined += "\n" + message.Content
	}
	for _, want := range []string{"22 candidate references", "not proof that every requested path is covered", "investigate one specific missing critical hop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("pre-retrieval guidance missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "already searched across 22 sources") {
		t.Fatalf("candidate count still presented as complete source coverage: %s", joined)
	}
}

func TestAgentPromptRequiresLookupBeforeEndingIncompleteFlow(t *testing.T) {
	for _, want := range []string{
		"Complete requested chains before stopping",
		"MUST make one targeted tool round",
		"Keep distinct entry points and execution branches separate",
		"same natural language as the user's current question",
	} {
		if !strings.Contains(agentSystemPrompt, want) {
			t.Fatalf("agent prompt missing incomplete-flow guard %q", want)
		}
	}
	if strings.Contains(agentSystemPrompt, "ONE short English sentence") {
		t.Fatal("tool rationale still conflicts with the question-language rule")
	}
}

func TestAgentPromptForPlanUsesCompactWebPromptOnlyForWeb(t *testing.T) {
	direct := agentPromptForPlan(domain.DirectPlan())
	if direct != directAgentSystemPrompt || strings.Contains(direct, "trace_calls") {
		t.Fatalf("direct prompt was not compact: %q", direct)
	}
	web := agentPromptForPlan(domain.EvidencePlan{Sources: domain.Web})
	if web != webAgentSystemPrompt || strings.Contains(web, "trace_calls") {
		t.Fatalf("web prompt was not compact: %q", web)
	}
	mixed := agentPromptForPlan(domain.EvidencePlan{Sources: domain.Web | domain.Internal})
	if mixed != agentSystemPrompt {
		t.Fatal("mixed plan lost engineering prompt")
	}
}

func TestBuildRouteContextIncludesBoundedSummaryWithoutChangingRetrievalPrefix(t *testing.T) {
	retrievalPrefix := "[user]: recent"
	got := buildRouteContext(strings.Repeat("a", 1600), retrievalPrefix)
	if !strings.HasPrefix(got, "[summary]: ") || !strings.Contains(got, retrievalPrefix) {
		t.Fatalf("route context = %q", got)
	}
	if len([]rune(got)) != len([]rune("[summary]: "))+1500+1+len([]rune(retrievalPrefix)) {
		t.Fatalf("route context was not bounded: %d", len([]rune(got)))
	}
}
