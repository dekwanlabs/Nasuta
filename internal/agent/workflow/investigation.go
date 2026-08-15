package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const FlowID = "delegated.investigation"

const maxAttempts = 2

const maxDuplicateRatio = 0.8

const minTrustTier = 1

const defaultPurpose = "Run independent code, runtime-topology, and documentation investigations before grounded synthesis."

// Goal is one server-recognized evidence facet.
type Goal struct {
	Facet           string
	Required        bool
	Sources         []agentapi.EvidenceSource
	Freshness       agentapi.FreshnessPolicy
	MinimumCoverage int
	HighRisk        bool
}

// Budgets pins one-attempt budgets for each agent role.
type Budgets struct {
	Code        NodeBudget
	Runtime     NodeBudget
	Docs        NodeBudget
	Web         NodeBudget
	Memory      NodeBudget
	Observe     NodeBudget
	Synthesizer NodeBudget
}

// DefaultFlow builds the standard read-only investigation DAG.
func DefaultFlow(
	version int64,
	nodeTimeout time.Duration,
	budgets Budgets,
) (Definition, error) {
	if version <= 0 {
		return Definition{}, fmt.Errorf("investigation flow version must be positive")
	}
	if nodeTimeout <= 0 {
		return Definition{}, fmt.Errorf("investigation flow node timeout must be positive")
	}
	workflowBudget, err := defaultFlowBudget(nodeTimeout, budgets)
	if err != nil {
		return Definition{}, err
	}
	contract := agentapi.SchemaRef{ID: "task.contract", Version: 1}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	bundle := agentapi.SchemaRef{ID: "investigation.bundle", Version: 1}
	verified := agentapi.SchemaRef{ID: "investigation.verified_bundle", Version: 1}
	answer := agentapi.SchemaRef{ID: "investigation.answer", Version: 1}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	definition := Definition{
		ID: FlowID, Version: version,
		Purpose:       defaultPurpose,
		InputSchema:   contract,
		OutputSchema:  answer,
		Permissions:   readOnly,
		Budget:        workflowBudget,
		FailurePolicy: FailurePolicy{Mode: CollectAvailable},
		Nodes: []NodeDefinition{
			investigationNode("investigate.code", "investigator.code", version, contract, report, readOnly, budgets.Code, nodeTimeout),
			investigationNode("investigate.runtime", "investigator.runtime", version, contract, report, readOnly, budgets.Runtime, nodeTimeout),
			investigationNode("investigate.docs", "investigator.docs", version, contract, report, readOnly, budgets.Docs, nodeTimeout),
			{
				ID: "evidence.join", Kind: NodeJoin, InputSchema: report, OutputSchema: bundle,
				JoinMode:    JoinEvidenceView,
				Permissions: readOnly, Timeout: nodeTimeout,
			},
			{
				ID: "evidence.verify", Kind: NodeVerifier,
				InputSchema: bundle, OutputSchema: verified,
				Verifier: &VerifierSpec{
					HighRiskMinimumTrustTier: minTrustTier,
				},
				Permissions: readOnly, Timeout: nodeTimeout,
			},
			{
				ID: "evidence.risk", Kind: NodeGate,
				InputSchema: verified, OutputSchema: verified,
				Gate: &GateSpec{
					ID: EvidenceRiskGateID,
					AllowedDecisions: []string{
						EvidenceRiskPassDecision,
						string(StopNeedsClarification),
					},
					ForwardInput: true,
				},
				Permissions: readOnly, Timeout: nodeTimeout,
			},
			{
				ID: "synthesize", Kind: NodeAgent,
				Agent:       agentapi.DefinitionRef{ID: "synthesizer", Version: version},
				InputSchema: verified, OutputSchema: answer,
				Permissions: readOnly, Budget: budgets.Synthesizer,
				Retry:   RetryPolicy{MaxAttempts: maxAttempts},
				Timeout: nodeTimeout,
			},
		},
		Edges: []EdgeDefinition{
			{From: "investigate.code", To: "evidence.join", Required: false},
			{From: "investigate.runtime", To: "evidence.join", Required: false},
			{From: "investigate.docs", To: "evidence.join", Required: false},
			{From: "evidence.join", To: "evidence.verify", Required: true},
			{From: "evidence.verify", To: "evidence.risk", Required: true},
			{From: "evidence.risk", To: "synthesize", Required: true},
		},
	}
	return definition, nil
}

