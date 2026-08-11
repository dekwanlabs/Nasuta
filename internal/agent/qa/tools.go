package qa

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

func routingCandidates(tools []tool.Tool) []retrieval.ToolRouteCandidate {
	candidates := make([]retrieval.ToolRouteCandidate, 0, len(tools))
	for _, candidate := range tools {
		if candidate.Kind != tool.KindRead || candidate.Routing == nil {
			continue
		}
		candidates = append(candidates, retrieval.ToolRouteCandidate{
			ID: string(candidate.ID), Intent: candidate.Routing.Intent, Temporal: candidate.Routing.Temporal,
		})
	}
	return candidates
}

func routedToolsNeedFullInvestigation(candidates []retrieval.ToolRouteCandidate, selected []string) bool {
	if len(selected) == 0 {
		return false
	}
	temporal := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.Temporal {
			temporal[candidate.ID] = struct{}{}
		}
	}
	for _, id := range selected {
		if _, ok := temporal[id]; ok {
			return true
		}
	}
	return false
}

func scenarioToolIDs(tools []tool.Tool) []string {
	ids := make([]string, 0, len(tools))
	for _, candidate := range tools {
		ids = append(ids, string(candidate.ID))
	}
	return ids
}

func (svc *QA) prunedToolIDSet(tools []tool.Tool, routed []string) map[tool.ToolID]struct{} {
	candidates := routingCandidates(tools)
	allowed := baseToolIDSet(tools, candidates)
	for id := range pruneAllowance(routed, candidates) {
		allowed[id] = struct{}{}
	}
	return allowed
}

type filteredScenarioTools struct {
	base  ScenarioToolSet
	tools []tool.Tool
	byID  map[tool.ToolID]tool.Tool
}

func (filtered filteredScenarioTools) Tools() []tool.Tool {
	return append([]tool.Tool(nil), filtered.tools...)
}

func (filtered filteredScenarioTools) Get(id tool.ToolID) (tool.Tool, bool) {
	candidate, ok := filtered.byID[id]
	return candidate, ok
}

func (filtered filteredScenarioTools) ExecuteArguments(ctx context.Context, id tool.ToolID, arguments tool.Arguments) (tool.Result, error) {
	if _, ok := filtered.byID[id]; !ok {
		return tool.Result{}, fmt.Errorf("tool %q is outside the prepared scenario tools", id)
	}
	return filtered.base.ExecuteArguments(ctx, id, arguments)
}

func withoutSessionHistoryTools(prepared ScenarioToolSet) ScenarioToolSet {
	tools := prepared.Tools()
	filtered := filteredScenarioTools{
		base: prepared, tools: make([]tool.Tool, 0, len(tools)),
		byID: make(map[tool.ToolID]tool.Tool, len(tools)),
	}
	for _, candidate := range tools {
		if candidate.ID == "get_turn" || candidate.ID == "find_turns" {
			continue
		}
		filtered.tools = append(filtered.tools, candidate)
		filtered.byID[candidate.ID] = candidate
	}
	return filtered
}

func preferredToolsInstruction(ids []string) string {
	return prompts.MustRender(prompts.AgentPreferredTool, struct {
		ToolIDs string
	}{ToolIDs: strings.Join(ids, ", ")})
}

