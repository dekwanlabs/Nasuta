package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// flowIRContextKey keeps the server-owned flow separate from model messages.
// It is deliberately private: callers can only pass a flow through the
// execution pipeline, not manufacture one through an exported context key.
type flowIRContextKey struct{}

func withFlows(ctx context.Context, flows []*agentapi.FlowIR) context.Context {
	if ctx == nil || len(flows) == 0 {
		return ctx
	}
	return context.WithValue(ctx, flowIRContextKey{}, cloneExecutionFlowPtrs(flows))
}

func flowsFromContext(ctx context.Context) []*agentapi.FlowIR {
	if ctx == nil {
		return nil
	}
	flows, _ := ctx.Value(flowIRContextKey{}).([]*agentapi.FlowIR)
	return cloneExecutionFlowPtrs(flows)
}

// validateRenderableFlowIR is the deterministic server-side quality gate. It
// validates the typed FlowIR before it is emitted, so omitted, injected, or
// malformed graph elements cannot reach the final answer.
func validateRenderableFlowIR(flow *agentapi.FlowIR) []string {
	if flow == nil {
		return []string{"flow IR is required"}
	}
	var violations []string
	violations = append(violations, validateRenderFlowHeader(flow)...)
	nodes := validateRenderFlowNodes(flow.Nodes, &violations)
	violations = append(violations, validateRenderFlowEdges(flow.Edges, nodes)...)
	violations = append(violations, validateRenderFlowOpenHops(flow.OpenHops)...)
	return violations
}

func validateRenderFlowHeader(flow *agentapi.FlowIR) []string {
	var violations []string
	if strings.TrimSpace(flow.Subject) == "" {
		violations = append(violations, "flow subject is required")
	}
	switch flow.Status {
	case "complete", "partial":
	default:
		violations = append(violations, fmt.Sprintf("flow status %q is invalid", flow.Status))
	}
	switch flow.Confidence {
	case "low", "medium", "high":
	default:
		violations = append(violations, fmt.Sprintf("flow confidence %q is invalid", flow.Confidence))
	}
	return violations
}

func validateRenderFlowNodes(nodes []agentapi.FlowNode, violations *[]string) map[string]struct{} {
	ids := make(map[string]struct{}, len(nodes))
	for index, node := range nodes {
		id := strings.TrimSpace(node.ID)
		if id == "" {
			*violations = append(*violations, fmt.Sprintf("flow node %d has an empty id", index))
			continue
		}
		if node.ID != id {
			*violations = append(*violations, fmt.Sprintf("flow node %d id is not canonical", index))
		}
		if strings.TrimSpace(node.Label) == "" || strings.TrimSpace(node.Kind) == "" {
			*violations = append(*violations, fmt.Sprintf("flow node %q requires label and kind", id))
		}
		if _, exists := ids[id]; exists {
			*violations = append(*violations, fmt.Sprintf("flow node id %q is duplicated", id))
			continue
		}
		ids[id] = struct{}{}
		*violations = append(*violations, validateRenderEvidenceRefs("flow node "+id, node.EvidenceRefs)...)
	}
	return ids
}

func validateRenderFlowEdges(edges []agentapi.FlowEdge, nodes map[string]struct{}) []string {
	var violations []string
	seen := make(map[string]struct{}, len(edges))
	for index, edge := range edges {
		edgeKey := strings.Join([]string{edge.From, edge.To, edge.Protocol, edge.SyncMode, edge.EvidenceState}, "\x00")
		if _, exists := seen[edgeKey]; exists {
			violations = append(violations, fmt.Sprintf("flow edge %d is duplicated", index))
		} else {
			seen[edgeKey] = struct{}{}
		}
		if edge.From != strings.TrimSpace(edge.From) || edge.To != strings.TrimSpace(edge.To) {
			violations = append(violations, fmt.Sprintf("flow edge %d endpoint ids are not canonical", index))
		}
		if _, ok := nodes[strings.TrimSpace(edge.From)]; !ok {
			violations = append(violations, fmt.Sprintf("flow edge %d references unknown from node %q", index, edge.From))
		}
		if _, ok := nodes[strings.TrimSpace(edge.To)]; !ok {
			violations = append(violations, fmt.Sprintf("flow edge %d references unknown to node %q", index, edge.To))
		}
		state := strings.TrimSpace(edge.EvidenceState)
		if edge.EvidenceState != state {
			violations = append(violations, fmt.Sprintf("flow edge %d evidence state is not canonical", index))
		}
		switch state {
		case "verified", "inferred", "unresolved":
		default:
			violations = append(violations, fmt.Sprintf("flow edge %d has invalid evidence state %q", index, edge.EvidenceState))
		}
		syncMode := strings.TrimSpace(edge.SyncMode)
		if edge.SyncMode != syncMode {
			violations = append(violations, fmt.Sprintf("flow edge %d sync mode is not canonical", index))
		}
		switch syncMode {
		case "", "sync", "async", "unknown":
		default:
			violations = append(violations, fmt.Sprintf("flow edge %d has invalid sync mode %q", index, edge.SyncMode))
		}
		if state == "verified" && len(edge.EvidenceRefs) == 0 {
			violations = append(violations, fmt.Sprintf("flow edge %d is verified without evidence refs", index))
		}
		violations = append(violations, validateRenderEvidenceRefs(fmt.Sprintf("flow edge %d", index), edge.EvidenceRefs)...)
	}
	return violations
}