// DefaultPlan describes the standard capability graph.
func DefaultPlan() agentapi.TaskGraphProposal {
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
				MaxAttempts:    maxAttempts,
			},
			{
				ID: "investigate.runtime", Purpose: "Trace service topology evidence.",
				RequiredFacets: []string{"service.topology"},
				Capability:     "knowledge.service.trace",
				OutputSchema:   report,
				ParallelGroup:  "investigation",
				Optional:       true,
				MaxAttempts:    maxAttempts,
			},
			{
				ID: "investigate.docs", Purpose: "Verify documentation evidence.",
				RequiredFacets: []string{"documentation"},
				Capability:     "knowledge.docs.verify",
				OutputSchema:   report,
				ParallelGroup:  "investigation",
				Optional:       true,
				MaxAttempts:    maxAttempts,
			},
			{
				ID: "synthesize", Purpose: "Synthesize the available evidence.",
				Capability:   "evidence.synthesize",
				OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
				MaxAttempts:  maxAttempts,
			},
		},
		Edges: []agentapi.TaskEdge{
			{From: "investigate.code", To: "synthesize"},
			{From: "investigate.runtime", To: "synthesize"},
			{From: "investigate.docs", To: "synthesize"},
		},
	}
}

// BuildPlan maps evidence goals to a bounded capability graph.
func BuildPlan(
	goals []Goal,
) (agentapi.TaskGraphProposal, error) {
	selections, _, err := selectInvestigators(goals)
	if err != nil {
		return agentapi.TaskGraphProposal{}, err
	}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	plan := agentapi.TaskGraphProposal{
		Tasks: make([]agentapi.TaskSpec, 0, len(selections)+1),
		Edges: make([]agentapi.TaskEdge, 0, len(selections)),
	}
	for _, selection := range selections {
		plan.Tasks = append(plan.Tasks, agentapi.TaskSpec{
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
			Optional:       true,
			MaxAttempts:    maxAttempts,
		})
		plan.Edges = append(plan.Edges, agentapi.TaskEdge{
			From: selection.nodeID,
			To:   "synthesize",
		})
	}
	plan.Tasks = append(plan.Tasks, agentapi.TaskSpec{
		ID: "synthesize", Purpose: "Synthesize the available evidence.",
		Capability:   "evidence.synthesize",
		OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		MaxAttempts:  maxAttempts,
	})
	return plan, nil
}

// DefaultPolicy fixes server-owned limits for the default plan.
func DefaultPolicy(
	version int64,
	nodeTimeout time.Duration,
	budgets Budgets,
) (CompilationPolicy, error) {
	workflowBudget, err := defaultFlowBudget(nodeTimeout, budgets)
	if err != nil {
		return CompilationPolicy{}, err
	}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	return CompilationPolicy{
		WorkflowID:        FlowID,
		WorkflowVersion:   version,
		Purpose:           defaultPurpose,
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
		MaxAttempts:             maxAttempts,
		MaxRounds:               1,
		MaxDepth:                5,
		JoinID:                  "evidence.join",
		JoinMode:                JoinEvidenceView,
		RejectEvidenceConflicts: false,
		JoinInputSchema:         agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		JoinOutputSchema: agentapi.SchemaRef{
			ID: "investigation.bundle", Version: 1,
		},
		VerifierID: "evidence.verify",
		VerifierInputSchema: agentapi.SchemaRef{
			ID: "investigation.bundle", Version: 1,
		},
		VerifierOutputSchema: agentapi.SchemaRef{
			ID: "investigation.verified_bundle", Version: 1,
		},
		HighRiskMinimumTrustTier: minTrustTier,
		RiskGateID:               "evidence.risk",
		FailureMode:              CollectAvailable,
	}, nil
}

