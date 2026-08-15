package retrieval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestBindPlanDecision(t *testing.T) {
	decision, err := bindPlanDecision(map[string]any{
		"sources":    []any{"memory", "internal", "web", "internal"},
		"confidence": 0.93,
	})
	if err != nil {
		t.Fatalf("bindPlanDecision: %v", err)
	}
	for _, source := range []domain.EvidenceSources{domain.Memory, domain.Internal, domain.Web} {
		if !decision.Plan.Has(source) {
			t.Fatalf("sources = %08b, missing %08b", decision.Plan.Sources, source)
		}
	}
	if decision.Origin != domain.Model {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestBindPlanDecisionRejectsUnknownSource(t *testing.T) {
	_, err := bindPlanDecision(map[string]any{
		"sources": []any{"database"}, "confidence": 0.8,
	})
	if err == nil {
		t.Fatal("unknown source must be rejected")
	}
}

func TestBindPlanDecisionAllowsDirect(t *testing.T) {
	decision, err := bindPlanDecision(map[string]any{
		"sources": []any{}, "confidence": 0.99,
	})
	if err != nil {
		t.Fatalf("bindPlanDecision: %v", err)
	}
	if !decision.Plan.Direct() {
		t.Fatalf("sources = %08b, want direct", decision.Plan.Sources)
	}
}

func TestAnalyzeEvidenceParsesModelDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"web\"],\"confidence\":0.96},\"query_terms\":{\"domain_terms\":[\"设备删除\"],\"identifiers\":[\"question\"]},\"execution\":{\"strategy\":\"multi_agent\",\"complexity\":0.82,\"confidence\":0.91,\"tasks\":[{\"id\":\"design\",\"objective\":\"Establish the intended behavior.\",\"independently_useful\":true,\"depends_on\":[]},{\"id\":\"implementation\",\"objective\":\"Verify the implementation behavior.\",\"independently_useful\":true,\"depends_on\":[]}],\"reasons\":[\"requires_multiple_subproblems\",\"supports_parallel_investigation\"]}}"}}]}`))
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())

	result, err := AnalyzeEvidence(context.Background(), client, "question", "", "question", RoutingCapabilities{Web: true}, nil, 512)
	if err != nil {
		t.Fatalf("AnalyzeEvidence: %v", err)
	}
	if !result.Decision.Plan.Has(domain.Web) || result.Decision.Origin != domain.Model {
		t.Fatalf("decision = %+v", result.Decision)
	}
	if len(result.Terms.DomainTerms) != 1 || result.Terms.DomainTerms[0] != "设备删除" || len(result.Terms.Identifiers) != 1 {
		t.Fatalf("terms = %+v", result.Terms)
	}
	if result.Execution.Strategy != ExecutionMultiAgent || result.Execution.Complexity != 0.82 ||
		len(result.Execution.Tasks) != 2 || len(result.Execution.Reasons) != 2 {
		t.Fatalf("execution = %+v", result.Execution)
	}
}

func TestAnalyzeEvidenceDerivesHistoryRelationInSameCall(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "previous question metadata") {
			t.Fatalf("request missing bounded history metadata: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[],\"confidence\":0.99},\"query_terms\":{\"domain_terms\":[],\"identifiers\":[]},\"execution\":{\"strategy\":\"single_agent\",\"complexity\":0.2,\"confidence\":0.9,\"tasks\":[],\"reasons\":[\"single_focused_question\"]},\"history_relation\":{\"topic_affinity\":0.7,\"confidence\":0.8,\"needs_prior_entities\":true,\"needs_prior_conclusion\":false,\"needs_prior_evidence\":false,\"explicit_turn_refs\":[]}}"}}]}`))
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())

	result, err := AnalyzeEvidence(
		context.Background(), client, "继续查", "previous question metadata", "继续查",
		RoutingCapabilities{}, nil, 512,
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !result.History.NeedsPriorEntities || result.History.TopicAffinity != 0.7 {
		t.Fatalf("calls=%d history=%+v", calls, result.History)
	}
}

