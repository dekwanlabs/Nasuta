package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

var canonicalID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type NodeKind string

const (
	NodeAgent         NodeKind = "agent"
	NodeGate          NodeKind = "gate"
	NodeHumanApproval NodeKind = "human_approval"
	NodeJoin          NodeKind = "join"
	NodeVerifier      NodeKind = "verifier"
	NodeTransform     NodeKind = "transform"
)

type JoinMode string

const (
	JoinPayloadList  JoinMode = ""
	JoinEvidenceView JoinMode = "evidence_view"
)

type FailureMode string

const (
	FailFast         FailureMode = "fail_fast"
	CollectAvailable FailureMode = "collect_available"
)

type Completeness string

const (
	Complete    Completeness = "complete"
	Partial     Completeness = "partial"
	Unavailable Completeness = "unavailable"
)

type StopReason string

const (
	StopRequiredGoalsCovered  StopReason = "required_goals_covered"
	StopNoNewEvidence         StopReason = "no_new_evidence"
	StopNoAffordableTask      StopReason = "no_affordable_task"
	StopDuplicateEvidence     StopReason = "duplicate_evidence_limit"
	StopVerificationFailed    StopReason = "verification_failed"
	StopDeadlineExceeded      StopReason = "deadline_exceeded"
	StopBudgetExhausted       StopReason = "budget_exhausted"
	StopCapabilityUnavailable StopReason = "capability_unavailable"
	StopEvidenceInsufficient  StopReason = "evidence_insufficient"
	StopNeedsClarification    StopReason = "needs_clarification"
)

type Definition struct {
	ID            string                    `json:"id"`
	Version       int64                     `json:"version"`
	Purpose       string                    `json:"purpose"`
	InputSchema   agentapi.SchemaRef        `json:"input_schema"`
	OutputSchema  agentapi.SchemaRef        `json:"output_schema"`
	Nodes         []NodeDefinition          `json:"nodes"`
	Edges         []EdgeDefinition          `json:"edges"`
	Permissions   agentapi.PermissionPolicy `json:"permissions"`
	Budget        Budget                    `json:"budget"`
	FailurePolicy FailurePolicy             `json:"failure_policy"`
	ContentHash   string                    `json:"content_hash"`

	// Set only after a persisted pre-limit hash is verified.
	legacyExecutionBudget bool
}

type NodeDefinition struct {
	ID                       string                    `json:"id"`
	Kind                     NodeKind                  `json:"kind"`
	Agent                    agentapi.DefinitionRef    `json:"agent,omitempty"`
	Capability               agentapi.CapabilityRef    `json:"capability,omitempty"`
	Task                     *TaskDirective            `json:"task,omitempty"`
	CapabilityMaxConcurrency int                       `json:"capability_max_concurrency,omitempty"`
	RestrictVisibleTools     bool                      `json:"restrict_visible_tools,omitempty"`
	VisibleToolIDs           []string                  `json:"visible_tool_ids,omitempty"`
	InputSchema              agentapi.SchemaRef        `json:"input_schema"`
	OutputSchema             agentapi.SchemaRef        `json:"output_schema"`
	JoinMode                 JoinMode                  `json:"join_mode,omitempty"`
	RejectEvidenceConflicts  bool                      `json:"reject_evidence_conflicts,omitempty"`
	Verifier                 *VerifierSpec             `json:"verifier,omitempty"`
	Gate                     *GateSpec                 `json:"gate,omitempty"`
	TransformID              string                    `json:"transform_id,omitempty"`
	Permissions              agentapi.PermissionPolicy `json:"permissions"`
	Budget                   NodeBudget                `json:"budget"`
	Retry                    RetryPolicy               `json:"retry"`
	RetrySafe                bool                      `json:"retry_safe"`
	Timeout                  time.Duration             `json:"timeout"`
	Optional                 bool                      `json:"optional"`
}

// TaskDirective preserves the validated task semantics bound to an agent node.
type TaskDirective struct {
	Purpose              string                 `json:"purpose"`
	InvestigationGoalIDs []string               `json:"investigation_goal_ids,omitempty"`
	RequiredFacets       []string               `json:"required_facets,omitempty"`
	InputRefs            []agentapi.EvidenceRef `json:"input_refs"`
	ParallelGroup        string                 `json:"parallel_group,omitempty"`
}

// RetryPolicy bounds repeated execution of a node after a classified transient failure.
type RetryPolicy struct {
	MaxAttempts int           `json:"max_attempts"`
	Backoff     time.Duration `json:"backoff"`
}

type EdgeDefinition struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Required bool   `json:"required"`
}

type GateSpec struct {
	ID               string   `json:"id"`
	AllowedDecisions []string `json:"allowed_decisions"`
	ForwardInput     bool     `json:"forward_input,omitempty"`
}

