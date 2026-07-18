package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/llm"
)

type RoutingCapabilities struct {
	Memory bool
	Web    bool
}

type AnalysisResult struct {
	Decision types.PlanDecision
	Question string
	Terms    QueryTerms
	ToolIDs  []string
}

// ToolRouteCandidate is trusted routing metadata from a registered read tool.
type ToolRouteCandidate struct {
	ID     string `json:"id"`
	Intent string `json:"intent"`
}

const routingContract = `You are the evidence router for a software knowledge agent.
Decide which external evidence sources are required to answer the current user request reliably.

Sources:
- memory: durable user facts or preferences from earlier sessions that are not already present in the supplied conversation context.
- internal: facts about the currently indexed workspace, code, services, APIs, configuration, runbooks, schemas, or call chains.
- web: current external product documentation, third-party capabilities, standards, news, or other facts that may have changed.

Rules:
- Return no sources when the request is fully answerable from stable general knowledge or material already supplied by the user.
- A technical topic alone does not require internal retrieval. Select internal only when the answer depends on this workspace.
- Memory never establishes the current workspace, service, configuration, or schema. Select internal for those facts even when a similar memory may exist.
- Select memory only when the answer depends on cross-session user preferences, responsibilities, work context, or explicitly historical experience.
- When user background and current workspace facts are both required, select memory and internal; internal supplies the current fact and memory only supplies the user perspective.
- Select every independently required source; multi-source answers are allowed.
- Source availability does not change the evidence need. If a required source is unavailable, still select it so the application can report the missing prerequisite.
- confidence measures confidence in this routing decision, from 0 to 1.

Return a JSON object with this exact shape, using zero or more individual source names:
{"sources":["internal","web"],"confidence":0.0}`

func AnalyzeEvidence(
	ctx context.Context,
	client *llm.LLMClient,
	question, routeContext, termsQuestion string,
	capabilities RoutingCapabilities,
	toolCandidates []ToolRouteCandidate,
	maxTokens int,
) (AnalysisResult, error) {
	return analyzeQuestion(ctx, client, question, routeContext, termsQuestion, capabilities, toolCandidates, maxTokens, nil)
}

// AnalyzeForPlan runs configured preprocessing without asking the model to route.
func AnalyzeForPlan(
	ctx context.Context,
	client *llm.LLMClient,
	question, routeContext, termsQuestion string,
	toolCandidates []ToolRouteCandidate,
	maxTokens int,
	plan types.EvidencePlan,
) (AnalysisResult, error) {
	return analyzeQuestion(ctx, client, question, routeContext, termsQuestion, RoutingCapabilities{}, toolCandidates, maxTokens, &plan)
}

