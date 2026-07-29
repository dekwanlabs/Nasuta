package featuredelivery

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBuildArtifactRejectsTrailingJSON(t *testing.T) {
	_, err := BuildArtifact(
		KindRequirement, "feat_1", "", OriginUser,
		[]byte(`{"description":"needed"} {"description":"extra"}`), nil, 1,
	)
	if err == nil || !strings.Contains(err.Error(), "multiple JSON values") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}

func TestBuildArtifactRendersDeterministically(t *testing.T) {
	raw, err := json.Marshal(RequirementDocument{
		Description:        "Add delivery runs",
		AcceptanceCriteria: []string{"A patch is produced"},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := BuildArtifact(KindRequirement, "feat_1", "", OriginUser, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildArtifact(KindRequirement, "feat_1", "", OriginUser, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash != second.ContentHash || first.RenderedMarkdown != second.RenderedMarkdown {
		t.Fatal("same document did not produce stable content")
	}
	if first.Evidence == nil {
		t.Fatal("empty evidence must be represented as an empty array")
	}
}

func TestBlockingQuestions(t *testing.T) {
	raw := []byte(`{
		"problem_statement":"b","goals":["g"],"functional_requirements":["f"],
		"acceptance_criteria":["a"],"blocking_questions":["Who owns this?"]
	}`)
	artifact, err := BuildArtifact(KindRequirementAnalysis, "feat_1", "art_req", OriginAgent, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	questions, err := BlockingQuestions(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 {
		t.Fatalf("unexpected questions: %#v", questions)
	}
}

func TestBuildArtifactRejectsEvidenceOutsideSnapshot(t *testing.T) {
	raw := []byte(`{
		"current_technical_baseline":[{"statement":"existing behavior","classification":"fact","evidence_ids":[0]}],
		"architecture_drivers":["driver"],
		"candidate_architectures":[
			{"name":"a","summary":"a","architecture_pattern":"modular monolith","communication_pattern":"calls","data_pattern":"crud","deployment_pattern":"binary","contract_pattern":"api","migration_pattern":"expand-contract","reliability_pattern":"timeouts","observability_pattern":"logs","benefits":[],"costs":[],"risks":[],"reversibility":[]},
			{"name":"b","summary":"b","architecture_pattern":"service","communication_pattern":"events","data_pattern":"owned data","deployment_pattern":"container","contract_pattern":"events","migration_pattern":"parallel run","reliability_pattern":"retries","observability_pattern":"traces","benefits":[],"costs":[],"risks":[],"reversibility":[]}
		],
		"technical_decision":{"selected_option":"a","rationale":"lower cost","accepted_tradeoffs":["coupling"]}
	}`)
	_, err := BuildArtifact(KindTechnicalProposal, "feat_1", "art_analysis", OriginAgent, raw, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "outside snapshot") {
		t.Fatalf("expected invalid evidence reference, got %v", err)
	}
}

func TestBuildArtifactCanonicalizesImplementationRepositories(t *testing.T) {
	raw := []byte(`{
		"delivery_goal":"deliver the feature",
		"repositories":[{
			"repository":" team/nasuta/ ",
			"steps":[{"description":"implement","done_when":["tests pass"]}]
		}],
		"definition_of_done":["accepted behavior passes"]
	}`)
	artifact, err := BuildArtifact(KindImplementationPlan, "feat_1", "art_design", OriginAgent, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	var document ImplementationPlanDocument
	if err := json.Unmarshal(artifact.DocumentJSON, &document); err != nil {
		t.Fatal(err)
	}
	if got := document.Repositories[0].Repository; got != "team/nasuta" {
		t.Fatalf("repository = %q", got)
	}
}

func TestBuildArtifactCanonicalizesImplementationExpectedPaths(t *testing.T) {
	raw := []byte(`{
		"delivery_goal":"deliver the feature",
		"repositories":[{
			"repository":"team/nasuta",
			"expected_paths":[" internal/featuredelivery/ ","internal/featuredelivery"],
			"steps":[{"description":"implement","done_when":["tests pass"]}]
		}],
		"definition_of_done":["accepted behavior passes"]
	}`)
	artifact, err := BuildArtifact(KindImplementationPlan, "feat_1", "art_design", OriginAgent, raw, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	var document ImplementationPlanDocument
	if err := json.Unmarshal(artifact.DocumentJSON, &document); err != nil {
		t.Fatal(err)
	}
	paths := document.Repositories[0].ExpectedPaths
	if len(paths) != 1 || paths[0] != "internal/featuredelivery" {
		t.Fatalf("expected paths = %v", paths)
	}

	raw = []byte(`{
		"delivery_goal":"deliver the feature",
		"repositories":[{
			"repository":"team/nasuta","expected_paths":["../outside"],
			"steps":[{"description":"implement","done_when":["tests pass"]}]
		}],
		"definition_of_done":["accepted behavior passes"]
	}`)
	if _, err := BuildArtifact(KindImplementationPlan, "feat_1", "art_design", OriginAgent, raw, nil, 1); err == nil {
		t.Fatal("repository-escaping expected path must be rejected")
	}
}

func TestGenerationDocumentContractsHaveExactTopLevelKeys(t *testing.T) {
	tests := []struct {
		kind ArtifactKind
		keys []string
	}{
		{KindRequirementAnalysis, []string{
			"problem_statement", "goals", "success_metrics", "non_goals", "personas_and_scenarios",
			"user_stories", "functional_requirements", "quality_expectations", "in_scope",
			"business_constraints", "business_rules", "acceptance_criteria", "assumptions",
			"blocking_questions", "open_questions",
		}},
		{KindTechnicalProposal, []string{
			"current_technical_baseline", "architecture_drivers", "affected_capabilities",
			"candidate_architectures", "technical_decision", "compatibility_obligations",
			"security_obligations", "performance_obligations", "operational_obligations",
			"delivery_and_migration_strategy", "open_decisions", "blocking_questions",
		}},
		{KindSystemDesign, []string{
			"architecture_decision_record", "domain_model", "architecture_boundaries", "modules",
			"key_flows", "interface_contracts", "data_ownership_and_model",
			"consistency_and_concurrency", "scalability", "maintainability",
			"reliability_and_recovery", "security", "configuration", "observability",
			"evolution_and_migration", "testing_strategy", "blocking_questions",
		}},
		{KindImplementationPlan, []string{
			"delivery_goal", "repositories", "dependencies_and_contracts", "migration_work",
			"definition_of_done", "risks_and_mitigations", "do_not_modify", "blocking_questions",
		}},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			var document map[string]json.RawMessage
			if err := json.Unmarshal([]byte(generationDocumentContract(test.kind)), &document); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(document))
			for key := range document {
				got = append(got, key)
			}
			sort.Strings(got)
			want := append([]string(nil), test.keys...)
			sort.Strings(want)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("top-level keys = %v, want %v", got, want)
			}
		})
	}
}

func TestBuildArtifactRejectsLegacyDocumentFields(t *testing.T) {
	tests := []struct {
		kind  ArtifactKind
		field string
		raw   string
	}{
		{KindRequirementAnalysis, "background", `{"background":"legacy"}`},
		{KindTechnicalProposal, "current_facts", `{"current_facts":[]}`},
		{KindSystemDesign, "rejected_alternatives", `{"rejected_alternatives":[]}`},
		{KindImplementationPlan, "contracts", `{"contracts":[]}`},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			_, err := BuildArtifact(test.kind, "feat_1", "parent", OriginAgent, []byte(test.raw), nil, 1)
			if err == nil || !strings.Contains(err.Error(), `unknown field "`+test.field+`"`) {
				t.Fatalf("expected legacy field %q to be rejected, got %v", test.field, err)
			}
		})
	}
}

func TestTechnicalProposalCandidateSelectionAndValidation(t *testing.T) {
	document := validTechnicalProposalDocument()
	document.CandidateArchitectures[0].Name = " option-a "
	document.TechnicalDecision.SelectedOption = " option-a "
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := BuildArtifact(KindTechnicalProposal, "feat_1", "analysis", OriginAgent, raw, technicalProposalEvidence(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var canonical TechnicalProposalDocument
	if err := json.Unmarshal(artifact.DocumentJSON, &canonical); err != nil {
		t.Fatal(err)
	}
	if canonical.CandidateArchitectures[0].Name != "option-a" || canonical.TechnicalDecision.SelectedOption != "option-a" {
		t.Fatalf("candidate names were not canonicalized: %+v", canonical.TechnicalDecision)
	}

	tests := []struct {
		name   string
		mutate func(*TechnicalProposalDocument)
		want   string
	}{
		{
			name: "selection is case sensitive",
			mutate: func(document *TechnicalProposalDocument) {
				document.TechnicalDecision.SelectedOption = "Option-A"
			},
			want: "selects unknown candidate",
		},
		{
			name: "trimmed names are unique",
			mutate: func(document *TechnicalProposalDocument) {
				document.CandidateArchitectures[1].Name = " option-a "
			},
			want: "appears more than once",
		},
		{
			name: "all architecture patterns are required",
			mutate: func(document *TechnicalProposalDocument) {
				document.CandidateArchitectures[0].ReliabilityPattern = ""
			},
			want: "every architecture pattern",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validTechnicalProposalDocument()
			test.mutate(&document)
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			_, err = BuildArtifact(KindTechnicalProposal, "feat_1", "analysis", OriginAgent, raw, technicalProposalEvidence(), 1)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSystemDesignRequiresModuleInvariants(t *testing.T) {
	document := validSystemDesignDocument()
	document.Modules[0].Invariants = nil
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildArtifact(KindSystemDesign, "feat_1", "proposal", OriginAgent, raw, nil, 1)
	if err == nil || !strings.Contains(err.Error(), "invariants") {
		t.Fatalf("expected missing invariant error, got %v", err)
	}
}

func TestRenderedMarkdownUsesStableChapterHierarchyAndOrder(t *testing.T) {
	tests := []struct {
		kind     ArtifactKind
		document any
		headings []string
	}{
		{KindRequirementAnalysis, fullRequirementAnalysisDocument(), []string{
			"# Requirement Analysis", "## Problem Statement", "## Goals", "## Success Metrics",
			"## Non-Goals", "## Personas And Scenarios", "## User Stories", "## Functional Requirements",
			"## Quality Expectations", "## In Scope", "## Business Constraints", "## Business Rules",
			"## Acceptance Criteria", "## Assumptions", "## Blocking Questions", "## Open Questions",
		}},
		{KindTechnicalProposal, fullTechnicalProposalDocument(), []string{
			"# Technical Proposal", "## Current Technical Baseline", "## Architecture Drivers",
			"## Affected Capabilities", "## Candidate Architectures", "### option-a", "#### Summary",
			"#### Architecture Pattern", "#### Communication Pattern", "#### Data Pattern",
			"#### Deployment Pattern", "#### Contract Pattern", "#### Migration Pattern",
			"#### Reliability Pattern", "#### Observability Pattern", "#### Benefits", "#### Costs",
			"#### Risks", "#### Reversibility", "### option-b", "#### Summary", "#### Architecture Pattern",
			"#### Communication Pattern", "#### Data Pattern", "#### Deployment Pattern", "#### Contract Pattern",
			"#### Migration Pattern", "#### Reliability Pattern", "#### Observability Pattern", "#### Benefits",
			"#### Costs", "#### Risks", "#### Reversibility", "## Technical Decision", "### Selected Option",
			"### Rationale", "### Accepted Tradeoffs", "## Compatibility Obligations", "## Security Obligations",
			"## Performance Obligations", "## Operational Obligations", "## Delivery And Migration Strategy",
			"## Open Decisions", "## Blocking Questions",
		}},
		{KindSystemDesign, fullSystemDesignDocument(), []string{
			"# System Design", "## Architecture Decision Record", "### Status", "### Context", "### Decision",
			"### Consequences", "## Domain Model", "## Architecture Boundaries", "## Modules", "### delivery",
			"#### Responsibilities", "#### Dependencies", "#### Invariants", "## Key Flows", "## Interface Contracts",
			"## Data Ownership And Model", "## Consistency And Concurrency", "## Scalability", "## Maintainability",
			"## Reliability And Recovery", "## Security", "## Configuration", "## Observability",
			"## Evolution And Migration", "## Testing Strategy", "## Blocking Questions",
		}},
		{KindImplementationPlan, fullImplementationPlanDocument(), []string{
			"# Implementation Plan", "## Delivery Goal", "## Repositories", "### team/service", "#### Expected Paths",
			"#### Dependencies", "#### Steps", "#### Validation Commands", "## Dependencies And Contracts",
			"## Migration Work", "## Definition Of Done", "## Risks And Mitigations", "### Risk 1",
			"#### Description", "#### Likelihood", "#### Impact", "#### Mitigation", "## Do Not Modify",
			"## Blocking Questions",
		}},
	}
	for _, test := range tests {
		t.Run(string(test.kind), func(t *testing.T) {
			got := markdownHeadings(renderDocument(test.kind, test.document))
			if !reflect.DeepEqual(got, test.headings) {
				t.Fatalf("headings:\n%q\nwant:\n%q", got, test.headings)
			}
		})
	}
}

func validTechnicalProposalDocument() TechnicalProposalDocument {
	option := func(name, pattern string) ArchitectureOption {
		return ArchitectureOption{
			Name: name, Summary: name + " summary", ArchitecturePattern: pattern,
			CommunicationPattern: "request-response", DataPattern: "owned records", DeploymentPattern: "binary",
			ContractPattern: "versioned API", MigrationPattern: "expand-contract", ReliabilityPattern: "timeouts",
			ObservabilityPattern: "logs and metrics", Benefits: []string{"benefit"}, Costs: []string{"cost"},
			Risks: []string{"risk"}, Reversibility: []string{"revert deployment"},
		}
	}
	return TechnicalProposalDocument{
		CurrentTechnicalBaseline: []EvidenceClaim{{Statement: "current behavior", Classification: "fact", EvidenceIDs: []int{0}}},
		ArchitectureDrivers:      []string{"driver"},
		AffectedCapabilities:     []string{"delivery"},
		CandidateArchitectures:   []ArchitectureOption{option("option-a", "modular monolith"), option("option-b", "service")},
		TechnicalDecision: TechnicalDecision{
			SelectedOption: "option-a", Rationale: "best trade-off", AcceptedTradeoffs: []string{"coupling"},
		},
	}
}

func technicalProposalEvidence() []EvidenceRef {
	return []EvidenceRef{{Kind: "code", Summary: "current behavior"}}
}

func TestBuildArtifactCanonicalizesLongUTF8EvidenceSummary(t *testing.T) {
	document, err := json.Marshal(validTechnicalProposalDocument())
	if err != nil {
		t.Fatal(err)
	}
	evidence := []EvidenceRef{{Kind: "code", Summary: "  " + strings.Repeat("证据", maxEvidenceText) + "  "}}
	artifact, err := BuildArtifact(KindTechnicalProposal, "req-1", "parent-1", OriginUser, document, evidence, 1)
	if err != nil {
		t.Fatal(err)
	}
	summary := artifact.Evidence[0].Summary
	if len(summary) > maxEvidenceText || !utf8.ValidString(summary) {
		t.Fatalf("summary bytes=%d valid_utf8=%t", len(summary), utf8.ValidString(summary))
	}
}

func validSystemDesignDocument() SystemDesignDocument {
	return SystemDesignDocument{
		ArchitectureDecisionRecord: ArchitectureDecisionRecord{
			Status: "accepted", Context: "context", Decision: "decision", Consequences: []string{"consequence"},
		},
		ArchitectureBoundaries: []string{"boundary"},
		Modules: []DesignModule{{
			Name: "delivery", Responsibilities: []string{"deliver"}, Dependencies: []string{"storage"},
			Invariants: []string{"one owner"},
		}},
		TestingStrategy: []string{"unit tests"},
	}
}

func fullRequirementAnalysisDocument() *RequirementAnalysisDocument {
	return &RequirementAnalysisDocument{
		ProblemStatement: "problem", Goals: []string{"goal"}, SuccessMetrics: []string{"metric"},
		NonGoals: []string{"non-goal"}, PersonasAndScenarios: []string{"persona"}, UserStories: []string{"story"},
		FunctionalRequirements: []string{"behavior"}, QualityExpectations: []string{"quality"}, InScope: []string{"scope"},
		BusinessConstraints: []string{"constraint"}, BusinessRules: []string{"rule"},
		AcceptanceCriteria: []string{"accepted"}, Assumptions: []string{"assumption"},
		BlockingQuestions: []string{"blocker"}, OpenQuestions: []string{"question"},
	}
}

func fullTechnicalProposalDocument() *TechnicalProposalDocument {
	document := validTechnicalProposalDocument()
	document.CompatibilityObligations = []string{"compatibility"}
	document.SecurityObligations = []string{"security"}
	document.PerformanceObligations = []string{"performance"}
	document.OperationalObligations = []string{"operations"}
	document.DeliveryAndMigrationStrategy = []string{"migration"}
	document.OpenDecisions = []string{"decision"}
	document.BlockingQuestions = []string{"blocker"}
	return &document
}

func fullSystemDesignDocument() *SystemDesignDocument {
	document := validSystemDesignDocument()
	document.DomainModel = []string{"domain"}
	document.KeyFlows = []string{"flow"}
	document.InterfaceContracts = []string{"contract"}
	document.DataOwnershipAndModel = []string{"owner"}
	document.ConsistencyAndConcurrency = []string{"consistency"}
	document.Scalability = []string{"scale"}
	document.Maintainability = []string{"maintain"}
	document.ReliabilityAndRecovery = []string{"recover"}
	document.Security = []string{"secure"}
	document.Configuration = []string{"configure"}
	document.Observability = []string{"observe"}
	document.EvolutionAndMigration = []string{"evolve"}
	document.BlockingQuestions = []string{"blocker"}
	return &document
}

func fullImplementationPlanDocument() *ImplementationPlanDocument {
	return &ImplementationPlanDocument{
		DeliveryGoal: "deliver", Repositories: []RepositoryPlan{{
			Repository: "team/service", ExpectedPaths: []string{"internal/delivery"}, Dependencies: []string{"team/common"},
			Steps:              []ImplementationStep{{Description: "implement", DoneWhen: []string{"tests pass"}}},
			ValidationCommands: [][]string{{"go", "test", "./..."}},
		}},
		DependenciesAndContracts: []string{"contract"}, MigrationWork: []string{"migration"},
		DefinitionOfDone: []string{"done"}, RisksAndMitigations: []DeliveryRisk{{
			Description: "risk", Likelihood: "low", Impact: "high", Mitigation: "mitigate",
		}},
		DoNotModify: []string{"unrelated"}, BlockingQuestions: []string{"blocker"},
	}
}

func markdownHeadings(markdown string) []string {
	var headings []string
	for _, line := range strings.Split(markdown, "\n") {
		if strings.HasPrefix(line, "#") {
			headings = append(headings, line)
		}
	}
	return headings
}