// SubjectRequirement describes the evidence contract for one comparison entity.
type SubjectRequirement struct {
	EntityID        string                    `json:"entity_id"`
	Label           string                    `json:"label,omitempty"`
	Role            string                    `json:"role,omitempty"`
	Aliases         []string                  `json:"aliases,omitempty"`
	RequiredFacets  []string                  `json:"required_facets"`
	RequiredSources []agentapi.EvidenceSource `json:"required_sources,omitempty"`
}

func cloneSubjectRequirements(values []SubjectRequirement) []SubjectRequirement {
	if len(values) == 0 {
		return nil
	}
	out := make([]SubjectRequirement, len(values))
	for index, value := range values {
		out[index] = value
		out[index].Aliases = append([]string(nil), value.Aliases...)
		out[index].RequiredFacets = append([]string(nil), value.RequiredFacets...)
		out[index].RequiredSources = append([]agentapi.EvidenceSource(nil), value.RequiredSources...)
	}
	return out
}

// VerifierSpec fixes deterministic evidence acceptance policy.
type VerifierSpec struct {
	RequiredGoals            []string             `json:"required_goals,omitempty"`
	HighRiskGoals            []string             `json:"high_risk_goals,omitempty"`
	HighRiskMinimumTrustTier int                  `json:"high_risk_minimum_trust_tier,omitempty"`
	RejectEvidenceConflicts  bool                 `json:"reject_evidence_conflicts"`
	SubjectRequirements      []SubjectRequirement `json:"subject_requirements,omitempty"`
	// MaxPayloadTokens bounds one verified bundle passed to the next agent.
	MaxPayloadTokens int `json:"max_payload_tokens,omitempty"`
}

type Budget struct {
	MaxNodes       int `json:"max_nodes"`
	MaxParallelism int `json:"max_parallelism"`
	// Zero omission preserves hashes published before orchestration limits.
	MaxRounds         int           `json:"max_rounds,omitempty"`
	MaxDepth          int           `json:"max_depth,omitempty"`
	Timeout           time.Duration `json:"timeout"`
	MaxHandoffBytes   int64         `json:"max_handoff_bytes"`
	MaxDuplicateRatio float64       `json:"max_duplicate_ratio,omitempty"`
	MaxInputTokens    int64         `json:"max_input_tokens"`
	MaxOutputTokens   int64         `json:"max_output_tokens"`
	MaxTotalTokens    int64         `json:"max_total_tokens"`
	MaxToolCalls      int64         `json:"max_tool_calls"`
	MaxCostMicros     int64         `json:"max_cost_micros"`
	MaxRetries        int64         `json:"max_retries"`
}

// NodeBudget reserves one Attempt's maximum resource consumption.
type NodeBudget struct {
	MaxInputTokens  int64 `json:"max_input_tokens"`
	MaxOutputTokens int64 `json:"max_output_tokens"`
	MaxTotalTokens  int64 `json:"max_total_tokens"`
	MaxToolCalls    int64 `json:"max_tool_calls"`
	MaxCostMicros   int64 `json:"max_cost_micros"`
}

type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	ToolCalls       int64 `json:"tool_calls"`
	CostMicros      int64 `json:"cost_micros"`
	Retries         int64 `json:"retries"`
}

// IsZero reports whether this usage contributes nothing to the Workflow total.
func (usage Usage) IsZero() bool {
	return usage == (Usage{})
}

type FailurePolicy struct {
	Mode FailureMode `json:"mode"`
}

// WorkflowArtifact is an immutable secondary artifact emitted alongside a node handoff.
// It is kept out of the handoff payload/hash so large audit details do not enter
// downstream model context.
type WorkflowArtifact struct {
	ID             string             `json:"id"`
	WorkflowRunID  string             `json:"workflow_run_id"`
	ProducerNodeID string             `json:"producer_node_id"`
	Kind           string             `json:"kind"`
	Schema         agentapi.SchemaRef `json:"schema"`
	ContentHash    string             `json:"content_hash"`
	Content        []byte             `json:"content"`
}

type Handoff struct {
	ID                string                      `json:"id"`
	WorkflowRunID     string                      `json:"workflow_run_id"`
	ProducerNodeID    string                      `json:"producer_node_id"`
	ProducerRunID     string                      `json:"producer_run_id,omitempty"`
	Schema            agentapi.SchemaRef          `json:"schema"`
	Payload           json.RawMessage             `json:"payload"`
	References        []agentapi.Reference        `json:"references,omitempty"`
	EvidenceUnits     []tool.EvidenceUnit         `json:"evidence_units,omitempty"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts,omitempty"`
	Completeness      Completeness                `json:"completeness"`
	ContentHash       string                      `json:"content_hash"`
	CreatedAt         time.Time                   `json:"created_at"`
	Artifacts         []WorkflowArtifact          `json:"-"`
}

