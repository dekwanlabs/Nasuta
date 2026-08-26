package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/tool"
)

type fakeRuntime struct {
	result agentapi.RunResult
	err    error
	gotReq *agentapi.RunRequest
}

func (f *fakeRuntime) Run(_ context.Context, req agentapi.RunRequest) (agentapi.RunResult, error) {
	f.gotReq = &req
	return f.result, f.err
}

type fakeDefinitionResolver struct {
	def agentapi.Definition
	err error
}

func (r fakeDefinitionResolver) Resolve(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
	if r.err != nil {
		return agentapi.Definition{}, r.err
	}
	def := r.def
	def.ID = ref.ID
	if ref.Version > 0 {
		def.Version = ref.Version
	}
	return def, nil
}

func testDefinition() agentapi.Definition {
	return agentapi.Definition{
		ID:           "investigator.code",
		Version:      1,
		InputSchema:  agentapi.TaskContractSchemaRef(),
		OutputSchema: agentapi.InvestigationReportSchemaRef(),
		Tools: agentapi.ToolPolicy{
			VisibleToolIDs: []string{"search_code"},
		},
		Budget: agentapi.BudgetPolicy{
			MaxSteps:     8,
			MaxToolCalls: 12,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		ContentHash: "abc123",
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal value: %v", err)
	}
	return raw
}

func testRuntimeTaskInput(task ExecutableTask) TaskExecutionInput {
	return TaskExecutionInput{
		Task: task, WorkflowRunID: "workflow-test", ParentRunID: "run-parent",
		Actor: agentapi.Actor{UserID: 42, TenantID: "tenant-a"}, Attempt: 1,
	}
}

func TestAgentRuntimeTaskExecutorLimitsRespectTaskReservation(t *testing.T) {
	definition := testDefinition()
	definition.Budget.MaxToolCalls = 12
	task := ExecutableTask{
		Budget: TaskBudget{Limit: BudgetVector{
			InputTokens: 100, OutputTokens: 50, ToolCalls: 1,
		}},
	}
	limits := (AgentRuntimeTaskExecutor{}).limits(task, definition)
	if limits.MaxToolCalls != 1 {
		t.Fatalf("max tool calls = %d, want 1", limits.MaxToolCalls)
	}
	if limits.MaxInputTokens != 100 {
		t.Fatalf("max input tokens = %d, want 100", limits.MaxInputTokens)
	}
	if limits.MaxTotalTokens != 150 {
		t.Fatalf("max total tokens = %d, want 150", limits.MaxTotalTokens)
	}
}

func TestAgentRuntimeTaskExecutorDoesNotGrantToolsToToollessDefinition(t *testing.T) {
	definition := testDefinition()
	definition.Tools.VisibleToolIDs = nil
	definition.Budget.MaxToolCalls = 0

	limits := (AgentRuntimeTaskExecutor{}).limitsForBudget(BudgetVector{
		InputTokens: 20000,
		ToolCalls:   24,
	}, definition)
	if limits.MaxToolCalls != 0 {
		t.Fatalf("max tool calls = %d, want 0 for tool-less definition", limits.MaxToolCalls)
	}
}

func TestAgentRuntimeTaskExecutorPropagatesInputBudgetToRequest(t *testing.T) {
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID: "task-contract", Status: agentapi.RunSucceeded,
		Output: mustJSON(t, investigationReportOutput{Focus: "runtime", Summary: "ok"}),
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime: runtime, Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{
		ID: "task-contract", Executor: ExecutorInvestigator,
		Objective: "inspect runtime", EvidenceGoalIDs: []string{"runtime"},
		Budget: TaskBudget{Limit: BudgetVector{InputTokens: 70, OutputTokens: 30}},
	}
	if _, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runtime.gotReq == nil {
		t.Fatal("runtime received no request")
	}
	if runtime.gotReq.Limits.MaxInputTokens != 70 {
		t.Fatalf("request max input tokens = %d, want 70", runtime.gotReq.Limits.MaxInputTokens)
	}
	if runtime.gotReq.Limits.MaxTotalTokens != 100 {
		t.Fatalf("request max total tokens = %d, want 100", runtime.gotReq.Limits.MaxTotalTokens)
	}
}

func TestAgentRuntimeTaskExecutorUsesSharedRuntimeBudget(t *testing.T) {
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID: "task-shared-budget", Status: agentapi.RunSucceeded,
		Output: mustJSON(t, investigationReportOutput{Focus: "runtime", Summary: "ok"}),
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime: runtime, Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{
		ID: "task-shared-budget", Executor: ExecutorInvestigator,
		Objective: "inspect runtime", EvidenceGoalIDs: []string{"runtime"},
		Budget: TaskBudget{Limit: BudgetVector{InputTokens: 20, OutputTokens: 10}},
	}
	input := testRuntimeTaskInput(task)
	input.RuntimeBudget = BudgetVector{InputTokens: 60, OutputTokens: 30, TotalTokens: 90}
	if _, err := executor.Execute(context.Background(), task, input); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runtime.gotReq == nil {
		t.Fatal("runtime received no request")
	}
	if runtime.gotReq.Limits.MaxInputTokens != 0 {
		t.Fatalf("request max input tokens = %d, want 0 when shared ledger accounts usage", runtime.gotReq.Limits.MaxInputTokens)
	}
	if runtime.gotReq.Limits.MaxTotalTokens != 0 {
		t.Fatalf("request max total tokens = %d, want 0 when shared ledger accounts usage", runtime.gotReq.Limits.MaxTotalTokens)
	}
	if runtime.gotReq.Limits.MaxContextTokens <= 0 {
		t.Fatalf("request max context tokens = %d, want a per-request context limit", runtime.gotReq.Limits.MaxContextTokens)
	}
}

func TestAgentRuntimeTaskExecutor_InvestigatorProjection(t *testing.T) {
	report := investigationReportOutput{
		Focus:   "ai integration",
		Summary: "fallback summary",
		Findings: []investigationFinding{
			{
				Claim:           "service calls the model",
				EvidenceGoalIDs: []string{"G1"},
				Evidence: []investigationEvidence{
					{Kind: "code", Reference: "svc.go:42", Summary: "the call site is here"},
				},
			},
		},
	}
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID:  "task-1",
		Status: agentapi.RunSucceeded,
		Output: mustJSON(t, report),
		EvidenceUnits: []tool.EvidenceUnit{
			{
				SourceKind:    "code",
				Target:        "svc.go:42",
				Sections:      []string{"call"},
				Facets:        []string{"ai"},
				TrustTier:     2,
				EvidenceClass: "primary",
				Version:       "v1",
				TimeRange:     "stable",
			},
		},
		Usage: agentapi.Usage{InputTokens: 100, OutputTokens: 50, CostMicros: 7},
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime:     runtime,
		Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{
		ID:              "task-1",
		Executor:        ExecutorInvestigator,
		Objective:       "find the model entrypoint",
		EvidenceGoalIDs: []string{"G1"},
		Entities:        []string{"hsas-aiot-service"},
	}
	result, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.EvidenceCandidates) != 1 {
		t.Fatalf("evidence candidates = %d, want 1", len(result.EvidenceCandidates))
	}
	candidate := result.EvidenceCandidates[0]
	if candidate.Content != "the call site is here" {
		t.Fatalf("evidence content = %q", candidate.Content)
	}
	if candidate.SourceKind != "code" || candidate.Target != "svc.go:42" {
		t.Fatalf("evidence identity = %q/%q", candidate.SourceKind, candidate.Target)
	}
	if result.Usage.OutputTokens != 50 {
		t.Fatalf("output tokens = %d, want 50", result.Usage.OutputTokens)
	}
	if runtime.gotReq == nil {
		t.Fatal("runtime never received a request")
	}
	if runtime.gotReq.Policy.EvidenceRequired != true {
		t.Fatal("expected evidence-required policy")
	}
	if runtime.gotReq.Policy.OutputMode != agentapi.RunOutputEvidenceWorker {
		t.Fatalf("output mode = %q, want evidence_worker", runtime.gotReq.Policy.OutputMode)
	}
	wantRunID, err := childAgentRunID("workflow-test", task.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.gotReq.RunID != wantRunID {
		t.Fatalf("child run ID = %q, want %q", runtime.gotReq.RunID, wantRunID)
	}
	if runtime.gotReq.Actor != (agentapi.Actor{UserID: 42, TenantID: "tenant-a"}) {
		t.Fatalf("actor = %#v", runtime.gotReq.Actor)
	}
	wantCorrelation := (agentapi.Correlation{
		ParentRunID: "run-parent", WorkflowRunID: "workflow-test", NodeID: task.ID,
	})
	if runtime.gotReq.Correlation != wantCorrelation {
		t.Fatalf("correlation = %#v, want %#v", runtime.gotReq.Correlation, wantCorrelation)
	}
}

func TestChildAgentRunIDScopesWorkflowAndAttempt(t *testing.T) {
	first, err := childAgentRunID("workflow-a", "task-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := childAgentRunID("workflow-a", "task-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	otherWorkflow, err := childAgentRunID("workflow-b", "task-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	otherAttempt, err := childAgentRunID("workflow-a", "task-1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first != repeated {
		t.Fatalf("same child identity produced %q and %q", first, repeated)
	}
	if first == otherWorkflow || first == otherAttempt || otherWorkflow == otherAttempt {
		t.Fatalf("child run IDs are not isolated: %q %q %q", first, otherWorkflow, otherAttempt)
	}
}

func TestChildAgentRunIDRejectsMissingRuntimeIdentity(t *testing.T) {
	for _, test := range []struct {
		name       string
		workflowID string
		taskID     string
		attempt    int
	}{
		{name: "workflow", taskID: "task-1", attempt: 1},
		{name: "task", workflowID: "workflow-a", attempt: 1},
		{name: "attempt", workflowID: "workflow-a", taskID: "task-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := childAgentRunID(test.workflowID, test.taskID, test.attempt); err == nil {
				t.Fatal("expected invalid child identity error")
			}
		})
	}
}

func TestAgentRuntimeTaskExecutor_InvestigatorRejectsEmptyContent(t *testing.T) {
	report := investigationReportOutput{Summary: "   "}
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID:  "task-1",
		Status: agentapi.RunSucceeded,
		Output: mustJSON(t, report),
		EvidenceUnits: []tool.EvidenceUnit{
			{SourceKind: "code", Target: "svc.go:42"},
		},
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime:     runtime,
		Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{ID: "task-1", Executor: ExecutorInvestigator, EvidenceGoalIDs: []string{"G1"}}
	result, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.EvidenceCandidates) != 0 {
		t.Fatalf("evidence candidates = %d, want 0 for empty content", len(result.EvidenceCandidates))
	}
}

