package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

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

// renderableFlowBlock is the server-owned diagram plus the subject section
// it belongs to. Keeping the subject alongside the rendered block lets the
// answer composer place the diagram next to the matching explanation instead
// of appending every diagram after all prose.
type renderableFlowBlock struct {
	subject string
	content string
	order   int
}

// flowSectionMarker identifies a Markdown heading or a standalone subject
// line that can delimit a module in the model's prose.
type flowSectionMarker struct {
	line          int
	level         int
	title         string
	isSubjectLine bool
}

// canonicalFlowAnswer removes model-owned diagrams and installs one FlowIR
// block per subject from the merged server-owned flows. Explanatory prose is
// retained as context but can no longer alter the architecture edges. Each
// diagram is inserted at the end of its matching subject section; unmatched
// diagrams are appended as a safe fallback so authoritative server data is
// never lost.
//
// The FlowIRs are authoritative: any subject whose flow is invalid is skipped,
// and if none is valid the original model answer is returned unchanged rather
// than being replaced by a placeholder "unresolved" diagram.
func canonicalFlowAnswer(candidate string, flows []*agentapi.FlowIR) string {
	if len(flows) == 0 {
		return candidate
	}
	blocks := make([]renderableFlowBlock, 0, len(flows))
	for _, flow := range flows {
		if flow == nil || len(validateRenderableFlowIR(flow)) > 0 {
			continue
		}
		flowJSON, err := json.Marshal(flow)
		if err != nil {
			continue
		}
		blocks = append(blocks, renderableFlowBlock{
			subject: flow.Subject,
			content: "```flowir\n" + string(flowJSON) + "\n```",
			order:   flow.Order,
		})
	}
	if len(blocks) == 0 {
		return candidate
	}
	prose := flowFallbackProse(candidate)
	if prose == "" {
		prose = "说明：流程图由服务端根据子 agent 返回的结构化 FlowIR 生成；未验证的连接以虚线和 unresolved 标记表示。"
	}
	return interleaveFlowBlocks(prose, blocks)
}

