package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
		ID:      "investigator.code",
		Version: 1,
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

func TestAgentRuntimeTaskExecutor_InvestigatorProjection(t *testing.T) {
	report := investigationReportOutput{
		Focus:   "ai integration",
		Summary: "fallback summary",
		Findings: []investigationFinding{
			{
				Claim:   "service calls the model",
				GoalIDs: []string{"G1"},
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
		ID:        "task-1",
		Executor:  ExecutorInvestigator,
		Objective: "find the model entrypoint",
		GoalIDs:   []string{"G1"},
		Entities:  []string{"hsas-aiot-service"},
	}
	result, err := executor.Execute(context.Background(), task, TaskExecutionInput{Task: task})
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
	task := ExecutableTask{ID: "task-1", Executor: ExecutorInvestigator, GoalIDs: []string{"G1"}}
	result, err := executor.Execute(context.Background(), task, TaskExecutionInput{Task: task})
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
	task := ExecutableTask{ID: "task-2", Executor: ExecutorVerifier, Objective: "verify ai entry", GoalIDs: []string{"G1"}}
	input := TaskExecutionInput{
		Task: task,
		Evidence: []EvidenceUnit{
			{ID: "ev-1", Content: "the service calls the model"},
		},
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

func TestAgentRuntimeTaskExecutor_RequiresRuntimeAndResolver(t *testing.T) {
	executor := AgentRuntimeTaskExecutor{}
	task := ExecutableTask{ID: "task-1", Executor: ExecutorInvestigator}
	_, err := executor.Execute(context.Background(), task, TaskExecutionInput{Task: task})
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
	_, err := executor.Execute(context.Background(), task, TaskExecutionInput{Task: task})
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
		GoalIDs: []string{"runtime"}, EvidenceGoals: []EvidenceGoal{{
			ID: "runtime", Kind: GoalKindRuntimeOperations, Facets: []string{"failure_path"},
			Sources:         []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime},
			RequiredSources: []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime},
			Freshness:       agentapi.FreshnessCurrent, Required: true, MinimumCoverage: 2,
		}}, InputRefs: []EvidenceRef{{SourceKind: "runtime", Target: "service-a", Version: "now", TimeRange: "5m", ContentHash: "hash"}},
	}
	if _, err := executor.Execute(context.Background(), task, TaskExecutionInput{Task: task}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var input struct {
		Capability string `json:"capability"`
		Goals      []struct {
			Facet           string   `json:"facet"`
			Sources         []string `json:"sources"`
			RequiredSources []string `json:"required_sources"`
			Freshness       string   `json:"freshness"`
			MinimumCoverage int      `json:"minimum_coverage"`
		} `json:"evidence_goals"`
		Refs []agentapi.EvidenceRef `json:"input_refs"`
	}
	if err := json.Unmarshal(runtime.gotReq.Input, &input); err != nil {
		t.Fatalf("decode investigator input: %v", err)
	}
	if input.Capability != "knowledge.runtime.observe" || len(input.Goals) != 1 || input.Goals[0].Facet != "failure_path" || input.Goals[0].Sources[0] != "runtime" || input.Goals[0].RequiredSources[0] != "runtime" || input.Goals[0].Freshness != string(agentapi.FreshnessCurrent) || input.Goals[0].MinimumCoverage != 2 {
		t.Fatalf("investigator contract projection = %+v", input)
	}
	if len(input.Refs) != 1 || input.Refs[0].Target != "service-a" || input.Refs[0].TimeRange != "5m" {
		t.Fatalf("investigator input refs = %+v", input.Refs)
	}
}
