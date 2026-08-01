package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/knowledge"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestOutcomeForRejectsEmptySuccess(t *testing.T) {
	tests := []struct {
		name   string
		result *RunResult
		runErr error
		status RunStatus
		err    error
	}{
		{name: "done", result: &RunResult{Answer: "answer", Steps: 1}, status: RunStatusDone},
		{name: "empty", result: &RunResult{Steps: 1}, status: RunStatusFailed, err: ErrEmptyAnswer},
		{name: "aborted", result: &RunResult{Aborted: true}, status: RunStatusAborted},
		{name: "run error", result: &RunResult{}, runErr: errors.New("provider failed"), status: RunStatusFailed},
		{name: "result error", result: &RunResult{Err: errors.New("truncated")}, status: RunStatusFailed},
		{name: "nil result", runErr: errors.New("missing"), status: RunStatusFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := outcomeFor(test.result, test.runErr)
			if outcome.Status != test.status {
				t.Fatalf("status = %s, want %s", outcome.Status, test.status)
			}
			if test.err != nil && !errors.Is(outcome.Err, test.err) {
				t.Fatalf("error = %v, want %v", outcome.Err, test.err)
			}
		})
	}
}

func TestEvidenceMetricsFinalStatus(t *testing.T) {
	tests := []struct {
		name   string
		direct bool
		input  EvidenceMetrics
		want   EvidenceStatus
	}{
		{name: "direct", direct: true, want: EvidenceNotRequired},
		{name: "missing", input: EvidenceMetrics{ToolCallCount: 1, ToolFailureCount: 1}, want: EvidenceUnavailable},
		{name: "complete", input: EvidenceMetrics{ToolCallCount: 1, ResultCount: 1}, want: EvidenceComplete},
		{name: "partial", input: EvidenceMetrics{ToolCallCount: 2, ResultCount: 1, PartialResultCount: 1, OmittedItemCount: 3}, want: EvidencePartial},
		{name: "forced", input: EvidenceMetrics{ResultCount: 1, ForcedConclusion: true}, want: EvidencePartial},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metrics := test.input
			metrics.finalize(test.direct)
			if metrics.Status != test.want {
				t.Fatalf("status = %q, want %q", metrics.Status, test.want)
			}
		})
	}
}

func TestRoutedTemporalToolUsesFullInvestigationBudget(t *testing.T) {
	candidates := []retrieval.ToolRouteCandidate{
		{ID: "search_code", Temporal: false},
		{ID: "observe_logs", Temporal: true},
	}
	if !routedToolsNeedFullInvestigation(candidates, []string{"observe_logs"}) {
		t.Fatal("selected temporal tool did not enable full investigation")
	}
	agent := &Agent{cfg: AgentConfig{MaxSteps: 8}}
	if got := agent.MaxStepsForContext("查一下问题", domain.EvidencePlan{}, true); got != 8 {
		t.Fatalf("max steps = %d, want 8", got)
	}
	if routedToolsNeedFullInvestigation(candidates, []string{"search_code"}) {
		t.Fatal("non-temporal tool enabled full investigation")
	}
}

func TestForcedConclusionCannotExtractLongTermMemory(t *testing.T) {
	outcome := RunOutcome{Status: RunStatusDone}
	if memoryExtractionAllowed(outcome, &RunResult{Answer: "answer", ForcedConclusion: true}) {
		t.Fatal("forced conclusion was eligible for memory extraction")
	}
	if !memoryExtractionAllowed(outcome, &RunResult{Answer: "answer"}) {
		t.Fatal("normal completed answer was not eligible for memory extraction")
	}
}

func TestAdmitExtractedMemoriesRejectsAssistantInference(t *testing.T) {
	records := []memory.MemoryRecord{
		{FactKey: "user:response-language", SourceType: memory.SourceExplicitUser},
		{FactKey: "workspace:service:root-cause", SourceType: memory.SourceAssistantInference},
	}
	admitted, rejected := admitExtractedMemories(records, EvidencePartial)
	if len(admitted) != 1 || admitted[0].SourceType != memory.SourceExplicitUser {
		t.Fatalf("admitted = %#v", admitted)
	}
	if rejected["assistant_inference"] != 1 {
		t.Fatalf("rejected = %#v", rejected)
	}
}

