package workflow

import (
	"fmt"
	"math"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const DelegatedInvestigationID = "delegated.investigation"

const delegatedInvestigationMaxAttempts = 2

const delegatedInvestigationPurpose = "Run independent code, runtime-topology, and documentation investigations before grounded synthesis."

// DelegatedInvestigationBudgetPolicy pins one-attempt budgets for each agent role.
type DelegatedInvestigationBudgetPolicy struct {
	Code        NodeBudget
	Runtime     NodeBudget
	Docs        NodeBudget
	Synthesizer NodeBudget
}

// DefaultDelegatedInvestigation builds the standard read-only investigation DAG.
func DefaultDelegatedInvestigation(
	version int64,
	nodeTimeout time.Duration,
	budgets DelegatedInvestigationBudgetPolicy,
) (WorkflowDefinition, error) {
	if version <= 0 {
		return WorkflowDefinition{}, fmt.Errorf("delegated investigation version must be positive")
	}
	if nodeTimeout <= 0 {
		return WorkflowDefinition{}, fmt.Errorf("delegated investigation node timeout must be positive")
	}
	workflowBudget, err := delegatedInvestigationWorkflowBudget(nodeTimeout, budgets)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	contract := agentapi.SchemaRef{ID: "task.contract", Version: 1}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundle := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	answer := agentapi.SchemaRef{ID: "investigation.answer", Version: 1}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition := WorkflowDefinition{
		ID: DelegatedInvestigationID, Version: version,
		Purpose:       delegatedInvestigationPurpose,
		InputSchema:   contract,
		OutputSchema:  answer,
		Permissions:   readOnly,
		Budget:        workflowBudget,
		FailurePolicy: WorkflowFailurePolicy{Mode: CollectAvailable},
		Nodes: []NodeDefinition{
			investigationNode("investigate.code", "investigator.code", version, contract, report, readOnly, budgets.Code, nodeTimeout),
			investigationNode("investigate.runtime", "investigator.runtime", version, contract, report, readOnly, budgets.Runtime, nodeTimeout),
			investigationNode("investigate.docs", "investigator.docs", version, contract, report, readOnly, budgets.Docs, nodeTimeout),
			{
				ID: "evidence.join", Kind: NodeJoin, InputSchema: report, OutputSchema: bundle,
				JoinMode: JoinEvidenceView, Permissions: readOnly, Timeout: nodeTimeout,
			},
			{
				ID: "synthesize", Kind: NodeAgent,
				Agent:       agentapi.DefinitionRef{ID: "synthesizer", Version: version},
				InputSchema: bundle, OutputSchema: answer,
				Permissions: readOnly, Budget: budgets.Synthesizer,
				Retry:   RetryPolicy{MaxAttempts: delegatedInvestigationMaxAttempts},
				Timeout: nodeTimeout,
			},
		},
		Edges: []EdgeDefinition{
			{From: "investigate.code", To: "evidence.join", Required: false},
			{From: "investigate.runtime", To: "evidence.join", Required: false},
			{From: "investigate.docs", To: "evidence.join", Required: false},
			{From: "evidence.join", To: "synthesize", Required: true},
		},
	}
	return definition, nil
}

// DefaultDelegatedInvestigationProposal describes the standard graph through capabilities.
func DefaultDelegatedInvestigationProposal() agentapi.TaskGraphProposal {
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	return agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{
			{
				ID: "investigate.code", Purpose: "Inspect implementation evidence.",
				RequiredFacets: []string{"implementation"},
				Capability:     "knowledge.code.inspect",
				OutputSchema:   report,
				ParallelGroup:  "investigation",
				Optional:       true,
				MaxAttempts:    delegatedInvestigationMaxAttempts,
			},
			{
				ID: "investigate.runtime", Purpose: "Trace service topology evidence.",
				RequiredFacets: []string{"service.topology"},
				Capability:     "knowledge.service.trace",
				OutputSchema:   report,
				ParallelGroup:  "investigation",
				Optional:       true,
				MaxAttempts:    delegatedInvestigationMaxAttempts,
			},
			{
				ID: "investigate.docs", Purpose: "Verify documentation evidence.",
				RequiredFacets: []string{"documentation"},
				Capability:     "knowledge.docs.verify",
				OutputSchema:   report,
				ParallelGroup:  "investigation",
				Optional:       true,
				MaxAttempts:    delegatedInvestigationMaxAttempts,
			},
			{
				ID: "synthesize", Purpose: "Synthesize the available evidence.",
				Capability:   "evidence.synthesize",
				OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
				MaxAttempts:  delegatedInvestigationMaxAttempts,
			},
		},
		Edges: []agentapi.TaskEdge{
			{From: "investigate.code", To: "synthesize"},
			{From: "investigate.runtime", To: "synthesize"},
			{From: "investigate.docs", To: "synthesize"},
		},
	}
}