func TestAgentRuntimeTaskExecutor_VerifierProjection(t *testing.T) {
	verification := verificationResult{
		Summary: "supported",
		Verdicts: []verificationVerdict{
			{
				ClaimIDs:     []string{"ev-1"},
				Decision:     "supported",
				Rationale:    "backed by source",
				EvidenceRefs: []string{"ev-1"},
			},
		},
	}
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID:  "task-2",
		Status: agentapi.RunSucceeded,
		Output: mustJSON(t, verification),
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime:     runtime,
		Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{ID: "task-2", Executor: ExecutorVerifier, Objective: "verify ai entry", EvidenceGoalIDs: []string{"G1"}}
	input := testRuntimeTaskInput(task)
	input.Evidence = []EvidenceUnit{
		{ID: "ev-1", Content: "the service calls the model"},
	}
	result, err := executor.Execute(context.Background(), task, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(result.Claims))
	}
	claim := result.Claims[0]
	if claim.Status != ClaimSupported {
		t.Fatalf("claim status = %q, want supported", claim.Status)
	}
	if claim.Text != "the service calls the model" {
		t.Fatalf("claim text = %q", claim.Text)
	}
	if len(claim.EvidenceRefs) != 1 || claim.EvidenceRefs[0].EvidenceID != "ev-1" {
		t.Fatalf("evidence refs = %+v", claim.EvidenceRefs)
	}
}

