package workflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestProjectInvestigatorHandoffScopesFacetSourceAndReferences(t *testing.T) {
	code := tool.EvidenceUnit{
		SourceKind: "code", Target: "repo/order.go", Sections: []string{"core"},
		ContentHash: "code-hash", Facets: []string{"core_flow"},
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	runtime := tool.EvidenceUnit{
		SourceKind: "runtime", Target: "trace-1", Sections: []string{"events"},
		ContentHash: "runtime-hash", Facets: []string{"runtime_behavior"},
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	block := agentapi.ContextBlock{
		Source: "qa.evidence", Title: "large seed", Content: strings.Repeat("raw evidence ", 500),
		References: []agentapi.Reference{
			{Type: "code", Target: code.Target}, {Type: "runtime", Target: runtime.Target},
		},
		Evidence: []tool.EvidenceUnit{code, runtime}, Complete: true, ContentHash: "original",
	}
	payload := mustJSON(t, map[string]any{
		"task_id": "task-1", "objective": "investigate failure",
		"entities": []map[string]string{{"id": "order"}},
		"evidence_goals": []map[string]any{
			{"id": "core_flow", "facet": "core_flow", "required": true, "sources": []string{"internal"}, "freshness": "stable", "minimum_coverage": 1},
			{"id": "runtime_behavior", "facet": "runtime_behavior", "required": true, "sources": []string{"runtime"}, "freshness": "current", "minimum_coverage": 1},
		},
		"context": map[string]any{"seed_material": []agentapi.ContextBlock{block}},
	})
	input := Handoff{
		ID: "handoff-original", WorkflowRunID: "workflow-1", ProducerNodeID: "workflow.input",
		Schema: agentapi.SchemaRef{ID: "task.contract", Version: 1}, Payload: payload,
		References: block.References, EvidenceUnits: []tool.EvidenceUnit{code, runtime}, Completeness: Complete,
	}
	original := append([]byte(nil), input.Payload...)
	result, err := projectInvestigatorHandoff(input, &TaskDirective{
		Purpose: "inspect code", RequiredFacets: []string{"core_flow"},
		InputRefs: []agentapi.EvidenceRef{{SourceKind: "code", Target: code.Target, Section: "core"}},
	}, "investigate.code.1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != projectionMatched || result.MatchedSeedCount != 1 || result.DroppedSeedCount != 0 {
		t.Fatalf("projection result = %+v", result)
	}
	if string(input.Payload) != string(original) || len(input.EvidenceUnits) != 2 {
		t.Fatal("original handoff was modified")
	}
	if len(result.Input.EvidenceUnits) != 1 || result.Input.EvidenceUnits[0].Target != code.Target {
		t.Fatalf("projected handoff evidence = %+v", result.Input.EvidenceUnits)
	}
	if len(result.Input.References) != 1 || result.Input.References[0].Target != code.Target {
		t.Fatalf("projected references = %+v", result.Input.References)
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal(result.Input.Payload, &projected); err != nil {
		t.Fatal(err)
	}
	var goals []map[string]any
	if err := json.Unmarshal(projected["evidence_goals"], &goals); err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 || goals[0]["facet"] != "core_flow" {
		t.Fatalf("projected goals = %+v", goals)
	}
	var context struct {
		SeedMaterial []agentapi.ContextBlock `json:"seed_material"`
	}
	if err := json.Unmarshal(projected["context"], &context); err != nil {
		t.Fatal(err)
	}
	if len(context.SeedMaterial) != 1 || context.SeedMaterial[0].Content == block.Content ||
		len(context.SeedMaterial[0].Evidence) != 1 || context.SeedMaterial[0].Evidence[0].Target != code.Target {
		t.Fatalf("projected seed = %+v", context.SeedMaterial)
	}
	if context.SeedMaterial[0].ContentHash == block.ContentHash || context.SeedMaterial[0].ContentHash == "" {
		t.Fatalf("projected seed hash = %q", context.SeedMaterial[0].ContentHash)
	}
}

func TestProjectInvestigatorHandoffEmptyMatchIsNotComplete(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "runtime", Target: "trace-1", ContentHash: "h",
		Facets: []string{"runtime_behavior"}, Coverage: tool.EvidenceCoverage{Complete: true},
	}
	payload := mustJSON(t, map[string]any{
		"task_id": "task-1", "objective": "investigate", "entities": []any{},
		"evidence_goals": []map[string]any{{"id": "core_flow", "facet": "core_flow", "required": true, "sources": []string{"internal"}, "freshness": "stable", "minimum_coverage": 1}},
		"context": map[string]any{"seed_material": []agentapi.ContextBlock{{
			Source: "seed", Title: "seed", Content: "raw", Evidence: []tool.EvidenceUnit{unit}, Complete: true, ContentHash: "h0",
		}}},
	})
	result, err := projectInvestigatorHandoff(Handoff{
		WorkflowRunID: "workflow-1", ProducerNodeID: "workflow.input",
		Schema: agentapi.SchemaRef{ID: "task.contract", Version: 1}, Payload: payload,
		Completeness: Complete,
	}, &TaskDirective{RequiredFacets: []string{"core_flow"}}, "investigate.code.1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != projectionEmpty || result.MatchedSeedCount != 0 {
		t.Fatalf("projection result = %+v", result)
	}
	if result.Input.Completeness != Complete {
		t.Fatal("projection changed handoff completeness into a false failure")
	}
	var projected map[string]json.RawMessage
	if err := json.Unmarshal(result.Input.Payload, &projected); err != nil {
		t.Fatal(err)
	}
	var context struct {
		SeedMaterial []agentapi.ContextBlock `json:"seed_material"`
	}
	if err := json.Unmarshal(projected["context"], &context); err != nil {
		t.Fatal(err)
	}
	if len(context.SeedMaterial) != 0 {
		t.Fatalf("unmatched seed was retained = %+v", context.SeedMaterial)
	}
}

func TestProjectInvestigatorHandoffLegacyAndBudget(t *testing.T) {
	input := Handoff{
		Schema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		Payload: json.RawMessage(`{"subject":"x"}`), ContentHash: "original-hash",
	}
	legacy, err := projectInvestigatorHandoff(input, &TaskDirective{RequiredFacets: []string{"core_flow"}}, "review.a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Status != projectionLegacy || string(legacy.Input.Payload) != string(input.Payload) {
		t.Fatalf("legacy result = %+v", legacy)
	}

	payload := json.RawMessage(`{"task_id":"task-1","objective":"investigate","entities":[],"evidence_goals":[],"context":{}}`)
	_, err = projectInvestigatorHandoff(Handoff{
		Schema: agentapi.SchemaRef{ID: "task.contract", Version: 1}, Payload: payload,
	}, &TaskDirective{RequiredFacets: []string{"core_flow"}}, "investigate.code.1", 1)
	if err == nil || !strings.Contains(err.Error(), "exceeds input budget") {
		t.Fatalf("budget error = %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestProjectInvestigatorHandoffReportsMissingEntityWithCanonicalSeparators(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "our-agent/handler.go", Sections: []string{"core_flow"},
		ContentHash: "code-hash", Facets: []string{"core_flow"}, Coverage: tool.EvidenceCoverage{Complete: true},
	}
	payload := mustJSON(t, map[string]any{
		"task_id": "task-compare", "objective": "compare systems",
		"entities": []map[string]any{
			{"id": "our_agent", "label": "Our Agent"},
			{"id": "google", "label": "Google"},
		},
		"evidence_goals": []map[string]any{{
			"id": "core_flow", "facet": "core_flow", "required": true,
			"sources": []string{"internal"}, "minimum_coverage": 2,
		}},
		"context": map[string]any{"seed_material": []agentapi.ContextBlock{{
			Source: "seed", Title: "code", Content: "our agent code",
			Evidence: []tool.EvidenceUnit{unit}, Complete: true, ContentHash: "seed-hash",
		}}},
	})
	result, err := projectInvestigatorHandoff(Handoff{
		Schema: agentapi.SchemaRef{ID: "task.contract", Version: 1}, Payload: payload,
		EvidenceUnits: []tool.EvidenceUnit{unit}, Completeness: Complete,
	}, &TaskDirective{RequiredFacets: []string{"core_flow"}, InputRefs: []agentapi.EvidenceRef{{SourceKind: "code", Target: unit.Target}}}, "investigate.code.1", 2000)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != projectionInsufficient {
		t.Fatalf("status = %s, want insufficient", result.Status)
	}
	if !reflect.DeepEqual(result.MissingEntities, []string{"google"}) {
		t.Fatalf("missing entities = %v", result.MissingEntities)
	}
	if len(result.MissingFacets) != 0 {
		t.Fatalf("missing facets = %v", result.MissingFacets)
	}
}
