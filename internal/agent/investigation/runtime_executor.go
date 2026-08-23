package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const (
	defaultInvestigatorDefinitionID = "investigator.code"
	defaultVerifierDefinitionID     = "delegation.verifier"
	defaultComposerDefinitionID     = "synthesizer"
)

// AgentRuntimeTaskExecutor routes investigator, verifier, and composer tasks to
// the real agent runtime. Each role is pinned to an immutable agent definition;
// direct tool tasks stay on the deterministic executors in this package.
type AgentRuntimeTaskExecutor struct {
	Runtime     agentapi.Runtime
	Definitions agentapi.DefinitionResolver
	// InvestigatorDefinition, VerifierDefinition, and ComposerDefinition override
	// the default role definitions. A zero Version resolves the catalog default.
	InvestigatorDefinition agentapi.DefinitionRef
	VerifierDefinition     agentapi.DefinitionRef
	ComposerDefinition     agentapi.DefinitionRef
}

func (e AgentRuntimeTaskExecutor) Execute(
	ctx context.Context,
	task ExecutableTask,
	input TaskExecutionInput,
) (TaskExecutionResult, error) {
	if e.Runtime == nil {
		return TaskExecutionResult{}, fmt.Errorf("agent runtime is required")
	}
	if e.Definitions == nil {
		return TaskExecutionResult{}, fmt.Errorf("agent definition resolver is required")
	}
	ref, err := e.definitionRefForTask(task)
	if err != nil {
		return TaskExecutionResult{}, err
	}
	definition, err := e.Definitions.Resolve(ref)
	if err != nil {
		return TaskExecutionResult{}, fmt.Errorf("resolve agent definition %q: %w", ref.ID, err)
	}
	request, err := e.buildRequest(task, input, definition)
	if err != nil {
		return TaskExecutionResult{}, err
	}
	result, err := e.Runtime.Run(ctx, request)
	if err != nil {
		return TaskExecutionResult{}, err
	}
	return e.project(task, input, result)
}

func (e AgentRuntimeTaskExecutor) definitionRefForTask(task ExecutableTask) (agentapi.DefinitionRef, error) {
	if task.Executor == ExecutorInvestigator && task.Capability != "" {
		if id := investigatorDefinitionForCapability(task.Capability); id != "" {
			return agentapi.DefinitionRef{ID: id}, nil
		}
	}
	return e.definitionRef(task.Executor)
}

func investigatorDefinitionForCapability(capability string) string {
	switch capability {
	case "knowledge.code.inspect":
		return "investigator.code"
	case "knowledge.service.trace":
		return "investigator.runtime"
	case "knowledge.docs.verify":
		return "investigator.docs"
	case "knowledge.web.research":
		return "investigator.web"
	case "knowledge.memory.recall":
		return "investigator.memory"
	case "knowledge.runtime.observe":
		return "investigator.observe"
	default:
		return ""
	}
}

func (e AgentRuntimeTaskExecutor) definitionRef(executor ExecutorType) (agentapi.DefinitionRef, error) {
	switch executor {
	case ExecutorInvestigator:
		return defaultRef(e.InvestigatorDefinition, defaultInvestigatorDefinitionID), nil
	case ExecutorVerifier:
		return defaultRef(e.VerifierDefinition, defaultVerifierDefinitionID), nil
	case ExecutorComposer:
		return defaultRef(e.ComposerDefinition, defaultComposerDefinitionID), nil
	default:
		return agentapi.DefinitionRef{}, fmt.Errorf("executor %q has no agent definition", executor)
	}
}

func defaultRef(ref agentapi.DefinitionRef, id string) agentapi.DefinitionRef {
	if ref.ID == "" {
		ref.ID = id
	}
	return ref
}

