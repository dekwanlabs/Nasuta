package execution

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
)

const flowOutputContractPrefix = "[NASUTA_FLOW_OUTPUT_CONTRACT]"

// ValidateFlowAnswer validates the small, user-facing contract used by flow
// questions. It deliberately parses fenced blocks instead of trying to parse
// Mermaid itself: the runtime needs to guarantee a useful diagram envelope,
// not prove that every Mermaid feature is semantically valid.
func ValidateFlowAnswer(answer string, contract agentapi.RunOutputContract) []string {
	if contract.Kind != "flow" || !contract.RequireMermaid {
		return nil
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return []string{"flow answer is empty"}
	}
	blocks, malformed := scanMermaidBlocks(answer)
	if malformed {
		return []string{"flow answer contains an unterminated fenced block"}
	}
	if len(blocks) == 0 {
		return []string{"flow answer must start with one or more fenced Mermaid diagrams"}
	}
	violations := make([]string, 0, 4)
	if first := firstNonEmptyLine(answer); !strings.HasPrefix(strings.ToLower(first), "```mermaid") {
		violations = append(violations, "flow answer must start with a fenced Mermaid diagram")
	}
	if !mermaidBlocksPrecedeProse(answer, blocks) {
		violations = append(violations, "all Mermaid diagrams must appear before explanatory prose")
	}
	if strings.TrimSpace(answer[blocks[len(blocks)-1].end:]) == "" {
		violations = append(violations, "flow answer must include explanatory text after the diagrams")
	}

	hasEdge := false
	for index, block := range blocks {
		lower := strings.ToLower(block.content)
		if !strings.Contains(lower, "flowchart") && !strings.Contains(lower, "sequencediagram") {
			violations = append(violations, fmt.Sprintf("Mermaid block %d must be a flowchart or sequenceDiagram", index+1))
		}
		edges := countMermaidEdges(block.content)
		if edges > 0 {
			hasEdge = true
		}
		if contract.MaxHops > 0 && edges > contract.MaxHops {
			violations = append(violations, fmt.Sprintf("Mermaid block %d exceeds the %d-hop limit", index+1, contract.MaxHops))
		}
	}
	if !hasEdge {
		violations = append(violations, "Mermaid diagrams must contain at least one flow edge")
	}
	if len(contract.Subjects) > 1 && len(blocks) < len(contract.Subjects) {
		violations = append(violations, fmt.Sprintf("flow answer needs at least one Mermaid diagram per subject (%d required)", len(contract.Subjects)))
	}
	for _, subject := range contract.Subjects {
		found := false
		for _, block := range blocks {
			if strings.Contains(strings.ToLower(block.content), strings.ToLower(strings.TrimSpace(subject))) {
				found = true
				break
			}
		}
		if !found {
			violations = append(violations, fmt.Sprintf("Mermaid diagrams must mention subject %q", subject))
		}
	}
	return uniqueStrings(violations)
}

type mermaidBlock struct {
	start   int
	content string
	end     int
}

func scanMermaidBlocks(answer string) ([]mermaidBlock, bool) {
	lines := splitLinesWithOffsets(answer)
	var blocks []mermaidBlock
	for index := 0; index < len(lines); index++ {
		line := strings.TrimSpace(lines[index].text)
		if !strings.HasPrefix(line, "```") {
			continue
		}
		info := strings.TrimSpace(strings.TrimPrefix(line, "```"))
		if !strings.EqualFold(info, "mermaid") {
			continue
		}
		start := lines[index].start
		contentStart := lines[index].end
		for index++; index < len(lines); index++ {
			if strings.TrimSpace(lines[index].text) != "```" {
				continue
			}
			content := answer[contentStart:lines[index].start]
			blocks = append(blocks, mermaidBlock{
				start: start, content: content, end: lines[index].end,
			})
			break
		}
		if index >= len(lines) || len(blocks) == 0 || blocks[len(blocks)-1].start != start {
			return blocks, true
		}
	}
	return blocks, false
}

type lineOffset struct {
	start int
	end   int
	text  string
}

func splitLinesWithOffsets(value string) []lineOffset {
	var lines []lineOffset
	start := 0
	for start < len(value) {
		end := strings.IndexByte(value[start:], '\n')
		if end < 0 {
			lines = append(lines, lineOffset{start: start, end: len(value), text: value[start:]})
			break
		}
		end += start
		lines = append(lines, lineOffset{start: start, end: end + 1, text: value[start:end]})
		start = end + 1
	}
	if len(value) == 0 {
		return nil
	}
	return lines
}

