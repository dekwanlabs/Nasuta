package featuredelivery

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

//go:embed prompts/*/*.md
var featureDeliveryPromptFS embed.FS

const runtimeFeaturePromptLocale = "en"

var generationSystemPrompts = map[ArtifactKind]string{
	KindRequirementAnalysis: mustReadFeaturePrompt("requirement_analysis.md"),
	KindTechnicalProposal:   mustReadFeaturePrompt("technical_proposal.md"),
	KindSystemDesign:        mustReadFeaturePrompt("system_design.md"),
	KindImplementationPlan:  mustReadFeaturePrompt("implementation_plan.md"),
}

var (
	generationRequestTemplate = template.Must(template.New("generation_request").Parse(
		mustReadFeaturePrompt("generation_request.md"),
	))
	codingTaskTemplate = template.Must(
		template.New("coding_task").
			Funcs(template.FuncMap{
				"addOne": func(index int) int { return index + 1 },
			}).
			Parse(mustReadFeaturePrompt("coding_task.md")),
	)
)

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
	rendered, err := renderFeaturePrompt(generationRequestTemplate, generationPromptData{
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
	return renderFeaturePrompt(codingTaskTemplate, codingTaskPromptData{
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
	return mustReadLocalizedFeaturePrompt(runtimeFeaturePromptLocale, name)
}

func mustReadLocalizedFeaturePrompt(locale, name string) string {
	content, err := featureDeliveryPromptFS.ReadFile("prompts/" + locale + "/" + name)
	if err != nil {
		panic(fmt.Sprintf("featuredelivery: read %s prompt %q: %v", locale, name, err))
	}
	return string(content)
}