func (e AgentRuntimeTaskExecutor) buildRequest(
	task ExecutableTask,
	input TaskExecutionInput,
	definition agentapi.Definition,
) (agentapi.RunRequest, error) {
	rawInput, err := e.buildInput(task, input)
	if err != nil {
		return agentapi.RunRequest{}, err
	}
	request := agentapi.RunRequest{
		RunID:          task.ID,
		Agent:          agentapi.DefinitionRef{ID: definition.ID, Version: definition.Version},
		DefinitionHash: definition.ContentHash,
		Input:          rawInput,
		Permissions:    definition.Permissions,
		ToolScope: agentapi.ToolScope{
			RestrictVisible: true,
			VisibleToolIDs:  definition.Tools.VisibleToolIDs,
		},
		Policy: agentapi.RunPolicy{EvidenceRequired: true},
		Limits: e.limits(task, definition),
	}
	if task.Executor == ExecutorComposer {
		contract := composerTaskContract(task)
		objective, err := synthesisObjectiveBlock(contract)
		if err != nil {
			return agentapi.RunRequest{}, err
		}
		request.Context = []agentapi.ContextBlock{objective}
	}
	return request, nil
}

func (e AgentRuntimeTaskExecutor) buildInput(task ExecutableTask, input TaskExecutionInput) (json.RawMessage, error) {
	switch task.Executor {
	case ExecutorInvestigator:
		return investigatorInput(task)
	case ExecutorVerifier:
		return verifierInput(task, input)
	case ExecutorComposer:
		return composerInput(task, input)
	default:
		return nil, fmt.Errorf("executor %q has no input builder", task.Executor)
	}
}

func (e AgentRuntimeTaskExecutor) limits(task ExecutableTask, definition agentapi.Definition) agentapi.RunLimits {
	limits := agentapi.RunLimits{
		MaxSteps:     definition.Budget.MaxSteps,
		MaxToolCalls: definition.Budget.MaxToolCalls,
	}
	if totalTokens := tokenTotal(task.Budget.Limit); totalTokens > 0 {
		// The runtime exposes one total-token limit. The ledger still enforces
		// input and output components independently; this prevents an
		// output-only projection from accidentally ignoring input consumption.
		limits.MaxTotalTokens = totalTokens
	}
	if task.Budget.Limit.CostMicros > 0 {
		limits.MaxCostMicros = task.Budget.Limit.CostMicros
	}
	if task.Budget.Limit.Duration > 0 {
		limits.Deadline = time.Now().UTC().Add(task.Budget.Limit.Duration)
	}
	return limits
}

func composerTaskContract(task ExecutableTask) InvestigationContract {
	goals := make([]EvidenceGoal, 0, len(task.GoalIDs))
	for _, goalID := range task.GoalIDs {
		goals = append(goals, EvidenceGoal{ID: goalID, Kind: goalID, Description: task.Objective, Required: true})
	}
	return InvestigationContract{ID: task.ID, Question: task.Objective, Goals: goals}
}

func composerInput(task ExecutableTask, input TaskExecutionInput) (json.RawMessage, error) {
	contract := composerTaskContract(task)
	bundle, err := marshalVerifiedBundle(contract, InvestigationReport{
		Evidence: append([]EvidenceUnit(nil), input.Evidence...),
		Claims:   append([]VerifiedClaim(nil), input.Claims...),
	})
	if err != nil {
		return nil, fmt.Errorf("build composer verified bundle: %w", err)
	}
	return bundle, nil
}

