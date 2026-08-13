package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const DelegatedInvestigationID = "delegated.investigation"

const delegatedInvestigationMaxAttempts = 2

const delegatedInvestigationPurpose = "Run independent code, runtime-topology, and documentation investigations before grounded synthesis."

// DelegatedInvestigationGoal is one server-recognized evidence facet.
type DelegatedInvestigationGoal struct {
	Facet    string
	Required bool
}

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
				JoinMode: JoinEvidenceView, RejectEvidenceConflicts: true,
				Permissions: readOnly, Timeout: nodeTimeout,
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

// DelegatedInvestigationProposalForGoals maps canonical evidence goals to a bounded capability graph.
func DelegatedInvestigationProposalForGoals(
	goals []DelegatedInvestigationGoal,
) (agentapi.TaskGraphProposal, error) {
	selections, _, err := delegatedInvestigationSelections(goals)
	if err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	proposal := agentapi.TaskGraphProposal{
		Tasks: make([]agentapi.TaskSpec, 0, len(selections)+1),
		Edges: make([]agentapi.TaskEdge, 0, len(selections)),
	}
	for _, selection := range selections {
		proposal.Tasks = append(proposal.Tasks, agentapi.TaskSpec{
			ID: selection.nodeID,
			Purpose: fmt.Sprintf(
				"Investigate requested %s evidence facets: %s.",
				selection.focus,
				strings.Join(selection.facets, ", "),
			),
			RequiredFacets: append([]string(nil), selection.facets...),
			Capability:     selection.capabilityID,
			OutputSchema:   report,
			ParallelGroup:  "investigation",
			Optional:       !selection.required,
			MaxAttempts:    delegatedInvestigationMaxAttempts,
		})
		proposal.Edges = append(proposal.Edges, agentapi.TaskEdge{
			From: selection.nodeID,
			To:   "synthesize",
		})
	}
	proposal.Tasks = append(proposal.Tasks, agentapi.TaskSpec{
		ID: "synthesize", Purpose: "Synthesize the available evidence.",
		Capability:   "evidence.synthesize",
		OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		MaxAttempts:  delegatedInvestigationMaxAttempts,
	})
	return proposal, nil
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
		CapabilityVersions: map[string]int64{
			"knowledge.code.inspect":  version,
			"knowledge.service.trace": version,
			"knowledge.docs.verify":   version,
			"evidence.synthesize":     version,
		},
		MaxTasks:                4,
		MaxParallelism:          3,
		MaxAttempts:             delegatedInvestigationMaxAttempts,
		MaxRounds:               1,
		MaxDepth:                3,
		JoinID:                  "evidence.join",
		JoinMode:                JoinEvidenceView,
		RejectEvidenceConflicts: true,
		JoinInputSchema:         agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		JoinOutputSchema: agentapi.SchemaRef{
			ID: "investigation.bundle", Version: 1,
		},
		FailureMode: CollectAvailable,
	}, nil
}