func TestRunStoreCompleteTransitionsOnlyActiveRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	outcome := RunOutcome{
		Status: RunStatusDone, StepCount: 2, TokenUsed: 12,
		Evidence: EvidenceMetrics{
			Status: EvidencePartial, ForcedConclusion: true, ResultCount: 3,
			ToolCallCount: 4, ToolFailureCount: 1, PartialResultCount: 2, OmittedItemCount: 5,
		},
	}
	mock.ExpectExec("UPDATE agent_runs").
		WithArgs(
			RunStatusDone, 2, 12, EvidencePartial, true, 3, 4, 1, 2, 5,
			sqlmock.AnyArg(), "run", RunStatusRunning, RunStatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.Complete("run", outcome); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreEvidenceByIDsIsBoundToUserAndSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	mock.ExpectQuery(`SELECT id,evidence_status.*FROM agent_runs WHERE user_id=\? AND session_id=\? AND id IN \(\?,\?\)`).
		WithArgs(int64(42), "session-1", "run-1", "run-2").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "evidence_status", "forced_conclusion", "evidence_result_count", "tool_call_count",
			"tool_failure_count", "partial_result_count", "omitted_evidence_count",
		}).AddRow("run-1", EvidencePartial, true, 2, 3, 1, 1, 4))

	evidence, err := store.EvidenceByIDs(42, "session-1", []string{"run-1", "run-2"})
	if err != nil {
		t.Fatalf("EvidenceByIDs: %v", err)
	}
	metrics, ok := evidence["run-1"]
	if !ok || metrics.Status != EvidencePartial || !metrics.ForcedConclusion || metrics.OmittedItemCount != 4 {
		t.Fatalf("evidence = %#v", evidence)
	}
	if _, ok := evidence["run-2"]; ok {
		t.Fatalf("unexpected evidence for absent run: %#v", evidence)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAskStopsWhenRunCreationFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec("INSERT INTO agent_runs").WillReturnError(errors.New("database unavailable"))

	qa := &QA{
		runStore: &RunStore{db: db},
		agent:    &Agent{cfg: AgentConfig{MaxSteps: 3}},
	}
	result, err := qa.Ask(context.Background(), QARequest{Question: "where is the endpoint?", RunID: "run-create-fail"})
	if err == nil || !strings.Contains(err.Error(), "create agent run") {
		t.Fatalf("result=%+v error=%v, want run creation failure", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreRecordLLMCallUpdatesDetailAndAggregateAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	call := llm.CallUsage{
		RunID: "run", Phase: llm.PhaseAgentStep, Provider: "openai", Model: "model",
		MaxOutputTokens: 50, Duration: 12 * time.Millisecond, Status: llm.CallStatusSucceeded,
		Usage: llm.Usage{
			InputTokens: 10, CachedInputTokens: 2, OutputTokens: 5,
			ReasoningTokens: 1, TotalTokens: 15,
		},
	}

	mock.ExpectBegin()
	mock.ExpectQuery("SELECT llm_call_count FROM agent_runs").
		WithArgs("run").
		WillReturnRows(sqlmock.NewRows([]string{"llm_call_count"}).AddRow(2))
	mock.ExpectExec("INSERT INTO agent_llm_calls").
		WithArgs("run", 3, llm.PhaseAgentStep, "openai", "model", 10, 2, 5, 1, 15, 50, int64(12), llm.CallStatusSucceeded).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE agent_runs SET").
		WithArgs(10, 2, 5, 1, 15, 3, 10, 60, "run").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.RecordLLMCall(t.Context(), call); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreUsageSummaryUsesSessionAggregateAndLatestRound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}

	mock.ExpectQuery("SELECT COALESCE\\(SUM\\(total_tokens\\),0\\) FROM agent_runs").
		WithArgs(int64(7), "session-1").
		WillReturnRows(sqlmock.NewRows([]string{"total_tokens"}).AddRow(420))
	mock.ExpectQuery("SELECT id,input_tokens,cached_input_tokens,total_tokens,peak_input_tokens,peak_reserved_tokens").
		WithArgs(int64(7), "session-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "input_tokens", "cached_input_tokens", "total_tokens", "peak_input_tokens", "peak_reserved_tokens",
		}).AddRow("run-2", 100, 90, 125, 80, 120))

	summary, err := store.UsageSummary(t.Context(), 7, "session-1", "")
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	want := RunUsageSummary{
		RunID: "run-2", SessionTotalTokens: 420,
		RoundInputTokens: 100, RoundCachedInputTokens: 90, RoundTotalTokens: 125,
		RoundPeakInputTokens: 80, RoundPeakReservedTokens: 120,
	}
	if summary != want {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreLatestContextUsageReturnsBothPeaks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	mock.ExpectQuery(`SELECT peak_input_tokens,peak_reserved_tokens.*ORDER BY started_at DESC,id DESC LIMIT 1`).
		WithArgs(int64(7), "session-1").
		WillReturnRows(sqlmock.NewRows([]string{"peak_input_tokens", "peak_reserved_tokens"}).AddRow(86000, 118000))

	usage, err := store.LatestContextUsage(7, "session-1")
	if err != nil {
		t.Fatalf("LatestContextUsage: %v", err)
	}
	if usage.PeakInputTokens != 86000 || usage.PeakReservedTokens != 118000 {
		t.Fatalf("usage = %+v", usage)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreRejectsTerminalOverwrite(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	mock.ExpectExec("UPDATE agent_runs").
		WithArgs(
			RunStatusFailed, 0, 0, EvidenceUnavailable, false, 0, 0, 0, 0, 0,
			sqlmock.AnyArg(), "run", RunStatusRunning, RunStatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))
	err = store.Complete("run", RunOutcome{
		Status:   RunStatusFailed,
		Evidence: EvidenceMetrics{Status: EvidenceUnavailable},
	})
	if !errors.Is(err, ErrRunNotActive) {
		t.Fatalf("Complete error = %v, want ErrRunNotActive", err)
	}
}

func TestRunStoreControlTransitionIsConditional(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	mock.ExpectExec("UPDATE agent_runs SET status=\\? WHERE id=\\? AND status=\\?").
		WithArgs(RunStatusPaused, "run", RunStatusRunning).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := store.TransitionControl("run", RunStatusRunning, RunStatusPaused); err != nil {
		t.Fatalf("TransitionControl: %v", err)
	}
	if err := store.TransitionControl("run", RunStatusDone, RunStatusPaused); err == nil {
		t.Fatal("invalid terminal transition was accepted")
	}
}

func TestRunStoreRecoversInterruptedRuns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := &RunStore{db: db}
	mock.ExpectExec("UPDATE agent_runs SET status=\\?,ended_at=\\? WHERE status IN \\(\\?,\\?\\)").
		WithArgs(RunStatusAborted, sqlmock.AnyArg(), RunStatusRunning, RunStatusPaused).
		WillReturnResult(sqlmock.NewResult(0, 2))
	recovered, err := store.RecoverInterrupted()
	if err != nil {
		t.Fatalf("RecoverInterrupted: %v", err)
	}
	if recovered != 2 {
		t.Fatalf("recovered = %d, want 2", recovered)
	}
}