// interleaveFlowBlocks places each authoritative FlowIR after the prose for
// its subject. Subject sections are detected from Markdown headings first,
// then from standalone subject lines such as "1、主题" or "主题：". If a
// subject cannot be located, its block is appended after the prose rather
// than silently dropped.
func interleaveFlowBlocks(prose string, blocks []renderableFlowBlock) string {
	prose = strings.TrimSpace(prose)
	if prose == "" || len(blocks) == 0 {
		return strings.TrimSpace(strings.Join([]string{prose, renderableFlowBlockContents(blocks)}, "\n\n"))
	}

	lines := strings.Split(prose, "\n")
	markers := flowSectionMarkers(lines, blocks)
	insertions := make(map[int][]string, len(blocks))
	var unmatched []string
	for _, block := range blocks {
		boundary, ok := flowSectionBoundary(lines, markers, block.subject, block.order)
		if !ok {
			unmatched = append(unmatched, block.content)
			continue
		}
		insertions[boundary] = append(insertions[boundary], block.content)
	}

	if len(insertions) == 0 {
		return strings.TrimSpace(strings.Join([]string{prose, strings.Join(unmatched, "\n\n")}, "\n\n"))
	}

	boundaries := make([]int, 0, len(insertions))
	for boundary := range insertions {
		boundaries = append(boundaries, boundary)
	}
	sort.Ints(boundaries)

	parts := make([]string, 0, len(boundaries)*2+2)
	last := 0
	for _, boundary := range boundaries {
		if segment := trimFlowProseSegment(lines[last:boundary]); segment != "" {
			parts = append(parts, segment)
		}
		parts = append(parts, insertions[boundary]...)
		last = boundary
	}
	if segment := trimFlowProseSegment(lines[last:]); segment != "" {
		parts = append(parts, segment)
	}
	parts = append(parts, unmatched...)
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func renderableFlowBlockContents(blocks []renderableFlowBlock) string {
	contents := make([]string, 0, len(blocks))
	for _, block := range blocks {
		contents = append(contents, block.content)
	}
	return strings.Join(contents, "\n\n")
}

// trimFlowProseSegment removes only separator newlines. In particular, it
// preserves indentation in lists and other Markdown prose inside a section.
func trimFlowProseSegment(lines []string) string {
	segment := strings.Trim(strings.Join(lines, "\n"), "\r\n")
	if strings.TrimSpace(segment) == "" {
		return ""
	}
	return segment
}

func flowSectionMarkers(lines []string, blocks []renderableFlowBlock) []flowSectionMarker {
	subjects := make(map[string]struct{}, len(blocks))
	for _, block := range blocks {
		if subject := normalizeFlowSubject(block.subject); subject != "" {
			subjects[subject] = struct{}{}
		}
	}

	markers := make([]flowSectionMarker, 0, len(lines))
	for lineIndex, line := range lines {
		if title, level, ok := parseFlowMarkdownHeading(line); ok {
			markers = append(markers, flowSectionMarker{line: lineIndex, level: level, title: title})
			continue
		}
		if _, ok := subjects[normalizeFlowSubject(line)]; ok {
			markers = append(markers, flowSectionMarker{
				line:          lineIndex,
				title:         line,
				isSubjectLine: true,
			})
		}
	}
	return markers
}

func flowSectionBoundary(lines []string, markers []flowSectionMarker, subject string, order int) (int, bool) {
	// Prefer the numbered heading matching the flow's task index. The answer
	// contract numbers each subject section as 1、2、… in task_index order, so
	// the index is language-stable and beats subject-text matching. A zero order
	// means the flow carried no task index, so fall through to subject matching.
	if order > 0 {
		want := strconv.Itoa(order)
		for markerIndex, marker := range markers {
			if marker.isSubjectLine {
				continue
			}
			if headingNumber(marker.title) == want {
				return flowSectionEnd(lines, markers, markerIndex), true
			}
		}
	}

	want := normalizeFlowSubject(subject)
	if want == "" {
		return 0, false
	}

	// Prefer a real Markdown heading. This avoids accidentally matching a
	// subject mention in body text when the model did provide section headings.
	// Exact matches win, while containment handles useful headings such as
	// "订单创建（主链路）" or "模块：订单创建".
	bestIndex, bestScore := -1, 0
	for markerIndex, marker := range markers {
		if marker.isSubjectLine {
			continue
		}
		score := flowSubjectMatchScore(marker.title, want)
		if score > bestScore {
			bestIndex, bestScore = markerIndex, score
		}
	}
	if bestIndex >= 0 {
		return flowSectionEnd(lines, markers, bestIndex), true
	}

	for markerIndex, marker := range markers {
		if !marker.isSubjectLine || normalizeFlowSubject(marker.title) != want {
			continue
		}
		return flowSectionEnd(lines, markers, markerIndex), true
	}
	return 0, false
}

// headingNumber extracts the leading decimal section number from a heading
// title like "1、主题" or "2. 主题", returning "" when the title has no leading
// number.
func headingNumber(title string) string {
	trimmed := strings.TrimSpace(title)
	end := 0
	for end < len(trimmed) && trimmed[end] >= '0' && trimmed[end] <= '9' {
		end++
	}
	return trimmed[:end]
}

func flowSubjectMatchScore(title, subject string) int {
	title = normalizeFlowSubject(title)
	if title == "" || subject == "" {
		return 0
	}
	switch {
	case title == subject:
		return 5
	case strings.HasPrefix(subject, title):
		return 4
	case strings.HasPrefix(title, subject):
		return 3
	case strings.Contains(subject, title) || strings.Contains(title, subject):
		return 2
	}
	return 0
}

func flowSectionEnd(lines []string, markers []flowSectionMarker, markerIndex int) int {
	current := markers[markerIndex]
	for _, next := range markers[markerIndex+1:] {
		if current.isSubjectLine || next.isSubjectLine || next.level <= current.level {
			return next.line
		}
	}
	// The last subject section has no following heading. A trailing
	// document-level summary paragraph (for example "**整体判断**：…") still
	// belongs after the subject diagram, so the diagram must be inserted before
	// that summary instead of after the whole document.
	if !current.isSubjectLine {
		if boundary := flowTrailingSummaryStart(lines, current.line+1); boundary >= 0 {
			return boundary
		}
	}
	return len(lines)
}

// flowTrailingSummaryStart returns the first line after start that begins a
// paragraph labeled with Markdown strong emphasis. Such paragraphs are used by
// the model for document-level summaries and should delimit the tail of the
// previous subject section.
func flowTrailingSummaryStart(lines []string, start int) int {
	previousBlank := true
	for index := start; index < len(lines); index++ {
		trimmed := strings.TrimSpace(lines[index])
		if trimmed == "" {
			previousBlank = true
			continue
		}
		if previousBlank && isFlowStrongLabelParagraph(trimmed) {
			return index
		}
		previousBlank = false
	}
	return -1
}

func isFlowStrongLabelParagraph(line string) bool {
	if !strings.HasPrefix(line, "**") {
		return false
	}
	rest := line[2:]
	closeIndex := strings.Index(rest, "**")
	if closeIndex <= 0 || strings.TrimSpace(rest[:closeIndex]) == "" {
		return false
	}
	after := strings.TrimSpace(rest[closeIndex+2:])
	if after == "" {
		return true
	}
	first, _ := utf8.DecodeRuneInString(after)
	return unicode.IsPunct(first) || strings.ContainsRune("：:", first)
}

func parseFlowMarkdownHeading(line string) (string, int, bool) {
	trimmed := strings.TrimSpace(line)
	level := 0
	for level < len(trimmed) && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level == len(trimmed) || (trimmed[level] != ' ' && trimmed[level] != '\t') {
		return "", 0, false
	}
	title := strings.TrimSpace(trimmed[level:])
	if title == "" {
		return "", 0, false
	}
	return title, level, true
}

func normalizeFlowSubject(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "#*_` \t")
	value = strings.TrimSpace(stripFlowNumberPrefix(value))
	value = strings.Trim(value, "#*_` \t")
	value = strings.TrimRightFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("：:。；;，,", r)
	})
	value = strings.ToLower(value)
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
}

func stripFlowNumberPrefix(value string) string {
	runes := []rune(value)
	index := 0
	for index < len(runes) && unicode.IsDigit(runes[index]) {
		index++
	}
	if index == 0 {
		return value
	}
	for index < len(runes) && unicode.IsSpace(runes[index]) {
		index++
	}
	if index >= len(runes) || !strings.ContainsRune("、.．)）", runes[index]) {
		return value
	}
	return strings.TrimSpace(string(runes[index+1:]))
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
		if inFence {
			continue
		}
		prose = append(prose, line)
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
