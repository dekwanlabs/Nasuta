package delegation

import (
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

// CanonicalFlowIR turns a model-authored FlowIR into the authoritative one the
// renderer may install. Both the child investigator and the parent answerer
// submit flows as their own model output, so one producer-agnostic
// canonicalization must run before either source can reach a diagram.
//
// The flow is copied, trimmed, validated, and reduced to evidence handles this
// run actually observed. A flow that still fails validation is rejected rather
// than repaired: an answer with no diagram is always better than one carrying a
// graph the evidence does not support.
func CanonicalFlowIR(flow *agentapi.FlowIR, observed map[string]tool.EvidenceUnit) (*agentapi.FlowIR, error) {
	if flow == nil {
		return nil, fmt.Errorf("flow IR is required")
	}
	canonical := trimFlowIR(cloneFlowIR(flow))
	// Normalize before validating: refs the run never observed are removed, and
	// an edge that was only "verified" by those refs is demoted to unresolved
	// rather than rejected, so the diagram survives with an honest state.
	normalizeFlowEvidenceRefs(canonical, observed)
	if err := validateFlowIR(canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

// trimFlowIR canonicalizes the free text a model supplies. The renderer treats
// a padded identifier as non-canonical and refuses the whole diagram, so the
// padding is removed here instead of silently discarding an otherwise valid
// flow.
func trimFlowIR(flow *agentapi.FlowIR) *agentapi.FlowIR {
	flow.EntityID = strings.TrimSpace(flow.EntityID)
	flow.Subject = strings.TrimSpace(flow.Subject)
	flow.Status = strings.TrimSpace(flow.Status)
	flow.Confidence = strings.TrimSpace(flow.Confidence)
	flow.Origin = strings.TrimSpace(flow.Origin)
	for index := range flow.Nodes {
		node := &flow.Nodes[index]
		node.ID = strings.TrimSpace(node.ID)
		node.Label = strings.TrimSpace(node.Label)
		node.Kind = strings.TrimSpace(node.Kind)
		node.EvidenceRefs = trimFlowValues(node.EvidenceRefs)
	}
	for index := range flow.Edges {
		edge := &flow.Edges[index]
		edge.From = strings.TrimSpace(edge.From)
		edge.To = strings.TrimSpace(edge.To)
		edge.Protocol = strings.TrimSpace(edge.Protocol)
		edge.SyncMode = strings.TrimSpace(edge.SyncMode)
		edge.EvidenceState = strings.TrimSpace(edge.EvidenceState)
		edge.EvidenceRefs = trimFlowValues(edge.EvidenceRefs)
	}
	flow.OpenHops = trimFlowValues(flow.OpenHops)
	flow.Uncertainties = trimFlowValues(flow.Uncertainties)
	return flow
}

func trimFlowValues(values []string) []string {
	if len(values) == 0 {
		return values
	}
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	if len(trimmed) == 0 {
		return nil
	}
	return trimmed
}