type emptyRetrievalTools struct{}

func (emptyRetrievalTools) AllServices(context.Context) ([]domain.ServiceRecord, error) {
	return nil, nil
}
func (emptyRetrievalTools) FindServices(context.Context, string, int) (domain.SearchResult[domain.ServiceRecord], error) {
	return domain.SearchResult[domain.ServiceRecord]{}, nil
}
func (emptyRetrievalTools) FindCode(context.Context, string, string, int) (domain.SearchResult[domain.CodeSearchHit], error) {
	return domain.SearchResult[domain.CodeSearchHit]{}, nil
}
func (emptyRetrievalTools) FindAPIs(context.Context, string, string, int) ([]domain.EndpointRecord, error) {
	return nil, nil
}
func (emptyRetrievalTools) FindRunbooks(context.Context, knowledge.RunbookQuery) (domain.RunbookSearchResult, error) {
	return domain.RunbookSearchResult{}, nil
}
func (emptyRetrievalTools) TraceDeps(context.Context, string, string, int) (domain.DependencyTrace, error) {
	return domain.DependencyTrace{}, nil
}
func (emptyRetrievalTools) ServiceModules(context.Context, []string) ([]domain.ServiceRecord, error) {
	return nil, nil
}

func TestRunAgentFinishesHubWhenLLMCallFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 100, server.Client())
	registry := testRegistry(t)
	executor := NewToolExecutor(registry)
	hub := NewRunHub(nil)
	qa := &QA{
		llm:      client,
		executor: executor,
		agent:    NewAgent(client, executor, AgentConfig{MaxSteps: 1, Timeout: time.Second, AnswerReserve: 100 * time.Millisecond, AnswerMaxTokens: 100}, hub, hub),
		hub:      hub,
	}

	const runID = "run-llm-failure"
	ch := hub.Subscribe(runID)
	if _, err := qa.runAgentWithPlan(context.Background(), "question", ConversationContext{}, 0, nil, nil, "", runID, domain.EvidencePlan{Sources: domain.AllEvidence}); err != nil {
		t.Fatalf("runAgentWithPlan: %v", err)
	}

	select {
	case event := <-ch:
		terminal := TerminalFromEvent(event)
		if terminal == nil || terminal.Status != RunStatusFailed {
			t.Fatalf("first hub event = %+v, want failed terminal", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run did not emit Done after LLM failure")
	}
}

func waitForTerminal(t *testing.T, ch chan SSEEvent) *RunTerminal {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-ch:
			if terminal := TerminalFromEvent(event); terminal != nil {
				return terminal
			}
		case <-timer.C:
			t.Fatal("run did not emit terminal event")
		}
	}
}