// DelegatedInvestigationCompilationPolicyForGoals fixes a request-level immutable graph snapshot.
func DelegatedInvestigationCompilationPolicyForGoals(
	version int64,
	nodeTimeout time.Duration,
	budgets DelegatedInvestigationBudgetPolicy,
	goals []DelegatedInvestigationGoal,
) (CompilationPolicy, error) {
	if version <= 0 {
		return CompilationPolicy{}, fmt.Errorf("delegated investigation version must be positive")
	}
	if nodeTimeout <= 0 {
		return CompilationPolicy{}, fmt.Errorf("delegated investigation node timeout must be positive")
	}
	selections, normalized, err := delegatedInvestigationSelections(goals)
	if err != nil {
		return CompilationPolicy{}, err
	}
	agentBudgets := make([]NodeBudget, 0, len(selections)+1)
	capabilityBudgets := make(map[string]NodeBudget, len(selections)+1)
	capabilityVersions := make(map[string]int64, len(selections)+1)
	for _, selection := range selections {
		budget := delegatedInvestigationCapabilityBudget(
			selection.capabilityID,
			budgets,
		)
		agentBudgets = append(agentBudgets, budget)
		capabilityBudgets[selection.capabilityID] = budget
		capabilityVersions[selection.capabilityID] = version
	}
	agentBudgets = append(agentBudgets, budgets.Synthesizer)
	capabilityBudgets["evidence.synthesize"] = budgets.Synthesizer
	capabilityVersions["evidence.synthesize"] = version
	workflowBudget, err := delegatedInvestigationWorkflowBudgetForNodes(
		nodeTimeout,
		agentBudgets,
		len(selections),
	)
	if err != nil {
		return CompilationPolicy{}, err
	}
	requiredGoals := make([]string, 0, len(normalized))
	for _, goal := range normalized {
		if goal.Required {
			requiredGoals = append(requiredGoals, goal.Facet)
		}
	}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	return CompilationPolicy{
		WorkflowID:              delegatedInvestigationWorkflowID(normalized),
		WorkflowVersion:         version,
		Purpose:                 delegatedInvestigationPurpose,
		InputSchema:             agentapi.SchemaRef{ID: "task.contract", Version: 1},
		OutputSchema:            agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		Permissions:             readOnly,
		CallerPermissions:       readOnly,
		Budget:                  workflowBudget,
		NodeTimeout:             nodeTimeout,
		CapabilityBudgets:       capabilityBudgets,
		CapabilityVersions:      capabilityVersions,
		MaxTasks:                len(selections) + 1,
		MaxParallelism:          len(selections),
		MaxAttempts:             delegatedInvestigationMaxAttempts,
		MaxRounds:               1,
		MaxDepth:                3,
		RequiredGoals:           requiredGoals,
		JoinID:                  "evidence.join",
		JoinMode:                JoinEvidenceView,
		RejectEvidenceConflicts: true,
		JoinInputSchema:         agentapi.SchemaRef{ID: "investigation.report", Version: 1},
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
	return delegatedInvestigationWorkflowBudgetForNodes(
		nodeTimeout,
		[]NodeBudget{
			budgets.Code,
			budgets.Runtime,
			budgets.Docs,
			budgets.Synthesizer,
		},
		3,
	)
}

func delegatedInvestigationWorkflowBudgetForNodes(
	nodeTimeout time.Duration,
	agentBudgets []NodeBudget,
	investigatorCount int,
) (WorkflowBudget, error) {
	if investigatorCount <= 0 ||
		len(agentBudgets) != investigatorCount+1 {
		return WorkflowBudget{}, fmt.Errorf(
			"delegated investigation requires investigators and one synthesizer",
		)
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
	for index, budget := range agentBudgets[:investigatorCount] {
		if budget.MaxToolCalls <= 0 {
			return WorkflowBudget{}, fmt.Errorf(
				"delegated investigation investigator budget %d requires a positive tool limit",
				index,
			)
		}
	}
	if agentBudgets[len(agentBudgets)-1].MaxToolCalls != 0 {
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
		MaxNodes: len(agentBudgets) + 1, MaxParallelism: investigatorCount,
		Timeout:         3 * nodeTimeout,
		MaxHandoffBytes: 1 << 20,
		MaxInputTokens:  total.MaxInputTokens,
		MaxOutputTokens: total.MaxOutputTokens,
		MaxTotalTokens:  total.MaxTotalTokens,
		MaxToolCalls:    total.MaxToolCalls,
		MaxCostMicros:   total.MaxCostMicros,
		MaxRetries:      maxRetries,
	}, nil
}

type delegatedInvestigationSelection struct {
	capabilityID string
	nodeID       string
	focus        string
	facets       []string
	required     bool
}

func delegatedInvestigationSelections(
	goals []DelegatedInvestigationGoal,
) ([]delegatedInvestigationSelection, []DelegatedInvestigationGoal, error) {
	normalized, err := normalizeDelegatedInvestigationGoals(goals)
	if err != nil {
		return nil, nil, err
	}
	selections := []delegatedInvestigationSelection{
		{
			capabilityID: "knowledge.code.inspect",
			nodeID:       "investigate.code",
			focus:        "code",
		},
		{
			capabilityID: "knowledge.service.trace",
			nodeID:       "investigate.runtime",
			focus:        "runtime",
		},
		{
			capabilityID: "knowledge.docs.verify",
			nodeID:       "investigate.docs",
			focus:        "documentation",
		},
	}
	selected := make(map[string]int, len(selections))
	for index := range selections {
		selected[selections[index].capabilityID] = index
	}
	for _, goal := range normalized {
		capabilityID, ok := delegatedInvestigationCapabilityForFacet(goal.Facet)
		if !ok {
			return nil, nil, fmt.Errorf(
				"delegated investigation facet %q has no registered capability",
				goal.Facet,
			)
		}
		index := selected[capabilityID]
		selections[index].facets = append(selections[index].facets, goal.Facet)
		selections[index].required = selections[index].required || goal.Required
	}
	filtered := make([]delegatedInvestigationSelection, 0, len(selections))
	for _, selection := range selections {
		if len(selection.facets) > 0 {
			filtered = append(filtered, selection)
		}
	}
	return filtered, normalized, nil
}

func normalizeDelegatedInvestigationGoals(
	goals []DelegatedInvestigationGoal,
) ([]DelegatedInvestigationGoal, error) {
	if len(goals) == 0 {
		return nil, fmt.Errorf("delegated investigation evidence goals are required")
	}
	required := make(map[string]bool, len(goals))
	for _, goal := range goals {
		if !canonicalID.MatchString(goal.Facet) {
			return nil, fmt.Errorf(
				"delegated investigation facet %q is not canonical",
				goal.Facet,
			)
		}
		required[goal.Facet] = required[goal.Facet] || goal.Required
	}
	facets := make([]string, 0, len(required))
	for facet := range required {
		facets = append(facets, facet)
	}
	sort.Strings(facets)
	normalized := make([]DelegatedInvestigationGoal, 0, len(facets))
	for _, facet := range facets {
		normalized = append(normalized, DelegatedInvestigationGoal{
			Facet: facet, Required: required[facet],
		})
	}
	return normalized, nil
}

func delegatedInvestigationCapabilityForFacet(facet string) (string, bool) {
	switch facet {
	case "implementation", "entrypoint", "core_flow", "data_and_state":
		return "knowledge.code.inspect", true
	case "service.topology", "system_boundary", "external_dependency",
		"runtime_and_operations":
		return "knowledge.service.trace", true
	case "documentation", "business_domain":
		return "knowledge.docs.verify", true
	default:
		return "", false
	}
}

func delegatedInvestigationCapabilityBudget(
	capabilityID string,
	budgets DelegatedInvestigationBudgetPolicy,
) NodeBudget {
	switch capabilityID {
	case "knowledge.code.inspect":
		return budgets.Code
	case "knowledge.service.trace":
		return budgets.Runtime
	case "knowledge.docs.verify":
		return budgets.Docs
	default:
		return NodeBudget{}
	}
}

func delegatedInvestigationWorkflowID(
	goals []DelegatedInvestigationGoal,
) string {
	var canonical strings.Builder
	for _, goal := range goals {
		canonical.WriteString(goal.Facet)
		if goal.Required {
			canonical.WriteString(":required\n")
		} else {
			canonical.WriteString(":optional\n")
		}
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return DelegatedInvestigationID + ".goals." + hex.EncodeToString(sum[:8])
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