// DelegatedInvestigationCompilationPolicy fixes the server-owned limits for the default proposal.
func DelegatedInvestigationCompilationPolicy(
	version int64,
	nodeTimeout time.Duration,
	budgets DelegatedInvestigationBudgetPolicy,
) (CompilationPolicy, error) {
	workflowBudget, err := delegatedInvestigationWorkflowBudget(nodeTimeout, budgets)
	if err != nil {
		return CompilationPolicy{}, err
	}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	return CompilationPolicy{
		WorkflowID:        DelegatedInvestigationID,
		WorkflowVersion:   version,
		Purpose:           delegatedInvestigationPurpose,
		InputSchema:       agentapi.SchemaRef{ID: "task.contract", Version: 1},
		OutputSchema:      agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		Permissions:       readOnly,
		CallerPermissions: readOnly,
		Budget:            workflowBudget,
		NodeTimeout:       nodeTimeout,
		CapabilityBudgets: map[string]NodeBudget{
			"knowledge.code.inspect":  budgets.Code,
			"knowledge.service.trace": budgets.Runtime,
			"knowledge.docs.verify":   budgets.Docs,
			"evidence.synthesize":     budgets.Synthesizer,
		},
		MaxTasks:        4,
		MaxParallelism:  3,
		MaxAttempts:     delegatedInvestigationMaxAttempts,
		MaxRounds:       1,
		MaxDepth:        3,
		JoinID:          "evidence.join",
		JoinMode:        JoinEvidenceView,
		JoinInputSchema: agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		JoinOutputSchema: agentapi.SchemaRef{
			ID: "investigation.bundle", Version: 1,
		},
		FailureMode: CollectAvailable,
	}, nil
}

func delegatedInvestigationWorkflowBudget(
	nodeTimeout time.Duration,
	budgets DelegatedInvestigationBudgetPolicy,
) (WorkflowBudget, error) {
	agentBudgets := []NodeBudget{
		budgets.Code,
		budgets.Runtime,
		budgets.Docs,
		budgets.Synthesizer,
	}
	for index, budget := range agentBudgets {
		if budget.MaxInputTokens <= 0 ||
			budget.MaxOutputTokens <= 0 ||
			budget.MaxTotalTokens <= 0 {
			return WorkflowBudget{}, fmt.Errorf(
				"delegated investigation agent budget %d requires positive token limits",
				index,
			)
		}
	}
	for index, budget := range agentBudgets[:3] {
		if budget.MaxToolCalls <= 0 {
			return WorkflowBudget{}, fmt.Errorf(
				"delegated investigation investigator budget %d requires a positive tool limit",
				index,
			)
		}
	}
	if budgets.Synthesizer.MaxToolCalls != 0 {
		return WorkflowBudget{}, fmt.Errorf(
			"delegated investigation synthesizer tool limit must be zero",
		)
	}
	total := NodeBudget{}
	for _, budget := range agentBudgets {
		var err error
		total, err = addNodeBudget(total, budget)
		if err != nil {
			return WorkflowBudget{}, fmt.Errorf("delegated investigation budget: %w", err)
		}
	}
	total, err := multiplyNodeBudget(total, delegatedInvestigationMaxAttempts)
	if err != nil {
		return WorkflowBudget{}, fmt.Errorf("delegated investigation budget: %w", err)
	}
	maxRetries, err := multiplyBudgetValue(
		int64(len(agentBudgets)),
		delegatedInvestigationMaxAttempts-1,
	)
	if err != nil {
		return WorkflowBudget{}, fmt.Errorf("delegated investigation retries overflow")
	}
	return WorkflowBudget{
		MaxNodes: 5, MaxParallelism: 3, Timeout: 3 * nodeTimeout,
		MaxHandoffBytes: 1 << 20,
		MaxInputTokens:  total.MaxInputTokens,
		MaxOutputTokens: total.MaxOutputTokens,
		MaxTotalTokens:  total.MaxTotalTokens,
		MaxToolCalls:    total.MaxToolCalls,
		MaxCostMicros:   total.MaxCostMicros,
		MaxRetries:      maxRetries,
	}, nil
}