func analyzeQuestion(
	ctx context.Context,
	client *llm.LLMClient,
	question, routeContext, termsQuestion string,
	capabilities RoutingCapabilities,
	toolCandidates []ToolRouteCandidate,
	maxTokens int,
	fixedPlan *types.EvidencePlan,
) (AnalysisResult, error) {
	clean := strings.TrimSpace(question)
	terms := QueryTerms{DomainTerms: ExtractTechTerms(termsQuestion)}.normalize()
	empty := AnalysisResult{Question: clean, Terms: terms}

	var contracts []string
	var properties []string
	decision := types.PlanDecision{}
	if fixedPlan == nil {
		contracts = append(contracts, fmt.Sprintf("Routing contract:\n%s\nRuntime capabilities: memory=%t internal=true web=%t", routingContract, capabilities.Memory, capabilities.Web))
		properties = append(properties, "\"route\"")
	} else {
		decision = types.PlanDecision{Plan: *fixedPlan, Confidence: 1, Origin: types.Explicit}
	}
	if len(toolCandidates) > 0 {
		encoded, _ := json.Marshal(toolCandidates)
		contracts = append(contracts, `Tool routing contract:
Select only registered read tools whose declared intent is required by the current request.
Do not select a tool merely because its capability is available or topically related.
Return a JSON object with this exact shape:
{"tool_ids":[]}
Available tools: `+string(encoded))
		properties = append(properties, "\"tools\"")
	}
	if len(properties) == 0 {
		empty.Decision = decision
		return empty, nil
	}
	if client == nil {
		return empty, fmt.Errorf("evidence planner unavailable: LLM client is nil")
	}
	system := fmt.Sprintf("Complete the configured analyses in one response. Return JSON only with exactly these top-level properties: %s.\n\n%s",
		strings.Join(properties, ", "), strings.Join(contracts, "\n\n"))
	payload, _ := json.Marshal(map[string]string{
		"question":             question,
		"conversation_context": routeContext,
		"query_terms_question": termsQuestion,
	})
	if maxTokens <= 0 {
		maxTokens = helperMaxTokens
	}
	var raw map[string]any
	var toolIDs []string
	opts := llm.CallOptions{
		MaxTokens: maxTokens,
		Validate: func(p any) error {
			m, _ := p.(*map[string]any)
			if m == nil || *m == nil {
				return fmt.Errorf("missing analysis object")
			}
			if fixedPlan == nil {
				routeRaw, ok := (*m)["route"].(map[string]any)
				if !ok {
					return fmt.Errorf("missing route object")
				}
				d, err := bindPlanDecision(routeRaw)
				if err != nil {
					return err
				}
				decision = d
			}
			if len(toolCandidates) > 0 {
				toolsRaw, ok := (*m)["tools"].(map[string]any)
				if !ok {
					return fmt.Errorf("missing tools object")
				}
				ids, err := bindToolIDs(toolsRaw, toolCandidates)
				if err != nil {
					return err
				}
				toolIDs = ids
			}
			return nil
		},
	}
	if err := client.ChatJSON(ctx, system, string(payload), &raw, opts); err != nil {
		if errors.Is(err, llm.ErrInvalidJSON) {
			return empty, fmt.Errorf("evidence router invalid output: %w", err)
		}
		return empty, fmt.Errorf("evidence router failed: %w", err)
	}

	return AnalysisResult{Decision: decision, Question: clean, Terms: terms, ToolIDs: toolIDs}, nil
}

func bindToolIDs(raw map[string]any, candidates []ToolRouteCandidate) ([]string, error) {
	items, ok := raw["tool_ids"].([]any)
	if !ok {
		return nil, fmt.Errorf("tool_ids must be an array")
	}
	allowed := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		allowed[candidate.ID] = struct{}{}
	}
	selected := make(map[string]struct{}, len(items))
	ids := make([]string, 0, len(items))
	for _, item := range items {
		id, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("tool id must be a string")
		}
		if _, ok := allowed[id]; !ok {
			return nil, fmt.Errorf("unknown routed tool %q", id)
		}
		if _, duplicate := selected[id]; duplicate {
			continue
		}
		selected[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

func bindPlanDecision(raw map[string]any) (types.PlanDecision, error) {
	items, ok := raw["sources"].([]any)
	if !ok {
		return types.PlanDecision{}, fmt.Errorf("sources must be an array")
	}
	var sources types.EvidenceSources
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return types.PlanDecision{}, fmt.Errorf("source must be a string")
		}
		switch name {
		case "memory":
			sources |= types.Memory
		case "internal":
			sources |= types.Internal
		case "web":
			sources |= types.Web
		default:
			return types.PlanDecision{}, fmt.Errorf("unknown source %q", name)
		}
	}
	confidence, ok := raw["confidence"].(float64)
	if !ok || confidence < 0 || confidence > 1 {
		return types.PlanDecision{}, fmt.Errorf("confidence must be between 0 and 1")
	}
	plan := types.EvidencePlan{Sources: sources}
	if !plan.Valid() {
		return types.PlanDecision{}, fmt.Errorf("invalid source bits")
	}
	return types.PlanDecision{
		Plan: plan, Confidence: confidence, Origin: types.Model,
	}, nil
}
