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
		"investigation_goals": []map[string]any{
			{"id": "explain_core", "objective": "Explain the core flow.", "independently_useful": true, "depends_on": []string{}},
			{"id": "explain_runtime", "objective": "Explain the runtime behavior.", "independently_useful": true, "depends_on": []string{}},
		},
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
		Purpose: "inspect code", InvestigationGoalIDs: []string{"explain_core"},
		RequiredFacets: []string{"core_flow"},
		InputRefs:      []agentapi.EvidenceRef{{SourceKind: "code", Target: code.Target, Section: "core"}},
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
	var objective string
	if err := json.Unmarshal(projected["objective"], &objective); err != nil {
		t.Fatal(err)
	}
	if objective != "Explain the core flow." {
		t.Fatalf("projected objective = %q", objective)
	}
	var investigationGoals []map[string]any
	if err := json.Unmarshal(projected["investigation_goals"], &investigationGoals); err != nil {
		t.Fatal(err)
	}
	if len(investigationGoals) != 1 || investigationGoals[0]["id"] != "explain_core" {
		t.Fatalf("projected investigation goals = %+v", investigationGoals)
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

func TestProjectInvestigatorHandoffRejectsMissingBoundGoal(t *testing.T) {
	payload := mustJSON(t, map[string]any{
		"task_id": "task-1", "objective": "investigate",
		"entities": []any{},
		"investigation_goals": []map[string]any{{
			"id": "known", "objective": "Known goal",
			"independently_useful": true, "depends_on": []string{},
		}},
		"evidence_goals": []map[string]any{{
			"id": "core_flow", "facet": "core_flow", "required": true,
			"sources": []string{"internal"}, "freshness": "stable", "minimum_coverage": 1,
		}},
		"context": map[string]any{},
	})
	_, err := projectInvestigatorHandoff(Handoff{
		Schema: agentapi.SchemaRef{ID: "task.contract", Version: 1}, Payload: payload,
	}, &TaskDirective{
		Purpose: "Unknown goal", InvestigationGoalIDs: []string{"unknown"},
		RequiredFacets: []string{"core_flow"},
	}, "investigate.code.1", 1000)
	if err == nil || !strings.Contains(err.Error(), "missing bound investigation goals") {
		t.Fatalf("binding error = %v", err)
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

func TestProjectInvestigatorHandoffExplicitEmptyRefsAdmitsNoSeed(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "code", Target: "repo/order.go",
		Facets:   []string{"core_flow"},
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	payload := mustJSON(t, map[string]any{
		"task_id": "task-1", "objective": "investigate", "entities": []any{},
		"evidence_goals": []map[string]any{{
			"id": "core_flow", "facet": "core_flow", "required": true,
			"sources": []string{"internal"}, "freshness": "stable", "minimum_coverage": 1,
		}},
		"context": map[string]any{"seed_material": []agentapi.ContextBlock{{
			Source: "seed", Title: "seed", Content: "raw",
			Evidence: []tool.EvidenceUnit{unit}, Complete: true, ContentHash: "seed",
		}}},
	})
	result, err := projectInvestigatorHandoff(Handoff{
		Schema:  agentapi.SchemaRef{ID: "task.contract", Version: 1},
		Payload: payload, EvidenceUnits: []tool.EvidenceUnit{unit},
	}, &TaskDirective{
		RequiredFacets: []string{"core_flow"},
		InputRefs:      []agentapi.EvidenceRef{},
	}, "investigate.code.1", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != projectionEmpty ||
		len(result.Input.EvidenceUnits) != 0 ||
		result.MatchedSeedCount != 0 {
		t.Fatalf("explicit empty projection = %+v", result)
	}
}

func TestProjectInvestigatorHandoffUsesServerTaskEvidenceAssignment(t *testing.T) {
	unit := tool.EvidenceUnit{
		SourceKind: "codegraph", Target: "checkout.PlaceOrder",
		Sections: []string{"caller", "callee"},
		Facets:   []string{"external_dependency"},
		Coverage: tool.EvidenceCoverage{Complete: true},
	}
	report := agentapi.ContextBlock{
		Source: "delegation.report", Title: "Delegation report report-1",
		Content: `{"summary":"runtime failure"}`,
		References: []agentapi.Reference{{
			Type: "delegation_report", Target: "report-1",
		}},
		Complete: true, ContentHash: "report-hash",
	}
	evidenceBlock := agentapi.ContextBlock{
		Source: "qa.evidence", Title: "Escalated evidence", Content: "",
		Evidence: []tool.EvidenceUnit{unit}, Complete: false,
		ContentHash: "evidence-block",
	}
	payload := mustJSON(t, map[string]any{
		"task_id": "task-1", "objective": "trace checkout",
		"entities": []any{},
		"evidence_goals": []map[string]any{{
			"id": "external_dependency", "facet": "external_dependency",
			"required": true, "sources": []string{"internal"},
			"freshness": "stable", "minimum_coverage": 1,
		}},
		"task_evidence_assignments": []map[string]any{
			{
				"task_id":         "investigate.code",
				"required_facets": []string{"external_dependency"},
				"input_refs": []agentapi.EvidenceRef{{
					SourceKind: "codegraph", Target: unit.Target,
					Section: "caller",
				}},
				"context_refs": []any{},
			},
			{
				"task_id":         "investigate.runtime",
				"required_facets": []string{"external_dependency"},
				"input_refs":      []any{},
				"context_refs": []map[string]string{{
					"source": "delegation.report", "content_hash": "report-hash",
				}},
			},
		},
		"context": map[string]any{
			"seed_material": []agentapi.ContextBlock{report, evidenceBlock},
		},
	})
	input := Handoff{
		Schema:  agentapi.SchemaRef{ID: "task.contract", Version: 1},
		Payload: payload, EvidenceUnits: []tool.EvidenceUnit{unit},
		References: report.References,
	}

	code, err := projectInvestigatorHandoff(
		input,
		nil,
		"investigate.code",
		2000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if code.Task == nil ||
		len(code.Task.InputRefs) != 1 ||
		len(code.Input.EvidenceUnits) != 1 ||
		code.Input.EvidenceUnits[0].Target != unit.Target {
		t.Fatalf("code projection = %+v", code)
	}
	var codeContract contractProjection
	if err := json.Unmarshal(code.Input.Payload, &codeContract); err != nil {
		t.Fatal(err)
	}
	if len(codeContract.TaskEvidenceAssignments) != 1 ||
		codeContract.TaskEvidenceAssignments[0].TaskID != "investigate.code" ||
		len(codeContract.Context.SeedMaterial) != 1 ||
		codeContract.Context.SeedMaterial[0].Source != "qa.evidence" {
		t.Fatalf("code contract = %+v", codeContract)
	}

	runtime, err := projectInvestigatorHandoff(
		input,
		nil,
		"investigate.runtime",
		2000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Task == nil ||
		runtime.Task.InputRefs == nil ||
		len(runtime.Task.InputRefs) != 0 ||
		len(runtime.Input.EvidenceUnits) != 0 {
		t.Fatalf("runtime projection = %+v", runtime)
	}
	var runtimeContract contractProjection
	if err := json.Unmarshal(runtime.Input.Payload, &runtimeContract); err != nil {
		t.Fatal(err)
	}
	if len(runtimeContract.TaskEvidenceAssignments) != 1 ||
		runtimeContract.TaskEvidenceAssignments[0].TaskID != "investigate.runtime" ||
		len(runtimeContract.Context.SeedMaterial) != 1 ||
		runtimeContract.Context.SeedMaterial[0].ContentHash != report.ContentHash ||
		len(runtime.Input.References) != 1 ||
		runtime.Input.References[0].Target != "report-1" {
		t.Fatalf("runtime contract = %+v", runtimeContract)
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