type contractEntity struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Role    string   `json:"role,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

type contractEvidenceGoal struct {
	ID              string   `json:"id"`
	Facet           string   `json:"facet"`
	Facets          []string `json:"facets,omitempty"`
	Required        bool     `json:"required"`
	Sources         []string `json:"sources"`
	RequiredSources []string `json:"required_sources,omitempty"`
	Freshness       string   `json:"freshness"`
	MinimumCoverage int      `json:"minimum_coverage"`
}

func investigatorInput(task ExecutableTask) (json.RawMessage, error) {
	entities := make([]contractEntity, 0, len(task.Entities))
	for _, entity := range task.Entities {
		entities = append(entities, contractEntity{ID: entity})
	}
	goals := make([]contractEvidenceGoal, 0, len(task.EvidenceGoals))
	if len(task.EvidenceGoals) > 0 {
		for _, goal := range task.EvidenceGoals {
			goals = append(goals, projectEvidenceGoal(goal))
		}
	} else {
		// Keep hand-built tasks valid while making compiled tasks use the full
		// admitted goal contract above.
		for _, goalID := range task.GoalIDs {
			goals = append(goals, contractEvidenceGoal{
				ID: goalID, Facet: goalID, Facets: []string{goalID},
				Required: true, Sources: []string{"internal"},
				Freshness: string(agentapi.FreshnessStable), MinimumCoverage: 1,
			})
		}
	}
	refs := make([]agentapi.EvidenceRef, 0, len(task.InputRefs))
	for _, ref := range task.InputRefs {
		refs = append(refs, agentapi.EvidenceRef{
			SourceKind: ref.SourceKind, Target: ref.Target, Section: ref.Section,
			Version: ref.Version, TimeRange: ref.TimeRange, ContentHash: ref.ContentHash,
		})
	}
	return json.Marshal(struct {
		TaskID        string                 `json:"task_id"`
		Objective     string                 `json:"objective"`
		Capability    string                 `json:"capability,omitempty"`
		Entities      []contractEntity       `json:"entities"`
		EvidenceGoals []contractEvidenceGoal `json:"evidence_goals"`
		InputRefs     []agentapi.EvidenceRef `json:"input_refs,omitempty"`
		Context       map[string]any         `json:"context"`
	}{
		TaskID: task.ID, Objective: task.Objective, Capability: task.Capability,
		Entities: entities, EvidenceGoals: goals, InputRefs: refs,
		Context: map[string]any{},
	})
}

func projectEvidenceGoal(goal EvidenceGoal) contractEvidenceGoal {
	facets := append([]string(nil), goal.Facets...)
	facet := goal.Kind
	if len(facets) > 0 && strings.TrimSpace(facets[0]) != "" {
		facet = facets[0]
	}
	return contractEvidenceGoal{
		ID: goal.ID, Facet: facet, Facets: facets, Required: goal.Required,
		Sources:         evidenceSources(goal.Sources),
		RequiredSources: evidenceSources(goal.RequiredSources),
		Freshness:       string(goal.Freshness), MinimumCoverage: goal.MinimumCoverage,
	}
}

func evidenceSources(sources []agentapi.EvidenceSource) []string {
	out := make([]string, 0, len(sources))
	for _, source := range sources {
		if value := strings.TrimSpace(string(source)); value != "" {
			out = append(out, value)
		}
	}
	return out
}

type verificationClaim struct {
	ID        string   `json:"id"`
	Statement string   `json:"statement"`
	Citations []string `json:"citations"`
}

func verifierInput(task ExecutableTask, input TaskExecutionInput) (json.RawMessage, error) {
	claims := make([]verificationClaim, 0, len(input.Evidence))
	evidenceRefs := make([]string, 0, len(input.Evidence))
	for _, unit := range input.Evidence {
		claims = append(claims, verificationClaim{
			ID:        unit.ID,
			Statement: unit.Content,
			Citations: []string{unit.ID},
		})
		evidenceRefs = append(evidenceRefs, unit.ID)
	}
	if len(claims) == 0 {
		return nil, fmt.Errorf("verifier has no evidence to verify")
	}
	return json.Marshal(struct {
		Question         string              `json:"question"`
		DecisionQuestion string              `json:"decision_question"`
		Claims           []verificationClaim `json:"claims"`
		Conflicts        []any               `json:"conflicts"`
		EvidenceRefs     []string            `json:"evidence_refs"`
		Reasons          []string            `json:"reasons"`
	}{
		Question:         task.Objective,
		DecisionQuestion: task.Objective,
		Claims:           claims,
		Conflicts:        []any{},
		EvidenceRefs:     evidenceRefs,
		Reasons:          []string{"verify evidence-backed claims"},
	})
}

func (e AgentRuntimeTaskExecutor) project(
	task ExecutableTask,
	input TaskExecutionInput,
	result agentapi.RunResult,
) (TaskExecutionResult, error) {
	if result.Error != nil {
		return TaskExecutionResult{}, fmt.Errorf("agent run %q failed: %s", result.RunID, result.Error.Message)
	}
	if result.Status != agentapi.RunSucceeded {
		return TaskExecutionResult{}, fmt.Errorf("agent run %q has status %q", result.RunID, result.Status)
	}
	out := TaskExecutionResult{
		Output: result.Output,
		Usage: BudgetVector{
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			TotalTokens:  result.Usage.InputTokens + result.Usage.OutputTokens,
			ToolCalls:    result.Evidence.ToolCallCount,
			CostMicros:   result.Usage.CostMicros,
		},
	}
	switch task.Executor {
	case ExecutorInvestigator:
		evidence, err := projectInvestigatorEvidence(result)
		if err != nil {
			return TaskExecutionResult{}, err
		}
		out.EvidenceCandidates = evidence
		out.Discoveries = projectInvestigatorDiscoveries(result.Output)
	case ExecutorVerifier:
		claims, err := projectVerifierClaims(task, input, result)
		if err != nil {
			return TaskExecutionResult{}, err
		}
		out.Claims = claims
	case ExecutorComposer:
		// The synthesized answer is carried in Output.
	default:
		return TaskExecutionResult{}, fmt.Errorf("executor %q has no projection", task.Executor)
	}
	return out, nil
}

type investigationReportOutput struct {
	Focus                  string                    `json:"focus"`
	Summary                string                    `json:"summary"`
	Findings               []investigationFinding    `json:"findings"`
	Gaps                   []string                  `json:"gaps"`
	CoveredGoals           []string                  `json:"covered_goals"`
	UnresolvedGoals        []string                  `json:"unresolved_goals"`
	DiscoveredEntities     []string                  `json:"discovered_entities"`
	DiscoveredDependencies []investigationDependency `json:"discovered_dependencies"`
}

type investigationFinding struct {
	Claim      string                  `json:"claim"`
	GoalIDs    []string                `json:"goal_ids"`
	Evidence   []investigationEvidence `json:"evidence"`
	Confidence float64                 `json:"confidence"`
}

type investigationEvidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

type investigationDependency struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// projectInvestigatorEvidence reconstructs content-bearing evidence candidates
// from a raw RunResult. RunResult.EvidenceUnits carries identity but not content,
// so content is recovered from the investigation.report output.
func projectInvestigatorEvidence(result agentapi.RunResult) ([]EvidenceCandidate, error) {
	var report investigationReportOutput
	if len(result.Output) > 0 {
		if err := json.Unmarshal(result.Output, &report); err != nil {
			return nil, fmt.Errorf("decode investigation report: %w", err)
		}
	}
	summaryByRef := make(map[string]string)
	for _, finding := range report.Findings {
		for _, evidence := range finding.Evidence {
			key := evidence.Kind + "\x00" + evidence.Reference
			if _, exists := summaryByRef[key]; !exists {
				summaryByRef[key] = evidence.Summary
			}
		}
	}
	candidates := make([]EvidenceCandidate, 0, len(result.EvidenceUnits))
	for _, unit := range result.EvidenceUnits {
		content := summaryByRef[unit.SourceKind+"\x00"+unit.Target]
		if content == "" {
			content = report.Summary
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		candidates = append(candidates, EvidenceCandidate{
			SourceKind:    unit.SourceKind,
			Target:        unit.Target,
			Section:       firstString(unit.Sections),
			Content:       content,
			Facets:        append([]string(nil), unit.Facets...),
			TrustTier:     unit.TrustTier,
			EvidenceClass: unit.EvidenceClass,
			Version:       unit.Version,
			TimeRange:     unit.TimeRange,
		})
	}
	// Fall back to report findings when the runtime returned no canonical units.
	if len(candidates) == 0 {
		for _, finding := range report.Findings {
			for _, evidence := range finding.Evidence {
				if strings.TrimSpace(evidence.Summary) == "" {
					continue
				}
				candidates = append(candidates, EvidenceCandidate{
					SourceKind: evidence.Kind,
					Target:     evidence.Reference,
					Content:    evidence.Summary,
				})
			}
		}
	}
	return candidates, nil
}

func projectInvestigatorDiscoveries(output json.RawMessage) []Discovery {
	if len(output) == 0 {
		return nil
	}
	var report investigationReportOutput
	if err := json.Unmarshal(output, &report); err != nil {
		return nil
	}
	discoveries := make([]Discovery, 0, len(report.DiscoveredEntities)+len(report.DiscoveredDependencies))
	for _, entity := range report.DiscoveredEntities {
		entity = strings.TrimSpace(entity)
		if entity != "" {
			discoveries = append(discoveries, Discovery{Type: "entity", Entity: entity})
		}
	}
	for _, dependency := range report.DiscoveredDependencies {
		from := strings.TrimSpace(dependency.From)
		to := strings.TrimSpace(dependency.To)
		if from == "" || to == "" {
			continue
		}
		discoveries = append(discoveries, Discovery{
			Type: "dependency", From: from, To: to, Kind: strings.TrimSpace(dependency.Kind),
		})
	}
	return discoveries
}

type verificationResult struct {
	Summary       string                `json:"summary"`
	Verdicts      []verificationVerdict `json:"verdicts"`
	Uncertainties []string              `json:"uncertainties"`
}

type verificationVerdict struct {
	ClaimIDs     []string `json:"claim_ids"`
	Decision     string   `json:"decision"`
	Rationale    string   `json:"rationale"`
	EvidenceRefs []string `json:"evidence_refs"`
}

func projectVerifierClaims(
	task ExecutableTask,
	input TaskExecutionInput,
	result agentapi.RunResult,
) ([]ClaimCandidate, error) {
	var verification verificationResult
	if err := json.Unmarshal(result.Output, &verification); err != nil {
		return nil, fmt.Errorf("decode verification result: %w", err)
	}
	goalID := ""
	if len(task.GoalIDs) > 0 {
		goalID = task.GoalIDs[0]
	}
	statements := make(map[string]string, len(input.Evidence))
	for _, unit := range input.Evidence {
		statements[unit.ID] = unit.Content
	}
	claims := make([]ClaimCandidate, 0, len(verification.Verdicts))
	for _, verdict := range verification.Verdicts {
		statement := ""
		for _, id := range verdict.ClaimIDs {
			if text := statements[id]; text != "" {
				statement = text
				break
			}
		}
		if statement == "" {
			statement = strings.TrimSpace(verdict.Rationale)
		}
		if statement == "" {
			continue
		}
		refs := make([]EvidenceRef, 0, len(verdict.EvidenceRefs))
		for _, id := range verdict.EvidenceRefs {
			refs = append(refs, EvidenceRef{EvidenceID: id})
		}
		claims = append(claims, ClaimCandidate{
			GoalID:       goalID,
			Text:         statement,
			Status:       verdictStatus(verdict.Decision),
			EvidenceRefs: refs,
		})
	}
	return claims, nil
}

func verdictStatus(decision string) ClaimStatus {
	switch decision {
	case "supported":
		return ClaimSupported
	case "contradicted", "distinct":
		return ClaimConflicting
	default:
		return ClaimPartial
	}
}
