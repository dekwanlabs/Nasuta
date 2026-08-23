package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvidenceLedgerPreservesConflictingVersions(t *testing.T) {
	ledger := NewEvidenceLedger()
	first := EvidenceCandidate{
		SourceKind: "code", Target: "svc-a", Section: "entrypoint",
		Content: "provider A", ContentHash: hashContent(t, "provider A"),
	}
	second := EvidenceCandidate{
		SourceKind: "code", Target: "svc-a", Section: "entrypoint",
		Content: "provider B", ContentHash: hashContent(t, "provider B"),
	}
	if _, admitted, err := ledger.Admit("task-1", first); err != nil || !admitted {
		t.Fatalf("first admit = %v, %v", admitted, err)
	}
	if _, admitted, err := ledger.Admit("task-2", second); err != nil || !admitted {
		t.Fatalf("second admit = %v, %v", admitted, err)
	}
	conflicts := ledger.Conflicts()
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %#v", conflicts)
	}
	if conflicts[0].Current.ContentHash == conflicts[0].Incoming.ContentHash {
		t.Fatalf("conflict did not preserve competing hashes: %#v", conflicts[0])
	}
}

func hashContent(t *testing.T, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestSchedulerSeparatesAgentAndToolConcurrency(t *testing.T) {
	var agentActive atomic.Int64
	var toolActive atomic.Int64
	var peakAgents atomic.Int64
	var peakTools atomic.Int64
	executor := TaskExecutorFunc(func(_ context.Context, task ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		var active *atomic.Int64
		var peak *atomic.Int64
		if isAgentExecutor(task.Executor) {
			active, peak = &agentActive, &peakAgents
		} else {
			active, peak = &toolActive, &peakTools
		}
		current := active.Add(1)
		for {
			peakNow := peak.Load()
			if current <= peakNow || peak.CompareAndSwap(peakNow, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		return TaskExecutionResult{Output: []byte(`{"ok":true}`)}, nil
	})
	registry := NewExecutorRegistry(map[ExecutorType]TaskExecutor{
		ExecutorInvestigator: executor, ExecutorVerifier: executor, ExecutorComposer: executor,
		ExecutorDirectTool: executor, ExecutorToolPipeline: executor,
	})
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 8})
	if err != nil {
		t.Fatal(err)
	}
	tasks := []ExecutableTask{
		testExecutableTask("agent-a", nil), testExecutableTask("agent-b", nil),
		testExecutableTask("tool-a", nil), testExecutableTask("tool-b", nil),
	}
	tasks[0].Executor = ExecutorInvestigator
	tasks[1].Executor = ExecutorVerifier
	tasks[2].Executor = ExecutorDirectTool
	tasks[3].Executor = ExecutorDirectTool
	_, err = (Scheduler{
		Executors: registry, Schemas: testSchemas(), Ledger: ledger,
		MaxParallelism: 4, MaxAgentParallelism: 1, MaxToolParallelism: 1,
	}).Execute(context.Background(), tasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	if peakAgents.Load() > 1 || peakTools.Load() > 1 {
		t.Fatalf("parallelism exceeded limits: agents=%d tools=%d", peakAgents.Load(), peakTools.Load())
	}
}

func TestCoordinatorRecordsTaskAttempt(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("inspect", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{ToolCalls: 1})); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(CoordinatorOptions{
		Catalog: catalog, Schemas: testSchemas(), Store: NewMemoryRunStore(),
		Executors: testExecutors(TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
			return TaskExecutionResult{Output: []byte(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
		})),
		BudgetLimit: BudgetVector{ToolCalls: 4}, MaxRounds: 1,
	})
	contract := testContract(EvidenceGoal{ID: "g1", Kind: "flow", Required: true})
	contract.CreatedAt = time.Now().UTC()
	run, err := coordinator.Execute(context.Background(), contract)
	if err != nil {
		t.Fatal(err)
	}
	var record TaskExecutionRecord
	for _, candidate := range run.Results {
		record = candidate
		break
	}
	if len(record.Attempts) != 1 || record.Attempts[0].Attempt != 1 || record.Attempts[0].Status != TaskSucceeded {
		t.Fatalf("task attempts = %#v", record.Attempts)
	}
}

func TestSchedulerRetriesRetryableFailure(t *testing.T) {
	ledger, err := NewBudgetLedger(BudgetVector{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	executor := TaskExecutorFunc(func(_ context.Context, _ ExecutableTask, _ TaskExecutionInput) (TaskExecutionResult, error) {
		if calls.Add(1) == 1 {
			return TaskExecutionResult{Failure: &RunFailure{
				Code: FailureToolUnavailable, Message: "transient", Retryable: true,
			}}, nil
		}
		return TaskExecutionResult{Output: []byte(`{"ok":true}`), Usage: BudgetVector{ToolCalls: 1}}, nil
	})
	task := testExecutableTask("retry-task", nil)
	task.Budget.MaxAttempts = 2
	results, err := (Scheduler{
		Executors: testExecutors(executor), Schemas: testSchemas(), Ledger: ledger,
	}).Execute(context.Background(), []ExecutableTask{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != TaskSucceeded || calls.Load() != 2 {
		t.Fatalf("retry result = %#v, calls = %d", results, calls.Load())
	}
	if len(results[0].Attempts) != 2 {
		t.Fatalf("attempts = %#v", results[0].Attempts)
	}
}

func TestMemoryLeaseStoreFencesConcurrentOwners(t *testing.T) {
	store := NewMemoryLeaseStore()
	if err := store.AcquireLease(context.Background(), "run-1", "owner-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireLease(context.Background(), "run-1", "owner-b", time.Minute); err == nil {
		t.Fatal("second owner acquired an active lease")
	}
	if err := store.RenewLease(context.Background(), "run-1", "owner-a", time.Minute); err != nil {
		t.Fatalf("renew lease: %v", err)
	}
	if err := store.ReleaseLease(context.Background(), "run-1", "owner-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.AcquireLease(context.Background(), "run-1", "owner-b", time.Minute); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestAggregateRunMetricsComputesPercentilesAndRates(t *testing.T) {
	runs := []InvestigationRun{
		{Status: RunDelivered, Metrics: RunMetrics{Duration: time.Second, InputTokens: 10, OutputTokens: 5, ToolCalls: 1}},
		{Status: RunBudgetExhausted, Metrics: RunMetrics{Duration: 3 * time.Second, InputTokens: 30, OutputTokens: 15, ToolCalls: 3, ComposerFallback: true}},
	}
	summary := AggregateRunMetrics(runs)
	if summary.Runs != 2 || summary.BudgetInsufficiency != 0.5 || summary.ComposerFallbackRate != 0.5 {
		t.Fatalf("summary = %#v", summary)
	}
	if summary.DurationP50 != time.Second || summary.DurationP95 != 3*time.Second ||
		summary.InputTokensP50 != 10 || summary.InputTokensP95 != 30 || summary.ToolCallsP95 != 3 {
		t.Fatalf("percentiles = %#v", summary)
	}
}

func TestDiscoveryCandidateGenerationTargetsUnresolvedGoals(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterExploreTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	contract := InvestigationContract{
		ID: "contract-discovery", Question: "trace new dependency",
		Goals: []EvidenceGoal{{ID: "g1", Kind: GoalKindExplore, Required: true}},
	}
	candidates, err := catalog.GenerateCandidatesForDiscoveries(
		contract,
		[]Discovery{
			{Type: "entity", Entity: "svc-b"},
			{Type: "dependency", From: "svc-a", To: "svc-b", Kind: "http"},
		},
		map[string]struct{}{"g1": {}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	for _, candidate := range candidates {
		if len(candidate.GoalIDs) != 1 || candidate.GoalIDs[0] != "g1" ||
			len(candidate.Entities) == 0 || candidate.Template.ID != "investigation.explore" {
			t.Fatalf("candidate = %#v", candidate)
		}
	}
}

func TestProjectInvestigatorDiscoveries(t *testing.T) {
	output := json.RawMessage(`{
		"discovered_entities":["svc-b"],
		"discovered_dependencies":[{"from":"svc-a","to":"svc-b","kind":"http"}]
	}`)
	discoveries := projectInvestigatorDiscoveries(output)
	if len(discoveries) != 2 {
		t.Fatalf("discoveries = %#v", discoveries)
	}
	if discoveries[0].Type != "entity" || discoveries[0].Entity != "svc-b" {
		t.Fatalf("entity discovery = %#v", discoveries[0])
	}
	if discoveries[1].Type != "dependency" || discoveries[1].From != "svc-a" ||
		discoveries[1].To != "svc-b" || discoveries[1].Kind != "http" {
		t.Fatalf("dependency discovery = %#v", discoveries[1])
	}
}

func TestCatalogDeprecateRemovesTemplateFromPlanning(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := catalog.Register(testTemplate("old", 1, []string{"flow"}, ExecutorDirectTool, nil, BudgetVector{})); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Deprecate(TaskTemplateRef{ID: "old", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(TaskTemplateRef{ID: "old", Version: 1}); err == nil {
		t.Fatal("deprecated template still resolves")
	}
	if templates := catalog.List(); len(templates) != 0 {
		t.Fatalf("deprecated template still listed: %#v", templates)
	}
	audit := catalog.Audit()
	if len(audit) != 2 || audit[0].Action != "publish" || audit[1].Action != "deprecate" {
		t.Fatalf("catalog audit = %#v", audit)
	}
}

func TestEvaluateDeliveryCatchesEmptyAndUntraceableDelivery(t *testing.T) {
	run := InvestigationRun{
		ID: "eval-run",
		Contract: InvestigationContract{
			Goals: []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
		},
		Delivery: &DeliveryResult{Status: DeliverySucceeded, Text: "answer"},
		Report: InvestigationReport{
			Claims: []VerifiedClaim{{
				ID: "claim-1", GoalID: "g1", Text: "found", Status: ClaimSupported,
				EvidenceRefs: []EvidenceRef{{EvidenceID: "missing"}},
			}},
			Coverage: []GoalCoverage{{GoalID: "g1", Required: true, Status: GoalCovered}},
		},
	}
	result := EvaluateDelivery(run)
	if result.Pass() {
		t.Fatalf("evaluation passed untraceable delivery: %#v", result)
	}
}

func TestCalibrateTemplateCostsUsesP95Usage(t *testing.T) {
	runs := []InvestigationRun{{
		Tasks: map[string]ExecutableTask{
			"task-a": {Template: TaskTemplateRef{ID: "inspect", Version: 1}},
		},
		Results: map[string]TaskExecutionRecord{
			"task-a": {Usage: BudgetVector{InputTokens: 10, OutputTokens: 5, ToolCalls: 1, CostMicros: 100}},
		},
	}}
	calibrated := CalibrateTemplateCosts(runs)
	if got := calibrated["inspect"]; got.InputTokens != 10 || got.OutputTokens != 5 ||
		got.ToolCalls != 1 || got.CostMicros != 100 {
		t.Fatalf("calibrated = %#v", calibrated)
	}
}

func TestEvaluationSuitePersistsAndReplays(t *testing.T) {
	suite := EvaluationSuite{Cases: []EvaluationCase{{
		ID: "case-1",
		Contract: InvestigationContract{
			Goals: []EvidenceGoal{{ID: "g1", Kind: GoalKindEntrypoint, Required: true}},
		},
		ExpectNonEmpty: true, ExpectTraceable: true, ExpectGoalsCovered: true,
	}}}
	path := filepath.Join(t.TempDir(), "evaluation-suite.json")
	if err := SaveEvaluationSuite(path, suite); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadEvaluationSuite(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Cases) != 1 || loaded.Cases[0].ID != "case-1" {
		t.Fatalf("loaded suite = %#v", loaded)
	}
	results := EvaluateSuite(loaded, map[string]InvestigationRun{"case-1": {ID: "case-1"}})
	if _, ok := results["case-1"]; !ok || len(results["case-1"].Failures) == 0 {
		t.Fatalf("suite results = %#v", results)
	}
}

func TestPruneUnreferencedEvidencePreservesTraceability(t *testing.T) {
	report := InvestigationReport{
		Evidence: []EvidenceUnit{
			{ID: "used", SourceKind: "code", Target: "svc-a", Content: "used"},
			{ID: "unused", SourceKind: "docs", Target: "svc-a", Content: "unused"},
		},
		Claims: []VerifiedClaim{{
			ID: "claim-1", GoalID: "g1", Text: "found", Status: ClaimSupported,
			EvidenceRefs: []EvidenceRef{{EvidenceID: "used"}},
		}},
	}
	pruned := PruneUnreferencedEvidence(report)
	if len(pruned.Evidence) != 1 || pruned.Evidence[0].ID != "used" {
		t.Fatalf("pruned evidence = %#v", pruned.Evidence)
	}
}

func TestDiscoveryCandidateGenerationUsesTypedTemplatesAndDeduplicates(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterFacetCoverageTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	if err := RegisterExtendedInvestigationTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	if err := RegisterExploreTemplates(catalog); err != nil {
		t.Fatal(err)
	}

	contract := InvestigationContract{
		ID:       "contract-typed-discovery",
		Question: "trace a newly discovered external dependency",
		Goals: []EvidenceGoal{{
			ID: "external", Kind: GoalKindExternalDependency, Required: true,
		}},
	}
	candidates, err := catalog.GenerateCandidatesForDiscoveries(contract, []Discovery{
		{Type: "dependency", From: " svc-a ", To: "svc-b", Kind: "http"},
		{Type: "dependency", From: "svc-a", To: "svc-b", Kind: "http"},
		{Type: "unknown", Entity: "ignored"},
	}, map[string]struct{}{"external": {}})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 3 {
		t.Fatalf("typed discovery candidates = %#v, want one per applicable source template", candidates)
	}
	seenTemplates := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		seenTemplates[candidate.Template.ID] = struct{}{}
		if len(candidate.Entities) != 2 || candidate.Entities[0] != "svc-a" || candidate.Entities[1] != "svc-b" {
			t.Fatalf("candidate entities = %#v", candidate)
		}
		if candidate.Objective != "Investigate the discovered dependency (http) svc-a -> svc-b" {
			t.Fatalf("candidate objective = %q", candidate.Objective)
		}
	}
	for _, templateID := range []string{"api.list_external_endpoints", "code.trace_call_chain", "runtime.trace_dependencies"} {
		if _, ok := seenTemplates[templateID]; !ok {
			t.Fatalf("typed discovery did not select %q: %#v", templateID, candidates)
		}
	}
}

func TestDiscoveryCandidateGenerationUsesEntityTemplate(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	if err := RegisterExtendedInvestigationTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	if err := RegisterExploreTemplates(catalog); err != nil {
		t.Fatal(err)
	}
	contract := InvestigationContract{
		ID: "contract-entity-discovery", Question: "inspect a newly discovered service",
		Goals: []EvidenceGoal{{ID: "entry", Kind: GoalKindEntrypoint, Required: true}},
	}
	candidates, err := catalog.GenerateCandidatesForDiscoveries(contract,
		[]Discovery{{Type: "entity", Entity: " svc-new "}},
		map[string]struct{}{"entry": {}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Template.ID != "code.find_ai_entrypoint" {
		t.Fatalf("entity discovery candidates = %#v", candidates)
	}
	if len(candidates[0].Entities) != 1 || candidates[0].Entities[0] != "svc-new" {
		t.Fatalf("entity discovery target = %#v", candidates[0].Entities)
	}
}

func TestTaskTemplateRejectsUnknownDiscoveryType(t *testing.T) {
	catalog := NewTaskTemplateCatalog()
	template := testTemplate("bad_discovery", 1, []string{GoalKindExplore}, ExecutorDirectTool, nil, BudgetVector{})
	template.DiscoveryTypes = []string{"database"}
	if err := catalog.Register(template); err == nil {
		t.Fatal("template with unknown discovery type was accepted")
	}
}
