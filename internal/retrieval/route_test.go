package retrieval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"web\"],\"confidence\":0.96},\"query_terms\":{\"domain_terms\":[\"设备删除\"],\"identifiers\":[\"question\"]}}"}}]}`))
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
}

func TestAnalyzeEvidenceReturnsUnavailableError(t *testing.T) {
	_, err := AnalyzeEvidence(context.Background(), nil, "question", "", "question", RoutingCapabilities{}, nil, 512)
	if err == nil {
		t.Fatal("missing router must return an error")
	}
}

func TestAnalyzeEvidenceReturnsInvalidOutputError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"preprocess\":{}}"}}]}`))
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())

	if _, err := AnalyzeEvidence(context.Background(), client, "question", "", "question", RoutingCapabilities{}, nil, 512); err == nil {
		t.Fatal("missing route must return an error")
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[],\"confidence\":0.98},\"tools\":{\"tool_ids\":[\"runtime_logs\"]},\"query_terms\":{\"domain_terms\":[\"runtime failure\"],\"identifiers\":[]}}"}}]}`))
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"internal\"],\"confidence\":0.98},\"tools\":{\"tool_ids\":[\"runtime_logs\"]},\"query_terms\":{\"domain_terms\":[],\"identifiers\":[]},\"time\":{\"kind\":\"last\",\"n\":0,\"unit\":\"day\",\"raw\":\"últimos días\"}}"}}]}`))
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

func TestBindToolIDsRejectsUnregisteredTool(t *testing.T) {
	_, err := bindToolIDs(
		map[string]any{"tool_ids": []any{"unknown"}},
		[]ToolRouteCandidate{{ID: "runtime_logs", Intent: "runtime evidence"}},
	)
	if err == nil {
		t.Fatal("unregistered routed tool was accepted")
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