func addNodeBudget(left, right NodeBudget) (NodeBudget, error) {
	input, err := addBudgetValue(left.MaxInputTokens, right.MaxInputTokens)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("input tokens overflow")
	}
	output, err := addBudgetValue(left.MaxOutputTokens, right.MaxOutputTokens)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("output tokens overflow")
	}
	total, err := addBudgetValue(left.MaxTotalTokens, right.MaxTotalTokens)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("total tokens overflow")
	}
	tools, err := addBudgetValue(left.MaxToolCalls, right.MaxToolCalls)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("tool calls overflow")
	}
	cost, err := addBudgetValue(left.MaxCostMicros, right.MaxCostMicros)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("cost overflow")
	}
	return NodeBudget{
		MaxInputTokens: input, MaxOutputTokens: output, MaxTotalTokens: total,
		MaxToolCalls: tools, MaxCostMicros: cost,
	}, nil
}

func multiplyNodeBudget(budget NodeBudget, factor int64) (NodeBudget, error) {
	input, err := multiplyBudgetValue(budget.MaxInputTokens, factor)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("input tokens overflow")
	}
	output, err := multiplyBudgetValue(budget.MaxOutputTokens, factor)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("output tokens overflow")
	}
	total, err := multiplyBudgetValue(budget.MaxTotalTokens, factor)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("total tokens overflow")
	}
	tools, err := multiplyBudgetValue(budget.MaxToolCalls, factor)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("tool calls overflow")
	}
	cost, err := multiplyBudgetValue(budget.MaxCostMicros, factor)
	if err != nil {
		return NodeBudget{}, fmt.Errorf("cost overflow")
	}
	return NodeBudget{
		MaxInputTokens: input, MaxOutputTokens: output, MaxTotalTokens: total,
		MaxToolCalls: tools, MaxCostMicros: cost,
	}, nil
}

func addBudgetValue(left, right int64) (int64, error) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, fmt.Errorf("invalid budget value")
	}
	return left + right, nil
}

func multiplyBudgetValue(value, factor int64) (int64, error) {
	if value < 0 || factor < 0 || value != 0 && factor > math.MaxInt64/value {
		return 0, fmt.Errorf("invalid budget value")
	}
	return value * factor, nil
}

func investigationNode(
	nodeID string,
	agentID string,
	version int64,
	input agentapi.SchemaRef,
	output agentapi.SchemaRef,
	permissions agentapi.PermissionPolicy,
	budget NodeBudget,
	timeout time.Duration,
) NodeDefinition {
	return NodeDefinition{
		ID: nodeID, Kind: NodeAgent, Optional: true,
		Agent:       agentapi.DefinitionRef{ID: agentID, Version: version},
		InputSchema: input, OutputSchema: output,
		Permissions: permissions, Budget: budget,
		Retry:   RetryPolicy{MaxAttempts: delegatedInvestigationMaxAttempts},
		Timeout: timeout,
	}
}
