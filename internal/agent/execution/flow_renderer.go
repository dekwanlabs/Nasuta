package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

// flowIRContextKey keeps the server-owned flow separate from model messages.
// It is deliberately private: callers can only pass a flow through the
// execution pipeline, not manufacture one through an exported context key.
type flowIRContextKey struct{}

func withFlowIR(ctx context.Context, flow *agentapi.FlowIR) context.Context {
	if ctx == nil || flow == nil {
		return ctx
	}
	return context.WithValue(ctx, flowIRContextKey{}, cloneExecutionFlow(flow))
}

func flowIRFromContext(ctx context.Context) *agentapi.FlowIR {
	if ctx == nil {
		return nil
	}
	flow, _ := ctx.Value(flowIRContextKey{}).(*agentapi.FlowIR)
	return cloneExecutionFlow(flow)
}

// flowRenderer accumulates the Mermaid output for a single FlowIR and owns
// the synthetic ID allocator state shared by nodes, edges, and open hops.
type flowRenderer struct {
	b    strings.Builder
	used map[string]struct{}
}

func newFlowRenderer() *flowRenderer {
	return &flowRenderer{used: make(map[string]struct{})}
}

// RenderFlowIR renders only server-owned, typed flow data. Model-provided
// Mermaid is never trusted for the final architecture diagram.
func RenderFlowIR(flow *agentapi.FlowIR) string {
	if flow == nil {
		return ""
	}
	renderer := newFlowRenderer()
	renderer.render(flow)
	return renderer.b.String()
}

func (renderer *flowRenderer) render(flow *agentapi.FlowIR) {
	nodes := sortedRenderNodes(flow.Nodes)
	ids := renderer.allocateRenderNodeIDs(nodes)

	renderer.b.WriteString("```mermaid\nflowchart LR\n")
	renderer.renderSubject(flow.Subject)
	renderer.renderNodes(nodes, ids)

	edges := sortedRenderEdges(flow.Edges)
	renderedEdges := renderer.renderEdges(edges, ids)

	openHopIDs := renderer.renderOpenHops(flow.OpenHops)
	renderer.renderUnresolvedTail(nodes, ids, openHopIDs, renderedEdges)
	renderer.b.WriteString("```\n")
}

func (renderer *flowRenderer) allocateRenderNodeIDs(nodes []agentapi.FlowNode) map[string]string {
	ids := make(map[string]string, len(nodes))
	for index, node := range nodes {
		id := allocateMermaidID(node.ID, fmt.Sprintf("node:%d:%s:%s", index, node.ID, node.Label), renderer.used)
		ids[node.ID] = id
	}
	return ids
}

func (renderer *flowRenderer) renderSubject(subject string) {
	if label := mermaidSafeLabel(subject); label != "" {
		// Mermaid comments are visible to validators and do not create an
		// unverified edge or an extra semantic node.
		fmt.Fprintf(&renderer.b, "    %%%% subject: %s\n", label)
	}
}

func (renderer *flowRenderer) renderNodes(nodes []agentapi.FlowNode, ids map[string]string) {
	for _, node := range nodes {
		id := ids[node.ID]
		label := mermaidSafeLabel(node.Label)
		kind := mermaidSafeLabel(node.Kind)
		if kind != "" {
			label = kind + ": " + label
		}
		if label == "" {
			label = "未命名节点"
		}
		fmt.Fprintf(&renderer.b, "    %s[\"%s\"]\n", id, label)
	}
}

func (renderer *flowRenderer) renderEdges(edges []agentapi.FlowEdge, ids map[string]string) int {
	renderedEdges := 0
	for _, edge := range edges {
		from, fromOK := ids[edge.From]
		to, toOK := ids[edge.To]
		if !fromOK || !toOK {
			continue
		}
		state := normalizeRenderEvidenceState(edge.EvidenceState)
		marker := "-->"
		if state != "verified" {
			marker = "-.->"
		}
		protocol := mermaidSafeLabel(edge.Protocol)
		if protocol == "" {
			protocol = "unknown"
		}
		syncMode := mermaidSafeLabel(edge.SyncMode)
		if syncMode == "" {
			syncMode = "unknown"
		}
		label := protocol + " / " + syncMode + " / " + state
		fmt.Fprintf(&renderer.b, "    %s %s|\"%s\"| %s\n", from, marker, label, to)
		renderedEdges++
	}
	return renderedEdges
}

