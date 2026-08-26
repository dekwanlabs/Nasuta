package investigation

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	maxVerifierEvidenceClaims = 8
	maxVerifierVerdicts       = 8

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
	// EvidenceContextBudget bounds evidence text entering verifier and composer
	// model inputs. Zero values use the package defaults.
	EvidenceContextBudget EvidenceContextBudget
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
	if task.Executor == ExecutorVerifier && len(taskEvidenceContext(task, input, e.EvidenceContextBudget).selected) == 0 {
		output, err := marshalUnresolvedVerification("no user-readable evidence was available")
		if err != nil {
			return TaskExecutionResult{}, err
		}
		return TaskExecutionResult{Output: output}, nil
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
	runID, err := childAgentRunID(input.WorkflowRunID, task.ID, input.Attempt)
	if err != nil {
		return agentapi.RunRequest{}, err
	}
	runtimeBudget := task.Budget.Limit
	sharedRuntimeBudget := !isZeroBudget(input.RuntimeBudget)
	if sharedRuntimeBudget {
		runtimeBudget = input.RuntimeBudget
	}
	limits := e.limitsForBudget(runtimeBudget, definition)
	if sharedRuntimeBudget {
		// The investigation ledger accounts cumulative input, total tokens, and
		// cost across all model calls and sibling tasks. Keeping those same values
		// on the child Run would turn the shared admission grant into a private
		// per-agent quota and stop a valid multi-turn investigator too early.
		limits.MaxInputTokens = 0
		limits.MaxTotalTokens = 0
		limits.MaxCostMicros = 0
	}
	if task.Budget.Limit.InputTokens > 0 &&
		(runtimeBudget.InputTokens == 0 || task.Budget.Limit.InputTokens < runtimeBudget.InputTokens) {
		limits.MaxContextTokens = contextLimitForInput(task.Budget.Limit.InputTokens, definition)
	}
	request := agentapi.RunRequest{
		RunID:          runID,
		Agent:          agentapi.DefinitionRef{ID: definition.ID, Version: definition.Version},
		DefinitionHash: definition.ContentHash,
		Input:          rawInput,
		Permissions:    definition.Permissions,
		ToolScope: agentapi.ToolScope{
			RestrictVisible: true,
			VisibleToolIDs:  definition.Tools.VisibleToolIDs,
		},
		Policy: agentapi.RunPolicy{
			EvidenceRequired: true,
			OutputMode:       agentOutputMode(task.Executor),
		},
		Limits: limits,
		Actor:  input.Actor,
		Correlation: agentapi.Correlation{
			ParentRunID:   input.ParentRunID,
			WorkflowRunID: input.WorkflowRunID,
			NodeID:        task.ID,
		},
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

func agentOutputMode(executor ExecutorType) agentapi.RunOutputMode {
	if executor == ExecutorInvestigator {
		return agentapi.RunOutputEvidenceWorker
	}
	return agentapi.RunOutputWorkflowNode
}

func childAgentRunID(workflowRunID, taskID string, attempt int) (string, error) {
	if workflowRunID == "" {
		return "", fmt.Errorf("workflow run ID is required for task %q", taskID)
	}
	if taskID == "" {
		return "", fmt.Errorf("task ID is required for workflow %q", workflowRunID)
	}
	if attempt <= 0 {
		return "", fmt.Errorf("task %q attempt must be positive", taskID)
	}
	key := workflowRunID + "\x00" + taskID + "\x00" + strconv.Itoa(attempt)
	return "run_inv_" + platform.UUIDFromString(key), nil
}

func (e AgentRuntimeTaskExecutor) buildInput(task ExecutableTask, input TaskExecutionInput) (json.RawMessage, error) {
	switch task.Executor {
	case ExecutorInvestigator:
		return investigatorInput(task)
	case ExecutorVerifier:
		return verifierInput(task, input, e.EvidenceContextBudget)
	case ExecutorComposer:
		return composerInput(task, input, e.EvidenceContextBudget)
	default:
		return nil, fmt.Errorf("executor %q has no input builder", task.Executor)
	}
}

func (e AgentRuntimeTaskExecutor) limits(task ExecutableTask, definition agentapi.Definition) agentapi.RunLimits {
	return e.limitsForBudget(task.Budget.Limit, definition)
}

func (e AgentRuntimeTaskExecutor) limitsForBudget(
	budget BudgetVector,
	definition agentapi.Definition,
) agentapi.RunLimits {
	limits := agentapi.RunLimits{
		MaxSteps:     definition.Budget.MaxSteps,
		MaxToolCalls: definition.Budget.MaxToolCalls,
	}
	if taskToolCalls := int64(budget.ToolCalls); taskToolCalls > 0 &&
		limits.MaxToolCalls > 0 && taskToolCalls < limits.MaxToolCalls {
		limits.MaxToolCalls = taskToolCalls
	}
	if budget.InputTokens > 0 {
		limits.MaxInputTokens = budget.InputTokens
		limits.MaxContextTokens = contextLimitForInput(budget.InputTokens, definition)
	}
	if totalTokens := tokenTotal(budget); totalTokens > 0 {
		// The runtime exposes one total-token limit. The ledger still enforces
		// input and output components independently; this prevents an
		// output-only projection from accidentally ignoring input consumption.
		limits.MaxTotalTokens = totalTokens
	}
	if budget.CostMicros > 0 {
		limits.MaxCostMicros = budget.CostMicros
	}
	if budget.Duration > 0 {
		limits.Deadline = time.Now().UTC().Add(budget.Duration)
	}
	return limits
}

func contextLimitForInput(inputTokens int64, definition agentapi.Definition) int64 {
	if inputTokens <= 0 {
		return 0
	}
	outputTokens := int64(definition.Model.MaxOutputTokens)
	if outputTokens < 0 || inputTokens > int64(^uint64(0)>>1)-outputTokens {
		return int64(definition.Budget.ContextTokens)
	}
	base := inputTokens + outputTokens
	limit := base + 1024
	for iteration := 0; iteration < 3; iteration++ {
		safety := maxInt64(limit/20, 1024)
		limit = base + safety
	}
	safety := maxInt64(limit/20, 1024)
	if base > int64(^uint64(0)>>1)-safety {
		return int64(definition.Budget.ContextTokens)
	}
	if definition.Budget.ContextTokens > 0 && limit > int64(definition.Budget.ContextTokens) {
		return int64(definition.Budget.ContextTokens)
	}
	return limit
}

func composerTaskContract(task ExecutableTask) InvestigationContract {
	goals := make([]EvidenceGoal, 0, len(task.EvidenceGoalIDs))
	for _, goalID := range task.EvidenceGoalIDs {
		goals = append(goals, EvidenceGoal{ID: goalID, Kind: goalID, Description: task.Objective, Facets: []string{goalID}, Required: true})
	}
	return InvestigationContract{
		ID: task.ID, Version: InvestigationContractVersion,
		Question: task.Objective, EvidenceGoals: goals,
	}
}

func composerInput(task ExecutableTask, input TaskExecutionInput, budget EvidenceContextBudget) (json.RawMessage, error) {
	contract := composerTaskContract(task)
	bundle, err := marshalVerifiedBundleWithBudget(contract, InvestigationReport{
		Evidence: append([]EvidenceUnit(nil), input.Evidence...),
		Claims:   append([]VerifiedClaim(nil), input.Claims...),
	}, budget)
	if err != nil {
		return nil, fmt.Errorf("build composer verified bundle: %w", err)
	}
	return bundle, nil
}

func taskEvidenceContext(task ExecutableTask, input TaskExecutionInput, budget EvidenceContextBudget) evidenceContextResult {
	goals := append([]EvidenceGoal(nil), task.EvidenceGoals...)
	if len(goals) == 0 {
		for _, goalID := range task.EvidenceGoalIDs {
			goals = append(goals, EvidenceGoal{
				ID: goalID, Kind: goalID, Facets: []string{goalID}, Required: true,
			})
		}
	}
	return buildEvidenceContext(input.Evidence, input.Claims, InvestigationContract{
		EvidenceGoals: goals,
	}, budget)
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
	Facets          []string `json:"facets"`
	Required        bool     `json:"required"`
	Sources         []string `json:"sources"`
	RequiredSources []string `json:"required_sources,omitempty"`
	Freshness       string   `json:"freshness"`
	MinimumCoverage int      `json:"minimum_coverage"`
	HighRisk        bool     `json:"high_risk,omitempty"`
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
		for _, goalID := range task.EvidenceGoalIDs {
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
	facet := goal.Kind
	if len(goal.Facets) > 0 {
		facet = goal.Facets[0]
	}
	return contractEvidenceGoal{
		ID: goal.ID, Facet: facet, Facets: append([]string(nil), goal.Facets...), Required: goal.Required,
		Sources:         evidenceSources(goal.Sources),
		RequiredSources: evidenceSources(goal.RequiredSources),
		Freshness:       string(goal.Freshness), MinimumCoverage: goal.MinimumCoverage,
		HighRisk: goal.HighRisk,
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

func verifierInput(task ExecutableTask, input TaskExecutionInput, budget EvidenceContextBudget) (json.RawMessage, error) {
	context := taskEvidenceContext(task, input, budget)
	claimLimit := len(context.selected)
	if claimLimit > maxVerifierEvidenceClaims {
		claimLimit = maxVerifierEvidenceClaims
	}
	claims := make([]verificationClaim, 0, claimLimit)
	evidenceRefs := make([]string, 0, claimLimit)
	for _, unit := range context.selected[:claimLimit] {
		statement := context.lookup[unit.ID].Summary
		claims = append(claims, verificationClaim{
			ID:        unit.ID,
			Statement: statement,
			Citations: []string{unit.ID},
		})
		evidenceRefs = append(evidenceRefs, unit.ID)
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
	totalTokens := result.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = result.Usage.InputTokens + result.Usage.OutputTokens
	}
	out := TaskExecutionResult{
		Output: result.Output,
		Usage: BudgetVector{
			InputTokens:  result.Usage.InputTokens,
			OutputTokens: result.Usage.OutputTokens,
			TotalTokens:  totalTokens,
			ToolCalls:    result.Evidence.ToolCallCount,
			CostMicros:   result.Usage.CostMicros,
		},
	}
	if task.Executor == ExecutorInvestigator {
		out.EvidenceCandidates = projectInvestigatorObservations(result)
	}
	// A failed Single-Agent Run can have consumed model/tool budget before the
	// failure was reported (for example when the shared Run gate rejects the
	// next turn). Return that usage to the Scheduler so it is settled instead
	// of silently disappearing from the parent ledger.
	if result.Error != nil {
		code := mapAgentRunFailureCode(result.Error.Code, result.Status)
		retryable := result.Error.Retryable
		if strictInvestigatorReportTask(task) && strings.EqualFold(strings.TrimSpace(result.Error.Code), "invalid_output") {
			code = FailureSchema
			retryable = true
		}
		out.Failure = &RunFailure{
			Code:      code,
			Message:   result.Error.Message,
			Retryable: retryable,
			Stage:     string(StageExecution),
			TaskID:    task.ID,
		}
		return out, nil
	}
	if result.Status != agentapi.RunSucceeded {
		out.Failure = &RunFailure{
			Code:    mapAgentRunFailureCode("", result.Status),
			Message: fmt.Sprintf("agent run %q has status %q", result.RunID, result.Status),
			Stage:   string(StageExecution),
			TaskID:  task.ID,
		}
		return out, nil
	}
	switch task.Executor {
	case ExecutorInvestigator:
		if len(out.EvidenceCandidates) == 0 && strictInvestigatorReportTask(task) {
			if err := validateInvestigationReportOutput(result.Output); err != nil {
				out.Failure = &RunFailure{Code: FailureSchema, Message: err.Error(), Stage: string(StageExecution), TaskID: task.ID, Retryable: true}
				return out, nil
			}
		}
		if len(out.EvidenceCandidates) == 0 {
			evidence, err := projectInvestigatorEvidence(result)
			if err != nil {
				// Evidence-worker output is supplemental: authoritative tool
				// observations have already been projected above. A worker may
				// stop after tools and leave an incomplete report; do not turn
				// that absence into a verifier dependency failure.
				if !strictInvestigatorReportTask(task) {
					out.Output = emptyInvestigationReport(task)
				} else {
					out.Failure = &RunFailure{Code: FailureSchema, Message: err.Error(), Stage: string(StageExecution), TaskID: task.ID, Retryable: true}
					return out, nil
				}
			} else {
				out.EvidenceCandidates = evidence
			}
		}
		if len(out.Output) == 0 && len(out.EvidenceCandidates) == 0 && len(strings.TrimSpace(string(result.Output))) == 0 {
			out.Output = emptyInvestigationReport(task)
		}
		out.Discoveries = projectInvestigatorDiscoveries(result.Output)
	case ExecutorVerifier:
		claims, err := projectVerifierClaims(task, input, result, e.EvidenceContextBudget)
		if err != nil {
			out.Failure = &RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification), TaskID: task.ID}
			return out, nil
		}
		out.Claims = claims
	case ExecutorComposer:
		// The synthesized answer is carried in Output.
	default:
		out.Failure = &RunFailure{Code: FailureExecution, Message: fmt.Sprintf("executor %q has no projection", task.Executor), Stage: string(StageExecution), TaskID: task.ID}
		return out, nil
	}
	return out, nil
}

func projectInvestigatorObservations(result agentapi.RunResult) []EvidenceCandidate {
	if len(result.EvidenceObservations) == 0 {
		return nil
	}
	candidates := make([]EvidenceCandidate, 0, len(result.EvidenceObservations))
	for _, observation := range result.EvidenceObservations {
		if !isReadableEvidenceContent(observation.Summary) ||
			strings.TrimSpace(observation.SourceKind) == "" ||
			strings.TrimSpace(observation.Target) == "" {
			continue
		}
		candidates = append(candidates, EvidenceCandidate{
			SourceKind:    observation.SourceKind,
			Target:        observation.Target,
			Section:       observation.Section,
			Content:       observation.Summary,
			ContentHash:   observation.ContentHash,
			Facets:        append([]string(nil), observation.Facets...),
			TrustTier:     observation.TrustTier,
			EvidenceClass: observation.EvidenceClass,
			Version:       observation.Version,
			TimeRange:     observation.TimeRange,
		})
	}
	return candidates
}

func strictInvestigatorReportTask(task ExecutableTask) bool {
	if task.Executor != ExecutorInvestigator {
		return false
	}
	return task.Capability == "knowledge.docs.verify" || task.Template.ID == "proposal.docs.verify"
}

func mapAgentRunFailureCode(code string, status agentapi.RunStatus) FailureCode {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "budget_exceeded", "budget_exhausted", "limit_exceeded", "run_limit_exceeded":
		return FailureBudget
	case "timeout", "deadline_exceeded":
		return FailureTimeout
	case "cancelled", "canceled":
		return FailureCancelled
	case "reasoning_truncated", "max_continue_rounds":
		return FailureReasoning
	case "tool_unavailable":
		return FailureToolUnavailable
	case "permission_denied":
		return FailurePermission
	}
	switch status {
	case agentapi.RunCancelled:
		return FailureCancelled
	case agentapi.RunFailed:
		return FailureExecution
	default:
		return FailureExecution
	}
}

type investigationReportOutput struct {
	Focus                   string                    `json:"focus"`
	Summary                 string                    `json:"summary"`
	Findings                []investigationFinding    `json:"findings"`
	Gaps                    []string                  `json:"gaps"`
	CoveredEvidenceGoals    []string                  `json:"covered_evidence_goals"`
	UnresolvedEvidenceGoals []string                  `json:"unresolved_evidence_goals"`
	DiscoveredEntities      []string                  `json:"discovered_entities"`
	DiscoveredDependencies  []investigationDependency `json:"discovered_dependencies"`
}

type investigationFinding struct {
	Claim           string                  `json:"claim"`
	EvidenceGoalIDs []string                `json:"evidence_goal_ids"`
	Evidence        []investigationEvidence `json:"evidence"`
	Confidence      float64                 `json:"confidence"`
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

func validateInvestigationReportOutput(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("investigation.report output is empty")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("decode investigation.report: %w", err)
	}
	if fields == nil {
		return fmt.Errorf("investigation.report must be a JSON object")
	}
	required := []string{"focus", "summary", "findings", "gaps", "covered_evidence_goals", "unresolved_evidence_goals"}
	for _, name := range required {
		value, ok := fields[name]
		if !ok || string(value) == "null" {
			return fmt.Errorf("investigation.report is missing required field %q", name)
		}
	}
	var report investigationReportOutput
	if err := json.Unmarshal(raw, &report); err != nil {
		return fmt.Errorf("decode investigation.report: %w", err)
	}
	switch report.Focus {
	case "code", "runtime", "docs", "web", "memory":
	default:
		return fmt.Errorf("investigation.report has invalid focus %q", report.Focus)
	}
	if strings.TrimSpace(report.Summary) == "" {
		return fmt.Errorf("investigation.report summary is empty")
	}
	for index, finding := range report.Findings {
		if strings.TrimSpace(finding.Claim) == "" || len(finding.EvidenceGoalIDs) == 0 || len(finding.Evidence) == 0 || finding.Confidence < 0 || finding.Confidence > 1 {
			return fmt.Errorf("investigation.report finding %d is invalid", index)
		}
		for evidenceIndex, evidence := range finding.Evidence {
			if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Reference) == "" || strings.TrimSpace(evidence.Summary) == "" {
				return fmt.Errorf("investigation.report finding %d evidence %d is invalid", index, evidenceIndex)
			}
		}
	}
	return nil
}

// projectInvestigatorEvidence reconstructs content-bearing evidence candidates
// from a raw RunResult. RunResult.EvidenceUnits carries identity but not content,
// so content is recovered from the investigation.report output.
func projectInvestigatorEvidence(result agentapi.RunResult) ([]EvidenceCandidate, error) {
	if len(strings.TrimSpace(string(result.Output))) == 0 {
		return nil, nil
	}
	var report investigationReportOutput
	if err := json.Unmarshal(result.Output, &report); err != nil {
		return nil, fmt.Errorf("decode investigation report: %w", err)
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
		if !isReadableEvidenceContent(content) {
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
				if !isReadableEvidenceContent(evidence.Summary) {
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

func emptyInvestigationReport(task ExecutableTask) json.RawMessage {
	focus := "code"
	switch task.Capability {
	case "knowledge.service.trace", "knowledge.runtime.observe":
		focus = "runtime"
	case "knowledge.docs.verify":
		focus = "docs"
	case "knowledge.web.research":
		focus = "web"
	case "knowledge.memory.recall":
		focus = "memory"
	}
	output, _ := json.Marshal(investigationReportOutput{
		Focus: focus, Summary: "No user-readable evidence was collected.",
		Findings: []investigationFinding{}, Gaps: []string{"investigation stopped before a complete report was produced"},
		CoveredEvidenceGoals: []string{}, UnresolvedEvidenceGoals: append([]string(nil), task.EvidenceGoalIDs...),
		DiscoveredEntities: []string{}, DiscoveredDependencies: []investigationDependency{},
	})
	return output
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

func marshalUnresolvedVerification(reason string) (json.RawMessage, error) {
	return json.Marshal(verificationResult{
		Summary:       "Verification could not establish a claim.",
		Verdicts:      []verificationVerdict{},
		Uncertainties: []string{strings.TrimSpace(reason)},
	})
}

func projectVerifierClaims(
	task ExecutableTask,
	input TaskExecutionInput,
	result agentapi.RunResult,
	budget EvidenceContextBudget,
) ([]ClaimCandidate, error) {
	var verification verificationResult
	if err := json.Unmarshal(result.Output, &verification); err != nil {
		return nil, fmt.Errorf("decode verification result: %w", err)
	}
	goalID := ""
	if len(task.EvidenceGoalIDs) > 0 {
		goalID = task.EvidenceGoalIDs[0]
	}
	context := taskEvidenceContext(task, input, budget)
	statements := make(map[string]string, len(context.lookup))
	for id, evidence := range context.lookup {
		statements[id] = evidence.Summary
	}
	availableEvidence := make(map[string]struct{}, len(context.lookup))
	for id := range context.lookup {
		availableEvidence[id] = struct{}{}
	}
	verdictLimit := len(task.EvidenceGoalIDs)
	if verdictLimit <= 0 {
		verdictLimit = 1
	}
	if verdictLimit > maxVerifierVerdicts {
		verdictLimit = maxVerifierVerdicts
	}
	claims := make([]ClaimCandidate, 0, verdictLimit)
	for _, verdict := range verification.Verdicts {
		if len(claims) >= verdictLimit {
			break
		}
		statement := ""
		for _, rawID := range verdict.ClaimIDs {
			id := strings.TrimSpace(rawID)
			if text := statements[id]; text != "" {
				statement = text
				break
			}
		}
		if !isUserReadableClaimText(statement) {
			statement = ""
		}
		if statement == "" {
			rationale := strings.TrimSpace(verdict.Rationale)
			if isUserReadableClaimText(rationale) {
				statement = rationale
			}
		}
		if statement == "" {
			// A verifier may cite identity-only evidence or echo an internal
			// identifier. Neither is a user-readable claim. Leave the goal
			// unresolved instead of promoting the identifier into a finding.
			continue
		}
		refs := verifierEvidenceRefs(verdict, availableEvidence)
		if len(refs) == 0 {
			// Never hand an ungrounded model claim to the claim ledger.
			continue
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

func verifierEvidenceRefs(verdict verificationVerdict, available map[string]struct{}) []EvidenceRef {
	refs := make([]EvidenceRef, 0, len(verdict.EvidenceRefs))
	seen := make(map[string]struct{}, len(verdict.EvidenceRefs))
	for _, rawID := range verdict.EvidenceRefs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, ok := available[id]; !ok {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		refs = append(refs, EvidenceRef{EvidenceID: id})
	}
	if len(refs) > 0 {
		return refs
	}
	// Verifier requests use canonical evidence IDs as claim IDs. Recover that
	// server-owned binding when a provider returns an empty evidence_refs array.
	for _, rawID := range verdict.ClaimIDs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			continue
		}
		if _, ok := available[id]; !ok {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		refs = append(refs, EvidenceRef{EvidenceID: id})
	}
	return refs
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
