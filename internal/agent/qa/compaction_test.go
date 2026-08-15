package qa

import (
	"context"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

type compactionToolSet struct {
	tools []tool.Tool
}

func (set compactionToolSet) Tools() []tool.Tool {
	return set.tools
}

func (set compactionToolSet) Get(id tool.ToolID) (tool.Tool, bool) {
	for _, candidate := range set.tools {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return tool.Tool{}, false
}

func (compactionToolSet) Execute(context.Context, tool.ToolID, tool.Arguments) (tool.Result, error) {
	return tool.Result{}, nil
}

func TestSessionCompactionIncomingTokensIncludesRetrievedContext(t *testing.T) {
	plan := domain.EvidencePlan{Sources: domain.Internal}
	withHistory := ConversationContext{
		Recent:       []llm.Message{{Role: "user", Content: strings.Repeat("old history ", 1000)}},
		Instructions: []llm.Message{{Role: "system", Content: "request instruction"}},
	}
	withoutHistory := withHistory
	withoutHistory.Recent = nil
	retrieved := &retrieval.RetrievedContext{Text: strings.Repeat("retrieved evidence ", 1000), HitCount: 3}

	got, _, err := compactionProjection(
		"current question", withHistory, retrieved, plan, "", nil, 0,
	)
	if err != nil {
		t.Fatalf("with history projection: %v", err)
	}
	want, _, err := compactionProjection(
		"current question", withoutHistory, retrieved, plan, "", nil, 0,
	)
	if err != nil {
		t.Fatalf("without history projection: %v", err)
	}
	withoutRetrieval, _, err := compactionProjection(
		"current question", withoutHistory, nil, plan, "", nil, 0,
	)
	if err != nil {
		t.Fatalf("without retrieval projection: %v", err)
	}

	if got != want {
		t.Fatalf("session history was counted as incoming context: got=%d want=%d", got, want)
	}
	if got <= withoutRetrieval {
		t.Fatalf("retrieved context was not counted: with=%d without=%d", got, withoutRetrieval)
	}
}

func TestSessionCompactionProjectionKeepsRetrievedHistory(t *testing.T) {
	plan := domain.EvidencePlan{Sources: domain.Internal}
	withArchivedHistory := ConversationContext{
		RetrievedHistory: strings.Repeat("archived answer ", 1000),
	}
	withoutArchivedHistory := withArchivedHistory
	withoutArchivedHistory.RetrievedHistory = ""

	withTokens, _, err := compactionProjection(
		"follow-up question", withArchivedHistory, nil, plan, "", nil, 0,
	)
	if err != nil {
		t.Fatalf("with archived history projection: %v", err)
	}
	withoutTokens, _, err := compactionProjection(
		"follow-up question", withoutArchivedHistory, nil, plan, "", nil, 0,
	)
	if err != nil {
		t.Fatalf("without archived history projection: %v", err)
	}
	if withTokens <= withoutTokens {
		t.Fatalf("retrieved archived history was dropped from incoming context: with=%d without=%d",
			withTokens, withoutTokens)
	}
}

func TestSessionCompactionProjectionIncludesToolSchemas(t *testing.T) {
	plan := domain.EvidencePlan{Sources: domain.Internal}
	candidate := tool.Tool{
		ID:          "inspect_large_schema",
		Description: strings.Repeat("tool description ", 300),
		InputSchema: tool.JSONSchema{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": strings.Repeat("query field ", 300),
				},
			},
		},
	}

	withoutTools, _, err := compactionProjection(
		"inspect the service", ConversationContext{}, nil, plan, "", nil, 0,
	)
	if err != nil {
		t.Fatalf("without tools projection: %v", err)
	}
	withTools, _, err := compactionProjection(
		"inspect the service", ConversationContext{}, nil, plan, "", []tool.Tool{candidate}, 0,
	)
	if err != nil {
		t.Fatalf("with tools projection: %v", err)
	}
	if withTools <= withoutTools {
		t.Fatalf("tool schemas were not included in incoming context: with=%d without=%d",
			withTools, withoutTools)
	}
}

func TestSessionCompactionToolsUsesPrunedSet(t *testing.T) {
	prepared := compactionToolSet{tools: []tool.Tool{
		{ID: "inspect_service"},
		{ID: "inspect_runbook"},
	}}
	selected := sessionCompactionTools(prepared, ConversationContext{
		PruneApplied:  true,
		PrunedToolIDs: map[tool.ToolID]struct{}{"inspect_runbook": {}},
	})
	if len(selected) != 1 || selected[0].ID != "inspect_runbook" {
		t.Fatalf("selected tools = %+v", selected)
	}
	selected = sessionCompactionTools(prepared, ConversationContext{
		PruneApplied:  true,
		PrunedToolIDs: map[tool.ToolID]struct{}{},
	})
	if len(selected) != 0 {
		t.Fatalf("empty pruned set restored all tools: %+v", selected)
	}
}

func TestNewQAUsesAnswerLimitForDefaultOutputReserve(t *testing.T) {
	settings := &config.PlatformSettings{
		LLMMaxTokens:       24000,
		LLMAnswerMaxTokens: 12000,
		LLMContextWindow:   256000,
	}
	svc := New(Deps{
		Platform: settings,
		Definitions: definitionResolverFunc(func(agentapi.DefinitionRef) (agentapi.Definition, error) {
			return agentapi.Definition{}, nil
		}),
	})

	if svc.outputReserve != 12000 {
		t.Fatalf("output reserve = %d, want answer limit 12000", svc.outputReserve)
	}
}

func TestCompactBeforeAnswerUsesResolvedDefinitionLimits(t *testing.T) {
	recorder := &compactionStatusRecorder{}
	svc := &Service{
		contextWindow: 256000,
		outputReserve: 24000,
		phaseEmitter:  recorder,
	}
	prepared := &preparation{
		request: Request{RunID: "run-custom-limits", Question: "current question"},
	}

	_, err := svc.compactAnswer(
		t.Context(),
		prepared,
		ConversationContext{},
		nil,
		domain.EvidencePlan{},
		128000,
		12000,
	)
	if err != nil {
		t.Fatalf("compact before answer: %v", err)
	}
	if recorder.contextEvent.ContextWindow != 128000 {
		t.Fatalf("context window = %d, want resolved definition window 128000",
			recorder.contextEvent.ContextWindow)
	}
	if recorder.contextEvent.OutputReserveTokens != 12000 {
		t.Fatalf("output reserve = %d, want resolved definition limit 12000",
			recorder.contextEvent.OutputReserveTokens)
	}
}
