package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/llm"
)

type RoutingCapabilities struct {
	Memory bool
	Web    bool
}

type AnalysisResult struct {
	Decision domain.PlanDecision
	Question string
	Terms    QueryTerms
	ToolIDs  []string
	Time     TimeExpr
}

// ToolRouteCandidate is trusted routing metadata from a registered read tool.
type ToolRouteCandidate struct {
	ID       string `json:"id"`
	Intent   string `json:"intent"`
	Temporal bool   `json:"temporal,omitempty"`
}

// routeExampleJSON and toolExampleJSON are the exact shapes the routing contract
// tells the model to return. Kept as named consts so a regression test can assert
// they validate against the schema enforced in analyzeQuestion (top-level
// "route"/"tools" wrappers). A prior flat form silently failed validation
// ("missing route object") and degraded every routed query to the internal fallback.
const (
	routeExampleJSON      = `{"route":{"sources":["internal","web"],"confidence":0.0}}`
	toolExampleJSON       = `{"tools":{"tool_ids":[]}}`
	queryTermsExampleJSON = `{"query_terms":{"domain_terms":[],"identifiers":[]}}`
	timeExampleJSON       = `{"time":{"kind":"none","n":0,"unit":"","raw":""}}`
)

const queryTermsContract = `Extract compact retrieval terms from the current question.
- domain_terms: at most 5 discriminative domain phrases, including useful non-English phrases.
- identifiers: at most 5 literal symbol names or opaque identifiers copied exactly from the question.
- Do not classify identifiers and do not return actions, resources, services, or inferred values.
Return a JSON object with this exact shape:
` + queryTermsExampleJSON

const toolRoutingContract = `Select only registered read tools whose declared intent is required by the current request.
Do not select a tool merely because its capability is available or topically related.
Resolve pronouns and omitted entities from conversation_context. When a contextual follow-up asks for the actual state of an entity already investigated with runtime evidence, keep the runtime evidence tool selected even if the user does not repeat words such as logs or online.`

const timeContract = `Normalize relative time semantics without calculating dates or using your own current time.
- kind=none when the current question has no relative time expression.
- kind=recent for an unqualified equivalent of "recently"; n=0 and unit="".
- kind=day for a calendar day relative to today; n=0 means today, -1 yesterday, -2 the day before yesterday; unit="".
- kind=last for a rolling duration; n is positive and unit is minute, hour, day, or week.
- For a vague equivalent of "recent days" with no number, use kind=last, n=0, unit=day. The server applies its configured default.
- raw must be an exact non-empty substring copied from the current question for every kind except none.
- Interpret any language, but never return absolute from/to timestamps for relative expressions.
Return a JSON object with this exact shape:
` + timeExampleJSON

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
` + routeExampleJSON

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
	plan domain.EvidencePlan,
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
	fixedPlan *domain.EvidencePlan,
) (AnalysisResult, error) {
	clean := strings.TrimSpace(question)
	terms := QueryTerms{DomainTerms: ExtractTechTerms(termsQuestion)}.normalize()
	empty := AnalysisResult{Question: clean, Terms: terms}

	var contracts []string
	var properties []string
	decision := domain.PlanDecision{}
	temporal := hasTemporalCandidate(toolCandidates)
	if fixedPlan == nil {
		contracts = append(contracts, fmt.Sprintf("Routing contract:\n%s\nRuntime capabilities: memory=%t internal=true web=%t", routingContract, capabilities.Memory, capabilities.Web))
		properties = append(properties, "\"route\"")
	} else {
		decision = domain.PlanDecision{Plan: *fixedPlan, Confidence: 1, Origin: domain.Explicit}
	}
	if len(toolCandidates) > 0 {
		encoded, _ := json.Marshal(toolCandidates)
		contracts = append(contracts, "Tool routing contract:\n"+toolRoutingContract+`
Return a JSON object with this exact shape:
`+toolExampleJSON+`
Available tools: `+string(encoded))
		properties = append(properties, "\"tools\"")
	}
	if fixedPlan == nil || len(toolCandidates) > 0 {
		contracts = append(contracts, "Query terms contract:\n"+queryTermsContract)
		properties = append(properties, "\"query_terms\"")
	}
	if temporal {
		contracts = append(contracts, "Time contract:\n"+timeContract)
		properties = append(properties, "\"time\"")
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
	var timeExpr TimeExpr
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
			if fixedPlan == nil || len(toolCandidates) > 0 {
				termsRaw, ok := (*m)["query_terms"].(map[string]any)
				if !ok {
					return fmt.Errorf("missing query_terms object")
				}
				extracted, err := bindQueryTerms(termsRaw)
				if err != nil {
					return err
				}
				extracted.Identifiers = groundedIdentifiers(extracted.Identifiers, termsQuestion)
				terms = extracted.normalize()
			}
			if temporal {
				timeRaw, ok := (*m)["time"].(map[string]any)
				if !ok {
					return fmt.Errorf("missing time object")
				}
				extracted, err := bindTimeExpr(timeRaw, question)
				if err != nil {
					return err
				}
				timeExpr = extracted
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

	return AnalysisResult{Decision: decision, Question: clean, Terms: terms, ToolIDs: toolIDs, Time: timeExpr}, nil
}

func hasTemporalCandidate(candidates []ToolRouteCandidate) bool {
	for _, candidate := range candidates {
		if candidate.Temporal {
			return true
		}
	}
	return false
}

func bindQueryTerms(raw map[string]any) (QueryTerms, error) {
	read := func(key string) ([]string, error) {
		items, ok := raw[key].([]any)
		if !ok {
			return nil, fmt.Errorf("query_terms.%s must be an array", key)
		}
		values := make([]string, 0, len(items))
		for i, item := range items {
			value, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("query_terms.%s[%d] must be a string", key, i)
			}
			values = append(values, value)
		}
		return values, nil
	}
	domainTerms, err := read("domain_terms")
	if err != nil {
		return QueryTerms{}, err
	}
	identifiers, err := read("identifiers")
	if err != nil {
		return QueryTerms{}, err
	}
	return QueryTerms{DomainTerms: domainTerms, Identifiers: identifiers}, nil
}

func groundedIdentifiers(identifiers []string, question string) []string {
	out := identifiers[:0]
	for _, identifier := range identifiers {
		identifier = strings.TrimSpace(identifier)
		if identifier != "" && strings.Contains(question, identifier) {
			out = append(out, identifier)
		}
	}
	return out
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

func bindPlanDecision(raw map[string]any) (domain.PlanDecision, error) {
	items, ok := raw["sources"].([]any)
	if !ok {
		return domain.PlanDecision{}, fmt.Errorf("sources must be an array")
	}
	var sources domain.EvidenceSources
	for _, item := range items {
		name, ok := item.(string)
		if !ok {
			return domain.PlanDecision{}, fmt.Errorf("source must be a string")
		}
		switch name {
		case "memory":
			sources |= domain.Memory
		case "internal":
			sources |= domain.Internal
		case "web":
			sources |= domain.Web
		default:
			return domain.PlanDecision{}, fmt.Errorf("unknown source %q", name)
		}
	}
	confidence, ok := raw["confidence"].(float64)
	if !ok || confidence < 0 || confidence > 1 {
		return domain.PlanDecision{}, fmt.Errorf("confidence must be between 0 and 1")
	}
	plan := domain.EvidencePlan{Sources: sources}
	if !plan.Valid() {
		return domain.PlanDecision{}, fmt.Errorf("invalid source bits")
	}
	return domain.PlanDecision{
		Plan: plan, Confidence: confidence, Origin: domain.Model,
	}, nil
}