func TestAnalyzeEvidenceReturnsUnavailableError(t *testing.T) {
	_, err := AnalyzeEvidence(context.Background(), nil, "question", "", "question", RoutingCapabilities{}, nil, 512)
	if err == nil {
		t.Fatal("missing router must return an error")
	}
}

func TestAnalyzeEvidenceReturnsInvalidOutputError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"preprocess\":{}}"}}]}`))
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())

	if _, err := AnalyzeEvidence(context.Background(), client, "question", "", "question", RoutingCapabilities{}, nil, 512); err == nil {
		t.Fatal("missing route must return an error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("planner requests = %d, want 1", got)
	}
}

func TestAnalyzeEvidenceDoesNotRetryTransportFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())

	if _, err := AnalyzeEvidence(context.Background(), client, "question", "", "question", RoutingCapabilities{}, nil, 512); err == nil {
		t.Fatal("transport failure must return an error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("planner requests = %d, want 1", got)
	}
}

func TestAnalyzeForPlanSkipsRouterWhenNoHelpersConfigured(t *testing.T) {
	plan := domain.EvidencePlan{Sources: domain.Web}
	result, err := AnalyzeForPlan(context.Background(), nil, "question", "", "question", nil, 128, plan)
	if err != nil {
		t.Fatalf("AnalyzeForPlan: %v", err)
	}
	if result.Decision.Plan.Sources != domain.Web || result.Decision.Origin != domain.Explicit {
		t.Fatalf("decision = %+v", result.Decision)
	}
}

func TestAnalyzeEvidenceSelectsRegisteredToolIntent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[],\"confidence\":0.98},\"tools\":{\"tool_ids\":[\"runtime_logs\"]},\"query_terms\":{\"domain_terms\":[\"runtime failure\"],\"identifiers\":[]},\"execution\":{\"strategy\":\"single_agent\",\"complexity\":0.3,\"confidence\":0.9,\"tasks\":[],\"reasons\":[\"single_source_sufficient\"]}}"}}]}`))
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())

	result, err := AnalyzeEvidence(
		context.Background(), client, "why did the request fail", "", "why did the request fail",
		RoutingCapabilities{}, []ToolRouteCandidate{{ID: "runtime_logs", Intent: "current runtime failures"}}, 512,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ToolIDs) != 1 || result.ToolIDs[0] != "runtime_logs" {
		t.Fatalf("tool ids = %v", result.ToolIDs)
	}
}

