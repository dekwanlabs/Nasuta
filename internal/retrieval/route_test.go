package retrieval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	types "github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/llm"
)

func TestBindPlanDecision(t *testing.T) {
	decision, err := bindPlanDecision(map[string]any{
		"sources":    []any{"memory", "internal", "web", "internal"},
		"confidence": 0.93,
	})
	if err != nil {
		t.Fatalf("bindPlanDecision: %v", err)
	}
	for _, source := range []types.EvidenceSources{types.Memory, types.Internal, types.Web} {
		if !decision.Plan.Has(source) {
			t.Fatalf("sources = %08b, missing %08b", decision.Plan.Sources, source)
		}
	}
	if decision.Origin != types.Model {
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
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[\"web\"],\"confidence\":0.96}}"}}]}`))
	}))
	defer server.Close()
	client := llm.NewLLMClientWithHTTP(server.URL, "key", "model", 512, server.Client())

	result, err := AnalyzeEvidence(context.Background(), client, "question", "", "question", RoutingCapabilities{Web: true}, nil, 512)
	if err != nil {
		t.Fatalf("AnalyzeEvidence: %v", err)
	}
	if !result.Decision.Plan.Has(types.Web) || result.Decision.Origin != types.Model {
		t.Fatalf("decision = %+v", result.Decision)
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
	plan := types.EvidencePlan{Sources: types.Web}
	result, err := AnalyzeForPlan(context.Background(), nil, "question", "", "question", nil, 128, plan)
	if err != nil {
		t.Fatalf("AnalyzeForPlan: %v", err)
	}
	if result.Decision.Plan.Sources != types.Web || result.Decision.Origin != types.Explicit {
		t.Fatalf("decision = %+v", result.Decision)
	}
}

func TestAnalyzeEvidenceSelectsRegisteredToolIntent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"route\":{\"sources\":[],\"confidence\":0.98},\"tools\":{\"tool_ids\":[\"runtime_logs\"]}}"}}]}`))
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

func TestBindToolIDsRejectsUnregisteredTool(t *testing.T) {
	_, err := bindToolIDs(
		map[string]any{"tool_ids": []any{"unknown"}},
		[]ToolRouteCandidate{{ID: "runtime_logs", Intent: "runtime evidence"}},
	)
	if err == nil {
		t.Fatal("unregistered routed tool was accepted")
	}
}