func TestProjectVerifierClaimsRecoversEvidenceRefFromCanonicalClaimID(t *testing.T) {
	const evidenceID = "evidence_fda78bad2fe387e24cba17548a43ea14e81cfa87acb81ebe05a9357567d41512"
	statement := `{
  "service": "service-a",
  "upstream": [],
  "downstream": [],
  "truncated": false
}`
	task := ExecutableTask{ID: "verify-json", Executor: ExecutorVerifier, Objective: "verify", EvidenceGoalIDs: []string{"goal"}}
	result := agentapi.RunResult{Status: agentapi.RunSucceeded, Output: mustJSON(t, verificationResult{
		Summary: "supported",
		Verdicts: []verificationVerdict{{
			ClaimIDs: []string{" " + evidenceID + " "}, Decision: "supported", Rationale: "backed by the supplied result",
		}},
	})}
	unit := EvidenceUnit{ID: evidenceID, SourceKind: "service", Target: "service-a", Content: statement}
	claims, err := projectVerifierClaims(task, TaskExecutionInput{Task: task, Evidence: []EvidenceUnit{unit}}, result, EvidenceContextBudget{})
	if err != nil {
		t.Fatalf("projectVerifierClaims: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("claims = %#v, want one grounded claim", claims)
	}
	if len(claims[0].EvidenceRefs) != 1 || claims[0].EvidenceRefs[0].EvidenceID != evidenceID {
		t.Fatalf("evidence refs = %#v", claims[0].EvidenceRefs)
	}

	ledger := NewClaimLedger([]EvidenceGoal{{ID: "goal"}}, NewEvidenceLedgerFrom([]EvidenceUnit{unit}))
	if _, _, err := ledger.Admit(task.ID, claims[0]); err != nil {
		t.Fatalf("admit recovered claim: %v", err)
	}
}

func TestProjectVerifierClaimsDropsVerdictWithoutCanonicalEvidence(t *testing.T) {
	task := ExecutableTask{ID: "verify-ungrounded", Executor: ExecutorVerifier, Objective: "verify", EvidenceGoalIDs: []string{"goal"}}
	result := agentapi.RunResult{Status: agentapi.RunSucceeded, Output: mustJSON(t, verificationResult{
		Summary: "supported",
		Verdicts: []verificationVerdict{{
			ClaimIDs: []string{"unknown-claim"}, Decision: "supported", Rationale: "the result is supported",
		}},
	})}
	claims, err := projectVerifierClaims(task, TaskExecutionInput{
		Task: task,
		Evidence: []EvidenceUnit{{
			ID: "ev-1", SourceKind: "code", Target: "service.go:42", Content: "the service calls its dependency",
		}},
	}, result, EvidenceContextBudget{})
	if err != nil {
		t.Fatalf("projectVerifierClaims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims = %#v, want ungrounded verdict dropped", claims)
	}
}

func TestVerifierInputDeduplicatesAndTruncatesLongEvidence(t *testing.T) {
	fullContent := strings.Repeat("a long runtime observation arrives here;", 30)
	task := ExecutableTask{ID: "task-2", Executor: ExecutorVerifier, Objective: "verify ai entry", EvidenceGoalIDs: []string{"G1"}}
	input := testRuntimeTaskInput(task)
	input.Evidence = []EvidenceUnit{
		{ID: "ev-1", Content: fullContent, SourceKind: "runtime", Target: "trace-1"},
		{ID: "ev-1", Content: fullContent, SourceKind: "runtime", Target: "trace-1"},
	}
	raw, err := verifierInput(task, input, EvidenceContextBudget{MaxSummaryTokens: 20})
	if err != nil {
		t.Fatalf("verifierInput: %v", err)
	}
	var payload struct {
		Claims []verificationClaim `json:"claims"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode verifier input: %v", err)
	}
	if len(payload.Claims) != 1 {
		t.Fatalf("claims = %d, want 1", len(payload.Claims))
	}
	if payload.Claims[0].Statement == fullContent {
		t.Fatal("verifier statement was not truncated")
	}
	if tooloutput.EstimateTokens(payload.Claims[0].Statement) > 25 {
		t.Fatalf("verifier statement tokens = %d, want <= 25", tooloutput.EstimateTokens(payload.Claims[0].Statement))
	}
}

func TestVerifierInputBoundsEvidenceContext(t *testing.T) {
	task := ExecutableTask{
		ID: "task-2", Executor: ExecutorVerifier, Objective: "verify ai entry",
		EvidenceGoalIDs: []string{"G1"},
	}
	input := testRuntimeTaskInput(task)
	input.Evidence = make([]EvidenceUnit, 0, 20)
	for index := 0; index < 20; index++ {
		facets := []string(nil)
		if index == 19 {
			facets = []string{"G1"}
		}
		input.Evidence = append(input.Evidence, EvidenceUnit{
			ID: "ev-" + strconv.Itoa(index), Content: strings.Repeat("bounded evidence ", 20), Facets: facets,
		})
	}
	raw, err := verifierInput(task, input, EvidenceContextBudget{
		MaxSummaryTokens: 20,
		MaxContextTokens: 50,
	})
	if err != nil {
		t.Fatalf("verifierInput: %v", err)
	}
	var payload struct {
		Claims []verificationClaim `json:"claims"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode verifier input: %v", err)
	}
	if len(payload.Claims) == 0 || len(payload.Claims) >= len(input.Evidence) {
		t.Fatalf("claims = %d, want a bounded non-empty subset", len(payload.Claims))
	}
	if payload.Claims[0].ID != "ev-19" {
		t.Fatalf("required facet evidence was not prioritized: %+v", payload.Claims)
	}
	if tokens := tooloutput.EstimateTokens(string(raw)); tokens > 250 {
		t.Fatalf("verifier input tokens = %d, want bounded request", tokens)
	}
}

func TestAgentRuntimeTaskExecutor_RequiresRuntimeAndResolver(t *testing.T) {
	executor := AgentRuntimeTaskExecutor{}
	task := ExecutableTask{ID: "task-1", Executor: ExecutorInvestigator}
	_, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task))
	if err == nil {
		t.Fatal("expected missing runtime error")
	}
}

