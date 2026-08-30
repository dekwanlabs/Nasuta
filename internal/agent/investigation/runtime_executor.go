package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	canonicalevidence "github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
)

const (
	maxVerifierEvidenceClaims = 8
	maxVerifierVerdicts       = 8

	// This margin covers provider/message framing around the deterministic
	// request estimate. It is deliberately small: the evidence context itself
	// is bounded separately by EvidenceContextBudget.
	verifierInputSafetyTokens int64 = 256

	// These floors protect one complete role response during admission. The shared
	// Run ledger still remains the cumulative hard limit for all actual usage.
	investigatorMinimumOutputTokens int64 = 8192
	verifierMinimumOutputTokens     int64 = 4096
	composerMinimumOutputTokens     int64 = 8192

	// Synthetic verifier evidence is only a shape used for admission estimation.
	// Keep materialization bounded so a malformed/huge context setting cannot
	// force an unbounded allocation; the omitted virtual tokens are added back to
	// the estimate below.
	maxSyntheticTokenTextTokens = 8192

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

// MinimumBudget returns the protected grant required for one usable model
// response. Explicit task limits may narrow a role floor, but never expand it.
//
// Investigator and Composer keep their historical output-only admission floor.
// A Verifier is different: it is mandatory downstream work, so its admission
// protects the first request's input, output, and aggregate token budget before
// any Investigator is started.
func (e AgentRuntimeTaskExecutor) MinimumBudget(task ExecutableTask) (BudgetVector, error) {
	if !isAgentExecutor(task.Executor) {
		return BudgetVector{}, nil
	}
	ref, err := e.definitionRefForTask(task)
	if err != nil {
		return BudgetVector{}, err
	}
	if e.Definitions == nil {
		return BudgetVector{}, fmt.Errorf("agent definition resolver is required")
	}
	definition, err := e.Definitions.Resolve(ref)
	if err != nil {
		return BudgetVector{}, fmt.Errorf("resolve agent definition %q: %w", ref.ID, err)
	}
	minimumOutput := investigatorMinimumOutputTokens
	switch task.Executor {
	case ExecutorVerifier:
		minimumOutput = verifierMinimumOutputTokens
	case ExecutorComposer:
		minimumOutput = composerMinimumOutputTokens
	}
	if definition.Model.MaxOutputTokens > 0 && int64(definition.Model.MaxOutputTokens) < minimumOutput {
		minimumOutput = int64(definition.Model.MaxOutputTokens)
	}
	if task.Budget.Limit.OutputTokens > 0 && task.Budget.Limit.OutputTokens < minimumOutput {
		minimumOutput = task.Budget.Limit.OutputTokens
	}
	if minimumOutput < 0 {
		minimumOutput = 0
	}

	grant := BudgetVector{OutputTokens: minimumOutput}
	if task.Executor != ExecutorVerifier {
		if minimumOutput <= 0 {
			return BudgetVector{}, nil
		}
		return grant, nil
	}

	minimumInput, err := e.verifierMinimumInputTokens(task, definition)
	if err != nil {
		return BudgetVector{}, err
	}
	if task.Budget.Limit.InputTokens > 0 {
		minimumInput = minInt64(minimumInput, task.Budget.Limit.InputTokens)
	}

	// TotalTokens is an aggregate reservation, not a second copy of input and
	// output. Keep the three dimensions self-consistent even when a task-level
	// total cap is tighter than the two component floors. We narrow output first
	// only after preserving as much input as that explicit cap allows.
	if task.Budget.Limit.TotalTokens > 0 {
		minimumInput = minInt64(minimumInput, task.Budget.Limit.TotalTokens)
		remaining := task.Budget.Limit.TotalTokens - minimumInput
		if remaining < 0 {
			remaining = 0
		}
		minimumOutput = minInt64(minimumOutput, remaining)
	}
	// Keep the three token dimensions self-consistent even for a pathological
	// configuration that drives the estimated input floor near int64's ceiling.
	// Prefer preserving the usable output floor, then narrow input by the tiny
	// amount that cannot be represented in TotalTokens.
	maxTokenBudget := int64(^uint64(0) >> 1)
	if minimumInput > maxTokenBudget-minimumOutput {
		minimumInput = maxTokenBudget - minimumOutput
	}
	total := minimumInput + minimumOutput
	if minimumInput == 0 && minimumOutput == 0 {
		return BudgetVector{}, nil
	}
	grant.InputTokens = minimumInput
	grant.OutputTokens = minimumOutput
	grant.TotalTokens = total
	return grant, nil
}

// verifierMinimumInputTokens estimates the first physical Verifier request
// using the same input shape as verifierInput plus the runtime's system/user
// envelope. Evidence bodies are represented by bounded synthetic summaries so
// admission is conservative without reserving the entire evidence pool.
func (e AgentRuntimeTaskExecutor) verifierMinimumInputTokens(task ExecutableTask, definition agentapi.Definition) (int64, error) {
	maxSummaryTokens, maxContextTokens, _ := e.EvidenceContextBudget.effective()
	claimCount := maxVerifierEvidenceClaims
	if maxContextTokens > 0 && maxContextTokens < int64(claimCount) {
		claimCount = int(maxContextTokens)
	}
	if claimCount <= 0 || maxSummaryTokens <= 0 {
		claimCount = 0
	}

	claims := make([]verificationClaim, 0, claimCount)
	evidenceRefs := make([]string, 0, claimCount)
	remainingContext := maxContextTokens
	var virtualEvidenceTokens int64
	for index := 0; index < claimCount; index++ {
		statementTokens := int64(maxSummaryTokens)
		if maxContextTokens > 0 {
			// Leave at least one estimated token for every remaining claim. This
			// mirrors buildEvidenceContext, which can admit several short summaries
			// even when the context budget is smaller than 8*MaxSummaryTokens.
			remainingClaims := int64(claimCount - index - 1)
			available := remainingContext - remainingClaims
			if available <= 0 {
				break
			}
			if statementTokens > available {
				statementTokens = available
			}
		}
		materializedTokens := minInt64(statementTokens, maxSyntheticTokenTextTokens)
		statement := syntheticTokenText(int(materializedTokens))
		if statement == "" {
			break
		}
		statementCost := int64(tooloutput.EstimateTokens(statement))
		if statement == "" || statementCost <= 0 {
			break
		}
		claimID := syntheticEvidenceID(index)
		claims = append(claims, verificationClaim{
			ID:        claimID,
			Statement: statement,
			Citations: []string{claimID},
		})
		evidenceRefs = append(evidenceRefs, claimID)
		if statementTokens > statementCost {
			virtualEvidenceTokens = saturatingAdd(virtualEvidenceTokens, statementTokens-statementCost)
		}
		if maxContextTokens > 0 {
			// Consume the requested worst-case context, not only the bounded
			// materialized sample. This keeps the estimate conservative for very
			// large MaxSummaryTokens values without allocating that much text.
			remainingContext -= statementTokens
		}
	}

	lookup := make(map[string]verifierEvidenceView, len(claims))
	for _, claim := range claims {
		lookup[claim.ID] = verifierEvidenceView{
			Kind: "runbook", Reference: claim.ID, Summary: claim.Statement,
		}
	}
	payload, err := json.Marshal(struct {
		Question         string                         `json:"question"`
		DecisionQuestion string                         `json:"decision_question"`
		Claims           []verificationClaim            `json:"claims"`
		Conflicts        []any                          `json:"conflicts"`
		EvidenceRefs     []string                       `json:"evidence_refs"`
		EvidenceLookup   map[string]verifierEvidenceView `json:"evidence_lookup,omitempty"`
		Reasons          []string                       `json:"reasons"`
	}{
		Question:         task.Objective,
		DecisionQuestion: task.Objective,
		Claims:           claims,
		Conflicts:        []any{},
		EvidenceRefs:     evidenceRefs,
		EvidenceLookup:   lookup,
		Reasons:          []string{"verify evidence-backed claims"},
	})
	if err != nil {
		return 0, fmt.Errorf("encode verifier input for budget admission: %w", err)
	}
	question := prompts.MustRender(prompts.AgentRuntimeExecuteInput, struct {
		SchemaID      string
		SchemaVersion int64
		Input         string
	}{
		SchemaID:      definition.OutputSchema.ID,
		SchemaVersion: definition.OutputSchema.Version,
		Input:         string(payload),
	})
	inputTokens := int64(tooloutput.EstimateTokens(definition.Prompt.System) + 4)
	inputTokens = saturatingAdd(inputTokens, int64(tooloutput.EstimateTokens(question)+4))
	// The JSON contains bounded samples above. Add the virtual tail that would
	// have occupied the same evidence statements, plus a one-token rounding
	// margin per claim, so the estimate never becomes optimistic for huge
	// configured summary limits.
	inputTokens = saturatingAdd(inputTokens, virtualEvidenceTokens)
	if virtualEvidenceTokens > 0 {
		inputTokens = saturatingAdd(inputTokens, int64(len(claims)+1))
	}
	inputTokens = saturatingAdd(inputTokens, verifierInputSafetyTokens)
	if inputTokens <= 0 {
		return 1, nil
	}
	return inputTokens, nil
}

func syntheticEvidenceID(index int) string {
	// Match the short canonical handle length so budget estimates stay honest
	// about how much identifier text actually reaches the model.
	return fmt.Sprintf("evidence_%07x", index+1)
}

func syntheticTokenText(tokens int) string {
	if tokens <= 0 {
		return ""
	}
	if tokens > maxSyntheticTokenTextTokens {
		tokens = maxSyntheticTokenTextTokens
	}
	// Two ASCII characters are one conservative estimated token under the
	// repository estimator. Compute the initial size without tokens*30 so a
	// hostile integer setting cannot overflow before the materialization cap.
	chars := (tokens/11)*30 + (tokens%11)*30/11
	if chars < 2 {
		chars = 2
	}
	text := strings.Repeat("x", chars)
	for tooloutput.EstimateTokens(text) > tokens {
		text = text[:len(text)-1]
	}
	for tooloutput.EstimateTokens(text) < tokens {
		text += "x"
	}
	return text
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
	if task.Executor == ExecutorVerifier {
		selected := len(taskEvidenceContext(task, input, e.EvidenceContextBudget).selected)
		collected := len(collectVerifierClaims(task, input, e.EvidenceContextBudget))
		log.InfofCtx(ctx, "[investigation] verifier admission task=%s selected_evidence=%d collected_claims=%d",
			task.ID, selected, collected)
		if selected == 0 {
			output, err := marshalUnresolvedVerification("no user-readable evidence was available")
			if err != nil {
				return TaskExecutionResult{}, err
			}
			log.WarnfCtx(ctx, "[investigation] verifier skipped task=%s reason=no_user_readable_evidence", task.ID)
			return TaskExecutionResult{Output: output}, nil
		}
		if collected == 0 {
			output, err := marshalUnresolvedVerification("no user-readable claims were available")
			if err != nil {
				return TaskExecutionResult{}, err
			}
			log.WarnfCtx(ctx, "[investigation] verifier skipped task=%s reason=no_user_readable_claims", task.ID)
			return TaskExecutionResult{Output: output}, nil
		}
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
	result, runErr := e.Runtime.Run(ctx, request)
	if runErr != nil {
		// Some runtimes return a durable partial RunResult together with a
		// transport/provider error. Project it before surfacing the error so
		// evidence collected before the failure is not discarded at this
		// boundary.
		budgetFailure := errors.Is(runErr, agentapi.ErrBudgetExceeded) ||
			(result.Error != nil && isBudgetRunError(result.Error.Code))
		if result.Error == nil || budgetFailure {
			code := "runtime_error"
			if budgetFailure {
				code = "budget_exhausted"
			}
			message := runErr.Error()
			if !errors.Is(runErr, agentapi.ErrBudgetExceeded) && result.Error != nil && strings.TrimSpace(result.Error.Message) != "" {
				message = result.Error.Message
			}
			result.Error = &agentapi.RunError{
				Code:      code,
				Message:   message,
				Retryable: false,
			}
		}
		projected, projectErr := e.project(task, input, result)
		if projectErr != nil {
			return projected, errors.Join(runErr, projectErr)
		}
		return projected, nil
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
	if sharedRuntimeBudget && task.Executor != ExecutorVerifier {
		// The investigation ledger accounts cumulative input, total tokens, and
		// cost across all model calls and sibling tasks. Keeping those same values
		// on a normal child Run would turn shared capacity into a private quota and
		// stop a valid multi-turn Investigator too early. Verifier is intentionally
		// excluded: its RuntimeBudget is the role slice protected before execution,
		// so its child Run must honor that input/total ceiling too.
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
	if task.Executor == ExecutorInvestigator {
		if blocks := investigatorSeedBlocks(input.SeedMaterial); len(blocks) > 0 {
			request.Context = blocks
			request.Policy.EvidenceSeeded = true
		}
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
		return investigatorInput(task, input.SeedMaterial)
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
	return runLimitsForBudget(budget, definition)
}

func runLimitsForBudget(
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

func investigatorInput(task ExecutableTask, seed []agentapi.ContextBlock) (json.RawMessage, error) {
	entities := projectInvestigatorEntities(task)
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
	context := map[string]any{}
	if blocks := investigatorSeedBlocks(seed); len(blocks) > 0 {
		context["seed_material"] = blocks
	}
	return json.Marshal(struct {
		TaskID           string                  `json:"task_id"`
		Objective        string                  `json:"objective"`
		Capability       string                  `json:"capability,omitempty"`
		Entities         []contractEntity        `json:"entities"`
		IdentityBindings []EntityIdentityBinding `json:"identity_bindings,omitempty"`
		EvidenceGoals    []contractEvidenceGoal  `json:"evidence_goals"`
		InputRefs        []agentapi.EvidenceRef  `json:"input_refs,omitempty"`
		Context          map[string]any          `json:"context"`
	}{
		TaskID: task.ID, Objective: task.Objective, Capability: task.Capability,
		Entities: entities, IdentityBindings: cloneEntityIdentityBindings(task.IdentityBindings),
		EvidenceGoals: goals, InputRefs: refs, Context: context,
	})
}

// investigatorSeedBlocks keeps the shared retrieved prose and drops ledger
// units so the task.contract schema stays valid. Identity stays on input_refs.
func investigatorSeedBlocks(blocks []agentapi.ContextBlock) []agentapi.ContextBlock {
	if len(blocks) == 0 {
		return nil
	}
	out := make([]agentapi.ContextBlock, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block.Content) == "" {
			continue
		}
		source := strings.TrimSpace(block.Source)
		if source == "" {
			source = "qa.evidence"
		}
		title := strings.TrimSpace(block.Title)
		if title == "" {
			title = "QA Evidence"
		}
		sum := sha256.Sum256([]byte(block.Content))
		out = append(out, agentapi.ContextBlock{
			Source: source, Title: title, Content: block.Content,
			Complete: block.Complete, ContentHash: hex.EncodeToString(sum[:]),
		})
		if len(out) == 20 {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func projectInvestigatorEntities(task ExecutableTask) []contractEntity {
	if len(task.EntityDetails) > 0 {
		out := make([]contractEntity, 0, len(task.EntityDetails))
		for _, entity := range task.EntityDetails {
			out = append(out, contractEntity{
				ID: entity.ID, Label: entity.Label, Role: entity.Role,
				Aliases: append([]string(nil), entity.Aliases...),
			})
		}
		return out
	}
	out := make([]contractEntity, 0, len(task.Entities))
	for _, id := range task.Entities {
		out = append(out, contractEntity{ID: id})
	}
	return out
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

type verifierEvidenceView struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

func verifierInput(task ExecutableTask, input TaskExecutionInput, budget EvidenceContextBudget) (json.RawMessage, error) {
	context := taskEvidenceContext(task, input, budget)
	claims := collectVerifierClaims(task, input, budget)
	evidenceRefs := make([]string, 0, len(claims))
	seen := make(map[string]struct{}, len(claims))
	for _, claim := range claims {
		for _, id := range claim.Citations {
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			evidenceRefs = append(evidenceRefs, id)
		}
	}
	return json.Marshal(struct {
		Question         string                          `json:"question"`
		DecisionQuestion string                          `json:"decision_question"`
		Claims           []verificationClaim             `json:"claims"`
		Conflicts        []any                           `json:"conflicts"`
		EvidenceRefs     []string                        `json:"evidence_refs"`
		EvidenceLookup   map[string]verifierEvidenceView `json:"evidence_lookup,omitempty"`
		Reasons          []string                        `json:"reasons"`
	}{
		Question:         task.Objective,
		DecisionQuestion: task.Objective,
		Claims:           claims,
		Conflicts:        []any{},
		EvidenceRefs:     evidenceRefs,
		EvidenceLookup:   citedEvidenceLookup(claims, input.Upstream, context),
		Reasons:          []string{"verify evidence-backed claims"},
	})
}

func citedEvidenceLookup(
	claims []verificationClaim,
	upstream map[string]json.RawMessage,
	context evidenceContextResult,
) map[string]verifierEvidenceView {
	if len(claims) == 0 {
		return nil
	}
	findingSummaries := findingEvidenceSummaries(upstream, context.lookup)
	out := make(map[string]verifierEvidenceView, len(claims))
	for _, claim := range claims {
		for _, id := range claim.Citations {
			if _, exists := out[id]; exists {
				continue
			}
			view, ok := context.lookup[id]
			if !ok {
				continue
			}
			summary := strings.TrimSpace(findingSummaries[id])
			if !isUserReadableClaimText(summary) {
				summary = strings.TrimSpace(view.Summary)
			}
			if !isUserReadableClaimText(summary) {
				summary = strings.TrimSpace(claim.Statement)
			}
			if !isUserReadableClaimText(summary) {
				continue
			}
			kind := firstNonEmpty(view.Kind, "runbook")
			reference := firstNonEmpty(view.Reference, id)
			out[id] = verifierEvidenceView{Kind: kind, Reference: reference, Summary: summary}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func findingEvidenceSummaries(
	upstream map[string]json.RawMessage,
	lookup map[string]evidenceSummaryView,
) map[string]string {
	out := make(map[string]string)
	if len(upstream) == 0 || len(lookup) == 0 {
		return out
	}
	idByRef := evidenceIDsByReference(lookup)
	for _, raw := range upstream {
		var report investigationReportOutput
		if err := json.Unmarshal(raw, &report); err != nil {
			continue
		}
		for _, finding := range report.Findings {
			for _, item := range finding.Evidence {
				id, ok := resolveFindingCitation(item, idByRef, lookup)
				if !ok || !isUserReadableClaimText(item.Summary) {
					continue
				}
				if _, exists := out[id]; !exists {
					out[id] = strings.TrimSpace(item.Summary)
				}
			}
		}
	}
	return out
}

func collectVerifierClaims(
	task ExecutableTask,
	input TaskExecutionInput,
	budget EvidenceContextBudget,
) []verificationClaim {
	context := taskEvidenceContext(task, input, budget)
	claims := verifierClaimsFromReports(input.Upstream, context)
	if len(claims) == 0 {
		claims = verifierClaimsFromEvidence(context)
	}
	if len(claims) > maxVerifierEvidenceClaims {
		claims = claims[:maxVerifierEvidenceClaims]
	}
	return claims
}

func verifierClaimsFromReports(
	upstream map[string]json.RawMessage,
	context evidenceContextResult,
) []verificationClaim {
	if len(upstream) == 0 {
		return nil
	}
	idByRef := evidenceIDsByReference(context.lookup)
	claims := make([]verificationClaim, 0)
	for _, raw := range upstream {
		var report investigationReportOutput
		if err := json.Unmarshal(raw, &report); err != nil {
			continue
		}
		for _, finding := range report.Findings {
			statement := strings.TrimSpace(finding.Claim)
			if !isUserReadableClaimText(statement) {
				continue
			}
			citations := make([]string, 0, len(finding.Evidence))
			seen := make(map[string]struct{}, len(finding.Evidence))
			for _, item := range finding.Evidence {
				id, ok := resolveFindingCitation(item, idByRef, context.lookup)
				if !ok {
					continue
				}
				if _, exists := seen[id]; exists {
					continue
				}
				seen[id] = struct{}{}
				citations = append(citations, id)
			}
			if len(citations) == 0 {
				continue
			}
			claims = append(claims, verificationClaim{
				ID:        citations[0],
				Statement: statement,
				Citations: citations,
			})
		}
	}
	return claims
}

func verifierClaimsFromEvidence(context evidenceContextResult) []verificationClaim {
	claims := make([]verificationClaim, 0, len(context.selected))
	for _, unit := range context.selected {
		statement := context.lookup[unit.ID].Summary
		if !isUserReadableClaimText(statement) {
			continue
		}
		claims = append(claims, verificationClaim{
			ID:        unit.ID,
			Statement: statement,
			Citations: []string{unit.ID},
		})
	}
	return claims
}

func evidenceIDsByReference(lookup map[string]evidenceSummaryView) map[string]string {
	ids := make([]string, 0, len(lookup))
	for id := range lookup {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	idByRef := make(map[string]string, len(lookup)*8)
	add := func(key, id string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, exists := idByRef[key]; !exists {
			idByRef[key] = id
		}
	}
	for _, id := range ids {
		view := lookup[id]
		add(id, id)
		kind := firstNonEmpty(view.Kind, identitySourceKind(view.Identity))
		target := firstNonEmpty(view.Reference, identityTarget(view.Identity))
		section := identitySection(view.Identity)
		if view.Identity != nil {
			key := canonicalevidence.Key{
				SourceKind: view.Identity.SourceKind,
				Target:     view.Identity.Target,
				Section:    view.Identity.Section,
				Version:    view.Identity.Version,
				TimeRange:  view.Identity.TimeRange,
			}
			if key.SourceKind != "" && key.Target != "" {
				add(key.Handle(), id)
			}
		}
		addCitationAliases(add, id, kind, target, section)
	}
	return idByRef
}

func addCitationAliases(add func(string, string), id, kind, target, section string) {
	kind = strings.TrimSpace(kind)
	target = strings.TrimSpace(target)
	section = strings.TrimSpace(section)
	if kind != "" && target != "" {
		add(kind+"\x00"+target, id)
	}
	if target != "" {
		add(target, id)
	}
	if target == "" || section == "" {
		return
	}
	captions := []string{target + " (" + section + ")"}
	if display, ok := chunkCaption(section); ok {
		captions = append(captions, target+" ("+display+")")
	}
	for _, caption := range captions {
		if kind != "" {
			add(kind+"\x00"+caption, id)
		}
		add(caption, id)
	}
}

func chunkCaption(section string) (string, bool) {
	section = strings.TrimSpace(section)
	if trimmed, ok := strings.CutPrefix(section, "chunk:"); ok {
		if n := strings.TrimSpace(trimmed); n != "" {
			return "chunk " + n, true
		}
	}
	return "", false
}

func identitySourceKind(identity *agentapi.EvidenceIdentity) string {
	if identity == nil {
		return ""
	}
	return identity.SourceKind
}

func identityTarget(identity *agentapi.EvidenceIdentity) string {
	if identity == nil {
		return ""
	}
	return identity.Target
}

func identitySection(identity *agentapi.EvidenceIdentity) string {
	if identity == nil {
		return ""
	}
	return identity.Section
}

func resolveFindingCitation(
	item investigationEvidence,
	idByRef map[string]string,
	lookup map[string]evidenceSummaryView,
) (string, bool) {
	if id := strings.TrimSpace(item.EvidenceID); id != "" {
		if resolved, ok := idByRef[id]; ok {
			return resolved, true
		}
		if _, ok := lookup[id]; ok {
			return id, true
		}
	}
	if item.Identity != nil {
		key := canonicalevidence.Key{
			SourceKind: item.Identity.SourceKind,
			Target:     item.Identity.Target,
			Section:    item.Identity.Section,
			Version:    item.Identity.Version,
			TimeRange:  item.Identity.TimeRange,
		}
		if key.SourceKind != "" && key.Target != "" {
			if resolved, ok := idByRef[key.Handle()]; ok {
				return resolved, true
			}
		}
		if resolved, ok := lookupCitation(idByRef, item.Kind, item.Identity.Target, item.Identity.Section); ok {
			return resolved, true
		}
	}
	kind := strings.TrimSpace(item.Kind)
	reference := strings.TrimSpace(item.Reference)
	if resolved, ok := lookupCitation(idByRef, kind, reference, ""); ok {
		return resolved, true
	}
	if target, section := splitCaptionReference(reference); section != "" {
		if resolved, ok := lookupCitation(idByRef, kind, target, section); ok {
			return resolved, true
		}
	}
	for id, view := range lookup {
		if reference != "" && view.Reference == reference {
			return id, true
		}
		if kind != "" && view.Kind == kind && reference != "" &&
			(strings.HasSuffix(reference, view.Reference) || strings.HasSuffix(view.Reference, reference)) {
			return id, true
		}
	}
	return "", false
}

func lookupCitation(idByRef map[string]string, kind, target, section string) (string, bool) {
	kind = strings.TrimSpace(kind)
	target = strings.TrimSpace(target)
	section = strings.TrimSpace(section)
	keys := make([]string, 0, 6)
	if kind != "" && target != "" {
		keys = append(keys, kind+"\x00"+target)
		if section != "" {
			keys = append(keys, kind+"\x00"+target+" ("+section+")")
			if display, ok := chunkCaption(section); ok {
				keys = append(keys, kind+"\x00"+target+" ("+display+")")
			}
		}
	}
	if target != "" {
		keys = append(keys, target)
		if section != "" {
			keys = append(keys, target+" ("+section+")")
			if display, ok := chunkCaption(section); ok {
				keys = append(keys, target+" ("+display+")")
			}
		}
	}
	for _, key := range keys {
		if id, ok := idByRef[key]; ok {
			return id, true
		}
	}
	return "", false
}

func splitCaptionReference(reference string) (target, section string) {
	reference = strings.TrimSpace(reference)
	start := strings.LastIndex(reference, " (")
	if start <= 0 || !strings.HasSuffix(reference, ")") {
		return reference, ""
	}
	target = strings.TrimSpace(reference[:start])
	section = strings.TrimSpace(reference[start+2 : len(reference)-1])
	if n, ok := strings.CutPrefix(section, "chunk "); ok {
		section = "chunk:" + strings.TrimSpace(n)
	}
	return target, section
}

func appendEvidenceCandidates(existing, extra []EvidenceCandidate) []EvidenceCandidate {
	if len(extra) == 0 {
		return existing
	}
	seen := make(map[string]struct{}, len(existing))
	for _, candidate := range existing {
		seen[candidate.SourceKind+"\x00"+candidate.Target+"\x00"+candidate.Section] = struct{}{}
	}
	out := append([]EvidenceCandidate(nil), existing...)
	for _, candidate := range extra {
		key := candidate.SourceKind + "\x00" + candidate.Target + "\x00" + candidate.Section
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, candidate)
	}
	return out
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
		out.Discoveries = projectInvestigatorDiscoveries(result.Output)
		if evidence, err := projectInvestigatorEvidence(result); err == nil {
			out.EvidenceCandidates = appendEvidenceCandidates(out.EvidenceCandidates, evidence)
		}
		logInvestigatorProjection(task, result.Output, out)
	}
	// A failed Single-Agent Run can have consumed model/tool budget before the
	// failure was reported (for example when the shared Run gate rejects the
	// next turn). Return that usage to the Scheduler so it is settled instead
	// of silently disappearing from the parent ledger. The failure remains
	// authoritative; only content-bearing evidence can make the task partial.
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
		if len(out.EvidenceCandidates) == 0 {
			evidence, err := projectInvestigatorEvidence(result)
			if err == nil {
				out.EvidenceCandidates = evidence
			}
			if len(out.EvidenceCandidates) == 0 {
				// A worker is allowed to stop after tool calls. When neither
				// observations nor a readable report survived the runtime
				// boundary, emit an explicit unavailable report so verification
				// can close the workflow as evidence-insufficient instead of
				// blocking on a missing dependency.
				if err := validateInvestigationReportOutput(result.Output); err != nil {
					out.Output = emptyInvestigationReport(task)
				}
			}
		}
	case ExecutorVerifier:
		claims, err := projectVerifierClaims(task, input, result, e.EvidenceContextBudget)
		if err != nil {
			log.Warnf("[investigation] verifier projection failed task=%s err=%v output=%s",
				task.ID, err, platform.TruncateForLog(string(result.Output), 2000))
			out.Failure = &RunFailure{Code: FailureVerifier, Message: err.Error(), Stage: string(StageVerification), TaskID: task.ID}
			return out, nil
		}
		out.Claims = claims
		logVerifierProjection(task, input, result, claims, e.EvidenceContextBudget)
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

func isBudgetRunError(code string) bool {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "budget_exceeded", "budget_exhausted", "limit_exceeded", "run_limit_exceeded":
		return true
	default:
		return false
	}
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
	EntityIDs       []string                `json:"entity_ids,omitempty"`
	EvidenceGoalIDs []string                `json:"evidence_goal_ids"`
	Evidence        []investigationEvidence `json:"evidence"`
	Confidence      float64                 `json:"confidence"`
}

type investigationEvidence struct {
	Kind       string                     `json:"kind"`
	Reference  string                     `json:"reference"`
	Summary    string                     `json:"summary"`
	EvidenceID string                     `json:"evidence_id,omitempty"`
	Identity   *agentapi.EvidenceIdentity `json:"identity,omitempty"`
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

func logInvestigatorProjection(task ExecutableTask, output json.RawMessage, out TaskExecutionResult) {
	var report investigationReportOutput
	_ = json.Unmarshal(output, &report)
	findingEntities := 0
	for _, finding := range report.Findings {
		findingEntities += len(finding.EntityIDs)
	}
	log.Infof("[investigation] investigator projected task=%s discoveries=%d discovered=%v findings=%d finding_entity_ids=%d evidence_candidates=%d output_len=%d summary=%s",
		task.ID, len(out.Discoveries), report.DiscoveredEntities, len(report.Findings), findingEntities, len(out.EvidenceCandidates), len(output),
		platform.TruncateForLog(report.Summary, 200),
	)
}

func logVerifierProjection(
	task ExecutableTask,
	input TaskExecutionInput,
	result agentapi.RunResult,
	claims []ClaimCandidate,
	budget EvidenceContextBudget,
) {
	var verification verificationResult
	_ = json.Unmarshal(result.Output, &verification)
	context := taskEvidenceContext(task, input, budget)
	drops := verifierProjectionDrops(verification, task, input, budget)
	entityCounts := make([]int, 0, len(claims))
	for _, claim := range claims {
		entityCounts = append(entityCounts, len(claim.EntityIDs))
	}
	available := make(map[string]struct{}, len(context.lookup))
	for id := range context.lookup {
		available[id] = struct{}{}
	}
	log.Infof("[investigation] verifier projected task=%s output_len=%d verdicts=%d projected_claims=%d selected_evidence=%d lookup=%d available_ids=%v json_keys=%v claim_entity_ids=%v drops=%v output=%s",
		task.ID, len(result.Output), len(verification.Verdicts), len(claims),
		len(context.selected), len(context.lookup), availableEvidenceIDs(available), jsonObjectKeys(result.Output), entityCounts, drops,
		platform.TruncateForLog(string(result.Output), 2000),
	)
}

func jsonObjectKeys(raw json.RawMessage) []string {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return []string{"<not_object>"}
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	return keys
}

func availableEvidenceIDs(available map[string]struct{}) []string {
	ids := make([]string, 0, len(available))
	for id := range available {
		ids = append(ids, id)
	}
	return ids
}

func verifierProjectionDrops(
	verification verificationResult,
	task ExecutableTask,
	input TaskExecutionInput,
	budget EvidenceContextBudget,
) []string {
	context := taskEvidenceContext(task, input, budget)
	statements := make(map[string]string, len(context.lookup))
	for id, evidence := range context.lookup {
		statements[id] = evidence.Summary
	}
	findings := findingStatementsByEvidenceID(input.Upstream, context.lookup)
	availableEvidence := make(map[string]struct{}, len(context.lookup))
	for id := range context.lookup {
		availableEvidence[id] = struct{}{}
	}
	drops := make([]string, 0)
	for index, verdict := range verification.Verdicts {
		statement := ""
		for _, rawID := range verdict.ClaimIDs {
			id := strings.TrimSpace(rawID)
			if text := findings[id]; isUserReadableClaimText(text) {
				statement = text
				break
			}
			if text := statements[id]; isUserReadableClaimText(text) {
				statement = text
				break
			}
		}
		if statement == "" && isUserReadableClaimText(verdict.Rationale) {
			statement = strings.TrimSpace(verdict.Rationale)
		}
		if statement == "" {
			drops = append(drops, fmt.Sprintf("verdict_%d_empty_statement claim_ids=%v", index, verdict.ClaimIDs))
			continue
		}
		if len(verifierEvidenceRefs(verdict, availableEvidence)) == 0 {
			drops = append(drops, fmt.Sprintf("verdict_%d_no_evidence_refs claim_ids=%v evidence_refs=%v", index, verdict.ClaimIDs, verdict.EvidenceRefs))
		}
	}
	if len(verification.Verdicts) == 0 {
		drops = append(drops, "no_verdicts")
	}
	return drops
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
	findings := findingStatementsByEvidenceID(input.Upstream, context.lookup)
	availableEvidence := make(map[string]struct{}, len(context.lookup))
	for id := range context.lookup {
		availableEvidence[id] = struct{}{}
	}
	verdictLimit := len(task.EvidenceGoalIDs)
	if verdictLimit <= 0 {
		verdictLimit = 1
	}
	// Unscoped discovery findings share one evidence goal. Project every
	// readable verdict instead of keeping only the first goal slot.
	if len(executableTaskEntityIDs(task)) == 0 {
		verdictLimit = maxVerifierVerdicts
	}
	if verdictLimit > maxVerifierVerdicts {
		verdictLimit = maxVerifierVerdicts
	}
	if len(verification.Verdicts) > verdictLimit {
		log.Warnf("[investigation] verifier verdicts truncated task=%s verdicts=%d projected_limit=%d unscoped=%t",
			task.ID, len(verification.Verdicts), verdictLimit, len(executableTaskEntityIDs(task)) == 0)
	}
	entityIDs := executableTaskEntityIDs(task)
	findingEntities := findingEntityIDsByEvidenceID(input.Upstream, context.lookup)
	claims := make([]ClaimCandidate, 0, verdictLimit)
	for _, verdict := range verification.Verdicts {
		if len(claims) >= verdictLimit {
			break
		}
		statement := ""
		for _, rawID := range verdict.ClaimIDs {
			id := strings.TrimSpace(rawID)
			if text := findings[id]; isUserReadableClaimText(text) {
				statement = text
				break
			}
			if text := statements[id]; isUserReadableClaimText(text) {
				statement = text
				break
			}
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
			log.Warnf("[investigation] verifier verdict dropped task=%s reason=empty_statement claim_ids=%v rationale=%s",
				task.ID, verdict.ClaimIDs, platform.TruncateForLog(verdict.Rationale, 200),
			)
			continue
		}
		refs := verifierEvidenceRefs(verdict, availableEvidence)
		if len(refs) == 0 {
			// Never hand an ungrounded model claim to the claim ledger.
			log.Warnf("[investigation] verifier verdict dropped task=%s reason=no_evidence_refs claim_ids=%v evidence_refs=%v available=%v",
				task.ID, verdict.ClaimIDs, verdict.EvidenceRefs, availableEvidenceIDs(availableEvidence),
			)
			continue
		}
		boundEntities := entityIDs
		if len(boundEntities) == 0 {
			boundEntities = findingEntitiesForVerdict(verdict, findingEntities)
		}
		claims = append(claims, ClaimCandidate{
			GoalID:       goalID,
			Text:         statement,
			Status:       verdictStatus(verdict.Decision),
			EntityIDs:    append([]string(nil), boundEntities...),
			EvidenceRefs: refs,
		})
	}
	return claims, nil
}

func executableTaskEntityIDs(task ExecutableTask) []string {
	if len(task.EntityDetails) > 0 {
		ids := make([]string, 0, len(task.EntityDetails))
		for _, entity := range task.EntityDetails {
			if id := strings.TrimSpace(entity.ID); id != "" && !containsString(ids, id) {
				ids = append(ids, id)
			}
		}
		return ids
	}
	ids := make([]string, 0, len(task.Entities))
	for _, id := range task.Entities {
		id = strings.TrimSpace(id)
		if id != "" && !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func findingStatementsByEvidenceID(
	upstream map[string]json.RawMessage,
	lookup map[string]evidenceSummaryView,
) map[string]string {
	out := make(map[string]string)
	if len(upstream) == 0 || len(lookup) == 0 {
		return out
	}
	idByRef := evidenceIDsByReference(lookup)
	for _, raw := range upstream {
		var report investigationReportOutput
		if err := json.Unmarshal(raw, &report); err != nil {
			continue
		}
		for _, finding := range report.Findings {
			statement := strings.TrimSpace(finding.Claim)
			if !isUserReadableClaimText(statement) {
				continue
			}
			for _, item := range finding.Evidence {
				id, ok := resolveFindingCitation(item, idByRef, lookup)
				if !ok {
					continue
				}
				if _, exists := out[id]; !exists {
					out[id] = statement
				}
			}
		}
	}
	return out
}

func findingEntityIDsByEvidenceID(
	upstream map[string]json.RawMessage,
	lookup map[string]evidenceSummaryView,
) map[string][]string {
	out := make(map[string][]string)
	if len(upstream) == 0 || len(lookup) == 0 {
		return out
	}
	idByRef := evidenceIDsByReference(lookup)
	for _, raw := range upstream {
		var report investigationReportOutput
		if err := json.Unmarshal(raw, &report); err != nil {
			continue
		}
		for _, finding := range report.Findings {
			ids := normalizeFindingEntityIDs(finding.EntityIDs)
			if len(ids) == 0 {
				continue
			}
			for _, item := range finding.Evidence {
				id, ok := resolveFindingCitation(item, idByRef, lookup)
				if !ok {
					continue
				}
				if _, exists := out[id]; !exists {
					out[id] = append([]string(nil), ids...)
				}
			}
		}
	}
	return out
}

func findingEntitiesForVerdict(verdict verificationVerdict, byEvidenceID map[string][]string) []string {
	for _, rawID := range verdict.ClaimIDs {
		if ids := byEvidenceID[strings.TrimSpace(rawID)]; len(ids) > 0 {
			return append([]string(nil), ids...)
		}
	}
	for _, rawID := range verdict.EvidenceRefs {
		if ids := byEvidenceID[strings.TrimSpace(rawID)]; len(ids) > 0 {
			return append([]string(nil), ids...)
		}
	}
	return nil
}

func normalizeFindingEntityIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
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
