package delivery

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"github.com/dekwanlabs/nasuta/internal/prompts"
)

// runtimeFeatureInstructionLocale selects the language used to instruct the
// model. The generation request independently requires Simplified Chinese
// natural-language output.
const runtimeFeatureInstructionLocale = "en"

var featurePromptIDs = map[string]map[string]prompts.ID{
	"en": {
		"requirement_analysis.md": prompts.FeatureDeliveryRequirementAnalysis,
		"technical_proposal.md":   prompts.FeatureDeliveryTechnicalProposal,
		"system_design.md":        prompts.FeatureDeliverySystemDesign,
		"implementation_plan.md":  prompts.FeatureDeliveryImplementationPlan,
		"generation_request.md":   prompts.FeatureDeliveryGenerationRequest,
		"coding_task.md":          prompts.FeatureDeliveryCodingTask,
	},
	"zh-CN": {
		"requirement_analysis.md": prompts.FeatureDeliveryZHCNRequirementAnalysis,
		"technical_proposal.md":   prompts.FeatureDeliveryZHCNTechnicalProposal,
		"system_design.md":        prompts.FeatureDeliveryZHCNSystemDesign,
		"implementation_plan.md":  prompts.FeatureDeliveryZHCNImplementationPlan,
		"generation_request.md":   prompts.FeatureDeliveryZHCNGenerationRequest,
		"coding_task.md":          prompts.FeatureDeliveryZHCNCodingTask,
	},
}

var generationSystemPrompts = map[ArtifactKind]string{
	KindRequirementAnalysis: mustReadFeaturePrompt("requirement_analysis.md"),
	KindTechnicalProposal:   mustReadFeaturePrompt("technical_proposal.md"),
	KindSystemDesign:        mustReadFeaturePrompt("system_design.md"),
	KindImplementationPlan:  mustReadFeaturePrompt("implementation_plan.md"),
}

type generationPromptData struct {
	Contract string
	Kind     ArtifactKind
	Input    string
}

type codingTaskPromptData struct {
	Run               ImplementationRun
	RepositoryPlan    RepositoryPlan
	ApprovedArtifacts []*Artifact
}

func generationPrompt(parent Artifact, kind ArtifactKind, evidence []EvidenceRef) string {
	if kind == KindRequirementAnalysis {
		return renderGenerationPrompt(kind, struct {
			Requirement json.RawMessage `json:"requirement"`
		}{
			Requirement: parent.DocumentJSON,
		})
	}
	input := struct {
		Parent   Artifact      `json:"parent_artifact"`
		Evidence []EvidenceRef `json:"evidence"`
	}{
		Parent: Artifact{
			ID: parent.ID, Kind: parent.Kind, Version: parent.Version,
			DocumentJSON: parent.DocumentJSON, RenderedMarkdown: parent.RenderedMarkdown,
		},
		Evidence: evidence,
	}
	return renderGenerationPrompt(kind, input)
}

func renderGenerationPrompt(kind ArtifactKind, input any) string {
	payload, _ := json.Marshal(input)
	rendered, err := prompts.Render(mustFeaturePromptID(runtimeFeatureInstructionLocale, "generation_request.md"), generationPromptData{
		Contract: generationDocumentContract(kind),
		Kind:     kind,
		Input:    string(payload),
	})
	if err != nil {
		panic(err)
	}
	return rendered
}