func (svc *QA) executePrefetch(
	ctx context.Context,
	runID string,
	prepared ScenarioToolSet,
	plan ToolPlan,
	stepRecorder preparationStepRecorder,
) ([]ContextBlock, error) {
	if len(plan.Prefetch) == 0 {
		return nil, nil
	}
	blocks := make([]ContextBlock, 0, len(plan.Prefetch))
	for index, call := range plan.Prefetch {
		args, err := json.Marshal(call.Arguments)
		if err != nil {
			return nil, fmt.Errorf("marshal prefetch tool %q arguments: %w", call.ToolID, err)
		}
		if len(args) == 0 || string(args) == "null" {
			args = []byte("{}")
		}
		callID := prefetchToolCallID(runID, index, call.ToolID)
		startedAt := time.Now()
		if err := recordPrefetchStep(ctx, stepRecorder, run.StepRecord{
			Kind:       run.StepKindToolCall,
			ToolCallID: callID,
			Tool:       string(call.ToolID),
			Args:       string(args),
			CreatedAt:  startedAt,
		}); err != nil {
			return nil, fmt.Errorf("record prefetch tool %q call: %w", call.ToolID, err)
		}

		candidate, ok := prepared.Get(call.ToolID)
		if !ok {
			failure := fmt.Errorf("tool is unavailable")
			if err := recordPrefetchResult(
				ctx, stepRecorder, runID, callID, call.ToolID, string(args),
				tool.Result{}, failure, "tool_unavailable", startedAt,
			); err != nil {
				return nil, err
			}
			if call.Required {
				return nil, fmt.Errorf("required prefetch tool %q is unavailable", call.ToolID)
			}
			blocks = append(blocks, unavailableToolBlock(call.ToolID, "tool is unavailable"))
			continue
		}
		if candidate.Kind != tool.KindRead || candidate.Prefetch == nil {
			failure := fmt.Errorf("tool is not eligible for prefetch")
			if err := recordPrefetchResult(
				ctx, stepRecorder, runID, callID, call.ToolID, string(args),
				tool.Result{}, failure, "tool_not_prefetch_eligible", startedAt,
			); err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("prefetch tool %q is not eligible", call.ToolID)
		}
		result, err := prepared.ExecuteArguments(ctx, call.ToolID, call.Arguments)
		if err != nil {
			if recordErr := recordPrefetchResult(
				ctx, stepRecorder, runID, callID, call.ToolID, string(args),
				result, err, "tool_execution_failed", startedAt,
			); recordErr != nil {
				return nil, recordErr
			}
			if call.Required {
				return nil, fmt.Errorf("required prefetch tool %q: %w", call.ToolID, err)
			}
			blocks = append(blocks, unavailableToolBlock(call.ToolID, err.Error()))
			continue
		}
		if err := recordPrefetchResult(
			ctx, stepRecorder, runID, callID, call.ToolID, string(args),
			result, nil, "", startedAt,
		); err != nil {
			return nil, err
		}
		references := make([]retrieval.Reference, 0, len(result.References))
		for _, ref := range result.References {
			references = append(references, retrieval.Reference{
				Type: string(ref.Type), Label: ref.Label, Target: ref.Target,
			})
		}
		blocks = append(blocks, ContextBlock{
			Source: string(call.ToolID), Title: candidate.Description,
			Content: result.Content, References: references,
			Evidence: cloneEvidenceUnits(result.EvidenceUnits),
		})
	}
	return blocks, nil
}

func recordPrefetchStep(
	ctx context.Context,
	recorder preparationStepRecorder,
	step run.StepRecord,
) error {
	if recorder == nil {
		return nil
	}
	return recorder.RecordPreparationStep(ctx, step)
}

func recordPrefetchResult(
	ctx context.Context,
	recorder preparationStepRecorder,
	runID, callID string,
	toolID tool.ToolID,
	args string,
	result tool.Result,
	executionErr error,
	deliveryError string,
	startedAt time.Time,
) error {
	if recorder == nil {
		return nil
	}
	content := result.Content
	if executionErr != nil {
		content = "error: " + executionErr.Error()
	}
	step := run.StepRecord{
		Kind:                run.StepKindToolResult,
		TraceID:             prefetchToolResultTraceID(runID, callID),
		ToolCallID:          callID,
		Tool:                string(toolID),
		Args:                args,
		Content:             content,
		PromptContent:       content,
		AuthoritativeSHA256: hashString(content),
		PromptSHA256:        hashString(content),
		SizeBytes:           int64(len(content)),
		Coverage:            result.Coverage,
		AnswerContract:      result.AnswerContract,
		Failed:              executionErr != nil,
		DeliveryError:       deliveryError,
		DurationMs:          int(time.Since(startedAt) / time.Millisecond),
		CreatedAt:           time.Now(),
	}
	if err := recorder.RecordPreparationStep(ctx, step); err != nil {
		return fmt.Errorf("record prefetch tool %q result: %w", toolID, err)
	}
	return nil
}

func prefetchToolCallID(runID string, index int, toolID tool.ToolID) string {
	seed := fmt.Sprintf("prefetch_tool_call\x00%s\x00%d\x00%s", runID, index, toolID)
	return "call_" + platform.UUIDFromString(seed)
}

func prefetchToolResultTraceID(runID, callID string) string {
	return "trc_" + platform.UUIDFromString("prefetch_tool_result\x00"+runID+"\x00"+callID)
}

func unavailableToolBlock(id tool.ToolID, reason string) ContextBlock {
	return ContextBlock{
		Source:  string(id),
		Title:   string(id) + " unavailable",
		Content: "The prefetch could not be completed: " + reason,
	}
}

const preloadedContextBudget = 16000

func (svc *QA) contextBudget() int {
	if svc.retriever != nil {
		return svc.retriever.ContextBudget()
	}
	return 48000
}