func TestAnalyzeEvidenceExtractsGroundedMultilingualTime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"internal\"],\"confidence\":0.98},\"tools\":{\"tool_ids\":[\"runtime_logs\"]},\"query_terms\":{\"domain_terms\":[],\"identifiers\":[]},\"execution\":{\"strategy\":\"single_agent\",\"complexity\":0.3,\"confidence\":0.9,\"tasks\":[],\"reasons\":[\"single_source_sufficient\"]},\"time\":{\"kind\":\"last\",\"n\":0,\"unit\":\"day\",\"raw\":\"últimos días\"}}"}}]}`))
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())
	result, err := AnalyzeEvidence(
		context.Background(), client, "muestra los últimos días", "", "muestra los últimos días",
		RoutingCapabilities{}, []ToolRouteCandidate{{ID: "runtime_logs", Intent: "runtime evidence", Temporal: true}}, 512,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Time.Kind != "last" || result.Time.N != 0 || result.Time.Unit != "day" || result.Time.Raw != "últimos días" {
		t.Fatalf("time = %#v", result.Time)
	}
}

func TestToolRoutingContractUsesConversationContextForFollowUps(t *testing.T) {
	for _, required := range []string{"Resolve pronouns", "conversation_context", "actual state", "runtime evidence"} {
		if !strings.Contains(toolRoutingContract, required) {
			t.Fatalf("tool routing contract missing %q", required)
		}
	}
}

func TestQueryTermsContractKeepsExplicitCurrentTargetIsolated(t *testing.T) {
	for _, required := range []string{"current question dominates", "genuinely omitted", "do not copy unrelated targets"} {
		if !strings.Contains(strings.ToLower(queryTermsContract), required) {
			t.Fatalf("query terms contract missing %q", required)
		}
	}
}

func TestRoutingContractSelectsMixedEvidenceForAmbiguousScope(t *testing.T) {
	contract := strings.ToLower(routingContract)
	for _, required := range []string{
		"workspace's implementation",
		"select both internal and web",
		"select web without internal only",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("routing contract missing %q", required)
		}
	}
}

func TestRoutingContractsAllowDirectAnswersWithoutTools(t *testing.T) {
	routeContract := strings.ToLower(routingContract)
	for _, required := range []string{
		"direct-answer requests",
		"casual conversation",
		"basic arithmetic or reasoning",
		"stable everyday questions",
		"regardless of the language",
		"high confidence",
	} {
		if !strings.Contains(routeContract, required) {
			t.Fatalf("routing contract missing %q", required)
		}
	}

	toolContract := strings.ToLower(toolRoutingContract)
	for _, required := range []string{
		"select no tools",
		"casual conversation",
		"basic arithmetic or reasoning",
		"material already supplied by the user",
	} {
		if !strings.Contains(toolContract, required) {
			t.Fatalf("tool routing contract missing %q", required)
		}
	}
}

func TestBindToolIDsRejectsUnregisteredTool(t *testing.T) {
	_, err := bindToolIDs(
		map[string]any{"tool_ids": []any{"unknown"}},
		[]ToolRouteCandidate{{ID: "runtime_logs", Intent: "runtime evidence"}},
	)
	if err == nil {
		t.Fatal("unregistered routed tool was accepted")
	}
}

func TestBindHistoryRelationPromotesDependenciesAndGroundsReferences(t *testing.T) {
	relation, err := bindHistoryRelation(map[string]any{
		"topic_affinity":         0.72,
		"confidence":             0.41,
		"needs_prior_entities":   false,
		"needs_prior_conclusion": false,
		"needs_prior_evidence":   true,
		"explicit_turn_refs":     []any{"turn-12", "invented-run", "turn-12"},
	}, "继续 turn-12 的证据")
	if err != nil {
		t.Fatalf("bindHistoryRelation: %v", err)
	}
	if !relation.NeedsPriorEntities || !relation.NeedsPriorConclusion || !relation.NeedsPriorEvidence {
		t.Fatalf("dependencies were not promoted: %+v", relation)
	}
	if len(relation.ExplicitTurnRefs) != 1 || relation.ExplicitTurnRefs[0] != "turn-12" {
		t.Fatalf("grounded refs = %v", relation.ExplicitTurnRefs)
	}
}

func TestBindHistoryRelationRejectsInvalidScores(t *testing.T) {
	_, err := bindHistoryRelation(map[string]any{
		"topic_affinity":         1.1,
		"confidence":             0.8,
		"needs_prior_entities":   false,
		"needs_prior_conclusion": false,
		"needs_prior_evidence":   false,
		"explicit_turn_refs":     []any{},
	}, "question")
	if err == nil {
		t.Fatal("out-of-range affinity was accepted")
	}
}

// TestRoutingExamplesValidateAgainstSchema guards against the prompt/validator
// schema drift that silently dropped web routing: the contract examples must
// parse and validate under the same schema analyzeQuestion enforces (top-level
// "route"/"tools" wrappers). A flat example such as {"sources":...} without the
// "route" wrapper is rejected as "missing route object", degrading every routed
// query to the internal fallback so web is never triggered.
func TestRoutingExamplesValidateAgainstSchema(t *testing.T) {
	routeTop := mustParseJSON(t, routeExampleJSON)
	routeRaw, ok := routeTop["route"].(map[string]any)
	if !ok {
		t.Fatalf("routeExampleJSON missing top-level \"route\" object: %s", routeExampleJSON)
	}
	if _, err := bindPlanDecision(routeRaw); err != nil {
		t.Fatalf("routeExampleJSON does not validate: %v", err)
	}

	toolTop := mustParseJSON(t, toolExampleJSON)
	toolsRaw, ok := toolTop["tools"].(map[string]any)
	if !ok {
		t.Fatalf("toolExampleJSON missing top-level \"tools\" object: %s", toolExampleJSON)
	}
	if _, err := bindToolIDs(toolsRaw, nil); err != nil {
		t.Fatalf("toolExampleJSON does not validate: %v", err)
	}

	termsTop := mustParseJSON(t, queryTermsExampleJSON)
	termsRaw, ok := termsTop["query_terms"].(map[string]any)
	if !ok {
		t.Fatalf("queryTermsExampleJSON missing top-level query_terms object: %s", queryTermsExampleJSON)
	}
	if _, err := bindQueryTerms(termsRaw); err != nil {
		t.Fatalf("queryTermsExampleJSON does not validate: %v", err)
	}

	timeTop := mustParseJSON(t, timeExampleJSON)
	timeRaw, ok := timeTop["time"].(map[string]any)
	if !ok {
		t.Fatalf("timeExampleJSON missing top-level time object: %s", timeExampleJSON)
	}
	if _, err := bindTimeExpr(timeRaw, ""); err != nil {
		t.Fatalf("timeExampleJSON does not validate: %v", err)
	}

	historyTop := mustParseJSON(t, historyExampleJSON)
	historyRaw, ok := historyTop["history_relation"].(map[string]any)
	if !ok {
		t.Fatalf("historyExampleJSON missing top-level history_relation object: %s", historyExampleJSON)
	}
	if _, err := bindHistoryRelation(historyRaw, ""); err != nil {
		t.Fatalf("historyExampleJSON does not validate: %v", err)
	}

	executionTop := mustParseJSON(t, executionExampleJSON)
	executionRaw, ok := executionTop["execution"].(map[string]any)
	if !ok {
		t.Fatalf("executionExampleJSON missing top-level execution object: %s", executionExampleJSON)
	}
	if _, err := bindExecutionSuggestion(executionRaw); err != nil {
		t.Fatalf("executionExampleJSON does not validate: %v", err)
	}
}

func TestBindExecutionSuggestionRejectsUnknownAndUnboundedValues(t *testing.T) {
	validTasks := []any{
		map[string]any{
			"id": "design", "objective": "Establish the intended behavior.",
			"independently_useful": true, "depends_on": []any{},
		},
		map[string]any{
			"id": "implementation", "objective": "Verify the implementation behavior.",
			"independently_useful": true, "depends_on": []any{},
		},
	}
	tests := []map[string]any{
		{
			"strategy": "dynamic_team", "complexity": 0.8, "confidence": 0.9,
			"tasks": []any{}, "reasons": []any{"single_focused_question"},
		},
		{
			"strategy": "multi_agent", "complexity": 1.1, "confidence": 0.9,
			"tasks": validTasks, "reasons": []any{"requires_multiple_subproblems"},
		},
		{
			"strategy": "multi_agent", "complexity": 0.8, "confidence": 0.9,
			"tasks": validTasks, "reasons": []any{"specific_keyword_rule"},
		},
		// The blank signature (all template defaults) means the model echoed the
		// example verbatim instead of judging the request.
		{
			"strategy": "single_agent", "complexity": 0.0, "confidence": 0.0,
			"tasks": []any{}, "reasons": []any{},
		},
	}
	for _, raw := range tests {
		if _, err := bindExecutionSuggestion(raw); err == nil {
			t.Fatalf("invalid execution suggestion accepted: %#v", raw)
		}
	}
}

func TestBindExecutionSuggestionCountsObjectiveLengthInCharacters(t *testing.T) {
	tasks := []any{
		map[string]any{
			"id": "design", "objective": strings.Repeat("界", 500),
			"independently_useful": true, "depends_on": []any{},
		},
		map[string]any{
			"id": "implementation", "objective": "Verify the implementation behavior.",
			"independently_useful": true, "depends_on": []any{},
		},
	}
	raw := map[string]any{
		"strategy": "multi_agent", "complexity": 0.8, "confidence": 0.9,
		"tasks": tasks, "reasons": []any{"requires_multiple_subproblems"},
	}
	if _, err := bindExecutionSuggestion(raw); err != nil {
		t.Fatalf("500-character objective rejected: %v", err)
	}
	tasks[0].(map[string]any)["objective"] = strings.Repeat("界", 501)
	if _, err := bindExecutionSuggestion(raw); err == nil {
		t.Fatal("501-character objective accepted")
	}
}

func TestBindExecutionSuggestionDeduplicatesStableReasons(t *testing.T) {
	suggestion, err := bindExecutionSuggestion(map[string]any{
		"strategy": "multi_agent", "complexity": 0.8, "confidence": 0.9,
		"tasks": []any{
			map[string]any{
				"id": "design", "objective": "Establish the intended behavior.",
				"independently_useful": true, "depends_on": []any{},
			},
			map[string]any{
				"id": "implementation", "objective": "Verify the implementation behavior.",
				"independently_useful": true, "depends_on": []any{},
			},
		},
		"reasons": []any{"requires_multiple_subproblems", "requires_multiple_subproblems"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(suggestion.Reasons) != 1 || suggestion.Reasons[0] != "requires_multiple_subproblems" {
		t.Fatalf("suggestion = %+v", suggestion)
	}
}

func TestBindExecutionSuggestionRejectsNonIndependentMultiAgentTasks(t *testing.T) {
	validTasks := []any{
		map[string]any{
			"id": "design", "objective": "Establish the intended behavior.",
			"independently_useful": true, "depends_on": []any{},
		},
		map[string]any{
			"id": "implementation", "objective": "Verify the implementation behavior.",
			"independently_useful": true, "depends_on": []any{},
		},
	}
	tests := []struct {
		name     string
		strategy string
		tasks    []any
	}{
		{name: "single agent has tasks", strategy: "single_agent", tasks: validTasks},
		{name: "multi agent has one task", strategy: "multi_agent", tasks: validTasks[:1]},
		{
			name: "task is not independently useful", strategy: "multi_agent",
			tasks: []any{
				validTasks[0],
				map[string]any{
					"id": "implementation", "objective": "Verify the implementation behavior.",
					"independently_useful": false, "depends_on": []any{},
				},
			},
		},
		{
			name: "tasks are sequential", strategy: "multi_agent",
			tasks: []any{
				validTasks[0],
				map[string]any{
					"id": "implementation", "objective": "Verify the implementation behavior.",
					"independently_useful": true, "depends_on": []any{"design"},
				},
			},
		},
		{
			name: "duplicate objectives", strategy: "multi_agent",
			tasks: []any{
				validTasks[0],
				map[string]any{
					"id": "implementation", "objective": "Establish the intended behavior.",
					"independently_useful": true, "depends_on": []any{},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := bindExecutionSuggestion(map[string]any{
				"strategy": test.strategy, "complexity": 0.8, "confidence": 0.9,
				"tasks": test.tasks, "reasons": []any{"requires_multiple_subproblems"},
			})
			if err == nil {
				t.Fatalf("invalid execution tasks accepted: %#v", test.tasks)
			}
		})
	}
}

func TestGroundedIdentifiersDropsModelInventedValues(t *testing.T) {
	got := groundedIdentifiers([]string{"literal-42", "invented"}, "inspect literal-42")
	if len(got) != 1 || got[0] != "literal-42" {
		t.Fatalf("grounded identifiers = %v", got)
	}
}

func mustParseJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("example is not valid JSON %q: %v", raw, err)
	}
	return m
}
