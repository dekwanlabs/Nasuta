package agentworkflow

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
)

var canonicalID = regexp.MustCompile(`^[a-z][a-z0-9]*(?:[._-][a-z0-9]+)*$`)

type NodeKind string

const (
	NodeAgent         NodeKind = "agent"
	NodeGate          NodeKind = "gate"
	NodeHumanApproval NodeKind = "human_approval"
	NodeJoin          NodeKind = "join"
	NodeTransform     NodeKind = "transform"
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

type WorkflowDefinition struct {
	ID            string                    `json:"id"`
	Version       int64                     `json:"version"`
	Purpose       string                    `json:"purpose"`
	InputSchema   agentapi.SchemaRef        `json:"input_schema"`
	OutputSchema  agentapi.SchemaRef        `json:"output_schema"`
	Nodes         []NodeDefinition          `json:"nodes"`
	Edges         []EdgeDefinition          `json:"edges"`
	Permissions   agentapi.PermissionPolicy `json:"permissions"`
	Budget        WorkflowBudget            `json:"budget"`
	FailurePolicy WorkflowFailurePolicy     `json:"failure_policy"`
	ContentHash   string                    `json:"content_hash"`
}

type NodeDefinition struct {
	ID           string                    `json:"id"`
	Kind         NodeKind                  `json:"kind"`
	Agent        agentapi.DefinitionRef    `json:"agent,omitempty"`
	InputSchema  agentapi.SchemaRef        `json:"input_schema"`
	OutputSchema agentapi.SchemaRef        `json:"output_schema"`
	Gate         *GateSpec                 `json:"gate,omitempty"`
	TransformID  string                    `json:"transform_id,omitempty"`
	Permissions  agentapi.PermissionPolicy `json:"permissions"`
	Timeout      time.Duration             `json:"timeout"`
	Optional     bool                      `json:"optional"`
}

type EdgeDefinition struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Required bool   `json:"required"`
}

type GateSpec struct {
	ID               string   `json:"id"`
	AllowedDecisions []string `json:"allowed_decisions"`
}

type WorkflowBudget struct {
	MaxNodes        int           `json:"max_nodes"`
	MaxParallelism  int           `json:"max_parallelism"`
	Timeout         time.Duration `json:"timeout"`
	MaxHandoffBytes int64         `json:"max_handoff_bytes"`
}

type WorkflowFailurePolicy struct {
	Mode FailureMode `json:"mode"`
}

type Handoff struct {
	ID             string               `json:"id"`
	WorkflowRunID  string               `json:"workflow_run_id"`
	ProducerNodeID string               `json:"producer_node_id"`
	ProducerRunID  string               `json:"producer_run_id,omitempty"`
	Schema         agentapi.SchemaRef   `json:"schema"`
	Payload        json.RawMessage      `json:"payload"`
	References     []agentapi.Reference `json:"references,omitempty"`
	Completeness   Completeness         `json:"completeness"`
	ContentHash    string               `json:"content_hash"`
	CreatedAt      time.Time            `json:"created_at"`
}

