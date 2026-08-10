package execution

import (
	"strings"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

func TestBuildAgentMessagesUsesRecalledHistoryAndRecentTail(t *testing.T) {
	agent := &Agent{cfg: AgentConfig{HistoryLimit: 2}}
	conversation := ConversationContext{
		RetrievedHistory:     `{"version":1,"mode":"hybrid","turns":[{"ref":"cmp-124","turn":2,"summary":"recalled finding"}]}`,
		CompactedThroughTurn: 4,
		RolePrompt:           "## Identity\n- Role: SRE",
		Instructions:         []llm.Message{{Role: "system", Content: "role instruction"}},
		Recent: []llm.Message{
			{Role: "user", Content: "old turn"},
			{Role: "assistant", Content: "recent answer"},
			{Role: "user", Content: "recent question"},
		},
		RecentDialogue: []memory.RecentDialogueTurn{{
			TurnNumber: 7, User: "列出 UserController 选项",
			Assistant: "1. alpha\n2. hsas-backstage-user",
		}},
	}

	got := agent.buildAgentMessages("current question", conversation, &retrieval.RetrievedContext{}, domain.DirectPlan())
	joined := ""
	for _, message := range got {
		joined += "\n" + message.Content
	}
	for _, want := range []string{"recalled finding", "cmp-124", `<retrieved_session_history format="json">`, "get_turn", "find_turns", "## Identity\n- Role: SRE", "role instruction", "hsas-backstage-user", "recent answer", "recent question", "current question"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("messages missing %q: %s", want, joined)
		}
	}
	if got := got[0].Content; !strings.Contains(got, "## Identity\n- Role: SRE") {
		t.Fatalf("role prompt was not composed into the primary system prompt: %s", got)
	}
	if len(got) > 1 && strings.Contains(got[1].Content, "## Identity\n- Role: SRE") {
		t.Fatalf("role prompt was duplicated as a separate instruction: %+v", got)
	}
	if strings.Contains(joined, "old turn") {
		t.Fatalf("messages retained old turn: %s", joined)
	}
}

func TestReplayableTailMessagesKeepsCompleteToolGroup(t *testing.T) {
	call := llm.ToolCall{
		ID: "call-1", Type: "function",
		Function: llm.ToolFunction{Name: "observe", Arguments: `{"url":"/v1/items"}`},
	}
	messages := []llm.Message{
		{Role: "user", Content: "查一下"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call}},
		{Role: "tool", ToolCallID: "call-1", Name: "observe", Content: "found"},
		{Role: "assistant", Content: "查到了"},
	}

	got := ReplayableTailMessages(messages, 2)
	if len(got) != 4 || got[0].Content != "查一下" || len(got[1].ToolCalls) != 1 || got[2].Role != "tool" || got[3].Content != "查到了" {
		t.Fatalf("replayable tail = %#v", got)
	}
}

func TestReplayableTailMessagesDropsInvalidToolGroups(t *testing.T) {
	calls := []llm.ToolCall{
		{ID: "call-1", Function: llm.ToolFunction{Name: "observe", Arguments: `{}`}},
		{ID: "call-2", Function: llm.ToolFunction{Name: "search", Arguments: `{}`}},
	}
	messages := []llm.Message{
		{Role: "tool", ToolCallID: "orphan", Name: "observe", Content: "orphan"},
		{Role: "assistant", ToolCalls: calls},
		{Role: "tool", ToolCallID: "call-1", Name: "observe", Content: "partial"},
		{Role: "user", Content: "继续"},
	}

	got := ReplayableTailMessages(messages, 10)
	if len(got) != 1 || got[0].Role != "user" || got[0].Content != "继续" {
		t.Fatalf("invalid groups were replayed: %#v", got)
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
		"Keep runtime concepts endpoint-scoped",
		"Partial coverage never proves",
		"same natural language as the user's current question",
	} {
		if !strings.Contains(agentSystemPrompt, want) {
			t.Fatalf("agent prompt missing incomplete-flow guard %q", want)
		}
	}
	if !strings.Contains(agentSystemPrompt, "observe_url") || !strings.Contains(agentSystemPrompt, "在日志追踪中查看") {
		t.Fatal("agent prompt does not require Observe trace deep links")
	}
	if !strings.Contains(agentSystemPrompt, "Never enumerate more than five individual error records") {
		t.Fatal("agent prompt does not bound runtime error detail")
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

func TestEvidencePlanGuidesPreRetrievalWithoutRestrictingReadTools(t *testing.T) {
	instruction := evidencePlanInstruction(domain.EvidencePlan{Sources: domain.Web})
	for _, want := range []string{"automatic pre-retrieval", "Other registered read capabilities remain available"} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("evidence plan instruction missing %q: %s", want, instruction)
		}
	}
	for _, forbidden := range []string{"Use only the selected evidence capabilities", "using only fetched web evidence", "selected evidence plan allows internal tools"} {
		if strings.Contains(instruction, forbidden) || strings.Contains(webAgentSystemPrompt, forbidden) {
			t.Fatalf("evidence prompt still imposes hard source restriction %q", forbidden)
		}
	}
	if !strings.Contains(webAgentSystemPrompt, "another registered read capability") {
		t.Fatal("web prompt does not preserve access to registered read capabilities")
	}
}