func mergePreloadedContext(context *retrieval.RetrievedContext, blocks []ContextBlock, totalBudget int) {
	if context == nil || len(blocks) == 0 {
		return
	}
	if totalBudget <= 0 {
		totalBudget = 48000
	}
	seenContent := make(map[string]struct{}, len(blocks))
	seenRefs := make(map[string]struct{}, len(context.References)+len(blocks))
	seenEvidence := make(map[string]struct{}, len(blocks))
	existingEvidenceCount := len(context.EvidenceUnits)
	for _, ref := range context.References {
		seenRefs[ref.Type+"\x00"+ref.Target] = struct{}{}
	}
	var text strings.Builder
	preloadedLimit := min(preloadedContextBudget, totalBudget)
	budget := preloadedLimit
	for _, block := range blocks {
		content := strings.TrimSpace(block.Content)
		if content == "" {
			continue
		}
		if _, duplicate := seenContent[content]; duplicate {
			continue
		}
		seenContent[content] = struct{}{}
		title := strings.TrimSpace(block.Title)
		if title == "" {
			title = strings.TrimSpace(block.Source)
		}
		if title == "" {
			title = "Preloaded Context"
		}
		section := "## " + title + "\n" + content + "\n"
		runes := []rune(section)
		truncated := len(runes) > budget
		if len(runes) > budget {
			runes = runes[:budget]
		}
		delivered := string(runes)
		text.WriteString(delivered)
		budget -= len(runes)
		for _, ref := range block.References {
			key := ref.Type + "\x00" + ref.Target
			if ref.Target == "" {
				continue
			}
			if _, duplicate := seenRefs[key]; duplicate {
				continue
			}
			seenRefs[key] = struct{}{}
			context.References = append(context.References, ref)
		}
		if !truncated {
			appendQAEvidenceUnits(context, block.Evidence, delivered, seenEvidence)
		}
		if budget == 0 {
			break
		}
	}
	if text.Len() == 0 {
		return
	}
	if context.Text != "" {
		remaining := totalBudget - (preloadedLimit - budget)
		if remaining > 0 {
			text.WriteString("\n")
			remaining--
		}
		if remaining > 0 {
			existing := []rune(context.Text)
			if len(existing) > remaining {
				existing = existing[:remaining]
				context.EvidenceUnits = context.EvidenceUnits[existingEvidenceCount:]
			}
			text.WriteString(string(existing))
		}
	}
	context.Text = text.String()
	context.EvidenceUnits = dedupeQAEvidenceUnits(context.EvidenceUnits)
	context.HitCount = len(context.References)
}

func appendQAEvidenceUnits(
	context *retrieval.RetrievedContext,
	units []tool.EvidenceUnit,
	delivered string,
	seen map[string]struct{},
) {
	for _, unit := range units {
		unit.ContentHash = hashString(delivered)
		unit.TokenCost = tooloutput.EstimateTokens(delivered)
		sections := unit.Sections
		if len(sections) == 0 {
			sections = []string{""}
		}
		for _, section := range sections {
			key := qaEvidenceUnitKey(unit, section)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			item := unit
			if section == "" {
				item.Sections = nil
			} else {
				item.Sections = []string{section}
			}
			item.Facets = append([]string(nil), unit.Facets...)
			context.EvidenceUnits = append(context.EvidenceUnits, item)
		}
	}
}

func qaEvidenceUnitKeys(unit tool.EvidenceUnit) []string {
	if len(unit.Sections) == 0 {
		return []string{qaEvidenceUnitKey(unit, "")}
	}
	keys := make([]string, 0, len(unit.Sections))
	for _, section := range unit.Sections {
		keys = append(keys, qaEvidenceUnitKey(unit, section))
	}
	return keys
}

func qaEvidenceUnitKey(unit tool.EvidenceUnit, section string) string {
	return unit.SourceKind + "\x00" + unit.Target + "\x00" + section + "\x00" + unit.Version + "\x00" + unit.TimeRange
}

func dedupeQAEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	if len(units) < 2 {
		return units
	}
	seen := make(map[string]struct{}, len(units))
	out := units[:0]
	for _, unit := range units {
		keys := qaEvidenceUnitKeys(unit)
		if len(keys) != 1 {
			for _, section := range unit.Sections {
				item := unit
				item.Sections = []string{section}
				key := qaEvidenceUnitKey(item, section)
				if _, duplicate := seen[key]; duplicate {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, item)
			}
			continue
		}
		if _, duplicate := seen[keys[0]]; duplicate {
			continue
		}
		seen[keys[0]] = struct{}{}
		out = append(out, unit)
	}
	return out
}

func canonicalRetrievalQuery(cleanQuestion, contextTerms string) string {
	q := strings.TrimSpace(cleanQuestion)
	terms := strings.TrimSpace(contextTerms)
	if terms != "" {
		return q + " " + terms
	}
	return q
}

func appendUnavailableWeb(rc *retrieval.RetrievedContext, unavailable bool) {
	if !unavailable || rc == nil {
		return
	}
	if rc.Text != "" {
		rc.Text += "\n\n"
	}
	rc.Text += "## Evidence Availability\n- Web source unavailable: web search is not configured.\n"
}