func TestAgentRuntimeTaskExecutor_PropagatesResolverError(t *testing.T) {
	executor := AgentRuntimeTaskExecutor{
		Runtime:     &fakeRuntime{},
		Definitions: fakeDefinitionResolver{err: errors.New("not found")},
	}
	task := ExecutableTask{ID: "task-1", Executor: ExecutorInvestigator}
	_, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task))
	if err == nil {
		t.Fatal("expected resolver error")
	}
}

func TestAgentRuntimeTaskExecutor_InvestigatorInputRetainsEvidenceContract(t *testing.T) {
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID: "task-contract", Status: agentapi.RunSucceeded,
		Output: mustJSON(t, investigationReportOutput{Focus: "runtime", Summary: "ok"}),
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime: runtime, Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{
		ID: "task-contract", Executor: ExecutorInvestigator,
		Capability: "knowledge.runtime.observe", Objective: "observe the current failure path",
		EvidenceGoalIDs: []string{"runtime"}, EvidenceGoals: []EvidenceGoal{{
			ID: "runtime", Kind: GoalKindRuntimeOperations, Facets: []string{"failure_path"},
			Sources:         []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime},
			RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime},
			Freshness:       agentapi.FreshnessCurrent, Required: true, MinimumCoverage: 2, HighRisk: true,
		}}, InputRefs: []EvidenceRef{{SourceKind: "runtime", Target: "service-a", Version: "now", TimeRange: "5m", ContentHash: "hash"}},
	}
	if _, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var input struct {
		Capability string `json:"capability"`
		Goals      []struct {
			Facet           string   `json:"facet"`
			Facets          []string `json:"facets"`
			Sources         []string `json:"sources"`
			RequiredSources []string `json:"required_sources"`
			Freshness       string   `json:"freshness"`
			MinimumCoverage int      `json:"minimum_coverage"`
			HighRisk        bool     `json:"high_risk"`
		} `json:"evidence_goals"`
		Refs []agentapi.EvidenceRef `json:"input_refs"`
	}
	if err := json.Unmarshal(runtime.gotReq.Input, &input); err != nil {
		t.Fatalf("decode investigator input: %v", err)
	}
	if input.Capability != "knowledge.runtime.observe" || len(input.Goals) != 1 ||
		input.Goals[0].Facet != "failure_path" || len(input.Goals[0].Facets) != 1 || input.Goals[0].Facets[0] != "failure_path" ||
		input.Goals[0].Sources[0] != "runtime" || input.Goals[0].RequiredSources[0] != "runtime" ||
		input.Goals[0].Freshness != string(agentapi.FreshnessCurrent) || input.Goals[0].MinimumCoverage != 2 || !input.Goals[0].HighRisk {
		t.Fatalf("investigator contract projection = %+v", input)
	}
	if len(input.Refs) != 1 || input.Refs[0].Target != "service-a" || input.Refs[0].TimeRange != "5m" {
		t.Fatalf("investigator input refs = %+v", input.Refs)
	}
	registry := agentapi.NewSchemaRegistry()
	if err := registry.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	if err := registry.Validate(agentapi.TaskContractSchemaRef(), runtime.gotReq.Input); err != nil {
		t.Fatalf("runtime task contract does not match current schema: %v", err)
	}
}