// GoalPolicy fixes one request-level graph snapshot.
func GoalPolicy(
	version int64,
	nodeTimeout time.Duration,
	budgets Budgets,
	goals []Goal,
) (CompilationPolicy, error) {
	if version <= 0 {
		return CompilationPolicy{}, fmt.Errorf("investigation flow version must be positive")
	}
	if nodeTimeout <= 0 {
		return CompilationPolicy{}, fmt.Errorf("investigation flow node timeout must be positive")
	}
	selections, normalized, err := selectInvestigators(goals)
	if err != nil {
		return CompilationPolicy{}, err
	}
	agentBudgets := make([]NodeBudget, 0, len(selections)+1)
	capabilityBudgets := make(map[string]NodeBudget, len(selections)+1)
	capabilityVersions := make(map[string]int64, len(selections)+1)
	for _, selection := range selections {
		budget := capabilityBudget(
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
	workflowBudget, err := flowBudget(
		nodeTimeout,
		agentBudgets,
		len(selections),
	)
	if err != nil {
		return CompilationPolicy{}, err
	}
	requiredGoals := make([]string, 0, len(normalized))
	highRiskGoals := make([]string, 0, len(normalized))
	for _, goal := range normalized {
		if goal.Required {
			requiredGoals = append(requiredGoals, goal.Facet)
			if goal.HighRisk {
				highRiskGoals = append(highRiskGoals, goal.Facet)
			}
		}
	}
	readOnly := agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}}
	return CompilationPolicy{
		WorkflowID:               flowIDForGoals(normalized),
		WorkflowVersion:          version,
		Purpose:                  defaultPurpose,
		InputSchema:              agentapi.SchemaRef{ID: "task.contract", Version: 1},
		OutputSchema:             agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		Permissions:              readOnly,
		CallerPermissions:        readOnly,
		Budget:                   workflowBudget,
		NodeTimeout:              nodeTimeout,
		CapabilityBudgets:        capabilityBudgets,
		CapabilityVersions:       capabilityVersions,
		MaxTasks:                 len(selections) + 1,
		MaxParallelism:           len(selections),
		MaxAttempts:              maxAttempts,
		MaxRounds:                1,
		MaxDepth:                 5,
		RequiredGoals:            requiredGoals,
		HighRiskGoals:            highRiskGoals,
		HighRiskMinimumTrustTier: minTrustTier,
		JoinID:                   "evidence.join",
		JoinMode:                 JoinEvidenceView,
		RejectEvidenceConflicts:  false,
		JoinInputSchema:          agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		JoinOutputSchema: agentapi.SchemaRef{
			ID: "investigation.bundle", Version: 1,
		},
		VerifierID: "evidence.verify",
		VerifierInputSchema: agentapi.SchemaRef{
			ID: "investigation.bundle", Version: 1,
		},
		VerifierOutputSchema: agentapi.SchemaRef{
			ID: "investigation.verified_bundle", Version: 1,
		},
		RiskGateID:  "evidence.risk",
		FailureMode: CollectAvailable,
	}, nil
}

// PlanPolicy gives an accepted plan an immutable flow ID.
func PlanPolicy(
	version int64,
	nodeTimeout time.Duration,
	budgets Budgets,
	goals []Goal,
	plan agentapi.TaskGraphProposal,
) (CompilationPolicy, error) {
	policy, err := GoalPolicy(
		version,
		nodeTimeout,
		budgets,
		goals,
	)
	if err != nil {
		return CompilationPolicy{}, err
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return CompilationPolicy{}, fmt.Errorf(
			"marshal investigation plan identity: %w",
			err,
		)
	}
	sum := sha256.Sum256(payload)
	policy.WorkflowID += ".plan." + hex.EncodeToString(sum[:8])
	return policy, nil
}