func validateRenderFlowOpenHops(hops []string) []string {
	var violations []string
	seen := make(map[string]struct{}, len(hops))
	for index, hop := range hops {
		canonical := strings.TrimSpace(hop)
		if canonical == "" {
			violations = append(violations, fmt.Sprintf("flow open hop %d is empty", index))
			continue
		}
		if hop != canonical {
			violations = append(violations, fmt.Sprintf("flow open hop %d is not canonical", index))
		}
		key := strings.ToLower(canonical)
		if _, exists := seen[key]; exists {
			violations = append(violations, fmt.Sprintf("flow open hop %d is duplicated", index))
			continue
		}
		seen[key] = struct{}{}
	}
	return violations
}

func validateRenderEvidenceRefs(subject string, refs []string) []string {
	seen := make(map[string]struct{}, len(refs))
	var violations []string
	for _, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			violations = append(violations, subject+" has an empty evidence ref")
			continue
		}
		if _, exists := seen[ref]; exists {
			violations = append(violations, subject+" has duplicate evidence refs")
			continue
		}
		seen[ref] = struct{}{}
	}
	return violations
}

func uniqueRenderViolations(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// canonicalFlowAnswer removes model-owned diagrams and installs one FlowIR
// block per subject from the merged server-owned flows. Explanatory prose is
// retained as context but can no longer alter the architecture edges.
//
// The FlowIRs are authoritative: any subject whose flow is invalid is skipped,
// and if none is valid the original model answer is returned unchanged rather
// than being replaced by a placeholder "unresolved" diagram.
func canonicalFlowAnswer(candidate string, flows []*agentapi.FlowIR) string {
	if len(flows) == 0 {
		return candidate
	}
	blocks := make([]string, 0, len(flows))
	for _, flow := range flows {
		if flow == nil || len(validateRenderableFlowIR(flow)) > 0 {
			continue
		}
		flowJSON, err := json.Marshal(flow)
		if err != nil {
			continue
		}
		blocks = append(blocks, "```flowir\n"+string(flowJSON)+"\n```")
	}
	if len(blocks) == 0 {
		return candidate
	}
	prose := flowFallbackProse(candidate)
	if prose == "" {
		prose = "说明：流程图由服务端根据子 agent 返回的结构化 FlowIR 生成；未验证的连接以虚线和 unresolved 标记表示。"
	}
	return strings.TrimSpace(prose) + "\n\n" + strings.Join(blocks, "\n\n")
}

// flowFallbackProse extracts the non-fenced prose from a candidate answer so
// canonicalFlowAnswer can keep the model's explanatory text while replacing the
// diagrams with the server-rendered architecture graph.
func flowFallbackProse(value string) string {
	var prose []string
	inFence := false
	for _, line := range strings.Split(value, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if !inFence && trimmed != "" {
			prose = append(prose, line)
		}
	}
	return strings.TrimSpace(strings.Join(prose, "\n"))
}

func cloneExecutionFlow(flow *agentapi.FlowIR) *agentapi.FlowIR {
	if flow == nil {
		return nil
	}
	clone := *flow
	clone.Nodes = append([]agentapi.FlowNode(nil), flow.Nodes...)
	for index := range clone.Nodes {
		clone.Nodes[index].EvidenceRefs = append([]string(nil), flow.Nodes[index].EvidenceRefs...)
	}
	clone.Edges = append([]agentapi.FlowEdge(nil), flow.Edges...)
	for index := range clone.Edges {
		clone.Edges[index].EvidenceRefs = append([]string(nil), flow.Edges[index].EvidenceRefs...)
	}
	clone.OpenHops = append([]string(nil), flow.OpenHops...)
	clone.Uncertainties = append([]string(nil), flow.Uncertainties...)
	return &clone
}

func cloneExecutionFlowPtrs(flows []*agentapi.FlowIR) []*agentapi.FlowIR {
	if len(flows) == 0 {
		return nil
	}
	clones := make([]*agentapi.FlowIR, 0, len(flows))
	for _, flow := range flows {
		clones = append(clones, cloneExecutionFlow(flow))
	}
	return clones
}
