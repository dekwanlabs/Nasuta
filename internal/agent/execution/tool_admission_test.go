package execution

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestExecuteToolTurnShortCircuitsFullyCoveredSeedScope(t *testing.T) {
	var calls atomic.Int32
	registry := tool.NewRegistry()
	if err := registry.Register(tool.Tool{
		ID: "read_doc", Kind: tool.KindRead,
		Description: "read one document",
		InputSchema: objectSchema(map[string]any{
			"doc_id": propString("document id"),
		}, []string{"doc_id"}),
		Admission: &tool.AdmissionSpec{
			ResolveScope: func(args tool.Arguments) (tool.EvidenceScope, error) {
				return tool.EvidenceScope{SourceKind: "runbook", Target: args.String("doc_id")}, nil
			},
			MaxResultTokens: func(tool.Arguments) int { return 100 },
		},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			calls.Add(1)
			return tool.Result{Content: "backend result"}, nil
		}),
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	snapshot := registry.Snapshot(tool.ReadPolicy())
	agent := NewAgent(nil, NewToolExecutor(registry), AgentConfig{}, NoopObserver(), nil)
	state := &compiledLoop{
		ctx: t.Context(), runCtx: t.Context(), loopCtx: t.Context(), runID: "run-test",
		toolSnapshot: snapshot, tools: agent.executor.Definitions(snapshot),
		result: &RunResult{}, seenTools: map[string]bool{}, remainingToolTokens: -1,
		evidenceLedger: newRunEvidenceLedger([]tool.EvidenceUnit{{
			SourceKind: "runbook", Target: "doc-a",
			Coverage: tool.EvidenceCoverage{Complete: true},
		}}),
		answerContract: &exactAnswerContract{},
	}
	agent.executeToolTurn(state, []llm.ToolCall{{
		ID: "call-1", Function: llm.ToolFunction{
			Name: "read_doc", Arguments: `{"doc_id":"doc-a"}`,
		},
	}})
	if got := calls.Load(); got != 0 {
		t.Fatalf("backend calls = %d, want 0", got)
	}
	if len(state.messages) != 1 {
		t.Fatalf("messages = %d, want one structured observation", len(state.messages))
	}
	var observation map[string]any
	if err := json.Unmarshal([]byte(state.messages[0].Content), &observation); err != nil {
		t.Fatalf("observation JSON: %v", err)
	}
	if observation["action"] != string(toolAdmissionAlreadyAvailable) {
		t.Fatalf("observation = %#v", observation)
	}
}

func TestEvidenceLedgerFullTargetCoversSectionRequest(t *testing.T) {
	ledger := newRunEvidenceLedger([]tool.EvidenceUnit{{
		SourceKind: "runbook", Target: "doc-a",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}})
	keys, covered := ledger.fullyCovers(tool.EvidenceScope{
		SourceKind: "runbook", Target: "doc-a",
		Sections: []string{"overview"},
	})
	if !covered || len(keys) != 1 || keys[0].section != "" {
		t.Fatalf("keys = %#v covered=%t", keys, covered)
	}
}

func TestAdmitToolCallNarrowsLimitBeforeExecution(t *testing.T) {
	registry := tool.NewRegistry()
	if err := registry.Register(tool.Tool{
		ID: "paged", Kind: tool.KindRead,
		Description: "paged read",
		InputSchema: objectSchema(map[string]any{
			"limit": propInt("page size"),
		}, nil),
		Admission: &tool.AdmissionSpec{
			MaxResultTokens: func(args tool.Arguments) int {
				return args.Int("limit", 5) * 100
			},
			Narrow: func(args tool.Arguments, available int) (tool.Arguments, bool) {
				limit := available / 100
				if limit < 1 || limit >= args.Int("limit", 5) {
					return args, false
				}
				narrowed := tool.Arguments{"limit": limit}
				return narrowed, true
			},
		},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{}, nil
		}),
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	snapshot := registry.Snapshot(tool.ReadPolicy())
	agent := NewAgent(nil, NewToolExecutor(registry), AgentConfig{ContextWindow: 10000}, NoopObserver(), nil)
	state := &compiledLoop{
		ctx: t.Context(), toolSnapshot: snapshot, evidenceLedger: newRunEvidenceLedger(nil),
		remainingToolTokens: 250,
	}
	call, decision := agent.admitToolCall(state, llm.ToolCall{
		ID: "call-1", Function: llm.ToolFunction{Name: "paged", Arguments: `{"limit":5}`},
	})
	if decision.Action != toolAdmissionNarrow || decision.DeclaredTokens != 200 {
		t.Fatalf("decision = %#v", decision)
	}
	var args tool.Arguments
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		t.Fatalf("narrowed args: %v", err)
	}
	if got := args.Int("limit", 0); got != 2 {
		t.Fatalf("limit = %d, want 2", got)
	}
}
