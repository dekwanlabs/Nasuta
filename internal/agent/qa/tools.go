package qa

import (
	"context"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
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

func (svc *QA) executePrefetch(ctx context.Context, prepared ScenarioToolSet, plan ToolPlan) ([]ContextBlock, error) {
	if len(plan.Prefetch) == 0 {
		return nil, nil
	}
	blocks := make([]ContextBlock, 0, len(plan.Prefetch))
	for _, call := range plan.Prefetch {
		candidate, ok := prepared.Get(call.ToolID)
		if !ok {
			if call.Required {
				return nil, fmt.Errorf("required prefetch tool %q is unavailable", call.ToolID)
			}
			blocks = append(blocks, unavailableToolBlock(call.ToolID, "tool is unavailable"))
			continue
		}
		if candidate.Kind != tool.KindRead || candidate.Prefetch == nil {
			return nil, fmt.Errorf("prefetch tool %q is not eligible", call.ToolID)
		}
		result, err := prepared.ExecuteArguments(ctx, call.ToolID, call.Arguments)
		if err != nil {
			if call.Required {
				return nil, fmt.Errorf("required prefetch tool %q: %w", call.ToolID, err)
			}
			blocks = append(blocks, unavailableToolBlock(call.ToolID, err.Error()))
			continue
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
		})
	}
	return blocks, nil
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
		if len(runes) > budget {
			runes = runes[:budget]
		}
		text.WriteString(string(runes))
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
			}
			text.WriteString(string(existing))
		}
	}
	context.Text = text.String()
	context.HitCount = len(context.References)
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