type GateDecision struct {
	GateID      string    `json:"gate_id"`
	SubjectHash string    `json:"subject_hash"`
	Decision    string    `json:"decision"`
	ReasonCodes []string  `json:"reason_codes,omitempty"`
	FindingIDs  []string  `json:"finding_ids,omitempty"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

type ApprovalDecision string

const (
	ApprovalApproved ApprovalDecision = "approved"
	ApprovalRejected ApprovalDecision = "rejected"
)

type Approval struct {
	WorkflowRunID    string           `json:"workflow_run_id"`
	NodeID           string           `json:"node_id"`
	Decision         ApprovalDecision `json:"decision"`
	ApproverUserID   int64            `json:"approver_user_id"`
	ApproverTenantID string           `json:"approver_tenant_id"`
	Comment          string           `json:"comment,omitempty"`
	DecidedAt        time.Time        `json:"decided_at"`
}

const (
	defaultNodeMaxAttempts = 1
	maxNodeMaxAttempts     = 10
	maxNodeRetryBackoff    = 30 * time.Second
)

// Prepare validates and hashes one immutable workflow definition.
func Prepare(definition Definition, schemas *agentapi.SchemaRegistry) (Definition, error) {
	return prepareDefinition(definition, schemas, false)
}

func prepareStored(
	definition Definition,
	schemas *agentapi.SchemaRegistry,
) (Definition, error) {
	return prepareDefinition(definition, schemas, true)
}

func prepareRuntime(
	definition Definition,
	schemas *agentapi.SchemaRegistry,
) (Definition, error) {
	return prepareDefinition(
		definition,
		schemas,
		definition.legacyExecutionBudget,
	)
}

func prepareDefinition(
	definition Definition,
	schemas *agentapi.SchemaRegistry,
	allowLegacyExecutionBudget bool,
) (Definition, error) {
	if schemas == nil {
		return Definition{}, fmt.Errorf("workflow schema registry is required")
	}
	prepared := cloneDefinition(definition)
	prepared.ID = strings.TrimSpace(prepared.ID)
	if !canonicalID.MatchString(prepared.ID) {
		return Definition{}, fmt.Errorf("workflow id %q is not canonical", definition.ID)
	}
	if prepared.Version <= 0 {
		return Definition{}, fmt.Errorf("workflow %q version must be positive", prepared.ID)
	}
	if strings.TrimSpace(prepared.Purpose) == "" {
		return Definition{}, fmt.Errorf("workflow %q purpose is required", prepared.ID)
	}
	if err := validateSchema("workflow input", prepared.InputSchema, schemas); err != nil {
		return Definition{}, err
	}
	if err := validateSchema("workflow output", prepared.OutputSchema, schemas); err != nil {
		return Definition{}, err
	}
	legacyExecutionBudget := prepared.Budget.MaxRounds == 0 &&
		prepared.Budget.MaxDepth == 0
	if prepared.Budget.MaxNodes <= 0 || prepared.Budget.MaxParallelism <= 0 ||
		(!legacyExecutionBudget &&
			(prepared.Budget.MaxRounds <= 0 || prepared.Budget.MaxDepth <= 0)) ||
		(legacyExecutionBudget && !allowLegacyExecutionBudget) ||
		prepared.Budget.Timeout <= 0 || prepared.Budget.MaxHandoffBytes <= 0 {
		return Definition{}, fmt.Errorf("workflow %q budgets must be positive", prepared.ID)
	}
	if err := validateBudget(prepared.ID, prepared.Budget); err != nil {
		return Definition{}, err
	}
	if len(prepared.Nodes) == 0 || len(prepared.Nodes) > prepared.Budget.MaxNodes {
		return Definition{}, fmt.Errorf("workflow %q node count exceeds its budget", prepared.ID)
	}
	if prepared.Budget.MaxParallelism > len(prepared.Nodes) {
		return Definition{}, fmt.Errorf("workflow %q parallelism exceeds its node count", prepared.ID)
	}
	if prepared.FailurePolicy.Mode != FailFast && prepared.FailurePolicy.Mode != CollectAvailable {
		return Definition{}, fmt.Errorf("workflow %q failure mode %q is invalid", prepared.ID, prepared.FailurePolicy.Mode)
	}
	if err := validatePermissions("workflow "+prepared.ID, prepared.Permissions); err != nil {
		return Definition{}, err
	}
	for index := range prepared.Nodes {
		node := prepared.Nodes[index]
		if err := scope.EnsureSubset(
			node.Permissions.Scopes,
			prepared.Permissions.Scopes,
		); err != nil {
			return Definition{}, fmt.Errorf(
				"workflow %q node %q permissions: %w",
				prepared.ID,
				node.ID,
				err,
			)
		}
		if err := validateNodeBudget(prepared.ID, prepared.Nodes[index], prepared.Budget); err != nil {
			return Definition{}, err
		}
		retry, err := normalizeRetryPolicy(prepared.Nodes[index].Retry)
		if err != nil {
			return Definition{}, fmt.Errorf(
				"workflow %q node %q retry policy: %w",
				prepared.ID, prepared.Nodes[index].ID, err,
			)
		}
		prepared.Nodes[index].Retry = retry
	}
	metadata, err := graph(prepared, schemas)
	if err != nil {
		return Definition{}, err
	}
	maxDepth := prepared.Budget.MaxDepth
	if legacyExecutionBudget {
		maxDepth = prepared.Budget.MaxNodes
	}
	if metadata.maxDepth > maxDepth {
		return Definition{}, fmt.Errorf(
			"workflow %q graph depth %d exceeds its budget %d",
			prepared.ID,
			metadata.maxDepth,
			maxDepth,
		)
	}
	hash, err := definitionHash(prepared)
	if err != nil {
		return Definition{}, err
	}
	if prepared.ContentHash != "" && prepared.ContentHash != hash {
		return Definition{}, fmt.Errorf("workflow %q content hash mismatch", prepared.ID)
	}
	prepared.ContentHash = hash
	prepared.legacyExecutionBudget = legacyExecutionBudget
	return prepared, nil
}

func validateBudget(workflowID string, budget Budget) error {
	if budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 || budget.MaxTotalTokens < 0 ||
		budget.MaxToolCalls < 0 || budget.MaxCostMicros < 0 || budget.MaxRetries < 0 {
		return fmt.Errorf("workflow %q resource budgets cannot be negative", workflowID)
	}
	if budget.MaxDuplicateRatio < 0 || budget.MaxDuplicateRatio > 1 {
		return fmt.Errorf(
			"workflow %q duplicate ratio must be within [0,1]",
			workflowID,
		)
	}
	return nil
}

func validateNodeBudget(
	workflowID string,
	node NodeDefinition,
	workflowBudget Budget,
) error {
	budget := node.Budget
	if budget.MaxInputTokens < 0 || budget.MaxOutputTokens < 0 || budget.MaxTotalTokens < 0 ||
		budget.MaxToolCalls < 0 || budget.MaxCostMicros < 0 {
		return fmt.Errorf("workflow %q node %q budgets cannot be negative", workflowID, node.ID)
	}
	if node.Kind != NodeAgent {
		return nil
	}
	switch {
	case workflowBudget.MaxInputTokens > 0 && budget.MaxInputTokens <= 0:
		return fmt.Errorf("workflow %q agent node %q input token budget is required", workflowID, node.ID)
	case workflowBudget.MaxOutputTokens > 0 && budget.MaxOutputTokens <= 0:
		return fmt.Errorf("workflow %q agent node %q output token budget is required", workflowID, node.ID)
	case workflowBudget.MaxTotalTokens > 0 && budget.MaxTotalTokens <= 0:
		return fmt.Errorf("workflow %q agent node %q total token budget is required", workflowID, node.ID)
	case workflowBudget.MaxCostMicros > 0 && budget.MaxCostMicros <= 0:
		return fmt.Errorf("workflow %q agent node %q cost budget is required", workflowID, node.ID)
	}
	return nil
}

// TopologicalOrder returns the stable Node ID order used by scheduling and joins.
func TopologicalOrder(definition Definition, schemas *agentapi.SchemaRegistry) ([]NodeDefinition, error) {
	prepared, err := Prepare(definition, schemas)
	if err != nil {
		return nil, err
	}
	metadata, err := graph(prepared, schemas)
	if err != nil {
		return nil, err
	}
	out := make([]NodeDefinition, 0, len(metadata.order))
	for _, id := range metadata.order {
		out = append(out, metadata.nodes[id])
	}
	return out, nil
}

// PrepareHandoff validates and hashes one detached immutable handoff.
func PrepareHandoff(
	handoff Handoff,
	maxBytes int64,
	schemas *agentapi.SchemaRegistry,
) (Handoff, error) {
	if schemas == nil {
		return Handoff{}, fmt.Errorf("handoff schema registry is required")
	}
	prepared := handoff
	prepared.Payload = append(json.RawMessage(nil), handoff.Payload...)
	prepared.References = append([]agentapi.Reference(nil), handoff.References...)
	prepared.EvidenceUnits = evidence.CloneUnits(handoff.EvidenceUnits)
	prepared.EvidenceConflicts = cloneConflicts(handoff.EvidenceConflicts)
	prepared.Artifacts = cloneWorkflowArtifacts(handoff.Artifacts)
	if strings.TrimSpace(prepared.WorkflowRunID) == "" || !canonicalID.MatchString(prepared.ProducerNodeID) {
		return Handoff{}, fmt.Errorf("handoff workflow run and producer node are required")
	}
	if err := validateSchema("handoff", prepared.Schema, schemas); err != nil {
		return Handoff{}, err
	}
	if maxBytes > 0 && int64(len(prepared.Payload)) > maxBytes {
		return Handoff{}, fmt.Errorf("handoff from %q exceeds %d bytes", prepared.ProducerNodeID, maxBytes)
	}
	if err := schemas.Validate(prepared.Schema, prepared.Payload); err != nil {
		return Handoff{}, fmt.Errorf("handoff from %q payload: %w", prepared.ProducerNodeID, err)
	}
	switch prepared.Completeness {
	case Complete, Partial, Unavailable:
	default:
		return Handoff{}, fmt.Errorf("handoff from %q completeness %q is invalid", prepared.ProducerNodeID, prepared.Completeness)
	}
	hash, err := handoffHash(prepared)
	if err != nil {
		return Handoff{}, err
	}
	if prepared.ContentHash != "" && prepared.ContentHash != hash {
		return Handoff{}, fmt.Errorf("handoff from %q content hash mismatch", prepared.ProducerNodeID)
	}
	prepared.ContentHash = hash
	if prepared.ID == "" {
		prepared.ID = "handoff_" + hash[:24]
	}
	if prepared.CreatedAt.IsZero() {
		prepared.CreatedAt = time.Now().UTC()
	}
	return prepared, nil
}

func cloneWorkflowArtifacts(artifacts []WorkflowArtifact) []WorkflowArtifact {
	if len(artifacts) == 0 {
		return nil
	}
	out := make([]WorkflowArtifact, len(artifacts))
	for index, artifact := range artifacts {
		out[index] = artifact
		out[index].Content = append([]byte(nil), artifact.Content...)
	}
	return out
}

// IntersectPermissions ensures delegation never expands any caller scope.
func IntersectPermissions(policies ...agentapi.PermissionPolicy) agentapi.PermissionPolicy {
	if len(policies) == 0 {
		return agentapi.PermissionPolicy{}
	}
	counts := make(map[string]int)
	for index, policy := range policies {
		seen := make(map[string]struct{}, len(policy.Scopes))
		for _, scope := range policy.Scopes {
			if _, duplicate := seen[scope]; duplicate {
				continue
			}
			seen[scope] = struct{}{}
			if index == 0 || counts[scope] == index {
				counts[scope]++
			}
		}
	}
	scopes := make([]string, 0, len(counts))
	for scope, count := range counts {
		if count == len(policies) {
			scopes = append(scopes, scope)
		}
	}
	sort.Strings(scopes)
	return agentapi.PermissionPolicy{Scopes: scopes}
}

type graphMetadata struct {
	nodes        map[string]NodeDefinition
	predecessors map[string][]string
	successors   map[string][]string
	required     map[string]bool
	depths       map[string]int
	maxDepth     int
	order        []string
}

func graph(definition Definition, schemas *agentapi.SchemaRegistry) (graphMetadata, error) {
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		if !canonicalID.MatchString(node.ID) {
			return graphMetadata{}, fmt.Errorf("workflow %q node id %q is not canonical", definition.ID, node.ID)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return graphMetadata{}, fmt.Errorf("workflow %q node %q is duplicated", definition.ID, node.ID)
		}
		if err := validateNode(definition.ID, node, schemas); err != nil {
			return graphMetadata{}, err
		}
		nodes[node.ID] = node
	}
	predecessors := make(map[string][]string, len(nodes))
	successors := make(map[string][]string, len(nodes))
	required := make(map[string]bool, len(definition.Edges))
	edgeKeys := make(map[string]struct{}, len(definition.Edges))
	for _, edge := range definition.Edges {
		from, fromOK := nodes[edge.From]
		to, toOK := nodes[edge.To]
		if !fromOK || !toOK {
			return graphMetadata{}, fmt.Errorf("workflow %q edge %q -> %q references an unknown node", definition.ID, edge.From, edge.To)
		}
		key := edge.From + "\x00" + edge.To
		if _, duplicate := edgeKeys[key]; duplicate {
			return graphMetadata{}, fmt.Errorf("workflow %q edge %q -> %q is duplicated", definition.ID, edge.From, edge.To)
		}
		edgeKeys[key] = struct{}{}
		required[key] = edge.Required
		if err := schemas.ValidateCompatibility(from.OutputSchema, to.InputSchema); err != nil {
			return graphMetadata{}, fmt.Errorf(
				"workflow %q edge %q -> %q has incompatible schemas: %w",
				definition.ID, edge.From, edge.To, err,
			)
		}
		predecessors[edge.To] = append(predecessors[edge.To], edge.From)
		successors[edge.From] = append(successors[edge.From], edge.To)
	}
	for id := range nodes {
		sort.Strings(successors[id])
	}
	entries := make([]string, 0)
	terminals := make([]string, 0)
	for id := range nodes {
		if len(predecessors[id]) == 0 {
			entries = append(entries, id)
		}
		if len(successors[id]) == 0 {
			terminals = append(terminals, id)
		}
	}
	if len(entries) == 0 || len(terminals) == 0 {
		return graphMetadata{}, fmt.Errorf("workflow %q must have entry and terminal nodes", definition.ID)
	}
	sort.Strings(entries)
	sort.Strings(terminals)
	for _, entry := range entries {
		if err := schemas.ValidateCompatibility(definition.InputSchema, nodes[entry].InputSchema); err != nil {
			return graphMetadata{}, fmt.Errorf(
				"workflow %q input is incompatible with entry node %q: %w",
				definition.ID, entry, err,
			)
		}
	}
	for nodeID, node := range nodes {
		if node.Kind != NodeHumanApproval &&
			(node.Kind != NodeGate || !node.Gate.ForwardInput) {
			continue
		}
		if node.Kind == NodeGate && len(predecessors[nodeID]) != 1 {
			return graphMetadata{}, fmt.Errorf(
				"workflow %q input-forwarding node %q requires exactly one predecessor",
				definition.ID,
				nodeID,
			)
		}
		if len(predecessors[nodeID]) > 1 {
			continue
		}
		producerSchema := definition.InputSchema
		if len(predecessors[nodeID]) == 1 {
			producerSchema = nodes[predecessors[nodeID][0]].OutputSchema
		}
		if err := schemas.ValidateCompatibility(producerSchema, node.OutputSchema); err != nil {
			return graphMetadata{}, fmt.Errorf(
				"workflow %q input-forwarding node %q cannot pass through its input: %w",
				definition.ID, nodeID, err,
			)
		}
	}
	if len(terminals) == 1 {
		terminal := terminals[0]
		if err := schemas.ValidateCompatibility(nodes[terminal].OutputSchema, definition.OutputSchema); err != nil {
			return graphMetadata{}, fmt.Errorf(
				"workflow %q terminal node %q is incompatible with its output: %w",
				definition.ID, terminal, err,
			)
		}
	}
	indegree := make(map[string]int, len(nodes))
	depths := make(map[string]int, len(nodes))
	for id := range nodes {
		indegree[id] = len(predecessors[id])
		if indegree[id] == 0 {
			depths[id] = 1
		}
	}
	ready := append([]string(nil), entries...)
	order := make([]string, 0, len(nodes))
	maxDepth := 0
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		if depths[id] > maxDepth {
			maxDepth = depths[id]
		}
		for _, next := range successors[id] {
			if depths[next] < depths[id]+1 {
				depths[next] = depths[id] + 1
			}
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(nodes) {
		return graphMetadata{}, fmt.Errorf("workflow %q contains a cycle", definition.ID)
	}
	if len(order) != len(reachable(entries, successors)) {
		return graphMetadata{}, fmt.Errorf("workflow %q contains an unreachable node", definition.ID)
	}
	return graphMetadata{
		nodes: nodes, predecessors: predecessors, successors: successors,
		required: required, depths: depths, maxDepth: maxDepth, order: order,
	}, nil
}

func reachable(entries []string, successors map[string][]string) map[string]struct{} {
	seen := make(map[string]struct{}, len(successors))
	queue := append([]string(nil), entries...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		queue = append(queue, successors[id]...)
	}
	return seen
}

func validateNode(
	workflowID string,
	node NodeDefinition,
	schemas *agentapi.SchemaRegistry,
) error {
	if node.Timeout <= 0 {
		return fmt.Errorf("workflow %q node %q timeout must be positive", workflowID, node.ID)
	}
	if err := validateSchema("node "+node.ID+" input", node.InputSchema, schemas); err != nil {
		return err
	}
	if err := validateSchema("node "+node.ID+" output", node.OutputSchema, schemas); err != nil {
		return err
	}
	if err := validatePermissions("node "+node.ID, node.Permissions); err != nil {
		return err
	}
	if _, err := normalizeRetryPolicy(node.Retry); err != nil {
		return fmt.Errorf("workflow %q node %q retry policy: %w", workflowID, node.ID, err)
	}
	if node.Kind != NodeJoin && node.JoinMode != JoinPayloadList {
		return fmt.Errorf(
			"workflow %q node %q kind %q cannot use join mode %q",
			workflowID,
			node.ID,
			node.Kind,
			node.JoinMode,
		)
	}
	switch node.Kind {
	case NodeAgent:
		if !canonicalID.MatchString(node.Agent.ID) || node.Agent.Version <= 0 {
			return fmt.Errorf("workflow %q agent node %q requires an exact agent definition", workflowID, node.ID)
		}
		if node.Capability.ID != "" {
			if !canonicalID.MatchString(node.Capability.ID) ||
				node.Capability.Version <= 0 ||
				node.CapabilityMaxConcurrency <= 0 {
				return fmt.Errorf(
					"workflow %q agent node %q requires an exact capability binding",
					workflowID,
					node.ID,
				)
			}
			if !node.RestrictVisibleTools {
				return fmt.Errorf(
					"workflow %q capability node %q must restrict visible tools",
					workflowID,
					node.ID,
				)
			}
		} else if node.Capability.Version != 0 || node.CapabilityMaxConcurrency != 0 {
			return fmt.Errorf("workflow %q agent node %q capability binding is incomplete", workflowID, node.ID)
		}
		if err := validateCanonical(
			"node "+node.ID+" visible tool",
			node.VisibleToolIDs,
		); err != nil {
			return err
		}
		if err := validateTaskDirective(workflowID, node); err != nil {
			return err
		}
	case NodeGate:
		if node.Task != nil {
			return fmt.Errorf("workflow %q non-agent node %q cannot have a task directive", workflowID, node.ID)
		}
		if node.Gate == nil || !canonicalID.MatchString(node.Gate.ID) || len(node.Gate.AllowedDecisions) == 0 {
			return fmt.Errorf("workflow %q gate node %q requires a gate policy", workflowID, node.ID)
		}
		if err := validateCanonical("gate "+node.Gate.ID+" decision", node.Gate.AllowedDecisions); err != nil {
			return err
		}
	case NodeTransform:
		if node.Task != nil {
			return fmt.Errorf("workflow %q non-agent node %q cannot have a task directive", workflowID, node.ID)
		}
		if !canonicalID.MatchString(node.TransformID) {
			return fmt.Errorf("workflow %q transform node %q requires a registered transform", workflowID, node.ID)
		}
	case NodeVerifier:
		if node.Task != nil {
			return fmt.Errorf("workflow %q non-agent node %q cannot have a task directive", workflowID, node.ID)
		}
		if node.Verifier == nil {
			return fmt.Errorf("workflow %q verifier node %q requires a verifier policy", workflowID, node.ID)
		}
		if err := validateCanonical(
			"node "+node.ID+" verifier required goal",
			node.Verifier.RequiredGoals,
		); err != nil {
			return err
		}
		if err := validateCanonical(
			"node "+node.ID+" verifier high-risk goal",
			node.Verifier.HighRiskGoals,
		); err != nil {
			return err
		}
		requiredGoals := make(
			map[string]struct{},
			len(node.Verifier.RequiredGoals),
		)
		for _, goal := range node.Verifier.RequiredGoals {
			requiredGoals[goal] = struct{}{}
		}
		for _, goal := range node.Verifier.HighRiskGoals {
			if _, required := requiredGoals[goal]; !required {
				return fmt.Errorf(
					"workflow %q verifier node %q high-risk goal %q must also be required",
					workflowID,
					node.ID,
					goal,
				)
			}
		}
		seenSubjects := make(map[string]struct{}, len(node.Verifier.SubjectRequirements))
		for _, subject := range node.Verifier.SubjectRequirements {
			if !canonicalID.MatchString(subject.EntityID) {
				return fmt.Errorf(
					"workflow %q verifier node %q subject entity %q is not canonical",
					workflowID, node.ID, subject.EntityID,
				)
			}
			if _, duplicate := seenSubjects[subject.EntityID]; duplicate {
				return fmt.Errorf(
					"workflow %q verifier node %q subject entity %q is duplicated",
					workflowID, node.ID, subject.EntityID,
				)
			}
			seenSubjects[subject.EntityID] = struct{}{}
			if len(subject.RequiredFacets) == 0 {
				return fmt.Errorf(
					"workflow %q verifier node %q subject entity %q requires facets",
					workflowID, node.ID, subject.EntityID,
				)
			}
			if err := validateCanonical(
				"node "+node.ID+" verifier subject facet",
				subject.RequiredFacets,
			); err != nil {
				return err
			}
			for _, source := range subject.RequiredSources {
				switch source {
				case agentapi.EvidenceSourceInternal, agentapi.EvidenceSourceMemory,
					agentapi.EvidenceSourceWeb, agentapi.EvidenceSourceRuntime:
				default:
					return fmt.Errorf(
						"workflow %q verifier node %q subject entity %q has invalid evidence source %q",
						workflowID, node.ID, subject.EntityID, source,
					)
				}
			}
		}
		if node.Verifier.HighRiskMinimumTrustTier < 0 ||
			node.Verifier.HighRiskMinimumTrustTier > 100 {
			return fmt.Errorf(
				"workflow %q verifier node %q high-risk minimum trust tier must be between 0 and 100",
				workflowID,
				node.ID,
			)
		}
		if node.Verifier.MaxPayloadTokens < 0 {
			return fmt.Errorf(
				"workflow %q verifier node %q payload token budget cannot be negative",
				workflowID,
				node.ID,
			)
		}
	case NodeJoin:
		if node.Task != nil {
			return fmt.Errorf("workflow %q non-agent node %q cannot have a task directive", workflowID, node.ID)
		}
		if node.JoinMode != JoinPayloadList && node.JoinMode != JoinEvidenceView {
			return fmt.Errorf(
				"workflow %q join node %q mode %q is invalid",
				workflowID,
				node.ID,
				node.JoinMode,
			)
		}
		if node.RejectEvidenceConflicts && node.JoinMode != JoinEvidenceView {
			return fmt.Errorf(
				"workflow %q join node %q can reject evidence conflicts only in evidence view mode",
				workflowID,
				node.ID,
			)
		}
	case NodeHumanApproval:
		if node.Task != nil {
			return fmt.Errorf("workflow %q non-agent node %q cannot have a task directive", workflowID, node.ID)
		}
	default:
		return fmt.Errorf("workflow %q node %q kind %q is invalid", workflowID, node.ID, node.Kind)
	}
	return nil
}

func validateTaskDirective(workflowID string, node NodeDefinition) error {
	if node.Task == nil {
		return nil
	}
	if strings.TrimSpace(node.Task.Purpose) == "" {
		return fmt.Errorf("workflow %q agent node %q task purpose is required", workflowID, node.ID)
	}
	if err := validateCanonical(
		"node "+node.ID+" task investigation goal",
		node.Task.InvestigationGoalIDs,
	); err != nil {
		return err
	}
	if err := validateCanonical(
		"node "+node.ID+" task required facet",
		node.Task.RequiredFacets,
	); err != nil {
		return err
	}
	if node.Task.ParallelGroup != "" && !canonicalID.MatchString(node.Task.ParallelGroup) {
		return fmt.Errorf(
			"workflow %q agent node %q task parallel group %q is not canonical",
			workflowID,
			node.ID,
			node.Task.ParallelGroup,
		)
	}
	if err := validateEvidenceRefs(node.ID, node.Task.InputRefs); err != nil {
		return err
	}
	return nil
}

func normalizeRetryPolicy(policy RetryPolicy) (RetryPolicy, error) {
	if policy.MaxAttempts == 0 {
		policy.MaxAttempts = defaultNodeMaxAttempts
	}
	if policy.MaxAttempts < 0 {
		return RetryPolicy{}, fmt.Errorf("max attempts cannot be negative")
	}
	if policy.MaxAttempts > maxNodeMaxAttempts {
		return RetryPolicy{}, fmt.Errorf("max attempts cannot exceed %d", maxNodeMaxAttempts)
	}
	if policy.Backoff < 0 {
		return RetryPolicy{}, fmt.Errorf("backoff cannot be negative")
	}
	if policy.Backoff > maxNodeRetryBackoff {
		return RetryPolicy{}, fmt.Errorf("backoff cannot exceed %s", maxNodeRetryBackoff)
	}
	return policy, nil
}

func validateSchema(
	label string,
	schema agentapi.SchemaRef,
	schemas *agentapi.SchemaRegistry,
) error {
	if !canonicalID.MatchString(schema.ID) || schema.Version <= 0 {
		return fmt.Errorf("%s schema is invalid", label)
	}
	if _, err := schemas.Resolve(schema); err != nil {
		return fmt.Errorf("%s schema: %w", label, err)
	}
	return nil
}

func validatePermissions(label string, policy agentapi.PermissionPolicy) error {
	if err := scope.Validate(policy.Scopes); err != nil {
		return fmt.Errorf("%s permissions: %w", label, err)
	}
	return nil
}

func validateCanonical(label string, values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !canonicalID.MatchString(value) {
			return fmt.Errorf("%s %q is not canonical", label, value)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s %q is duplicated", label, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func definitionHash(definition Definition) (string, error) {
	definition.ContentHash = ""
	payload, err := json.Marshal(definition)
	if err != nil {
		return "", fmt.Errorf("marshal workflow %q: %w", definition.ID, err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func handoffHash(handoff Handoff) (string, error) {
	handoff.ID = ""
	handoff.ContentHash = ""
	handoff.CreatedAt = time.Time{}
	payload, err := json.Marshal(handoff)
	if err != nil {
		return "", fmt.Errorf("marshal handoff: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func cloneDefinition(definition Definition) Definition {
	definition.Nodes = append([]NodeDefinition(nil), definition.Nodes...)
	for index := range definition.Nodes {
		node := &definition.Nodes[index]
		node.Permissions.Scopes = append([]string(nil), node.Permissions.Scopes...)
		node.VisibleToolIDs = append([]string(nil), node.VisibleToolIDs...)
		if node.Task != nil {
			task := *node.Task
			task.InvestigationGoalIDs = append(
				[]string(nil),
				task.InvestigationGoalIDs...,
			)
			task.RequiredFacets = append([]string(nil), task.RequiredFacets...)
			task.InputRefs = cloneEvidenceRefs(task.InputRefs)
			node.Task = &task
		}
		if node.Gate != nil {
			gate := *node.Gate
			gate.AllowedDecisions = append([]string(nil), gate.AllowedDecisions...)
			node.Gate = &gate
		}
		if node.Verifier != nil {
			verifier := *node.Verifier
			verifier.RequiredGoals = append([]string(nil), verifier.RequiredGoals...)
			verifier.HighRiskGoals = append([]string(nil), verifier.HighRiskGoals...)
			verifier.SubjectRequirements = cloneSubjectRequirements(verifier.SubjectRequirements)
			node.Verifier = &verifier
		}
	}
	definition.Edges = append([]EdgeDefinition(nil), definition.Edges...)
	definition.Permissions.Scopes = append([]string(nil), definition.Permissions.Scopes...)
	return definition
}

func cloneEvidenceRefs(
	refs []agentapi.EvidenceRef,
) []agentapi.EvidenceRef {
	if refs == nil {
		return nil
	}
	return append(make([]agentapi.EvidenceRef, 0, len(refs)), refs...)
}
