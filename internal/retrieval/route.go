package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

type RoutingCapabilities struct {
	Memory bool
	Web    bool
}

type AnalysisResult struct {
	Decision  domain.PlanDecision
	Execution ExecutionSuggestion
	Question  string
	Terms     QueryTerms
	ToolIDs   []string
	Time      TimeExpr
	History   HistoryRelation
}

type ExecutionStrategy string

const (
	ExecutionSingleAgent ExecutionStrategy = "single_agent"
	ExecutionMultiAgent  ExecutionStrategy = "multi_agent"
)

type ExecutionSuggestion struct {
	Strategy   ExecutionStrategy `json:"strategy"`
	Complexity float64           `json:"complexity"`
	Confidence float64           `json:"confidence"`
	Reasons    []string          `json:"reasons"`
}

// HistoryRelation separates continuous topical affinity from concrete history dependencies.
type HistoryRelation struct {
	TopicAffinity        float64  `json:"topic_affinity"`
	Confidence           float64  `json:"confidence"`
	NeedsPriorEntities   bool     `json:"needs_prior_entities"`
	NeedsPriorConclusion bool     `json:"needs_prior_conclusion"`
	NeedsPriorEvidence   bool     `json:"needs_prior_evidence"`
	ExplicitTurnRefs     []string `json:"explicit_turn_refs"`
}

// ToolRouteCandidate is trusted routing metadata from a registered read tool.
type ToolRouteCandidate struct {
	ID             string `json:"id"`
	Intent         string `json:"intent"`
	Temporal       bool   `json:"temporal,omitempty"`
	EvidenceSource string `json:"evidence_source,omitempty"`
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
	historyExampleJSON    = `{"history_relation":{"topic_affinity":0.0,"confidence":0.0,"needs_prior_entities":false,"needs_prior_conclusion":false,"needs_prior_evidence":false,"explicit_turn_refs":[]}}`
	executionExampleJSON  = `{"execution":{"strategy":"single_agent","complexity":0.0,"confidence":0.0,"reasons":[]}}`
)

var executionReasonCodes = map[string]struct{}{
	"requires_multiple_subproblems":            {},
	"requires_cross_source_analysis":           {},
	"requires_cross_service_analysis":          {},
	"requires_independent_evidence_validation": {},
	"requires_conflict_resolution":             {},
	"requires_risk_sensitive_analysis":         {},
	"supports_parallel_investigation":          {},
	"single_focused_question":                  {},
	"single_source_sufficient":                 {},
	"subproblems_are_sequential":               {},
	"provided_context_sufficient":              {},
}

var (
	executionContract       = prompts.Text(prompts.RetrievalExecution)
	historyRelationContract = prompts.Text(prompts.RetrievalHistory)
	queryTermsContract      = prompts.Text(prompts.RetrievalQueryTerms)
	toolRoutingContract     = prompts.Text(prompts.RetrievalToolRouting)
	timeContract            = prompts.Text(prompts.RetrievalTime)
	routingContract         = prompts.Text(prompts.RetrievalRouting)
)