func TestProjectInvestigatorEvidenceRejectsOpaqueSummary(t *testing.T) {
	hash := "856d907454773e97fd50c8e2609629031f2910c0229376261da8e7d1b59f7ff7"
	result := agentapi.RunResult{
		Output: mustJSON(t, investigationReportOutput{
			Summary: hash,
			Findings: []investigationFinding{{
				Evidence: []investigationEvidence{{Kind: "code", Reference: "svc.go:42", Summary: hash}},
			}},
		}),
		EvidenceUnits: []tool.EvidenceUnit{{SourceKind: "code", Target: "svc.go:42"}},
	}
	candidates, err := projectInvestigatorEvidence(result)
	if err != nil {
		t.Fatalf("projectInvestigatorEvidence: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %#v, want no opaque evidence", candidates)
	}
}

func TestProjectInvestigatorObservationsRejectsOpaqueSummary(t *testing.T) {
	candidates := projectInvestigatorObservations(agentapi.RunResult{
		EvidenceObservations: []agentapi.EvidenceObservation{{
			SourceKind: "code",
			Target:     "svc.go:42",
			Summary:    "856d907454773e97fd50c8e2609629031f2910c0229376261da8e7d1b59f7ff7",
		}},
	})
	if len(candidates) != 0 {
		t.Fatalf("candidates = %#v, want no opaque observation", candidates)
	}
}