func defaultFlowBudget(
	nodeTimeout time.Duration,
	budgets Budgets,
) (Budget, error) {
	return flowBudget(
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

func flowBudget(
	nodeTimeout time.Duration,
	agentBudgets []NodeBudget,
	investigatorCount int,
) (Budget, error) {
	if investigatorCount <= 0 ||
		len(agentBudgets) != investigatorCount+1 {
		return Budget{}, fmt.Errorf(
			"investigation flow requires investigators and one synthesizer",
		)
	}
	for index, budget := range agentBudgets {
		if budget.MaxInputTokens <= 0 ||
			budget.MaxOutputTokens <= 0 ||
			budget.MaxTotalTokens <= 0 {
			return Budget{}, fmt.Errorf(
				"investigation agent budget %d requires positive token limits",
				index,
			)
		}
	}
	if agentBudgets[len(agentBudgets)-1].MaxToolCalls != 0 {
		return Budget{}, fmt.Errorf(
			"investigation synthesizer tool limit must be zero",
		)
	}
	total := NodeBudget{}
	for _, budget := range agentBudgets {
		var err error
		total, err = addNodeBudget(total, budget)
		if err != nil {
			return Budget{}, fmt.Errorf("investigation budget: %w", err)
		}
	}
	total, err := multiplyNodeBudget(total, maxAttempts)
	if err != nil {
		return Budget{}, fmt.Errorf("investigation budget: %w", err)
	}
	maxRetries, err := multiplyBudgetValue(
		int64(len(agentBudgets)),
		maxAttempts-1,
	)
	if err != nil {
		return Budget{}, fmt.Errorf("investigation retries overflow")
	}
	return Budget{
		MaxNodes: len(agentBudgets) + 3, MaxParallelism: investigatorCount,
		MaxRounds:         1,
		MaxDepth:          5,
		Timeout:           3 * nodeTimeout,
		MaxHandoffBytes:   1 << 20,
		MaxDuplicateRatio: maxDuplicateRatio,
		MaxInputTokens:    total.MaxInputTokens,
		MaxOutputTokens:   total.MaxOutputTokens,
		MaxTotalTokens:    total.MaxTotalTokens,
		MaxToolCalls:      total.MaxToolCalls,
		MaxCostMicros:     total.MaxCostMicros,
		MaxRetries:        maxRetries,
	}, nil
}

type investigatorSelection struct {
	capabilityID string
	nodeID       string
	focus        string
	facets       []string
}

func selectInvestigators(
	goals []Goal,
) ([]investigatorSelection, []Goal, error) {
	normalized, err := normalizeGoals(goals)
	if err != nil {
		return nil, nil, err
	}
	selections := make([]investigatorSelection, 0, 5)
	selected := make(map[string]int, 5)
	add := func(capabilityID, nodeID, focus, facet string) {
		index, exists := selected[capabilityID]
		if !exists {
			index = len(selections)
			selected[capabilityID] = index
			selections = append(selections, investigatorSelection{
				capabilityID: capabilityID,
				nodeID:       nodeID,
				focus:        focus,
			})
		}
		if !contains(selections[index].facets, facet) {
			selections[index].facets = append(selections[index].facets, facet)
		}
	}
	for _, goal := range normalized {
		sources := goal.Sources
		if len(sources) == 0 {
			sources = []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
		}
		for _, source := range sources {
			switch source {
			case agentapi.EvidenceSourceInternal:
				capabilityID, nodeID, focus, ok := capabilityForFacet(
					goal.Facet,
				)
				if !ok {
					return nil, nil, fmt.Errorf(
						"investigation facet %q has no registered capability",
						goal.Facet,
					)
				}
				add(capabilityID, nodeID, focus, goal.Facet)
			case agentapi.EvidenceSourceWeb:
				add(
					"knowledge.web.research",
					"investigate.web",
					"web",
					goal.Facet,
				)
			case agentapi.EvidenceSourceMemory:
				add(
					"knowledge.memory.recall",
					"investigate.memory",
					"memory",
					goal.Facet,
				)
			case agentapi.EvidenceSourceRuntime:
				add(
					"knowledge.runtime.observe",
					"investigate.observe",
					"runtime",
					goal.Facet,
				)
			default:
				return nil, nil, fmt.Errorf(
					"investigation evidence source %q is invalid",
					source,
				)
			}
		}
	}
	return selections, normalized, nil
}

func normalizeGoals(
	goals []Goal,
) ([]Goal, error) {
	if len(goals) == 0 {
		return nil, fmt.Errorf("investigation evidence goals are required")
	}
	type normalizedGoal struct {
		required        bool
		highRisk        bool
		sources         map[agentapi.EvidenceSource]struct{}
		freshness       agentapi.FreshnessPolicy
		minimumCoverage int
	}
	byFacet := make(map[string]*normalizedGoal, len(goals))
	for _, goal := range goals {
		if !canonicalID.MatchString(goal.Facet) {
			return nil, fmt.Errorf(
				"investigation facet %q is not canonical",
				goal.Facet,
			)
		}
		current := byFacet[goal.Facet]
		if current == nil {
			current = &normalizedGoal{
				sources: make(map[agentapi.EvidenceSource]struct{}, len(goal.Sources)),
			}
			byFacet[goal.Facet] = current
		}
		current.required = current.required || goal.Required
		current.highRisk = current.highRisk || goal.HighRisk
		for _, source := range goal.Sources {
			switch source {
			case agentapi.EvidenceSourceInternal, agentapi.EvidenceSourceMemory,
				agentapi.EvidenceSourceWeb, agentapi.EvidenceSourceRuntime:
				current.sources[source] = struct{}{}
			default:
				return nil, fmt.Errorf(
					"investigation evidence source %q is invalid",
					source,
				)
			}
		}
		freshness := goal.Freshness
		if freshness == "" {
			freshness = agentapi.FreshnessStable
		}
		switch freshness {
		case agentapi.FreshnessStable, agentapi.FreshnessCurrent,
			agentapi.FreshnessBoundedLive:
		default:
			return nil, fmt.Errorf(
				"investigation freshness policy %q is invalid",
				freshness,
			)
		}
		if freshnessRank(freshness) > freshnessRank(current.freshness) {
			current.freshness = freshness
		}
		minimumCoverage := goal.MinimumCoverage
		if minimumCoverage <= 0 {
			minimumCoverage = 1
		}
		if minimumCoverage > current.minimumCoverage {
			current.minimumCoverage = minimumCoverage
		}
	}
	facets := make([]string, 0, len(byFacet))
	for facet := range byFacet {
		facets = append(facets, facet)
	}
	sort.Strings(facets)
	normalized := make([]Goal, 0, len(facets))
	for _, facet := range facets {
		current := byFacet[facet]
		sources := make([]agentapi.EvidenceSource, 0, len(current.sources))
		for _, source := range []agentapi.EvidenceSource{
			agentapi.EvidenceSourceInternal,
			agentapi.EvidenceSourceMemory,
			agentapi.EvidenceSourceWeb,
			agentapi.EvidenceSourceRuntime,
		} {
			if _, ok := current.sources[source]; ok {
				sources = append(sources, source)
			}
		}
		normalized = append(normalized, Goal{
			Facet: facet, Required: current.required, Sources: sources,
			Freshness: current.freshness, MinimumCoverage: current.minimumCoverage,
			HighRisk: current.highRisk,
		})
	}
	return normalized, nil
}

func freshnessRank(freshness agentapi.FreshnessPolicy) int {
	switch freshness {
	case agentapi.FreshnessBoundedLive:
		return 3
	case agentapi.FreshnessCurrent:
		return 2
	case agentapi.FreshnessStable:
		return 1
	default:
		return 0
	}
}

func capabilityForFacet(
	facet string,
) (capabilityID, nodeID, focus string, ok bool) {
	switch facet {
	case "implementation", "entrypoint", "core_flow", "data_and_state":
		return "knowledge.code.inspect", "investigate.code", "code", true
	case "service.topology", "system_boundary", "external_dependency",
		"runtime_and_operations":
		return "knowledge.service.trace", "investigate.runtime", "runtime", true
	case "documentation", "business_domain":
		return "knowledge.docs.verify", "investigate.docs", "documentation", true
	default:
		return "", "", "", false
	}
}

func capabilityBudget(
	capabilityID string,
	budgets Budgets,
) NodeBudget {
	switch capabilityID {
	case "knowledge.code.inspect":
		return budgets.Code
	case "knowledge.service.trace":
		return budgets.Runtime
	case "knowledge.docs.verify":
		return budgets.Docs
	case "knowledge.web.research":
		return budgets.Web
	case "knowledge.memory.recall":
		return budgets.Memory
	case "knowledge.runtime.observe":
		return budgets.Observe
	default:
		return NodeBudget{}
	}
}

func flowIDForGoals(
	goals []Goal,
) string {
	var canonical strings.Builder
	for _, goal := range goals {
		canonical.WriteString(goal.Facet)
		if goal.Required {
			canonical.WriteString(":required\n")
		} else {
			canonical.WriteString(":optional\n")
		}
		for _, source := range goal.Sources {
			canonical.WriteString(string(source))
			canonical.WriteByte(',')
		}
		canonical.WriteByte('\n')
		canonical.WriteString(string(goal.Freshness))
		canonical.WriteByte('\n')
		fmt.Fprintf(&canonical, "%d\n", goal.MinimumCoverage)
		fmt.Fprintf(&canonical, "%t\n", goal.HighRisk)
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return FlowID + ".goals." + hex.EncodeToString(sum[:8])
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
		Retry:   RetryPolicy{MaxAttempts: maxAttempts},
		Timeout: timeout,
	}
}