func (renderer *flowRenderer) renderOpenHops(openHops []string) []string {
	openHops = sortedRenderStrings(openHops)
	openHopIDs := make([]string, len(openHops))
	for index, hop := range openHops {
		id := allocateMermaidID(
			fmt.Sprintf("open_hop_%d", index+1),
			fmt.Sprintf("open-hop:%d:%s", index, hop),
			renderer.used,
		)
		openHopIDs[index] = id
		fmt.Fprintf(&renderer.b, "    %s[\"待确认：%s\"]\n", id, mermaidSafeLabel(hop))
	}
	return openHopIDs
}

func (renderer *flowRenderer) renderUnresolvedTail(nodes []agentapi.FlowNode, ids map[string]string, openHopIDs []string, renderedEdges int) {
	if renderedEdges == 0 {
		// An edge-less flow is not evidence of a disconnected architecture.
		// Make the uncertainty explicit so the output remains useful and safe.
		from := ""
		if len(nodes) == 0 {
			from = allocateMermaidID("flow_scope", "synthetic:flow-scope", renderer.used)
			renderer.b.WriteString("    " + from + "[\"流程范围\"]\n")
		} else {
			from = ids[nodes[0].ID]
		}
		if len(openHopIDs) == 0 {
			unresolved := allocateMermaidID("unresolved_flow", "synthetic:unresolved-flow", renderer.used)
			renderer.b.WriteString("    " + unresolved + "[\"待确认：关键流程跳转\"]\n")
			fmt.Fprintf(&renderer.b, "    %s -.->|\"unknown / unknown / unresolved\"| %s\n", from, unresolved)
		} else {
			for _, openHopID := range openHopIDs {
				fmt.Fprintf(&renderer.b, "    %s -.->|\"unknown / unknown / unresolved\"| %s\n", from, openHopID)
				from = openHopID
			}
		}
	} else if len(openHopIDs) > 0 && len(nodes) > 0 {
		// Preserve explicit open hops in the graph without inventing a source:
		// attach them to the first canonical node as unresolved follow-ups.
		from := ids[nodes[0].ID]
		for _, openHopID := range openHopIDs {
			fmt.Fprintf(&renderer.b, "    %s -.->|\"unknown / unknown / unresolved\"| %s\n", from, openHopID)
			from = openHopID
		}
	}
}

func sortedRenderNodes(nodes []agentapi.FlowNode) []agentapi.FlowNode {
	nodes = append([]agentapi.FlowNode(nil), nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].ID != nodes[j].ID {
			return nodes[i].ID < nodes[j].ID
		}
		return nodes[i].Label < nodes[j].Label
	})
	return nodes
}

func sortedRenderEdges(edges []agentapi.FlowEdge) []agentapi.FlowEdge {
	edges = append([]agentapi.FlowEdge(nil), edges...)
	sort.Slice(edges, func(i, j int) bool {
		left := strings.Join([]string{edges[i].From, edges[i].To, edges[i].Protocol, edges[i].SyncMode, edges[i].EvidenceState}, "\x00")
		right := strings.Join([]string{edges[j].From, edges[j].To, edges[j].Protocol, edges[j].SyncMode, edges[j].EvidenceState}, "\x00")
		return left < right
	})
	return edges
}