func TestEvidenceContextTreatsHashContentAsUnavailable(t *testing.T) {
	hash := "856d907454773e97fd50c8e2609629031f2910c0229376261da8e7d1b59f7ff7"
	task := ExecutableTask{ID: "verify-hash", Executor: ExecutorVerifier, Objective: "verify", EvidenceGoalIDs: []string{"goal"}}
	context := taskEvidenceContext(task, TaskExecutionInput{
		Task:     task,
		Evidence: []EvidenceUnit{{ID: "ev-hash", SourceKind: "code", Target: "svc.go:42", Content: hash}},
	}, EvidenceContextBudget{})
	if len(context.selected) != 0 || len(context.omissions) != 1 || context.omissions[0].Reason != "evidence_content_unavailable" {
		t.Fatalf("context = %#v, want hash omitted as unavailable", context)
	}
}

func TestVerifierWithNoReadableEvidenceReturnsUnresolvedWithoutModelCall(t *testing.T) {
	runtime := &fakeRuntime{}
	executor := AgentRuntimeTaskExecutor{
		Runtime:     runtime,
		Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{ID: "verify-hash", Executor: ExecutorVerifier, Objective: "verify", EvidenceGoalIDs: []string{"goal"}}
	input := testRuntimeTaskInput(task)
	input.Evidence = []EvidenceUnit{{ID: "ev-hash", SourceKind: "code", Target: "svc.go:42", Content: "856d907454773e97fd50c8e2609629031f2910c0229376261da8e7d1b59f7ff7"}}
	result, err := executor.Execute(context.Background(), task, input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if runtime.gotReq != nil {
		t.Fatal("verifier made a model call without readable evidence")
	}
	var output verificationResult
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("decode unresolved verification: %v", err)
	}
	if len(output.Verdicts) != 0 || len(output.Uncertainties) != 1 {
		t.Fatalf("output = %#v, want empty verdicts and one uncertainty", output)
	}
}

func TestProjectVerifierClaimsRejectsOpaqueEvidenceIdentifier(t *testing.T) {
	hash := "856d907454773e97fd50c8e2609629031f2910c0229376261da8e7d1b59f7ff7"
	task := ExecutableTask{ID: "verify-hash", Executor: ExecutorVerifier, Objective: "verify", EvidenceGoalIDs: []string{"goal"}}
	result := agentapi.RunResult{Status: agentapi.RunSucceeded, Output: mustJSON(t, verificationResult{
		Summary: "unresolved",
		Verdicts: []verificationVerdict{{
			ClaimIDs: []string{"ev-hash"}, Decision: "supported", Rationale: hash, EvidenceRefs: []string{"ev-hash"},
		}},
		Uncertainties: []string{"insufficient"},
	})}
	claims, err := projectVerifierClaims(task, TaskExecutionInput{Task: task, Evidence: []EvidenceUnit{{ID: "ev-hash", SourceKind: "code", Target: "svc.go:42", Content: hash}}}, result, EvidenceContextBudget{})
	if err != nil {
		t.Fatalf("projectVerifierClaims: %v", err)
	}
	if len(claims) != 0 {
		t.Fatalf("claims = %#v, want no claim from opaque evidence", claims)
	}
}

func TestProjectInvestigatorObservationsAllowsReadableContentWithOpaqueMetadata(t *testing.T) {
	candidates := projectInvestigatorObservations(agentapi.RunResult{
		EvidenceObservations: []agentapi.EvidenceObservation{{
			SourceKind: "code",
			Target:     "search_code",
			Summary:    `{"matches":[{"text":"the service calls its dependency"}],"evidence_id":"ev_opaque"}`,
		}},
	})
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v, want one readable evidence candidate", candidates)
	}
	if !strings.Contains(candidates[0].Content, "calls its dependency") {
		t.Fatalf("candidate content = %q, want readable tool content", candidates[0].Content)
	}
}