// AnalyzeEvidence performs the model-backed preprocessing for one question.
// Evidence routing, tool selection, history relation, and execution strategy share one call.
// Every requested section is validated before the combined decision is returned.
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
	empty := AnalysisResult{
		Question: clean, Terms: terms,
		Execution: ExecutionSuggestion{Strategy: ExecutionSingleAgent},
	}

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
		toolContract, err := prompts.Render(prompts.RetrievalToolRouting, struct {
			AvailableTools string
		}{AvailableTools: string(encoded)})
		if err != nil {
			return empty, fmt.Errorf("render tool routing contract: %w", err)
		}
		contracts = append(contracts, "Tool routing contract:\n"+toolContract)
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
	if strings.TrimSpace(routeContext) != "" {
		contracts = append(contracts, "History relation contract:\n"+historyRelationContract)
		properties = append(properties, "\"history_relation\"")
	}
	analyzeExecution := len(properties) > 0
	if analyzeExecution {
		contracts = append(contracts, "Execution routing contract:\n"+executionContract)
		properties = append(properties, "\"execution\"")
	}
	if len(properties) == 0 {
		empty.Decision = decision
		return empty, nil
	}
	if client == nil {
		return empty, fmt.Errorf("evidence planner unavailable: LLM client is nil")
	}
	system, err := prompts.Render(prompts.RetrievalPlanner, struct {
		Properties string
		Contracts  string
	}{
		Properties: strings.Join(properties, ", "),
		Contracts:  strings.Join(contracts, "\n\n"),
	})
	if err != nil {
		return empty, fmt.Errorf("render evidence planner prompt: %w", err)
	}
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
	var historyRelation HistoryRelation
	execution := ExecutionSuggestion{Strategy: ExecutionSingleAgent}
	opts := llm.CallOptions{
		MaxTokens:   maxTokens,
		MaxAttempts: 1,
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
			if analyzeExecution {
				executionRaw, ok := (*m)["execution"].(map[string]any)
				if !ok {
					return fmt.Errorf("missing execution object")
				}
				extractedExecution, err := bindExecutionSuggestion(executionRaw)
				if err != nil {
					return err
				}
				execution = extractedExecution
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
			if strings.TrimSpace(routeContext) != "" {
				historyRaw, ok := (*m)["history_relation"].(map[string]any)
				if !ok {
					return fmt.Errorf("missing history_relation object")
				}
				extracted, err := bindHistoryRelation(historyRaw, question)
				if err != nil {
					return err
				}
				historyRelation = extracted
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

	return AnalysisResult{
		Decision: decision, Question: clean, Terms: terms, ToolIDs: toolIDs,
		Time: timeExpr, History: historyRelation, Execution: execution,
	}, nil
}

func bindExecutionSuggestion(raw map[string]any) (ExecutionSuggestion, error) {
	strategy, ok := raw["strategy"].(string)
	if !ok || ExecutionStrategy(strategy) != ExecutionSingleAgent && ExecutionStrategy(strategy) != ExecutionMultiAgent {
		return ExecutionSuggestion{}, fmt.Errorf("execution.strategy must be single_agent or multi_agent")
	}
	complexity, ok := raw["complexity"].(float64)
	if !ok || complexity < 0 || complexity > 1 {
		return ExecutionSuggestion{}, fmt.Errorf("execution.complexity must be between 0 and 1")
	}
	confidence, ok := raw["confidence"].(float64)
	if !ok || confidence < 0 || confidence > 1 {
		return ExecutionSuggestion{}, fmt.Errorf("execution.confidence must be between 0 and 1")
	}
	items, ok := raw["reasons"].([]any)
	if !ok {
		return ExecutionSuggestion{}, fmt.Errorf("execution.reasons must be an array")
	}
	if len(items) > 4 {
		return ExecutionSuggestion{}, fmt.Errorf("execution.reasons exceeds 4 items")
	}
	reasons := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for index, item := range items {
		reason, ok := item.(string)
		if !ok {
			return ExecutionSuggestion{}, fmt.Errorf("execution.reasons[%d] must be a string", index)
		}
		if _, allowed := executionReasonCodes[reason]; !allowed {
			return ExecutionSuggestion{}, fmt.Errorf("execution reason %q is unknown", reason)
		}
		if _, duplicate := seen[reason]; duplicate {
			continue
		}
		seen[reason] = struct{}{}
		reasons = append(reasons, reason)
	}
	return ExecutionSuggestion{
		Strategy: ExecutionStrategy(strategy), Complexity: complexity,
		Confidence: confidence, Reasons: reasons,
	}, nil
}

func bindHistoryRelation(raw map[string]any, question string) (HistoryRelation, error) {
	readScore := func(key string) (float64, error) {
		value, ok := raw[key].(float64)
		if !ok || value < 0 || value > 1 {
			return 0, fmt.Errorf("history_relation.%s must be between 0 and 1", key)
		}
		return value, nil
	}
	readBool := func(key string) (bool, error) {
		value, ok := raw[key].(bool)
		if !ok {
			return false, fmt.Errorf("history_relation.%s must be a boolean", key)
		}
		return value, nil
	}
	affinity, err := readScore("topic_affinity")
	if err != nil {
		return HistoryRelation{}, err
	}
	confidence, err := readScore("confidence")
	if err != nil {
		return HistoryRelation{}, err
	}
	entities, err := readBool("needs_prior_entities")
	if err != nil {
		return HistoryRelation{}, err
	}
	conclusion, err := readBool("needs_prior_conclusion")
	if err != nil {
		return HistoryRelation{}, err
	}
	evidence, err := readBool("needs_prior_evidence")
	if err != nil {
		return HistoryRelation{}, err
	}
	items, ok := raw["explicit_turn_refs"].([]any)
	if !ok {
		return HistoryRelation{}, fmt.Errorf("history_relation.explicit_turn_refs must be an array")
	}
	if len(items) > 4 {
		return HistoryRelation{}, fmt.Errorf("history_relation.explicit_turn_refs exceeds 4 items")
	}
	refs := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for i, item := range items {
		ref, ok := item.(string)
		if !ok {
			return HistoryRelation{}, fmt.Errorf("history_relation.explicit_turn_refs[%d] must be a string", i)
		}
		ref = strings.TrimSpace(ref)
		if ref == "" || !strings.Contains(strings.ToLower(question), strings.ToLower(ref)) {
			continue
		}
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		refs = append(refs, ref)
	}
	if evidence {
		conclusion = true
		entities = true
	} else if conclusion {
		entities = true
	}
	return HistoryRelation{
		TopicAffinity: affinity, Confidence: confidence,
		NeedsPriorEntities: entities, NeedsPriorConclusion: conclusion,
		NeedsPriorEvidence: evidence, ExplicitTurnRefs: refs,
	}, nil
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
