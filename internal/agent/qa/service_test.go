package qa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/agent/delegation"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	agentworkflow "github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
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
			outcome := outcomeFor(test.result, nil, test.runErr)
			if outcome.Status != test.status {
				t.Fatalf("status = %s, want %s", outcome.Status, test.status)
			}
			if test.err != nil && !errors.Is(outcome.Err, test.err) {
				t.Fatalf("error = %v, want %v", outcome.Err, test.err)
			}
		})
	}
}

func TestSetWriteAvailableUpdatesServiceWithoutRebuild(t *testing.T) {
	service := &Service{}
	if service.writeAvailable.Load() {
		t.Fatal("write availability defaulted to enabled")
	}

	service.SetWriteAvailable(true)

	if !service.writeAvailable.Load() {
		t.Fatal("write availability was not enabled in place")
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
			metrics.Finalize(test.direct)
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
	if !toolsNeedInvestigation(candidates, []string{"observe_logs"}) {
		t.Fatal("selected temporal tool did not enable full investigation")
	}
	agent := NewAgent(nil, nil, Config{MaxSteps: 8}, nil, nil)
	if got := agent.MaxStepsForContext("查一下问题", domain.EvidencePlan{}, true); got != 8 {
		t.Fatalf("max steps = %d, want 8", got)
	}
	if toolsNeedInvestigation(candidates, []string{"search_code"}) {
		t.Fatal("non-temporal tool enabled full investigation")
	}
}

func TestDegradedPlanningClearsRoutedToolsBeforeExecutionRouting(t *testing.T) {
	svc := &Service{}
	prepared := &preparation{
		ctx:     context.Background(),
		request: Request{RunID: "degraded-route"},
		planning: evidencePlanningOutput{
			Decision: domain.PlanDecision{
				Plan:       domain.EvidencePlan{Sources: domain.Internal},
				Confidence: 0.99,
				Origin:     domain.Model,
			},
			Effective: domain.PlanDecision{
				Plan:       domain.EvidencePlan{Sources: domain.Internal},
				Confidence: 0.99,
				Origin:     domain.Model,
			},
			Execution: retrieval.ExecutionSuggestion{
				Strategy:   retrieval.ExecutionMultiAgent,
				Complexity: 0.95,
				Confidence: 0.95,
			},
			RoutedToolIDs: []string{"observe_logs"},
			PlanningError: errors.New("planner response was invalid"),
		},
		toolCandidates: []retrieval.ToolRouteCandidate{
			{ID: "observe_logs", Temporal: true},
		},
	}

	svc.applyExecutionRoute(prepared)

	if prepared.execution.DowngradeReason != "workflow_unavailable" {
		t.Fatalf("downgrade reason = %q, want workflow_unavailable", prepared.execution.DowngradeReason)
	}
	if prepared.planning.RoutedToolIDs != nil {
		t.Fatalf("routed tools = %v, want nil after planning degradation", prepared.planning.RoutedToolIDs)
	}
}

func TestExecutionRoutingDoesNotUseResolvedHistoryRelation(t *testing.T) {
	svc := &Service{}
	prepared := &preparation{
		ctx:     context.Background(),
		request: Request{RunID: "resolved-history-route"},
		planning: evidencePlanningOutput{
			Decision: domain.PlanDecision{
				Plan:       domain.EvidencePlan{Sources: domain.Internal},
				Confidence: 0.99,
				Origin:     domain.Model,
			},
			Effective: domain.PlanDecision{
				Plan:       domain.EvidencePlan{Sources: domain.Internal},
				Confidence: 0.99,
				Origin:     domain.Model,
			},
			Execution: retrieval.ExecutionSuggestion{
				Strategy:   retrieval.ExecutionMultiAgent,
				Complexity: 0.95,
				Confidence: 0.95,
			},
		},
		analysis: queryAnalysisOutput{
			History: retrieval.HistoryRelation{NeedsPriorEvidence: true},
		},
	}

	svc.applyExecutionRoute(prepared)

	if prepared.execution.DowngradeReason != "workflow_unavailable" {
		t.Fatalf("downgrade reason = %q, want workflow_unavailable",
			prepared.execution.DowngradeReason)
	}
}

func TestStandardQARequestAllowsSeedContextPrefetchAndParentReference(t *testing.T) {
	defaultAgent := agentapi.DefinitionRef{ID: "qa.answerer", Version: 3}
	request := Request{
		PreloadedContext: []ContextBlock{{
			Source: "scenario", Content: "trusted seed context",
		}},
		ToolPlan: ToolPlan{Prefetch: []PlannedToolCall{{
			ToolID: "search_code",
		}}},
		ParentRunID: "qa-parent-run",
	}
	if !standardRequest(request, defaultAgent) {
		t.Fatal("seed context, prefetch, and parent reference disabled multi-agent routing")
	}
	request.WorkflowRunID = "workflow-parent"
	request.WorkflowNodeID = "answer"
	if standardRequest(request, defaultAgent) {
		t.Fatal("nested workflow node was accepted as a standard QA request")
	}
}

func TestNormalizeQARequestCanonicalizesWithoutMutatingConversationInstructions(t *testing.T) {
	request := Request{
		Question: "  explain the checkout flow  ",
		RunID:    "normalized-run",
		Conversation: ConversationContext{
			Instructions: []llm.Message{{Role: "system", Content: "existing"}},
		},
	}

	normalized, err := normalizeRequest(request)
	if err != nil {
		t.Fatalf("normalizeRequest: %v", err)
	}
	if normalized.Question != "explain the checkout flow" {
		t.Fatalf("question = %q", normalized.Question)
	}
	if len(normalized.Conversation.Instructions) != 1 ||
		normalized.Conversation.Instructions[0].Content != "existing" {
		t.Fatalf("instructions = %+v", normalized.Conversation.Instructions)
	}
	normalized.Conversation.Instructions[0].Content = "changed"
	if request.Conversation.Instructions[0].Content != "existing" {
		t.Fatal("normalization mutated the caller's conversation instructions")
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
	store := run.Bind(db)
	outcome := RunOutcome{
		Status: RunStatusDone, StepCount: 2, TokenUsed: 12,
		Evidence: EvidenceMetrics{
			Status: EvidencePartial, ForcedConclusion: true, ResultCount: 3,
			ToolCallCount: 4, ToolFailureCount: 1, PartialResultCount: 2, OmittedItemCount: 5,
		},
	}
	mock.ExpectExec("UPDATE agent_runs").
		WithArgs(
			RunStatusDone, "", 2, 12, EvidencePartial, true, 3, 4, 1, 2, 5,
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

func TestRunStoreCreatePersistsAgentAndWorkflowSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := run.Bind(db)
	record := RunRecord{
		ID: "run-1", UserID: 42, SessionID: "session-1",
		AgentID: "qa.answerer", DefinitionVersion: 3,
		DefinitionHash: strings.Repeat("a", 64),
		Selection: agentapi.DefinitionSelection{
			RuleVersion: 2, RuleHash: strings.Repeat("b", 64),
			CandidateVersion: 3, BucketBasisPoints: 812,
			PercentageBasisPoints: 2500, StableKeyHash: strings.Repeat("c", 64),
			Reason: "rollout_candidate",
		},
		ToolSnapshotID:     "tools_snapshot",
		InputSchemaVersion: 2, OutputSchemaVersion: 4,
		ParentRunID: "run-parent", WorkflowRunID: "workflow-1", WorkflowNodeID: "answer",
		Question: "question", Status: RunStatusRunning, Mode: "workflow", MaxSteps: 5,
		StartedAt: "2026-08-05T01:02:03Z",
	}
	selectionJSON, err := json.Marshal(record.Selection)
	if err != nil {
		t.Fatalf("marshal selection: %v", err)
	}
	limitsJSON, err := json.Marshal(record.RunLimits)
	if err != nil {
		t.Fatalf("marshal limits: %v", err)
	}
	mock.ExpectExec("INSERT INTO agent_runs").
		WithArgs(
			record.ID, RunKindAgent, record.UserID, record.SessionID, record.AgentID, record.DefinitionVersion,
			record.DefinitionHash, selectionJSON, record.ToolSnapshotID, record.InputSchemaVersion,
			record.OutputSchemaVersion, record.ParentRunID, record.CapabilityID, record.CapabilityVersion,
			record.CapabilityHash, record.DelegationID, record.DelegationDepth, limitsJSON,
			record.CapabilityRevision, record.WorkflowRunID,
			record.WorkflowNodeID, record.Question, record.Status, record.ErrorCode,
			record.Mode, record.MaxSteps, 0, 0, sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Create(record); err != nil {
		t.Fatalf("Create: %v", err)
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
	store := run.Bind(db)
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

func TestRunStoreRecordLLMCallUpdatesDetailAndAggregateAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := run.Bind(db)
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
		WithArgs(10, 2, 5, 1, 15, int64(0), 3, 10, 60, "run").
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
	store := run.Bind(db)

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

func TestStoreLatestUsageReturnsBothPeaks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := run.Bind(db)
	mock.ExpectQuery(`SELECT peak_input_tokens,peak_reserved_tokens.*ORDER BY started_at DESC,id DESC LIMIT 1`).
		WithArgs(int64(7), "session-1").
		WillReturnRows(sqlmock.NewRows([]string{"peak_input_tokens", "peak_reserved_tokens"}).AddRow(86000, 118000))

	usage, err := store.LatestUsage(7, "session-1")
	if err != nil {
		t.Fatalf("LatestUsage: %v", err)
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
	store := run.Bind(db)
	mock.ExpectExec("UPDATE agent_runs").
		WithArgs(
			RunStatusFailed, "", 0, 0, EvidenceUnavailable, false, 0, 0, 0, 0, 0,
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
	store := run.Bind(db)
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
	store := run.Bind(db)
	mock.ExpectBegin()
	mock.ExpectQuery("FROM agent_delegation_tasks t.*FOR UPDATE").
		WillReturnRows(sqlmock.NewRows([]string{
			"parent_run_id", "delegation_id", "task_index", "child_run_id",
			"capability_id", "input_tokens", "output_tokens", "reasoning_tokens",
			"total_tokens", "cost_micros", "tool_call_count",
		}))
	mock.ExpectExec("UPDATE agent_runs SET status=\\?,error_code=\\?,ended_at=\\?.*"+
		"WHERE run_kind=\\? AND status IN \\(\\?,\\?\\)").
		WithArgs(
			RunStatusAborted,
			"interrupted",
			sqlmock.AnyArg(),
			run.KindAgent,
			RunStatusRunning,
			RunStatusPaused,
		).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
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

	registry := testRegistry(t)
	definition := testReviewerDefinition(t, nil)
	runtime := newTestDefinitionRuntime(t, definition, registry, testRuntimeSettings(server.URL), nil)

	const runID = "run-llm-failure"
	request := testDefinitionRequest(definition)
	request.RunID = runID
	ch := runtime.Hub().Subscribe(runID)
	result, err := runtime.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != agentapi.RunFailed {
		t.Fatalf("result status = %s, want failed", result.Status)
	}

	terminal := waitForTerminal(t, ch)
	if terminal.Status != RunStatusFailed {
		t.Fatalf("terminal = %+v, want failed", terminal)
	}
}

func newQARuntimeFixture(
	t *testing.T,
	client *llm.LLMClient,
	baseURL string,
	registry *Registry,
	retriever contextRetriever,
	pruningEnabled bool,
) (*Service, *DefinitionRuntime) {
	return newQARuntimeFixtureWithStore(
		t, client, baseURL, registry, retriever, pruningEnabled, nil,
	)
}

func newQARuntimeFixtureWithStore(
	t *testing.T,
	client *llm.LLMClient,
	baseURL string,
	registry *Registry,
	retriever contextRetriever,
	pruningEnabled bool,
	store *RunStore,
) (*Service, *DefinitionRuntime) {
	t.Helper()
	definition := testQADefinition(t, func(definition *agentapi.Definition) {
		definition.Budget.ContextTokens = 32768
	})
	runtime := newTestDefinitionRuntime(
		t, definition, registry, testRuntimeSettings(baseURL), store,
	)
	if retriever == nil {
		retriever = emptyContextRetriever{}
	}
	qa := &Service{
		helperLLM: client, fastLLM: client,
		retriever: retriever,
		runtime:   runtime, runtimeTools: runtime, phaseEmitter: runtime,
		definitions: definitionResolverFunc(func(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
			if ref.ID != definition.ID || ref.Version != definition.Version {
				return agentapi.Definition{}, fmt.Errorf("definition not found")
			}
			return definition, nil
		}),
		agentRef:         agentapi.DefinitionRef{ID: definition.ID, Version: definition.Version},
		routerConfidence: 0.9, routerMaxTokens: 512,
		toolPruningEnabled: pruningEnabled,
	}
	return qa, runtime
}

type failingContextRetriever struct {
	err error
}

type emptyContextRetriever struct{}

func (emptyContextRetriever) RetrievePlan(
	context.Context,
	string,
	retrieval.QueryTerms,
	domain.EvidencePlan,
	domain.QueryPlan,
) (*retrieval.RetrievedContext, error) {
	return &retrieval.RetrievedContext{}, nil
}

func (emptyContextRetriever) ContextBudget() int {
	return 48000
}

type staticContextRetriever struct {
	context *retrieval.RetrievedContext
}

func (retriever staticContextRetriever) RetrievePlan(
	context.Context,
	string,
	retrieval.QueryTerms,
	domain.EvidencePlan,
	domain.QueryPlan,
) (*retrieval.RetrievedContext, error) {
	return retriever.context, nil
}

func (staticContextRetriever) ContextBudget() int {
	return 48000
}

type investigationRunnerRecorder struct {
	start  func(context.Context, InvestigationRequest) error
	await  func(context.Context, string) (InvestigationTerminal, error)
	load   func(context.Context, string) (InvestigationTerminal, error)
	cancel func(context.Context, string, int64) error
}

func (*investigationRunnerRecorder) Available() bool {
	return true
}

func (runner *investigationRunnerRecorder) Start(
	ctx context.Context,
	request InvestigationRequest,
) error {
	if runner.start == nil {
		panic("unexpected InvestigationRunner.Start")
	}
	return runner.start(ctx, request)
}

func (runner *investigationRunnerRecorder) AwaitTerminal(
	ctx context.Context,
	workflowRunID string,
) (InvestigationTerminal, error) {
	if runner.await == nil {
		panic("unexpected InvestigationRunner.AwaitTerminal")
	}
	return runner.await(ctx, workflowRunID)
}

func (runner *investigationRunnerRecorder) LoadTerminal(
	ctx context.Context,
	workflowRunID string,
) (InvestigationTerminal, error) {
	if runner.load == nil {
		panic("unexpected InvestigationRunner.LoadTerminal")
	}
	return runner.load(ctx, workflowRunID)
}

func (runner *investigationRunnerRecorder) Cancel(
	ctx context.Context,
	workflowRunID string,
	userID int64,
) error {
	if runner.cancel == nil {
		panic("unexpected InvestigationRunner.Cancel")
	}
	return runner.cancel(ctx, workflowRunID, userID)
}

type workflowEscalatorRecorder struct {
	requests chan agentapi.WorkflowEscalationRequest
}

func (recorder *workflowEscalatorRecorder) Escalate(
	_ context.Context,
	request agentapi.WorkflowEscalationRequest,
) (agentapi.WorkflowEscalationReceipt, error) {
	recorder.requests <- request
	return agentapi.WorkflowEscalationReceipt{
		RequestID: request.RequestID,
		WorkflowRunID: agentworkflow.StableWorkflowEscalationRunID(
			request.ParentRunID,
			request.RequestID,
		),
		Status: agentapi.EscalationAccepted,
	}, nil
}

type scenarioRunRecorder struct {
	context  context.Context
	released chan struct{}
	evidence []tool.EvidenceUnit
}

func (recorder *scenarioRunRecorder) Context(ctx context.Context) context.Context {
	recorder.context = ctx
	return ctx
}

func (recorder *scenarioRunRecorder) RecordStep(
	context.Context,
	RunStepRecord,
) error {
	return nil
}

func (recorder *scenarioRunRecorder) RecordEvidence(
	_ context.Context,
	units []tool.EvidenceUnit,
) error {
	recorder.evidence = cloneEvidenceUnits(units)
	return nil
}

func (recorder *scenarioRunRecorder) Release() {
	if recorder.released != nil {
		recorder.released <- struct{}{}
	}
}

type scenarioLifecycleRecorder struct {
	mu        sync.Mutex
	start     chan ScenarioRunStart
	completed chan RunOutcome
	scenario  *scenarioRunRecorder
	parent    QAParentRecord
	complete  func(context.Context, string, RunOutcome) error
}

func (recorder *scenarioLifecycleRecorder) Start(_ context.Context, start ScenarioRunStart) (ScenarioRun, error) {
	recorder.mu.Lock()
	recorder.parent = QAParentRecord{
		ID: start.RunID, WorkflowRunID: start.WorkflowRunID,
		UserID: start.UserID, SessionID: start.SessionID,
		Question: start.Question, Status: RunStatusRunning,
	}
	recorder.mu.Unlock()
	recorder.start <- start
	return recorder.scenario, nil
}

func (recorder *scenarioLifecycleRecorder) Complete(
	ctx context.Context,
	runID string,
	outcome RunOutcome,
) error {
	if recorder.complete != nil {
		return recorder.complete(ctx, runID, outcome)
	}
	recorder.mu.Lock()
	recorder.parent.Status = outcome.Status
	recorder.mu.Unlock()
	recorder.completed <- outcome
	return nil
}

func (recorder *scenarioLifecycleRecorder) GetQAParent(runID string) (QAParentRecord, error) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if recorder.parent.ID != runID {
		return QAParentRecord{}, fmt.Errorf("QA parent %q not found", runID)
	}
	return recorder.parent, nil
}

type executionEventRecord struct {
	eventType EventType
	event     ExecutionEvent
}

type executionEventRecorder struct {
	mu     sync.Mutex
	events []executionEventRecord
}

func (recorder *executionEventRecorder) EmitEvent(eventType EventType, event ExecutionEvent) {
	recorder.mu.Lock()
	recorder.events = append(recorder.events, executionEventRecord{eventType: eventType, event: event})
	recorder.mu.Unlock()
}

func (recorder *executionEventRecorder) Snapshot() []executionEventRecord {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]executionEventRecord(nil), recorder.events...)
}

func (retriever failingContextRetriever) RetrievePlan(
	context.Context,
	string,
	retrieval.QueryTerms,
	domain.EvidencePlan,
	domain.QueryPlan,
) (*retrieval.RetrievedContext, error) {
	return nil, retriever.err
}

func (failingContextRetriever) ContextBudget() int {
	return 48000
}

func TestSubmitInvestigationSurvivesCallerCancellation(t *testing.T) {
	const runID = "qa-parent-run"
	requests := make(chan InvestigationRequest, 1)
	awaited := make(chan context.Context, 1)
	release := make(chan struct{})
	scenario := &scenarioRunRecorder{released: make(chan struct{}, 1)}
	lifecycle := &scenarioLifecycleRecorder{
		start:     make(chan ScenarioRunStart, 1),
		completed: make(chan RunOutcome, 1),
		scenario:  scenario,
	}
	runner := &investigationRunnerRecorder{
		start: func(_ context.Context, request InvestigationRequest) error {
			requests <- request
			return nil
		},
		await: func(ctx context.Context, workflowRunID string) (InvestigationTerminal, error) {
			awaited <- ctx
			<-release
			result := InvestigationResult{Answer: "multi-agent answer"}
			return InvestigationTerminal{
				WorkflowRunID: workflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	qa := &Service{
		investigation: runner,
		scenarios:     lifecycle,
		coordinator: &Coordinator{
			investigation: runner,
			scenarios:     lifecycle,
			parentRuns:    lifecycle,
			sessions:      &coordinatorSessionStore{turnByRun: make(map[string]int)},
		},
	}
	request := Request{
		Question:     "trace the checkout flow",
		Conversation: ConversationContext{SessionID: "session-1"},
		UserID:       42,
		RunID:        runID,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result, err := qa.submitInvestigation(&preparation{ctx: ctx, request: request})
	if err != nil {
		t.Fatalf("submitInvestigation: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("result run ID = %q, want %q", result.RunID, runID)
	}
	select {
	case start := <-lifecycle.start:
		if start.RunID != runID || start.UserID != 42 || start.Mode != "multi_agent" ||
			start.SessionID != "session-1" {
			t.Fatalf("scenario start = %+v", start)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scenario did not start")
	}
	select {
	case investigation := <-requests:
		if investigation.Contract.TaskID != runID ||
			!strings.HasPrefix(investigation.WorkflowRunID, "workflow_") ||
			investigation.Actor.UserID != 42 {
			t.Fatalf("investigation request = %+v", investigation)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("investigation did not start")
	}
	runContext := <-awaited
	cancel()
	close(release)
	select {
	case outcome := <-lifecycle.completed:
		if outcome.Status != RunStatusDone || outcome.Answer != "multi-agent answer" {
			t.Fatalf("outcome = %+v", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parent run did not finish")
	}
	if runContext.Err() != nil {
		t.Fatalf("investigation context was canceled after caller cancellation: %v", runContext.Err())
	}
	if runContext.Err() != nil {
		t.Fatalf("run context = %v, want uncanceled context", runContext.Err())
	}
}

func TestSubmitInvestigationPassesParentEvidenceToWorkflowEscalation(
	t *testing.T,
) {
	const runID = "qa-escalation-parent"
	contentHash := strings.Repeat("a", 64)
	overview := tool.EvidenceUnit{
		SourceKind:  "retrieval",
		Target:      "doc-a",
		Sections:    []string{"overview"},
		ContentHash: contentHash,
		Coverage: tool.EvidenceCoverage{
			Complete: true,
			Included: 1,
		},
	}
	failure := overview
	failure.Sections = []string{"failure"}
	scenario := &scenarioRunRecorder{released: make(chan struct{}, 1)}
	lifecycle := &scenarioLifecycleRecorder{
		start:     make(chan ScenarioRunStart, 1),
		completed: make(chan RunOutcome, 1),
		scenario:  scenario,
	}
	runner := &investigationRunnerRecorder{
		await: func(
			_ context.Context,
			workflowRunID string,
		) (InvestigationTerminal, error) {
			result := InvestigationResult{Answer: "workflow answer"}
			return InvestigationTerminal{
				WorkflowRunID: workflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	escalator := &workflowEscalatorRecorder{
		requests: make(chan agentapi.WorkflowEscalationRequest, 1),
	}
	qa := &Service{
		retriever: staticContextRetriever{
			context: &retrieval.RetrievedContext{
				Text:          "retrieved evidence",
				EvidenceUnits: []tool.EvidenceUnit{overview, failure},
			},
		},
		investigation:      runner,
		scenarios:          lifecycle,
		delegationEnabled:  true,
		workflowEscalation: true,
		workflowEscalator:  escalator,
	}
	qa.coordinator = &Coordinator{
		investigation: runner,
		scenarios:     lifecycle,
		parentRuns:    lifecycle,
		sessions: &coordinatorSessionStore{
			turnByRun: make(map[string]int),
		},
	}
	prepared := &preparation{
		ctx: context.Background(),
		request: Request{
			Question: "trace checkout",
			Conversation: ConversationContext{
				SessionID: "session-1",
			},
			UserID: 42,
			RunID:  runID,
		},
		planning: evidencePlanningOutput{
			CleanQuestion: "trace checkout",
			Effective: domain.PlanDecision{
				Plan: domain.EvidencePlan{Sources: domain.Internal},
			},
		},
		execution: executionRouteDecision{
			Path:                     executionPathWorkflow,
			RouteReason:              string(agentapi.EscalationStrongTaskDependencies),
			EscalationCapability:     agentapi.CapabilityRef{ID: "knowledge.service.trace", Version: 1},
			EscalationCapabilityHash: strings.Repeat("b", 64),
			EscalationFocusFacets:    []string{"runtime_behavior"},
		},
	}
	result, err := qa.submitInvestigation(prepared)
	if err != nil {
		t.Fatalf("submitInvestigation: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("result run ID = %q", result.RunID)
	}
	var request agentapi.WorkflowEscalationRequest
	select {
	case request = <-escalator.requests:
	case <-time.After(time.Second):
		t.Fatal("workflow escalation did not start")
	}
	wantRefs := []string{
		run.EvidenceReferenceID(overview),
		run.EvidenceReferenceID(failure),
	}
	if !reflect.DeepEqual(request.EvidenceRefs, wantRefs) {
		t.Fatalf("evidence refs = %v, want %v", request.EvidenceRefs, wantRefs)
	}
	if !reflect.DeepEqual(scenario.evidence, []tool.EvidenceUnit{overview, failure}) {
		t.Fatalf("persisted evidence = %+v", scenario.evidence)
	}
	select {
	case outcome := <-lifecycle.completed:
		if outcome.Status != RunStatusDone ||
			outcome.Answer != "workflow answer" {
			t.Fatalf("outcome = %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("workflow parent did not complete")
	}
}

type shadowRuntimeCall struct {
	ctx     context.Context
	request agentapi.RunRequest
}

type shadowRuntimeRecorder struct {
	calls chan shadowRuntimeCall
}

func (runtime *shadowRuntimeRecorder) Run(
	ctx context.Context,
	request agentapi.RunRequest,
) (agentapi.RunResult, error) {
	runtime.calls <- shadowRuntimeCall{ctx: ctx, request: request}
	return agentapi.RunResult{
		RunID: request.RunID, Status: agentapi.RunSucceeded,
		Text: "shadow answer",
		Usage: agentapi.Usage{
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
		},
	}, nil
}

func (*shadowRuntimeRecorder) Begin(
	context.Context,
	agentapi.RunStart,
) (agentapi.ManagedRun, error) {
	return nil, errors.New("shadow runtime Begin must not be called")
}

func TestSubmitInvestigationRunsDelegationShadowWithoutChangingAuthority(
	t *testing.T,
) {
	const runID = "qa-shadow-parent"
	definition := testQADefinition(t, nil)
	registry := testRegistry(
		t,
		testAgentTool(
			delegation.DelegateToolID,
			ToolKindRead,
			noopTool,
		),
		testAgentTool("search_code", ToolKindRead, noopTool),
	)
	toolSet := preparedScenarioTools{
		snapshot: registry.Snapshot(ToolPolicy{AllowRead: true}),
		executor: NewToolExecutor(registry),
	}
	scenario := &scenarioRunRecorder{released: make(chan struct{}, 1)}
	lifecycle := &scenarioLifecycleRecorder{
		start:     make(chan ScenarioRunStart, 1),
		completed: make(chan RunOutcome, 1),
		scenario:  scenario,
	}
	investigationRequests := make(chan InvestigationRequest, 1)
	runner := &investigationRunnerRecorder{
		start: func(_ context.Context, request InvestigationRequest) error {
			investigationRequests <- request
			return nil
		},
		await: func(
			_ context.Context,
			workflowRunID string,
		) (InvestigationTerminal, error) {
			result := InvestigationResult{Answer: "authoritative workflow answer"}
			return InvestigationTerminal{
				WorkflowRunID: workflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
			}, nil
		},
	}
	shadow := &shadowRuntimeRecorder{
		calls: make(chan shadowRuntimeCall, 1),
	}
	events := &executionEventRecorder{}
	qa := &Service{
		retriever:         emptyContextRetriever{},
		runtime:           shadow,
		investigation:     runner,
		scenarios:         lifecycle,
		executionEvents:   events,
		delegationEnabled: true,
		delegationShadow:  true,
		definitions: definitionResolverFunc(func(
			ref agentapi.DefinitionRef,
		) (agentapi.Definition, error) {
			if ref.ID != definition.ID {
				return agentapi.Definition{}, fmt.Errorf("definition not found")
			}
			return definition, nil
		}),
		agentRef: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
	}
	qa.coordinator = &Coordinator{
		investigation: runner,
		scenarios:     lifecycle,
		parentRuns:    lifecycle,
		sessions: &coordinatorSessionStore{
			turnByRun: make(map[string]int),
		},
	}
	prepared := &preparation{
		ctx: context.Background(),
		request: Request{
			Question: "trace checkout",
			Conversation: ConversationContext{
				SessionID: "session-1",
			},
			UserID: 42,
			RunID:  runID,
		},
		candidateToolSet: toolSet,
		analysis: queryAnalysisOutput{
			QueryPlan: domain.QueryPlan{Kind: domain.QueryCodeReview},
		},
		execution: executionRouteDecision{
			Strategy:   retrieval.ExecutionMultiAgent,
			Path:       executionPathWorkflow,
			ShadowPath: executionPathDelegation,
		},
	}
	result, err := qa.submitInvestigation(prepared)
	if err != nil {
		t.Fatalf("submitInvestigation: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("result run ID = %q", result.RunID)
	}
	select {
	case authoritative := <-investigationRequests:
		if authoritative.Contract.TaskID != runID {
			t.Fatalf("authoritative request = %+v", authoritative)
		}
	case <-time.After(time.Second):
		t.Fatal("authoritative workflow did not start")
	}
	select {
	case call := <-shadow.calls:
		if call.request.RunID != delegationShadowRunID(runID) ||
			call.request.Correlation.ParentRunID != runID ||
			call.request.Correlation.WorkflowRunID != "" ||
			call.request.Actor.UserID != 42 {
			t.Fatalf("shadow request = %+v", call.request)
		}
		if !call.request.ToolScope.RestrictVisible ||
			!stringPresent(call.request.ToolScope.VisibleToolIDs, string(delegation.DelegateToolID)) {
			t.Fatalf("shadow tool scope = %+v", call.request.ToolScope)
		}
		parent, ok := delegation.ParentContextFrom(call.ctx)
		if !ok || parent.RunID != call.request.RunID ||
			parent.Correlation.ParentRunID != runID ||
			parent.Depth != 0 {
			t.Fatalf("shadow parent context = %+v, ok=%t", parent, ok)
		}
	case <-time.After(time.Second):
		t.Fatal("delegation shadow did not start")
	}
	deadline := time.Now().Add(time.Second)
	for {
		recorded := events.Snapshot()
		if len(recorded) > 0 {
			if len(recorded) != 1 ||
				recorded[0].eventType != run.EventDelegationShadow ||
				recorded[0].event.RunID != runID ||
				recorded[0].event.ChildRunID != delegationShadowRunID(runID) ||
				recorded[0].event.Status != "completed" ||
				recorded[0].event.QueryKind != string(domain.QueryCodeReview) ||
				!recorded[0].event.Shadow ||
				recorded[0].event.Usage.TotalTokens != 15 {
				t.Fatalf("shadow evaluation event = %+v", recorded)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delegation shadow evaluation event was not emitted")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case outcome := <-lifecycle.completed:
		if outcome.Status != RunStatusDone ||
			outcome.Answer != "authoritative workflow answer" {
			t.Fatalf("authoritative outcome = %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("authoritative workflow did not complete")
	}
}

func TestAskMultiAgentRoutePersistsParentOutcomeAndCorrelation(t *testing.T) {
	const runID = "qa-multi-agent-run"
	client, server := newQATestLLM(t, serverPromotableRouteBody(), "")
	defer server.Close()

	registry := tool.NewRegistry()
	qa, _ := newQARuntimeFixture(t, client, server.URL, registry, nil, false)
	scenario := &scenarioRunRecorder{released: make(chan struct{}, 1)}
	lifecycle := &scenarioLifecycleRecorder{
		start:     make(chan ScenarioRunStart, 1),
		completed: make(chan RunOutcome, 1),
		scenario:  scenario,
	}
	investigationRequests := make(chan InvestigationRequest, 1)
	qa.scenarios = lifecycle
	qa.investigation = &investigationRunnerRecorder{
		start: func(_ context.Context, request InvestigationRequest) error {
			investigationRequests <- request
			return nil
		},
		await: func(_ context.Context, workflowRunID string) (InvestigationTerminal, error) {
			result := InvestigationResult{
				Answer: "  grounded multi-agent answer  ",
				Citations: []InvestigationCitation{{
					Claim: "checkout calls inventory",
					Evidence: []InvestigationEvidence{{
						Kind: "call", Reference: "Checkout.Place", Summary: "calls inventory",
					}},
				}},
			}
			return InvestigationTerminal{
				WorkflowRunID: workflowRunID,
				Status:        InvestigationSucceeded,
				Completeness:  InvestigationComplete,
				Output:        &result,
				Usage:         InvestigationUsage{TotalTokens: 91, ToolCalls: 4},
			}, nil
		},
	}
	qa.coordinator = &Coordinator{
		investigation: qa.investigation,
		scenarios:     lifecycle,
		parentRuns:    lifecycle,
		sessions:      &coordinatorSessionStore{turnByRun: make(map[string]int)},
	}
	events := &executionEventRecorder{}
	qa.executionEvents = events

	result, err := qa.Ask(context.Background(), Request{
		Question:     "trace the checkout flow",
		Conversation: ConversationContext{SessionID: "session-1"},
		UserID:       42,
		RunID:        runID,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("result run ID = %q, want %q", result.RunID, runID)
	}

	select {
	case start := <-lifecycle.start:
		if start.RunID != runID || start.ParentRunID != "" || start.UserID != 42 ||
			!strings.HasPrefix(start.WorkflowRunID, "workflow_") ||
			start.SessionID != "session-1" || start.Mode != "multi_agent" {
			t.Fatalf("scenario start = %+v", start)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("scenario did not start")
	}

	select {
	case request := <-investigationRequests:
		if request.Contract.TaskID != runID || !strings.HasPrefix(request.WorkflowRunID, "workflow_") ||
			request.Contract.Question != "trace the checkout flow" ||
			request.Contract.Objective != "trace the checkout flow" ||
			request.Actor.UserID != 42 {
			t.Fatalf("investigation request = %+v", request)
		}
		if request.Proposal == nil || len(request.Proposal.Tasks) != 3 ||
			request.Proposal.Tasks[0].ID != "design" ||
			request.Proposal.Tasks[1].ID != "implementation" ||
			request.Proposal.Tasks[2].ID != "synthesize" {
			t.Fatalf("task graph proposal = %+v", request.Proposal)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("investigation did not start")
	}

	select {
	case outcome := <-lifecycle.completed:
		if outcome.Status != RunStatusDone || outcome.Answer != "grounded multi-agent answer" ||
			outcome.TokenUsed != 91 || outcome.HitCount != 1 ||
			outcome.Evidence.Status != EvidenceComplete ||
			outcome.Evidence.ToolCallCount != 4 {
			t.Fatalf("parent outcome = %+v", outcome)
		}
		if len(outcome.References) != 1 ||
			outcome.References[0].Type != "call" ||
			outcome.References[0].Target != "Checkout.Place" {
			t.Fatalf("parent references = %+v", outcome.References)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parent run did not finish")
	}

	recorded := events.Snapshot()
	if len(recorded) != 1 || recorded[0].eventType != EventExecutionRouted ||
		recorded[0].event.RunID != runID ||
		recorded[0].event.Strategy != string(retrieval.ExecutionMultiAgent) ||
		recorded[0].event.Status != "completed" {
		t.Fatalf("execution events = %+v", recorded)
	}
}

func TestAskMultiAgentInvestigationFailureCompletesParentAsFailed(t *testing.T) {
	const runID = "qa-multi-agent-failed-run"
	client, server := newQATestLLM(t, serverPromotableRouteBody(), "")
	defer server.Close()

	qa, _ := newQARuntimeFixture(t, client, server.URL, tool.NewRegistry(), nil, false)
	scenario := &scenarioRunRecorder{released: make(chan struct{}, 1)}
	lifecycle := &scenarioLifecycleRecorder{
		start:     make(chan ScenarioRunStart, 1),
		completed: make(chan RunOutcome, 1),
		scenario:  scenario,
	}
	qa.scenarios = lifecycle
	qa.investigation = &investigationRunnerRecorder{
		start: func(context.Context, InvestigationRequest) error {
			return nil
		},
		await: func(_ context.Context, workflowRunID string) (InvestigationTerminal, error) {
			return InvestigationTerminal{
				WorkflowRunID: workflowRunID,
				Status:        InvestigationFailed,
				ErrorCode:     "provider_failed",
			}, nil
		},
	}
	qa.coordinator = NewCoordinator(
		qa.investigation,
		lifecycle,
		lifecycle,
		nil,
	)

	if _, err := qa.Ask(context.Background(), Request{
		Question: "trace the checkout flow", UserID: 42, RunID: runID,
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}

	select {
	case outcome := <-lifecycle.completed:
		if outcome.Status != RunStatusFailed || outcome.ErrorCode != "provider_failed" {
			t.Fatalf("parent outcome = %+v", outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parent run did not finish")
	}
}

func TestAskExecutionEventsOrderDegradedRouteBeforeSingleAgent(t *testing.T) {
	const runID = "qa-policy-downgrade-run"
	client, server := newQATestLLM(t, serverPromotableRouteBody(), "single-agent answer")
	defer server.Close()

	retriever := retrieval.New(emptyRetrievalTools{}, config.Config{})
	qa, runtime := newQARuntimeFixture(t, client, server.URL, tool.NewRegistry(), retriever, false)
	scenario := &scenarioRunRecorder{}
	lifecycle := &scenarioLifecycleRecorder{
		start:     make(chan ScenarioRunStart, 1),
		completed: make(chan RunOutcome, 1),
		scenario:  scenario,
	}
	qa.scenarios = lifecycle
	qa.investigation = &investigationRunnerRecorder{}
	qa.coordinator = NewCoordinator(
		qa.investigation,
		lifecycle,
		lifecycle,
		nil,
	)
	events := &executionEventRecorder{}
	qa.executionEvents = events
	terminalEvents := runtime.Hub().Subscribe(runID)

	result, err := qa.Ask(context.Background(), Request{
		Question: "trace the checkout flow",
		PreloadedContext: []ContextBlock{{
			Source: "scenario", Content: "supplied scenario context",
		}},
		UserID:         42,
		RunID:          runID,
		WorkflowRunID:  "parent-workflow",
		WorkflowNodeID: "child-node",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if result.RunID != runID {
		t.Fatalf("result run ID = %q, want %q", result.RunID, runID)
	}
	terminal := waitForTerminal(t, terminalEvents)
	if terminal.Status != RunStatusDone || terminal.Answer != "single-agent answer" {
		t.Fatalf("terminal = %+v", terminal)
	}
	select {
	case start := <-lifecycle.start:
		t.Fatalf("policy-downgraded request started scenario: %+v", start)
	default:
	}

	recorded := events.Snapshot()
	if len(recorded) != 2 ||
		recorded[0].eventType != EventExecutionRouted ||
		recorded[1].eventType != EventExecutionDegraded {
		t.Fatalf("execution events = %+v", recorded)
	}
	if recorded[0].event.RunID != runID ||
		recorded[0].event.Strategy != string(retrieval.ExecutionSingleAgent) ||
		recorded[0].event.Status != "completed" {
		t.Fatalf("routed event = %+v", recorded[0])
	}
	if recorded[1].event.RunID != runID ||
		recorded[1].event.Strategy != string(retrieval.ExecutionSingleAgent) ||
		recorded[1].event.Status != "degraded" ||
		recorded[1].event.Reason != "policy_disallows_multi_agent" {
		t.Fatalf("degraded event = %+v", recorded[1])
	}
}

func TestAskSingleAgentRouteDoesNotStartScenario(t *testing.T) {
	const runID = "qa-single-agent-run"
	client, server := newQATestLLM(t, singleAgentRouteBody(), "single-agent answer")
	defer server.Close()

	retriever := retrieval.New(emptyRetrievalTools{}, config.Config{})
	qa, runtime := newQARuntimeFixture(t, client, server.URL, tool.NewRegistry(), retriever, false)
	scenario := &scenarioRunRecorder{}
	lifecycle := &scenarioLifecycleRecorder{
		start:     make(chan ScenarioRunStart, 1),
		completed: make(chan RunOutcome, 1),
		scenario:  scenario,
	}
	qa.scenarios = lifecycle
	qa.investigation = &investigationRunnerRecorder{}
	qa.coordinator = NewCoordinator(
		qa.investigation,
		lifecycle,
		lifecycle,
		nil,
	)
	events := &executionEventRecorder{}
	qa.executionEvents = events
	terminalEvents := runtime.Hub().Subscribe(runID)

	if _, err := qa.Ask(context.Background(), Request{
		Question: "what causes a rainbow?", UserID: 42, RunID: runID,
	}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	terminal := waitForTerminal(t, terminalEvents)
	if terminal.Status != RunStatusDone || terminal.Answer != "single-agent answer" {
		t.Fatalf("terminal = %+v", terminal)
	}
	select {
	case start := <-lifecycle.start:
		t.Fatalf("single-agent request started scenario: %+v", start)
	default:
	}
	recorded := events.Snapshot()
	if len(recorded) != 1 || recorded[0].eventType != EventExecutionRouted {
		t.Fatalf("execution events = %+v", recorded)
	}
	if recorded[0].event.Strategy != string(retrieval.ExecutionSingleAgent) ||
		recorded[0].event.Status != "completed" {
		t.Fatalf("routed event = %+v", recorded[0])
	}
}

func newQATestLLM(t *testing.T, routeBody, answer string) (*llm.LLMClient, *httptest.Server) {
	t.Helper()
	var nonStreamMu sync.Mutex
	nonStreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode LLM request: %v", err)
			return
		}
		if !payload.Stream {
			nonStreamMu.Lock()
			nonStreamCalls++
			call := nonStreamCalls
			nonStreamMu.Unlock()
			writer.Header().Set("Content-Type", "application/json")
			if call == 1 {
				_, _ = writer.Write([]byte(routeBody))
			} else {
				_, _ = writer.Write([]byte(multiAgentTaskGraphBody()))
			}
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		content, _ := json.Marshal(answer)
		_, _ = fmt.Fprintf(writer,
			"data: {\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}]}\n\n",
			content,
		)
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	return llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client()), server
}

func serverPromotableRouteBody() string {
	return `{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"internal\"],\"confidence\":0.99},\"query_terms\":{\"domain_terms\":[\"call chain\"],\"identifiers\":[]},\"execution\":{\"strategy\":\"single_agent\",\"complexity\":0.95,\"confidence\":0.95,\"tasks\":[{\"id\":\"design\",\"objective\":\"Establish the intended behavior.\",\"independently_useful\":true,\"depends_on\":[]},{\"id\":\"implementation\",\"objective\":\"Verify the implementation behavior.\",\"independently_useful\":true,\"depends_on\":[]}],\"reasons\":[\"requires_multiple_subproblems\",\"supports_parallel_investigation\"]}}"}}]}`
}

func multiAgentTaskGraphBody() string {
	return `{"choices":[{"message":{"content":"{\"tasks\":[{\"id\":\"design\",\"purpose\":\"Establish the intended behavior.\",\"capability\":\"knowledge.code.inspect\",\"required_facets\":[\"entrypoint\",\"core_flow\",\"data_and_state\"],\"depends_on\":[]},{\"id\":\"implementation\",\"purpose\":\"Verify the implementation behavior.\",\"capability\":\"knowledge.service.trace\",\"required_facets\":[\"external_dependency\"],\"depends_on\":[]}]}"}}]}`
}

func singleAgentRouteBody() string {
	return `{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"internal\"],\"confidence\":0.99},\"query_terms\":{\"domain_terms\":[],\"identifiers\":[]},\"execution\":{\"strategy\":\"single_agent\",\"complexity\":0.1,\"confidence\":0.99,\"tasks\":[],\"reasons\":[\"single_focused_question\"]}}"}}]}`
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

func TestAskDirectSkipsRetrieverButKeepsRegisteredReadTools(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[],\"confidence\":0.99},\"query_terms\":{\"domain_terms\":[],\"identifiers\":[]},\"execution\":{\"strategy\":\"single_agent\",\"complexity\":0.1,\"confidence\":0.99,\"tasks\":[],\"reasons\":[\"single_focused_question\"]}}"}}]}`))
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
	qa, runtime := newQARuntimeFixture(t, client, server.URL, registry, nil, false)

	terminalCh := runtime.Hub().Subscribe("direct-run")
	result, err := qa.AskWithHistory(context.Background(), "What causes a rainbow?", nil, 0, "", "direct-run")
	if err != nil {
		t.Fatalf("Ask: %v", err)
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

func TestAskSessionPersistenceFailureCompletesRunAsFailed(t *testing.T) {
	runDB, runMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer runDB.Close()
	sessionDB, sessionMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer sessionDB.Close()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	const runID = "session-persistence-failed-run"
	runMock.ExpectExec("INSERT INTO agent_runs").WillReturnResult(sqlmock.NewResult(1, 1))
	runMock.ExpectBegin()
	runMock.ExpectQuery("SELECT llm_call_count FROM agent_runs").WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"llm_call_count"}).AddRow(0))
	runMock.ExpectExec("INSERT INTO agent_llm_calls").WillReturnResult(sqlmock.NewResult(1, 1))
	runMock.ExpectExec("UPDATE agent_runs SET").WillReturnResult(sqlmock.NewResult(0, 1))
	runMock.ExpectCommit()
	runMock.ExpectBegin()
	runMock.ExpectExec("INSERT INTO agent_steps").WillReturnResult(sqlmock.NewResult(1, 1))
	runMock.ExpectCommit()
	runMock.ExpectExec("UPDATE agent_runs").WithArgs(
		RunStatusFailed, "session_persistence_failed", 1, len("answer"),
		EvidenceNotRequired, false, 0, 0, 0, 0, 0,
		sqlmock.AnyArg(), runID, RunStatusRunning, RunStatusPaused,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	sessionMock.ExpectBegin().WillReturnError(errors.New("session database unavailable"))

	client := llm.NewLLMClientWithHTTP(server.URL, "key", "review-model", 256, server.Client())
	qa, runtime := newQARuntimeFixtureWithStore(
		t, client, server.URL, tool.NewRegistry(), nil, false, run.Bind(runDB),
	)
	qa.sessions = memory.NewSessionStore(sessionDB)
	events := runtime.Hub().Subscribe(runID)
	_, err = qa.AskWithContext(
		context.Background(), "你能做什么？",
		ConversationContext{SessionID: "session-1"}, 42, "", runID, nil,
	)
	if err != nil {
		t.Fatalf("AskWithContext: %v", err)
	}
	terminal := waitForTerminal(t, events)
	if terminal.Status != RunStatusFailed || !strings.Contains(terminal.Error, "session database unavailable") {
		t.Fatalf("terminal = %+v", terminal)
	}
	select {
	case event := <-events:
		if event.Type == EventRunFinished {
			t.Fatal("session persistence failure published multiple terminal events")
		}
	case <-time.After(20 * time.Millisecond):
	}
	if err := runMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := sessionMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAskRetrievalFailureCompletesStartedRunAsFailed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const runID = "retrieval-failed-run"
	mock.ExpectExec("INSERT INTO agent_runs").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("UPDATE agent_runs").WithArgs(
		RunStatusFailed, "preparation_failed", 0, 0,
		EvidenceUnavailable, false, 0, 0, 0, 0, 0,
		sqlmock.AnyArg(), runID, RunStatusRunning, RunStatusPaused,
	).WillReturnResult(sqlmock.NewResult(0, 1))

	qa, runtime := newQARuntimeFixtureWithStore(
		t, nil, "http://unused", tool.NewRegistry(),
		failingContextRetriever{err: errors.New("retrieval backend unavailable")},
		false, run.Bind(db),
	)
	plan := domain.EvidencePlan{Sources: domain.Internal}
	events := runtime.Hub().Subscribe(runID)
	_, err = qa.AskWithContext(
		context.Background(), "find the implementation", ConversationContext{},
		42, "", runID, &plan,
	)
	if err == nil || !strings.Contains(err.Error(), "retrieve internal evidence") {
		t.Fatalf("error = %v", err)
	}
	terminal := waitForTerminal(t, events)
	if terminal.Status != RunStatusFailed || !strings.Contains(terminal.Error, "retrieval backend unavailable") {
		t.Fatalf("terminal = %+v", terminal)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAskRouterInvalidOutputFallsBackInternal(t *testing.T) {
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
	qa, runtime := newQARuntimeFixture(
		t, client, server.URL, registry,
		retrieval.New(emptyRetrievalTools{}, config.Config{}), false,
	)

	terminalCh := runtime.Hub().Subscribe("invalid-route-run")
	result, err := qa.AskWithHistory(context.Background(), "What causes a rainbow?", nil, 0, "", "invalid-route-run")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if terminal := waitForTerminal(t, terminalCh); terminal.Status != RunStatusDone {
		t.Fatalf("terminal status = %s, want done; error=%s", terminal.Status, terminal.Error)
	}
	// Planner failures fall back immediately without another model request.
	if routerCalls != 1 || agentCalls != 1 {
		t.Fatalf("routerCalls=%d agentCalls=%d", routerCalls, agentCalls)
	}
	if result.Context == nil {
		t.Fatal("context is nil")
	}
}

// TestAskPrunesScenarioToolsWhenRoutingSaysSo drives the full Ask path
// with two base read tools and two routing-gated scenario tools, asserting the
// offered tool set shrinks when pruning is live and stays full in dry-run.
func TestAskPrunesScenarioToolsWhenRoutingSaysSo(t *testing.T) {
	run := func(pruningEnabled bool, routeBody string) (toolNames []string, traces []domain.EvaluationTrace) {
		var agentToolNames []string
		recorder := &toolTraceRecorder{}
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
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(routeBody))
				return
			}
			for _, tool := range request.Tools {
				name, _ := tool.(map[string]any)["function"].(map[string]any)["name"].(string)
				agentToolNames = append(agentToolNames, name)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"direct answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		}))
		defer server.Close()

		client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())
		registry := testRegistry(t,
			testAgentTool("get_service", ToolKindRead, noopTool),
			testAgentTool("search_code", ToolKindRead, noopTool),
			scenarioTool("observe_logs"),
			scenarioTool("search_config"),
		)
		qa, runtime := newQARuntimeFixture(
			t, client, server.URL, registry,
			retrieval.New(emptyRetrievalTools{}, config.Config{}), pruningEnabled,
		)
		ctx := domain.WithTraceRecorder(context.Background(), recorder)
		runID := fmt.Sprintf("prune-run-%t", pruningEnabled)
		terminalCh := runtime.Hub().Subscribe(runID)
		result, err := qa.AskWithHistory(ctx, "how many requests failed?", nil, 0, "", runID)
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if terminal := waitForTerminal(t, terminalCh); terminal.Status != RunStatusDone {
			t.Fatalf("terminal status = %s, want done; error=%s", terminal.Status, terminal.Error)
		}
		if result.Context == nil {
			t.Fatal("context is nil")
		}
		return agentToolNames, recorder.events
	}

	// Router selects observe_logs only; the router requires the query_terms
	// object to accompany the route and tools objects.
	routeBody := `{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"internal\"],\"confidence\":0.99},\"tools\":{\"tool_ids\":[\"observe_logs\"]},\"query_terms\":{\"domain_terms\":[],\"identifiers\":[]},\"execution\":{\"strategy\":\"single_agent\",\"complexity\":0.4,\"confidence\":0.9,\"tasks\":[],\"reasons\":[\"single_source_sufficient\"]}}"}}]}`

	t.Run("live pruning keeps base plus routed", func(t *testing.T) {
		names, _ := run(true, routeBody)
		sort.Strings(names)
		if strings.Join(names, ",") != "get_service,observe_logs,search_code" {
			t.Fatalf("offered tools = %v, want get_service+observe_logs+search_code (search_config pruned)", names)
		}
	})

	t.Run("dry-run keeps the full set", func(t *testing.T) {
		names, traces := run(false, routeBody)
		sort.Strings(names)
		if strings.Join(names, ",") != "get_service,observe_logs,search_code,search_config" {
			t.Fatalf("dry-run offered tools = %v, want the full set", names)
		}
		var saved float64
		found := false
		for _, trace := range traces {
			if trace.Node != "tool_pruning" {
				continue
			}
			found = true
			if applied, _ := trace.Input["applied"].(bool); applied {
				t.Fatalf("dry-run trace recorded applied=true")
			}
			// The recorder stores the raw Go value (int); a JSON round-trip would
			// widen it to float64. Accept both.
			switch v := trace.Output["saved_tokens"].(type) {
			case int:
				saved = float64(v)
			case float64:
				saved = v
			}
		}
		if !found {
			t.Fatal("no tool_pruning trace node recorded in dry-run")
		}
		if saved <= 0 {
			t.Fatal("dry-run recorded no positive token saving")
		}
	})
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
	const sourceHash = "trace-version-1"
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
		Evidence: []tool.EvidenceUnit{{
			SourceKind: "runtime", Target: "trace-1",
			ContentHash: sourceHash,
			Coverage:    tool.EvidenceCoverage{Complete: true},
		}},
	}
	mergePreloadedContext(context, []ContextBlock{block, block}, 48000)
	if strings.Count(context.Text, "trace failed") != 1 {
		t.Fatalf("preloaded content was not deduped: %q", context.Text)
	}
	if len(context.References) != 2 || context.HitCount != 2 {
		t.Fatalf("references = %#v hitCount=%d", context.References, context.HitCount)
	}
	if len(context.EvidenceUnits) != 1 || context.EvidenceUnits[0].Target != "trace-1" ||
		context.EvidenceUnits[0].ContentHash != sourceHash || context.EvidenceUnits[0].TokenCost == 0 {
		t.Fatalf("evidence units = %#v", context.EvidenceUnits)
	}
}

func TestMergePreloadedContextKeepsDeliveredEvidenceWhenExistingContextIsTruncated(t *testing.T) {
	const sourceHash = "runbook-version-1"
	unit := tool.EvidenceUnit{
		SourceKind: "runbook", Target: "doc-a", ContentHash: sourceHash,
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	context := &retrieval.RetrievedContext{
		Text:          "workspace evidence that will be truncated",
		EvidenceUnits: []tool.EvidenceUnit{unit},
	}
	mergePreloadedContext(context, []ContextBlock{{
		Title:    "Runtime",
		Content:  "trace",
		Evidence: []tool.EvidenceUnit{unit},
	}}, 20)
	if len(context.EvidenceUnits) != 1 || context.EvidenceUnits[0].Target != "doc-a" ||
		context.EvidenceUnits[0].ContentHash != sourceHash {
		t.Fatalf("evidence units = %#v", context.EvidenceUnits)
	}
}

func TestMergePreloadedContextRecordsTruncatedEvidenceAsPartial(t *testing.T) {
	const sourceHash = "trace-version-1"
	context := &retrieval.RetrievedContext{}
	mergePreloadedContext(context, []ContextBlock{{
		Title:   "Runtime",
		Content: "trace content beyond the available preload budget",
		Evidence: []tool.EvidenceUnit{{
			SourceKind: "runtime", Target: "trace-1", ContentHash: sourceHash,
			Coverage: tool.EvidenceCoverage{Complete: true},
		}},
	}}, 16)
	if len(context.EvidenceUnits) != 1 {
		t.Fatalf("evidence units = %#v", context.EvidenceUnits)
	}
	unit := context.EvidenceUnits[0]
	if unit.ContentHash != sourceHash || unit.Coverage.Complete || !unit.Coverage.Partial ||
		unit.TokenCost != tooloutput.EstimateTokens(context.Text) {
		t.Fatalf("evidence unit = %#v text=%q", unit, context.Text)
	}
}

func TestMergePreloadedContextPropagatesOneEvidenceConflict(t *testing.T) {
	retrievalUnit := tool.EvidenceUnit{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "version-a",
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	context := &retrieval.RetrievedContext{
		Text:          "retrieved trace",
		EvidenceUnits: []tool.EvidenceUnit{retrievalUnit},
	}
	mergePreloadedContext(context, []ContextBlock{{
		Title:   "Runtime",
		Content: "preloaded trace",
		Evidence: []tool.EvidenceUnit{{
			SourceKind: "runtime", Target: "trace-1", ContentHash: "version-b",
			Coverage: tool.EvidenceCoverage{Complete: true},
		}},
	}}, 48000)
	if len(context.EvidenceUnits) != 1 ||
		context.EvidenceUnits[0].ContentHash != "version-a" {
		t.Fatalf("evidence units = %#v", context.EvidenceUnits)
	}
	if len(context.EvidenceConflicts) != 1 ||
		context.EvidenceConflicts[0].Current.ContentHash != "version-a" ||
		context.EvidenceConflicts[0].Incoming.ContentHash != "version-b" {
		t.Fatalf("evidence conflicts = %#v", context.EvidenceConflicts)
	}

	mergePreloadedContext(context, []ContextBlock{{
		Title:   "Runtime duplicate",
		Content: "preloaded trace duplicate",
		Evidence: []tool.EvidenceUnit{{
			SourceKind: "runtime", Target: "trace-1", ContentHash: "version-b",
		}},
	}}, 48000)
	if len(context.EvidenceConflicts) != 1 {
		t.Fatalf("duplicate evidence conflicts = %#v", context.EvidenceConflicts)
	}
}

func TestQAContextBlockHashesDeliveredTextAndPropagatesConflicts(t *testing.T) {
	rc := &retrieval.RetrievedContext{
		Text: "final delivered evidence",
		EvidenceConflicts: []evidence.Conflict{{
			Key: evidence.Key{SourceKind: "runtime", Target: "trace-1"},
			Current: tool.EvidenceUnit{
				SourceKind: "runtime", Target: "trace-1", ContentHash: "version-a",
			},
			Incoming: tool.EvidenceUnit{
				SourceKind: "runtime", Target: "trace-1", ContentHash: "version-b",
			},
		}},
	}
	blocks := contextBlocks(rc)
	if len(blocks) != 1 || blocks[0].ContentHash != hashString(rc.Text) {
		t.Fatalf("context blocks = %#v", blocks)
	}
	if len(blocks[0].EvidenceConflicts) != 1 ||
		blocks[0].EvidenceConflicts[0].Incoming.ContentHash != "version-b" {
		t.Fatalf("context conflicts = %#v", blocks[0].EvidenceConflicts)
	}
}

type preparationStepCapture struct {
	steps  []RunStepRecord
	failAt int
}

func (capture *preparationStepCapture) RecordStep(
	_ context.Context,
	step RunStepRecord,
) error {
	capture.steps = append(capture.steps, step)
	if capture.failAt > 0 && len(capture.steps) == capture.failAt {
		return errors.New("step persistence unavailable")
	}
	return nil
}

func TestExecutePrefetchUsesPinnedEligibleTool(t *testing.T) {
	registry := testRegistry(t, Tool{
		ID:          "prefetch",
		Description: "runtime evidence",
		Kind:        ToolKindRead,
		InputSchema: objectSchema(
			map[string]any{"query": propString("evidence query")},
			[]string{"query"},
		),
		Prefetch: &tool.PrefetchSpec{Description: "load runtime evidence"},
		Handler: tool.HandlerFunc(func(_ context.Context, arguments tool.Arguments) (tool.Result, error) {
			if arguments["query"] != "checkout" {
				t.Fatalf("arguments = %#v", arguments)
			}
			return tool.Result{
				Content:    "evidence",
				References: []tool.Reference{{Type: "trace", Target: "trace-1"}},
				EvidenceUnits: []tool.EvidenceUnit{{
					SourceKind: "runtime", Target: "trace-1",
					Coverage: tool.EvidenceCoverage{Complete: true},
				}},
				Coverage:       tool.EvidenceCoverage{Partial: true, OmittedItems: 2},
				AnswerContract: tool.AnswerContract{RequiredLiterals: []string{"TRACE-1"}},
			}, nil
		}),
	})
	qa := &Service{}
	prepared := preparedScenarioTools{
		snapshot: registry.Snapshot(ToolPolicy{AllowRead: true}),
		executor: NewToolExecutor(registry),
	}
	recorder := &preparationStepCapture{}
	const runID = "run-prefetch-success"
	blocks, err := qa.executePrefetch(
		context.Background(),
		runID,
		prepared,
		ToolPlan{Prefetch: []PlannedToolCall{{
			ToolID: "prefetch", Arguments: tool.Arguments{"query": "checkout"}, Required: true,
		}}},
		recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Content != "evidence" || len(blocks[0].References) != 1 ||
		len(blocks[0].Evidence) != 1 || blocks[0].Evidence[0].Target != "trace-1" {
		t.Fatalf("blocks = %#v", blocks)
	}
	if len(recorder.steps) != 2 {
		t.Fatalf("steps = %#v", recorder.steps)
	}
	call, result := recorder.steps[0], recorder.steps[1]
	wantCallID := prefetchToolCallID(runID, 0, "prefetch")
	if call.Kind != run.StepKindToolCall || call.ToolCallID != wantCallID ||
		call.Tool != "prefetch" || call.Args != `{"query":"checkout"}` ||
		call.CreatedAt.IsZero() {
		t.Fatalf("tool call step = %#v", call)
	}
	if result.Kind != run.StepKindToolResult || result.ToolCallID != call.ToolCallID ||
		result.TraceID != prefetchTraceID(runID, wantCallID) ||
		result.Content != "evidence" || result.PromptContent != "evidence" ||
		result.AuthoritativeSHA256 != hashString("evidence") ||
		result.PromptSHA256 != hashString("evidence") ||
		result.SizeBytes != int64(len("evidence")) || result.Failed ||
		!result.Coverage.Partial || result.Coverage.OmittedItems != 2 ||
		len(result.AnswerContract.RequiredLiterals) != 1 ||
		result.AnswerContract.RequiredLiterals[0] != "TRACE-1" ||
		result.DurationMs < 0 || result.CreatedAt.IsZero() {
		t.Fatalf("tool result step = %#v", result)
	}
}

func TestExecutePrefetchRecordsFailedToolResult(t *testing.T) {
	registry := testRegistry(t, Tool{
		ID:          "prefetch",
		Description: "runtime evidence",
		Kind:        ToolKindRead,
		InputSchema: objectSchema(map[string]any{}, nil),
		Prefetch:    &tool.PrefetchSpec{Description: "load runtime evidence"},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			return tool.Result{}, errors.New("backend unavailable")
		}),
	})
	prepared := preparedScenarioTools{
		snapshot: registry.Snapshot(ToolPolicy{AllowRead: true}),
		executor: NewToolExecutor(registry),
	}
	recorder := &preparationStepCapture{}
	blocks, err := (&Service{}).executePrefetch(
		context.Background(),
		"run-prefetch-failed",
		prepared,
		ToolPlan{Prefetch: []PlannedToolCall{{ToolID: "prefetch"}}},
		recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || !strings.Contains(blocks[0].Content, "backend unavailable") {
		t.Fatalf("blocks = %#v", blocks)
	}
	if len(recorder.steps) != 2 {
		t.Fatalf("steps = %#v", recorder.steps)
	}
	call, result := recorder.steps[0], recorder.steps[1]
	if call.ToolCallID == "" || result.ToolCallID != call.ToolCallID ||
		!result.Failed || result.DeliveryError != "tool_execution_failed" ||
		!strings.Contains(result.Content, "backend unavailable") {
		t.Fatalf("failed result = %#v, call = %#v", result, call)
	}
}

func TestExecutePrefetchStopsWhenToolCallCannotBeRecorded(t *testing.T) {
	called := false
	registry := testRegistry(t, Tool{
		ID:          "prefetch",
		Description: "runtime evidence",
		Kind:        ToolKindRead,
		InputSchema: objectSchema(map[string]any{}, nil),
		Prefetch:    &tool.PrefetchSpec{Description: "load runtime evidence"},
		Handler: tool.HandlerFunc(func(context.Context, tool.Arguments) (tool.Result, error) {
			called = true
			return tool.Result{Content: "evidence"}, nil
		}),
	})
	prepared := preparedScenarioTools{
		snapshot: registry.Snapshot(ToolPolicy{AllowRead: true}),
		executor: NewToolExecutor(registry),
	}
	_, err := (&Service{}).executePrefetch(
		context.Background(),
		"run-prefetch-persist-failed",
		prepared,
		ToolPlan{Prefetch: []PlannedToolCall{{ToolID: "prefetch"}}},
		&preparationStepCapture{failAt: 1},
	)
	if err == nil || !strings.Contains(err.Error(), "record prefetch tool") {
		t.Fatalf("error = %v", err)
	}
	if called {
		t.Fatal("tool executed after its call step failed to persist")
	}
}
