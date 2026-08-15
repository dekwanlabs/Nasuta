package execution

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/evidence"
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
	agent := NewAgent(nil, NewToolExecutor(registry), Config{}, NoopObserver(), nil)
	state := &compiledLoop{
		ctx: t.Context(), runCtx: t.Context(), loopCtx: t.Context(), runID: "run-test",
		toolSnapshot: snapshot, tools: agent.executor.Definitions(snapshot),
		result: &RunResult{}, seenTools: map[string]bool{}, remainingToolTokens: -1,
		evidenceLedger: newRunEvidenceLedger([]tool.EvidenceUnit{{
			SourceKind: "runbook", Target: "doc-a",
			Coverage: tool.EvidenceCoverage{Complete: true},
		}}, nil),
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
	}}, nil)
	keys, covered := ledger.fullyCovers(tool.EvidenceScope{
		SourceKind: "runbook", Target: "doc-a",
		Sections: []string{"overview"},
	})
	if !covered || len(keys) != 1 || keys[0].Section != "" {
		t.Fatalf("keys = %#v covered=%t", keys, covered)
	}
}

func TestFinalizeCompiledLoopPropagatesSeedConflictOnce(t *testing.T) {
	seed := tool.EvidenceUnit{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "version-a",
	}
	conflict := evidence.Conflict{
		Key:     evidence.Key{SourceKind: "runtime", Target: "trace-1"},
		Current: seed,
		Incoming: tool.EvidenceUnit{
			SourceKind: "runtime", Target: "trace-1", ContentHash: "version-b",
		},
		CurrentOrigin: "retrieval", IncomingOrigin: "preload",
	}
	ledger := newRunEvidenceLedger(
		[]tool.EvidenceUnit{seed},
		[]evidence.Conflict{conflict, conflict},
	)
	if conflicts := ledger.add([]tool.EvidenceUnit{conflict.Incoming}, "tool"); len(conflicts) != 0 {
		t.Fatalf("propagated conflict was reported again: %#v", conflicts)
	}
	state := &compiledLoop{
		ctx:            t.Context(),
		input:          Input{Direct: true},
		result:         &RunResult{},
		evidenceLedger: ledger,
	}
	NewAgent(nil, nil, Config{}, nil, nil).finalizeLoop(state)
	if len(state.result.EvidenceConflicts) != 1 ||
		state.result.EvidenceConflicts[0].Incoming.ContentHash != "version-b" {
		t.Fatalf("run result conflicts = %#v", state.result.EvidenceConflicts)
	}
}

func TestExecuteToolTurnSurfacesEvidenceConflictAsPartial(t *testing.T) {
	registry := tool.NewRegistry()
	if err := registry.Register(tool.Tool{
		ID: "read_trace", Kind: tool.KindRead,
		Description: "read one trace",
		InputSchema: objectSchema(map[string]any{
			"trace_id": propString("trace id"),
		}, []string{"trace_id"}),
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{
				Content: "incoming trace",
				EvidenceUnits: []tool.EvidenceUnit{{
					SourceKind: "runtime", Target: "trace-1", ContentHash: "hash-b",
					Coverage: tool.EvidenceCoverage{Complete: true},
				}},
				Coverage: tool.EvidenceCoverage{Complete: true},
			}, nil
		}),
	}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	snapshot := registry.Snapshot(tool.ReadPolicy())
	observer := &captureObserver{}
	agent := NewAgent(nil, NewToolExecutor(registry), Config{}, observer, nil)
	ledger := newRunEvidenceLedger([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "hash-a",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}}, nil)
	state := &compiledLoop{
		ctx: t.Context(), runCtx: t.Context(), loopCtx: t.Context(), runID: "run-conflict",
		toolSnapshot: snapshot, tools: agent.executor.Definitions(snapshot),
		result: &RunResult{}, seenTools: map[string]bool{}, remainingToolTokens: -1,
		evidenceLedger: ledger, answerContract: &exactAnswerContract{},
	}
	agent.executeToolTurn(state, []llm.ToolCall{{
		ID: "call-1", Function: llm.ToolFunction{
			Name: "read_trace", Arguments: `{"trace_id":"trace-1"}`,
		},
	}})

	if state.result.Err != nil {
		t.Fatalf("execute tool turn: %v", state.result.Err)
	}
	if len(state.messages) != 2 || !strings.Contains(state.messages[1].Content, `"type":"evidence_conflict"`) ||
		!strings.Contains(state.messages[1].Content, `"content_hash":"hash-a"`) ||
		!strings.Contains(state.messages[1].Content, `"content_hash":"hash-b"`) {
		t.Fatalf("messages = %#v", state.messages)
	}
	if state.result.Evidence.PartialResultCount != 1 {
		t.Fatalf("partial result count = %d", state.result.Evidence.PartialResultCount)
	}
	if len(observer.steps) != 2 || !observer.steps[1].Coverage.Partial ||
		observer.steps[1].Coverage.Complete {
		t.Fatalf("steps = %#v", observer.steps)
	}
	if conflicts := ledger.add([]tool.EvidenceUnit{{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "hash-a",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}}, "verification"); len(conflicts) != 0 {
		t.Fatalf("conflicting evidence replaced canonical version: %#v", conflicts)
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
	agent := NewAgent(nil, NewToolExecutor(registry), Config{ContextWindow: 10000}, NoopObserver(), nil)
	state := &compiledLoop{
		ctx: t.Context(), toolSnapshot: snapshot, evidenceLedger: newRunEvidenceLedger(nil, nil),
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