func TestAgentRuntimeTaskExecutor_InvestigatorEmptyOutputDoesNotDecodeJSONEOF(t *testing.T) {
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID: "empty-investigator", Status: agentapi.RunSucceeded,
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime: runtime, Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{
		ID: "empty-investigator", Executor: ExecutorInvestigator,
		Capability: "knowledge.code.inspect", Objective: "inspect the code",
		EvidenceGoalIDs: []string{"code"},
	}
	result, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Failure != nil {
		t.Fatalf("failure = %#v, want a usable partial report", result.Failure)
	}
	if len(result.Output) == 0 || !json.Valid(result.Output) {
		t.Fatalf("output = %q, want valid fallback JSON report", result.Output)
	}
	var report investigationReportOutput
	if err := json.Unmarshal(result.Output, &report); err != nil {
		t.Fatalf("decode fallback report: %v", err)
	}
	if len(report.UnresolvedEvidenceGoals) != 1 || report.UnresolvedEvidenceGoals[0] != "code" {
		t.Fatalf("unresolved goals = %#v, want code", report.UnresolvedEvidenceGoals)
	}
}

func TestProjectInvestigatorEvidenceEmptyOutputIsNotJSONEOF(t *testing.T) {
	candidates, err := projectInvestigatorEvidence(agentapi.RunResult{})
	if err != nil {
		t.Fatalf("projectInvestigatorEvidence: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("candidates = %#v, want none", candidates)
	}
}

func TestAgentRuntimeTaskExecutor_InvestigatorTruncatedReportFallsBackWithoutDecodeFailure(t *testing.T) {
	runtime := &fakeRuntime{result: agentapi.RunResult{
		RunID: "truncated-investigator", Status: agentapi.RunSucceeded,
		Output: json.RawMessage(`{"focus":"code","summary":"partial`),
	}}
	executor := AgentRuntimeTaskExecutor{
		Runtime: runtime, Definitions: fakeDefinitionResolver{def: testDefinition()},
	}
	task := ExecutableTask{
		ID: "truncated-investigator", Executor: ExecutorInvestigator,
		Capability: "knowledge.code.inspect", Objective: "inspect the code",
		EvidenceGoalIDs: []string{"code"},
	}
	result, err := executor.Execute(context.Background(), task, testRuntimeTaskInput(task))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Failure != nil {
		t.Fatalf("failure = %#v, want fallback report", result.Failure)
	}
	if !json.Valid(result.Output) {
		t.Fatalf("output = %q, want valid fallback JSON", result.Output)
	}
}