type GateDecision struct {
	GateID      string    `json:"gate_id"`
	SubjectHash string    `json:"subject_hash"`
	Decision    string    `json:"decision"`
	ReasonCodes []string  `json:"reason_codes,omitempty"`
	FindingIDs  []string  `json:"finding_ids,omitempty"`
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// Prepare validates and hashes one immutable workflow definition.
func Prepare(definition WorkflowDefinition) (WorkflowDefinition, error) {
	prepared := cloneDefinition(definition)
	prepared.ID = strings.TrimSpace(prepared.ID)
	if !canonicalID.MatchString(prepared.ID) {
		return WorkflowDefinition{}, fmt.Errorf("workflow id %q is not canonical", definition.ID)
	}
	if prepared.Version <= 0 {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q version must be positive", prepared.ID)
	}
	if strings.TrimSpace(prepared.Purpose) == "" {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q purpose is required", prepared.ID)
	}
	if err := validateSchema("workflow input", prepared.InputSchema); err != nil {
		return WorkflowDefinition{}, err
	}
	if err := validateSchema("workflow output", prepared.OutputSchema); err != nil {
		return WorkflowDefinition{}, err
	}
	if prepared.Budget.MaxNodes <= 0 || prepared.Budget.MaxParallelism <= 0 ||
		prepared.Budget.Timeout <= 0 || prepared.Budget.MaxHandoffBytes <= 0 {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q budgets must be positive", prepared.ID)
	}
	if len(prepared.Nodes) == 0 || len(prepared.Nodes) > prepared.Budget.MaxNodes {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q node count exceeds its budget", prepared.ID)
	}
	if prepared.Budget.MaxParallelism > len(prepared.Nodes) {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q parallelism exceeds its node count", prepared.ID)
	}
	if prepared.FailurePolicy.Mode != FailFast && prepared.FailurePolicy.Mode != CollectAvailable {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q failure mode %q is invalid", prepared.ID, prepared.FailurePolicy.Mode)
	}
	if err := validatePermissions("workflow "+prepared.ID, prepared.Permissions); err != nil {
		return WorkflowDefinition{}, err
	}
	if _, err := graph(prepared); err != nil {
		return WorkflowDefinition{}, err
	}
	hash, err := definitionHash(prepared)
	if err != nil {
		return WorkflowDefinition{}, err
	}
	if prepared.ContentHash != "" && prepared.ContentHash != hash {
		return WorkflowDefinition{}, fmt.Errorf("workflow %q content hash mismatch", prepared.ID)
	}
	prepared.ContentHash = hash
	return prepared, nil
}

// TopologicalOrder returns the stable Node ID order used by scheduling and joins.
func TopologicalOrder(definition WorkflowDefinition) ([]NodeDefinition, error) {
	prepared, err := Prepare(definition)
	if err != nil {
		return nil, err
	}
	metadata, err := graph(prepared)
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
func PrepareHandoff(handoff Handoff, maxBytes int64) (Handoff, error) {
	prepared := handoff
	prepared.Payload = append(json.RawMessage(nil), handoff.Payload...)
	prepared.References = append([]agentapi.Reference(nil), handoff.References...)
	if strings.TrimSpace(prepared.WorkflowRunID) == "" || !canonicalID.MatchString(prepared.ProducerNodeID) {
		return Handoff{}, fmt.Errorf("handoff workflow run and producer node are required")
	}
	if err := validateSchema("handoff", prepared.Schema); err != nil {
		return Handoff{}, err
	}
	if !json.Valid(prepared.Payload) {
		return Handoff{}, fmt.Errorf("handoff from %q has invalid JSON payload", prepared.ProducerNodeID)
	}
	if maxBytes > 0 && int64(len(prepared.Payload)) > maxBytes {
		return Handoff{}, fmt.Errorf("handoff from %q exceeds %d bytes", prepared.ProducerNodeID, maxBytes)
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
	order        []string
}

func graph(definition WorkflowDefinition) (graphMetadata, error) {
	nodes := make(map[string]NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		if !canonicalID.MatchString(node.ID) {
			return graphMetadata{}, fmt.Errorf("workflow %q node id %q is not canonical", definition.ID, node.ID)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return graphMetadata{}, fmt.Errorf("workflow %q node %q is duplicated", definition.ID, node.ID)
		}
		if err := validateNode(definition.ID, node); err != nil {
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
		if from.OutputSchema != to.InputSchema {
			return graphMetadata{}, fmt.Errorf("workflow %q edge %q -> %q has incompatible schemas", definition.ID, edge.From, edge.To)
		}
		predecessors[edge.To] = append(predecessors[edge.To], edge.From)
		successors[edge.From] = append(successors[edge.From], edge.To)
	}
	for id := range nodes {
		sort.Strings(predecessors[id])
		sort.Strings(successors[id])
	}
	entries := make([]string, 0)
	terminals := 0
	for id := range nodes {
		if len(predecessors[id]) == 0 {
			entries = append(entries, id)
		}
		if len(successors[id]) == 0 {
			terminals++
		}
	}
	if len(entries) == 0 || terminals == 0 {
		return graphMetadata{}, fmt.Errorf("workflow %q must have entry and terminal nodes", definition.ID)
	}
	sort.Strings(entries)
	indegree := make(map[string]int, len(nodes))
	for id := range nodes {
		indegree[id] = len(predecessors[id])
	}
	ready := append([]string(nil), entries...)
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, next := range successors[id] {
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
		required: required, order: order,
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

func validateNode(workflowID string, node NodeDefinition) error {
	if node.Timeout <= 0 {
		return fmt.Errorf("workflow %q node %q timeout must be positive", workflowID, node.ID)
	}
	if err := validateSchema("node "+node.ID+" input", node.InputSchema); err != nil {
		return err
	}
	if err := validateSchema("node "+node.ID+" output", node.OutputSchema); err != nil {
		return err
	}
	if err := validatePermissions("node "+node.ID, node.Permissions); err != nil {
		return err
	}
	switch node.Kind {
	case NodeAgent:
		if !canonicalID.MatchString(node.Agent.ID) || node.Agent.Version <= 0 {
			return fmt.Errorf("workflow %q agent node %q requires an exact agent definition", workflowID, node.ID)
		}
	case NodeGate:
		if node.Gate == nil || !canonicalID.MatchString(node.Gate.ID) || len(node.Gate.AllowedDecisions) == 0 {
			return fmt.Errorf("workflow %q gate node %q requires a gate policy", workflowID, node.ID)
		}
		if err := validateCanonicalValues("gate "+node.Gate.ID+" decision", node.Gate.AllowedDecisions); err != nil {
			return err
		}
	case NodeTransform:
		if !canonicalID.MatchString(node.TransformID) {
			return fmt.Errorf("workflow %q transform node %q requires a registered transform", workflowID, node.ID)
		}
	case NodeHumanApproval, NodeJoin:
	default:
		return fmt.Errorf("workflow %q node %q kind %q is invalid", workflowID, node.ID, node.Kind)
	}
	return nil
}

func validateSchema(label string, schema agentapi.SchemaRef) error {
	if !canonicalID.MatchString(schema.ID) || schema.Version <= 0 {
		return fmt.Errorf("%s schema is invalid", label)
	}
	return nil
}

func validatePermissions(label string, policy agentapi.PermissionPolicy) error {
	return validateCanonicalValues(label+" permission", policy.Scopes)
}

func validateCanonicalValues(label string, values []string) error {
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

func definitionHash(definition WorkflowDefinition) (string, error) {
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

func cloneDefinition(definition WorkflowDefinition) WorkflowDefinition {
	definition.Nodes = append([]NodeDefinition(nil), definition.Nodes...)
	for index := range definition.Nodes {
		node := &definition.Nodes[index]
		node.Permissions.Scopes = append([]string(nil), node.Permissions.Scopes...)
		if node.Gate != nil {
			gate := *node.Gate
			gate.AllowedDecisions = append([]string(nil), gate.AllowedDecisions...)
			node.Gate = &gate
		}
	}
	definition.Edges = append([]EdgeDefinition(nil), definition.Edges...)
	definition.Permissions.Scopes = append([]string(nil), definition.Permissions.Scopes...)
	return definition
}
