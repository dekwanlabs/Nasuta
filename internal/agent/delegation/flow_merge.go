package delegation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const (
	flowMergePartialUncertainty = "merged flow contains partial or unresolved evidence"
	flowMergeMissingNode        = "one or more flow hops could not be mapped to a canonical node"
)

// MergeFlowIRs deterministically combines independently investigated FlowIRs.
// Node identity is based on normalized kind and label rather than a model-owned
// node ID; this prevents two children from creating duplicate representations of
// the same responsibility. Conflicting evidence is always resolved
// conservatively: unresolved beats inferred, and inferred beats verified.
func MergeFlowIRs(flows []agentapi.FlowIR) (*agentapi.FlowIR, error) {
	if len(flows) == 0 {
		return nil, nil
	}
	state := newFlowMergeState()
	for flowIndex := range flows {
		if err := mergeFlowInto(state, flows[flowIndex]); err != nil {
			return nil, fmt.Errorf("flow %d: %w", flowIndex, err)
		}
	}
	merged := state.finalize()
	if err := validateFlowIR(merged); err != nil {
		return nil, fmt.Errorf("merged flow: %w", err)
	}
	return merged, nil
}

type nodeAggregate struct {
	key         string
	subject     string
	label       string
	kind        string
	evidenceRef map[string]struct{}
}

type edgeAggregate struct {
	edge        agentapi.FlowEdge
	evidenceRef map[string]struct{}
	states      map[string]struct{}
}

type flowMergeState struct {
	nodes         map[string]*nodeAggregate
	edges         map[string]*edgeAggregate
	subjects      map[string]string
	openHops      map[string]string
	uncertainties map[string]string
	status        string
	confidence    string
}

func newFlowMergeState() *flowMergeState {
	return &flowMergeState{
		nodes:         make(map[string]*nodeAggregate),
		edges:         make(map[string]*edgeAggregate),
		subjects:      make(map[string]string),
		openHops:      make(map[string]string),
		uncertainties: make(map[string]string),
		status:        "complete",
		confidence:    "high",
	}
}

func mergeFlowInto(state *flowMergeState, flow agentapi.FlowIR) error {
	if err := validateFlowIR(&flow); err != nil {
		return err
	}
	subject := mergeFlowMetadata(state, flow)
	idToKey := mergeFlowNodes(state, flow, subject)
	mergeFlowEdges(state, flow, subject, idToKey)
	return nil
}

func mergeFlowMetadata(state *flowMergeState, flow agentapi.FlowIR) string {
	subject := normalizeFlowText(flow.Subject)
	if subject != "" {
		state.subjects[strings.ToLower(subject)] = subject
	}
	if flow.Status == "partial" {
		state.status = "partial"
	}
	if flow.Confidence == "low" || (flow.Confidence == "medium" && state.confidence == "high") {
		state.confidence = flow.Confidence
	}
	for _, uncertainty := range flow.Uncertainties {
		if value := normalizeFlowText(uncertainty); value != "" {
			state.uncertainties[strings.ToLower(value)] = value
		}
	}
	for _, hop := range flow.OpenHops {
		if value := normalizeFlowText(hop); value != "" {
			state.openHops[strings.ToLower(value)] = value
		}
	}
	return subject
}

func mergeFlowNodes(state *flowMergeState, flow agentapi.FlowIR, subject string) map[string]string {
	idToKey := make(map[string]string, len(flow.Nodes))
	for _, node := range flow.Nodes {
		label := normalizeFlowText(node.Label)
		kind := normalizeFlowText(node.Kind)
		key := flowNodeKey(subject, kind, label)
		idToKey[node.ID] = key
		aggregate := state.nodes[key]
		if aggregate == nil {
			aggregate = &nodeAggregate{
				key: key, subject: subject, label: label, kind: kind,
				evidenceRef: make(map[string]struct{}),
			}
			state.nodes[key] = aggregate
		}
		for _, reference := range node.EvidenceRefs {
			if value := strings.TrimSpace(reference); value != "" {
				aggregate.evidenceRef[value] = struct{}{}
			}
		}
	}
	return idToKey
}