func firstNonEmptyLine(value string) string {
	for _, line := range strings.Split(value, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func mermaidBlocksPrecedeProse(answer string, blocks []mermaidBlock) bool {
	if len(blocks) == 0 {
		return false
	}
	lastDiagramEnd := blocks[len(blocks)-1].end
	for _, line := range splitLinesWithOffsets(answer) {
		if line.start < blocks[0].start {
			if strings.TrimSpace(line.text) != "" {
				return false
			}
		}
		if line.start >= lastDiagramEnd {
			continue
		}
		if line.start >= blocks[0].start && line.start < lastDiagramEnd {
			trimmed := strings.TrimSpace(line.text)
			if trimmed == "" || strings.HasPrefix(trimmed, "```") {
				continue
			}
			inside := false
			for _, block := range blocks {
				if line.start > block.start && line.start < block.end {
					inside = true
					break
				}
			}
			if !inside {
				return false
			}
		}
	}
	return true
}

var mermaidEdgeMarkers = []string{"-.->", "==>", "-->", "->>", "---", "->"}

func countMermaidEdges(content string) int {
	total := 0
	for _, line := range strings.Split(content, "\n") {
		for index := 0; index < len(line); {
			matched := ""
			for _, marker := range mermaidEdgeMarkers {
				if strings.HasPrefix(line[index:], marker) && len(marker) > len(matched) {
					matched = marker
				}
			}
			if matched == "" {
				_, size := utf8.DecodeRuneInString(line[index:])
				index += size
				continue
			}
			total++
			index += len(matched)
		}
	}
	return total
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func flowContractPrompt(contract agentapi.RunOutputContract) string {
	if contract.Kind != "flow" || !contract.RequireMermaid {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(flowOutputContractPrefix + "\n")
	builder.WriteString("This answer is a process/architecture flow response. Start with one or more fenced Mermaid diagrams, before any prose. Use `flowchart LR` or `sequenceDiagram`; show responsibility boundaries, calls/events/queues/data flow, and mark uncertainty when evidence is incomplete. After the diagrams, provide concise explanatory text. Do not output only prose.")
	if len(contract.Subjects) > 0 {
		builder.WriteString(" Required subjects: ")
		builder.WriteString(strings.Join(contract.Subjects, ", "))
		builder.WriteString(". Include each subject in at least one diagram.")
	}
	if contract.MaxHops > 0 {
		builder.WriteString(fmt.Sprintf(" Keep each diagram within %d hops.", contract.MaxHops))
	}
	return builder.String()
}

func appendFlowContractPrompt(messages []llm.Message, contract agentapi.RunOutputContract) []llm.Message {
	prompt := flowContractPrompt(contract)
	if prompt == "" {
		return messages
	}
	for _, message := range messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, flowOutputContractPrefix) {
			return messages
		}
	}
	message := llm.Message{Role: "system", Content: prompt}
	lastUser := -1
	for index := range messages {
		if messages[index].Role == "user" {
			lastUser = index
		}
	}
	if lastUser < 0 {
		return append(messages, message)
	}
	out := make([]llm.Message, 0, len(messages)+1)
	out = append(out, messages[:lastUser]...)
	out = append(out, message)
	out = append(out, messages[lastUser:]...)
	return out
}

func (agent *Agent) enforceFlowContract(ctx context.Context, messages []llm.Message, initial *llm.ChatStreamResult, contract agentapi.RunOutputContract, maxTokens int, stream *StreamPipe) *llm.ChatStreamResult {
	if initial == nil || contract.Kind != "flow" || !contract.RequireMermaid {
		return initial
	}
	if flow := flowIRFromContext(ctx); flow != nil {
		initial.Content = canonicalFlowAnswer(initial.Content, flow)
		return initial
	}
	violations := ValidateFlowAnswer(initial.Content, contract)
	if len(violations) == 0 {
		return initial
	}
	log.WarnfCtx(ctx, "[agent] flow answer contract rejected candidate: violations=%d", len(violations))
	repairMessages := append(append([]llm.Message{}, messages...),
		llm.Message{Role: "assistant", Content: initial.Content},
		llm.Message{Role: "user", Content: flowRepairInstruction(contract, violations)},
	)
	repaired, err := agent.generateWithContinue(ctx, repairMessages, maxTokens, stream)
	if err == nil && repaired != nil && len(ValidateFlowAnswer(repaired.Content, contract)) == 0 {
		return repaired
	}
	if err != nil {
		log.WarnfCtx(ctx, "[agent] flow answer repair failed; using deterministic unresolved fallback: %v", err)
	} else {
		log.WarnfCtx(ctx, "[agent] flow answer repair still violates contract; using deterministic unresolved fallback")
	}
	return &llm.ChatStreamResult{
		Content: deterministicFlowFallback(initial.Content, contract),
	}
}

// deterministicFlowFallback keeps a flow request visibly flow-shaped even
// when the model cannot repair its answer. Every edge is explicitly
// unresolved; this is a safe lower-quality answer, not invented architecture.
func deterministicFlowFallback(candidate string, contract agentapi.RunOutputContract) string {
	subjects := make([]string, 0, len(contract.Subjects))
	for _, subject := range contract.Subjects {
		subject = strings.TrimSpace(subject)
		if subject == "" {
			continue
		}
		subjects = append(subjects, subject)
	}
	if len(subjects) == 0 {
		subjects = []string{"主流程"}
	}

	var builder strings.Builder
	for index, subject := range subjects {
		if index > 0 {
			builder.WriteString("\n")
		}
		fmt.Fprintf(&builder, "```mermaid\nflowchart LR\n    subject_%d[\"%s\"] -.-> unresolved_%d[\"待确认：关键流程跳转\"]\n```\n",
			index, mermaidFallbackLabel(subject), index)
	}
	builder.WriteString("\n说明：模型答案未通过流程图契约校验。图中的连接关系、协议和同步方式均标记为待确认，未将其作为已验证架构事实。")
	if candidateText := flowFallbackProse(candidate); candidateText != "" {
		builder.WriteString("\n\n补充文字草稿（未通过流程图校验，仅供继续核对）：\n")
		builder.WriteString(candidateText)
	}
	return builder.String()
}

func mermaidFallbackLabel(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\"", "'")
	return strings.TrimSpace(value)
}

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

func flowRepairInstruction(contract agentapi.RunOutputContract, violations []string) string {
	return flowOutputContractPrefix + "\nThe previous answer violated the flow output contract: " + strings.Join(violations, "; ") + ". Return the corrected answer, not an explanation of the repair. Start with fenced Mermaid diagrams, put every diagram before prose, include each required subject, keep each diagram within the hop limit, and finish with concise explanatory text."
}

// useFlowFallback installs a deterministic, explicitly unresolved flow answer
// after the normal conclusion path cannot produce a contract-valid response.
// The fallback is still passed through the exact-answer contract so internal
// adoption metadata is consumed server-side and never leaks to the user.
func (agent *Agent) useFlowFallback(state *compiledLoop, candidate string, cause error) bool {
	if state == nil || state.result == nil || state.input.OutputContract.Kind != "flow" || !state.input.OutputContract.RequireMermaid {
		return false
	}
	if strings.TrimSpace(candidate) == "" {
		candidate = state.result.Answer
	}
	answer := candidate
	if state.result.Flow != nil {
		answer = canonicalFlowAnswer(candidate, state.result.Flow)
	} else {
		answer = deterministicFlowFallback(candidate, state.input.OutputContract)
	}
	if state.answerContract != nil && state.answerContract.Active() {
		withMetadata, err := state.answerContract.appendConservativeFallbackMetadata(answer)
		if err != nil {
			log.WarnfCtx(state.ctx, "[agent] run %s flow fallback contract metadata failed: %v", state.runID, err)
			return false
		}
		visible, violations := state.answerContract.ValidateAndStrip(withMetadata)
		if len(violations) > 0 {
			log.WarnfCtx(state.ctx, "[agent] run %s flow fallback contract rejected: %v", state.runID, violations)
			return false
		}
		answer = visible
		state.result.DelegationAdoptions = state.answerContract.Adoptions()
	}
	state.result.Answer = answer
	state.result.Err = nil
	if cause != nil {
		log.WarnfCtx(state.ctx, "[agent] run %s installed deterministic flow fallback after %v", state.runID, cause)
	}
	return true
}