func TestAskAgentDirectSkipsRetrieverButKeepsRegisteredReadTools(t *testing.T) {
	var routerCalls, agentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool  `json:"stream"`
			Tools  []any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if !request.Stream {
			routerCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[],\"confidence\":0.99},\"query_terms\":{\"domain_terms\":[],\"identifiers\":[]}}"}}]}`))
			return
		}
		agentCalls++
		if len(request.Tools) != 1 {
			t.Errorf("direct request exposed %d tools, want 1 registered read tool", len(request.Tools))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"direct answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())
	registry := testRegistry(t, testAgentTool("internal", ToolKindRead, noopTool))
	executor := NewToolExecutor(registry)
	hub := NewRunHub(nil)
	qa := &QA{
		llm: client, fastLLM: client, executor: executor,
		agent: NewAgent(client, executor, AgentConfig{
			MaxSteps: 1, Timeout: 2 * time.Second, AnswerReserve: 200 * time.Millisecond, AnswerMaxTokens: 512,
		}, hub, hub),
		hub: hub, routerConfidence: 0.9, routerMaxTokens: 512,
	}

	terminalCh := hub.Subscribe("direct-run")
	result, err := qa.AskAgent(context.Background(), "What causes a rainbow?", nil, 0, "", "direct-run")
	if err != nil {
		t.Fatalf("AskAgent: %v", err)
	}
	if terminal := waitForTerminal(t, terminalCh); terminal.Status != RunStatusDone {
		t.Fatalf("terminal status = %s, want done; error=%s", terminal.Status, terminal.Error)
	}
	if routerCalls != 1 || agentCalls != 1 {
		t.Fatalf("routerCalls=%d agentCalls=%d", routerCalls, agentCalls)
	}
	if result.Context == nil || result.Context.Text != "" {
		t.Fatalf("context = %+v, want empty direct context", result.Context)
	}
}

func TestAskAgentRouterInvalidOutputFallsBackInternal(t *testing.T) {
	var routerCalls, agentCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool  `json:"stream"`
			Tools  []any `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if !request.Stream {
			routerCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"preprocess\":{}}"}}]}`))
			return
		}
		agentCalls++
		if len(request.Tools) != 1 {
			t.Errorf("internal fallback exposed %d tools, want 1", len(request.Tools))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"direct answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())
	registry := testRegistry(t, testAgentTool("internal", ToolKindRead, noopTool))
	executor := NewToolExecutor(registry)
	hub := NewRunHub(nil)
	qa := &QA{
		llm: client, fastLLM: client, executor: executor,
		retriever: retrieval.New(emptyRetrievalTools{}, config.Config{}),
		agent: NewAgent(client, executor, AgentConfig{
			MaxSteps: 1, Timeout: 2 * time.Second, AnswerReserve: 200 * time.Millisecond, AnswerMaxTokens: 512,
		}, hub, hub),
		hub: hub, routerConfidence: 0.9, routerMaxTokens: 512,
	}

	terminalCh := hub.Subscribe("invalid-route-run")
	result, err := qa.AskAgent(context.Background(), "What causes a rainbow?", nil, 0, "", "invalid-route-run")
	if err != nil {
		t.Fatalf("AskAgent: %v", err)
	}
	if terminal := waitForTerminal(t, terminalCh); terminal.Status != RunStatusDone {
		t.Fatalf("terminal status = %s, want done; error=%s", terminal.Status, terminal.Error)
	}
	// One original router call plus one reprompt: the model omitted the route
	// object, so ChatJSON re-prompts once before giving up and falling back.
	if routerCalls != 2 || agentCalls != 1 {
		t.Fatalf("routerCalls=%d agentCalls=%d", routerCalls, agentCalls)
	}
	if result.Context == nil {
		t.Fatal("context is nil")
	}
}

func TestCanonicalRetrievalQueryAugmentsWithContextTerms(t *testing.T) {
	if got := canonicalRetrievalQuery(" current question ", ""); got != "current question" {
		t.Fatalf("no context terms: got %q", got)
	}
	if got := canonicalRetrievalQuery("how does it work", "PaymentGateway"); got != "how does it work PaymentGateway" {
		t.Fatalf("with context terms: got %q", got)
	}
}

func TestMergePreloadedContextDedupesContentAndReferences(t *testing.T) {
	context := &retrieval.RetrievedContext{
		Text:       "## Workspace\ncode",
		References: []retrieval.Reference{{Type: "code", Target: "svc/Foo.go"}},
	}
	block := ContextBlock{
		Title:   "Runtime Evidence",
		Content: "trace failed",
		References: []retrieval.Reference{
			{Type: "code", Target: "svc/Foo.go"},
			{Type: "trace", Target: "trace-1"},
		},
	}
	mergePreloadedContext(context, []ContextBlock{block, block}, 48000)
	if strings.Count(context.Text, "trace failed") != 1 {
		t.Fatalf("preloaded content was not deduped: %q", context.Text)
	}
	if len(context.References) != 2 || context.HitCount != 2 {
		t.Fatalf("references = %#v hitCount=%d", context.References, context.HitCount)
	}
}

func TestExecutePrefetchUsesPinnedEligibleTool(t *testing.T) {
	registry := testRegistry(t, Tool{
		ID:          "prefetch",
		Description: "runtime evidence",
		Kind:        ToolKindRead,
		InputSchema: objectSchema(map[string]any{}, nil),
		Prefetch:    &tool.PrefetchSpec{Description: "load runtime evidence"},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{
				Content:    "evidence",
				References: []tool.Reference{{Type: "trace", Target: "trace-1"}},
			}, nil
		}),
	})
	qa := &QA{executor: NewToolExecutor(registry)}
	snapshot := qa.executor.Snapshot(ToolPolicy{AllowRead: true})
	blocks, err := qa.executePrefetch(context.Background(), snapshot, ToolPlan{
		Prefetch: []PlannedToolCall{{ToolID: "prefetch", Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Content != "evidence" || len(blocks[0].References) != 1 {
		t.Fatalf("blocks = %#v", blocks)
	}
}