func mergeFlowEdges(state *flowMergeState, flow agentapi.FlowIR, subject string, idToKey map[string]string) {
	for _, edge := range flow.Edges {
		from, fromOK := idToKey[edge.From]
		to, toOK := idToKey[edge.To]
		if !fromOK || !toOK {
			state.status = "partial"
			state.uncertainties[strings.ToLower(flowMergeMissingNode)] = flowMergeMissingNode
			continue
		}
		protocol := normalizeFlowText(edge.Protocol)
		syncMode := normalizeFlowText(edge.SyncMode)
		edgeKey := strings.Join([]string{subject, from, to, protocol, syncMode}, "\x00")
		aggregate := state.edges[edgeKey]
		if aggregate == nil {
			aggregate = &edgeAggregate{
				edge: agentapi.FlowEdge{
					From: from, To: to, Protocol: protocol, SyncMode: syncMode,
					EvidenceState: normalizeEvidenceState(edge.EvidenceState),
				},
				evidenceRef: make(map[string]struct{}),
				states:      make(map[string]struct{}),
			}
			state.edges[edgeKey] = aggregate
		}
		stateValue := normalizeEvidenceState(edge.EvidenceState)
		aggregate.states[stateValue] = struct{}{}
		if evidenceRank(stateValue) > evidenceRank(aggregate.edge.EvidenceState) {
			aggregate.edge.EvidenceState = stateValue
		}
		for _, reference := range edge.EvidenceRefs {
			if value := strings.TrimSpace(reference); value != "" {
				aggregate.evidenceRef[value] = struct{}{}
			}
		}
	}
}

func (state *flowMergeState) finalize() *agentapi.FlowIR {
	if len(state.openHops) > 0 {
		state.status = "partial"
	}
	for _, edge := range state.edges {
		if edge.edge.EvidenceState != "verified" {
			state.status = "partial"
			state.uncertainties[strings.ToLower(flowMergePartialUncertainty)] = flowMergePartialUncertainty
		}
	}

	canonicalNodes := make([]agentapi.FlowNode, 0, len(state.nodes))
	canonicalIDByKey := make(map[string]string, len(state.nodes))
	for key, aggregate := range state.nodes {
		id := canonicalFlowNodeID(key)
		canonicalIDByKey[key] = id
		canonicalNodes = append(canonicalNodes, agentapi.FlowNode{
			ID: id, Label: aggregate.label, Kind: aggregate.kind,
			EvidenceRefs: sortedSet(aggregate.evidenceRef),
		})
	}
	sort.Slice(canonicalNodes, func(i, j int) bool {
		return canonicalNodes[i].ID < canonicalNodes[j].ID
	})

	canonicalEdges := make([]agentapi.FlowEdge, 0, len(state.edges))
	for _, aggregate := range state.edges {
		edge := aggregate.edge
		edge.From = canonicalIDByKey[edge.From]
		edge.To = canonicalIDByKey[edge.To]
		edge.EvidenceRefs = sortedSet(aggregate.evidenceRef)
		if edge.EvidenceState == "verified" && len(edge.EvidenceRefs) == 0 {
			edge.EvidenceState = "unresolved"
			state.status = "partial"
			state.uncertainties[strings.ToLower(flowMergePartialUncertainty)] = flowMergePartialUncertainty
		}
		canonicalEdges = append(canonicalEdges, edge)
	}
	sort.Slice(canonicalEdges, func(i, j int) bool {
		left := strings.Join([]string{canonicalEdges[i].From, canonicalEdges[i].To, canonicalEdges[i].Protocol, canonicalEdges[i].SyncMode, canonicalEdges[i].EvidenceState}, "\x00")
		right := strings.Join([]string{canonicalEdges[j].From, canonicalEdges[j].To, canonicalEdges[j].Protocol, canonicalEdges[j].SyncMode, canonicalEdges[j].EvidenceState}, "\x00")
		return left < right
	})

	merged := &agentapi.FlowIR{
		Subject:       joinSortedValues(state.subjects, " / "),
		Status:        state.status,
		Nodes:         canonicalNodes,
		Edges:         canonicalEdges,
		OpenHops:      sortedMapValues(state.openHops),
		Uncertainties: sortedMapValues(state.uncertainties),
		Confidence:    state.confidence,
	}
	if merged.Subject == "" {
		merged.Subject = "未命名流程"
	}
	return merged
}

func normalizeFlowText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func flowNodeKey(subject, kind, label string) string {
	// Subject is part of identity: two investigations may use the same
	// generic labels (for example "API" -> "Worker") while describing
	// different business systems. Merging them would create a false cross-
	// subject edge and would let evidence from one scope support another.
	return strings.ToLower(normalizeFlowText(subject)) + "\x00" + strings.ToLower(kind) + "\x00" + strings.ToLower(label)
}

func canonicalFlowNodeID(key string) string {
	digest := sha256.Sum256([]byte(key))
	return "node_" + hex.EncodeToString(digest[:])[:16]
}

func normalizeEvidenceState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unresolved":
		return "unresolved"
	case "inferred":
		return "inferred"
	default:
		return "verified"
	}
}

func evidenceRank(value string) int {
	switch value {
	case "verified":
		return 0
	case "inferred":
		return 1
	default:
		return 2
	}
}

func sortedSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedMapValues(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func joinSortedValues(values map[string]string, separator string) string {
	items := sortedMapValues(values)
	return strings.Join(items, separator)
}