func generationDocumentContract(kind ArtifactKind) string {
	var contract any
	switch kind {
	case KindRequirementAnalysis:
		contract = RequirementAnalysisDocument{
			ProblemStatement:       "string",
			Goals:                  []string{"string"},
			SuccessMetrics:         []string{},
			NonGoals:               []string{},
			PersonasAndScenarios:   []string{},
			UserStories:            []string{},
			FunctionalRequirements: []string{"string"},
			QualityExpectations:    []string{},
			InScope:                []string{},
			BusinessConstraints:    []string{},
			BusinessRules:          []string{},
			AcceptanceCriteria:     []string{"string"},
			Assumptions:            []string{},
			BlockingQuestions:      []string{},
			OpenQuestions:          []string{},
		}
	case KindTechnicalProposal:
		contract = TechnicalProposalDocument{
			CurrentTechnicalBaseline: []EvidenceClaim{{
				Statement: "string", Classification: "unknown", EvidenceIDs: []int{},
			}},
			ArchitectureDrivers:  []string{"string"},
			AffectedCapabilities: []string{},
			CandidateArchitectures: []ArchitectureOption{
				{
					Name: "option-a", Summary: "string", ArchitecturePattern: "string",
					CommunicationPattern: "string", DataPattern: "string", DeploymentPattern: "string",
					ContractPattern: "string", MigrationPattern: "string", ReliabilityPattern: "string",
					ObservabilityPattern: "string", Benefits: []string{}, Costs: []string{},
					Risks: []string{}, Reversibility: []string{},
				},
				{
					Name: "option-b", Summary: "string", ArchitecturePattern: "string",
					CommunicationPattern: "string", DataPattern: "string", DeploymentPattern: "string",
					ContractPattern: "string", MigrationPattern: "string", ReliabilityPattern: "string",
					ObservabilityPattern: "string", Benefits: []string{}, Costs: []string{},
					Risks: []string{}, Reversibility: []string{},
				},
			},
			TechnicalDecision: TechnicalDecision{
				SelectedOption: "option-a", Rationale: "string", AcceptedTradeoffs: []string{"string"},
			},
			CompatibilityObligations:     []string{},
			SecurityObligations:          []string{},
			PerformanceObligations:       []string{},
			OperationalObligations:       []string{},
			DeliveryAndMigrationStrategy: []string{},
			OpenDecisions:                []string{},
			BlockingQuestions:            []string{},
		}
	case KindSystemDesign:
		contract = SystemDesignDocument{
			ArchitectureDecisionRecord: ArchitectureDecisionRecord{
				Status: "accepted", Context: "string", Decision: "string", Consequences: []string{"string"},
			},
			DomainModel:            []string{},
			ArchitectureBoundaries: []string{"string"},
			Modules: []DesignModule{{
				Name: "string", Responsibilities: []string{"string"}, Dependencies: []string{},
				Invariants: []string{"string"},
			}},
			KeyFlows:                  []string{},
			InterfaceContracts:        []string{},
			DataOwnershipAndModel:     []string{},
			ConsistencyAndConcurrency: []string{},
			Scalability:               []string{},
			Maintainability:           []string{},
			ReliabilityAndRecovery:    []string{},
			Security:                  []string{},
			Configuration:             []string{},
			Observability:             []string{},
			EvolutionAndMigration:     []string{},
			TestingStrategy:           []string{"string"},
			BlockingQuestions:         []string{},
		}
	case KindImplementationPlan:
		contract = ImplementationPlanDocument{
			DeliveryGoal: "string",
			Repositories: []RepositoryPlan{{
				Repository:    "owner/repository",
				ExpectedPaths: []string{},
				Dependencies:  []string{},
				Steps: []ImplementationStep{{
					Description: "string", DoneWhen: []string{"string"},
				}},
				ValidationCommands: [][]string{{"test-command", "argument"}},
			}},
			DependenciesAndContracts: []string{},
			MigrationWork:            []string{},
			DefinitionOfDone:         []string{"string"},
			RisksAndMitigations: []DeliveryRisk{{
				Description: "string", Likelihood: "medium",
				Impact: "medium", Mitigation: "string",
			}},
			DoNotModify:       []string{},
			BlockingQuestions: []string{},
		}
	default:
		return "{}"
	}
	encoded, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func generationSystemPrompt(kind ArtifactKind) string {
	return generationSystemPrompts[kind]
}

func buildCodingTaskPrompt(run ImplementationRun, repositoryPlan RepositoryPlan, chain []*Artifact) (string, error) {
	artifacts := make([]*Artifact, len(chain))
	for index := range chain {
		artifacts[len(chain)-1-index] = chain[index]
	}
	return prompts.Render(mustFeaturePromptID(runtimeFeatureInstructionLocale, "coding_task.md"), codingTaskPromptData{
		Run:               run,
		RepositoryPlan:    repositoryPlan,
		ApprovedArtifacts: artifacts,
	})
}

func renderFeaturePrompt(prompt *template.Template, data any) (string, error) {
	var builder strings.Builder
	if err := prompt.Execute(&builder, data); err != nil {
		return "", fmt.Errorf("render feature delivery prompt %q: %w", prompt.Name(), err)
	}
	return strings.TrimSpace(builder.String()), nil
}

func mustReadFeaturePrompt(name string) string {
	return mustReadLocalizedFeaturePrompt(runtimeFeatureInstructionLocale, name)
}

func mustReadLocalizedFeaturePrompt(locale, name string) string {
	return prompts.Text(mustFeaturePromptID(locale, name))
}

func mustFeaturePromptID(locale, name string) prompts.ID {
	localized, ok := featurePromptIDs[locale]
	if !ok {
		panic(fmt.Sprintf("delivery: unknown prompt locale %q", locale))
	}
	id, ok := localized[name]
	if !ok {
		panic(fmt.Sprintf("delivery: unknown %s prompt %q", locale, name))
	}
	return id
}