// ValidateRenderedFlowIR is a deterministic server-side quality gate. It
// validates the typed FlowIR before comparing the candidate byte-for-byte with
// the canonical server renderer, so omitted, injected, or marker-mutated graph
// elements cannot pass merely because the output still looks like Mermaid.
func ValidateRenderedFlowIR(flow *agentapi.FlowIR, rendered string) []string {
	violations := validateRenderableFlowIR(flow)
	if !strings.HasPrefix(rendered, "```mermaid\nflowchart LR\n") {
		violations = append(violations, "rendered flow must start with a fenced Mermaid flowchart LR")
	}
	if !strings.HasSuffix(rendered, "```\n") || strings.Count(rendered, "```") != 2 {
		violations = append(violations, "rendered flow must contain exactly one closed Mermaid fence")
	}
	if len(violations) == 0 {
		expected := RenderFlowIR(flow)
		if rendered != expected {
			violations = append(violations, "rendered flow differs from the canonical server-owned graph")
		}
	}
	return uniqueRenderViolations(violations)
}

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

func allocateMermaidID(preferred, seed string, used map[string]struct{}) string {
	base := safeMermaidID(preferred)
	if _, exists := used[base]; !exists {
		used[base] = struct{}{}
		return base
	}
	digest := sha256.Sum256([]byte(seed))
	prefix := base + "_" + hex.EncodeToString(digest[:])[:8]
	candidate := prefix
	for suffix := 2; ; suffix++ {
		if _, exists := used[candidate]; !exists {
			used[candidate] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", prefix, suffix)
	}
}

// canonicalFlowAnswer removes model-owned diagrams and installs the one
// deterministic diagram rendered from the merged FlowIR. Explanatory prose is
// retained as context but can no longer alter the architecture edges.
func canonicalFlowAnswer(candidate string, flow *agentapi.FlowIR) string {
	if flow == nil {
		return candidate
	}
	diagram := RenderFlowIR(flow)
	degraded := len(ValidateRenderedFlowIR(flow, diagram)) > 0
	if degraded {
		fallback := unresolvedRenderableFlow(flow)
		diagram = RenderFlowIR(fallback)
		if len(ValidateRenderedFlowIR(fallback, diagram)) > 0 {
			return deterministicFlowFallback(candidate, agentapi.RunOutputContract{Kind: "flow", RequireMermaid: true, Subjects: []string{fallback.Subject}, MaxHops: 1})
		}
	}
	prose := flowFallbackProse(candidate)
	if prose == "" {
		prose = "说明：流程图由服务端根据子 agent 返回的结构化 FlowIR 生成；未验证的连接以虚线和 unresolved 标记表示。"
	}
	if degraded {
		prose = "说明：结构化 FlowIR 未通过服务端渲染质量门禁，已降级为 unresolved 图，不将原始连接作为已验证事实。\n\n" + prose
	}
	return strings.TrimSpace(diagram) + "\n\n" + prose
}

func unresolvedRenderableFlow(flow *agentapi.FlowIR) *agentapi.FlowIR {
	subject := "主流程"
	if flow != nil && strings.TrimSpace(flow.Subject) != "" {
		subject = strings.TrimSpace(flow.Subject)
	}
	return &agentapi.FlowIR{
		Subject:    subject,
		Status:     "partial",
		Confidence: "low",
		Nodes: []agentapi.FlowNode{{
			ID: "flow_scope", Label: subject, Kind: "scope",
		}},
		Uncertainties: []string{"source FlowIR failed deterministic render validation"},
	}
}

var mermaidIDPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func safeMermaidID(value string) string {
	value = strings.TrimSpace(value)
	if mermaidIDPattern.MatchString(value) {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return "node_" + hex.EncodeToString(digest[:])[:16]
}

func mermaidSafeLabel(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	value = strings.ReplaceAll(value, "\"", "'")
	value = strings.ReplaceAll(value, "`", "'")
	value = strings.ReplaceAll(value, "[", "(")
	value = strings.ReplaceAll(value, "]", ")")
	value = strings.ReplaceAll(value, "{", "(")
	value = strings.ReplaceAll(value, "}", ")")
	return value
}

func normalizeRenderEvidenceState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "inferred":
		return "inferred"
	case "unresolved":
		return "unresolved"
	default:
		return "verified"
	}
}

func sortedRenderStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
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
