package delegation

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

// A salvaged report's flow was rebuilt from leftover evidence, so the merge must
// be able to tell it apart from one the investigator authored. Origin is stamped
// server-side because the model-facing schema forbids the field.
func TestSalvagedChildReportMarksFlowOrigin(t *testing.T) {
	output := []byte(`{
		"focus":"code",
		"summary":"Evidence preserved after truncation.",
		"findings":[],
		"gaps":[],
		"covered_evidence_goals":[],
		"unresolved_evidence_goals":[],
		"flow":{
			"subject":"Evidence-derived flow",
			"status":"partial",
			"nodes":[
				{"id":"svc-a","label":"svc-a","kind":"service"},
				{"id":"svc-b","label":"svc-b","kind":"service"}
			],
			"edges":[{"from":"svc-a","to":"svc-b","protocol":"http","sync_mode":"unknown","evidence_state":"inferred"}],
			"open_hops":[],
			"confidence":"low"
		}
	}`)
	result := agentapi.RunResult{
		RunID:  "run_child_salvage",
		Status: agentapi.RunFailed,
		Output: output,
		Error:  &agentapi.RunError{Code: "invalid_output"},
		Evidence: agentapi.EvidenceSummary{
			Status: "partial", ToolCallCount: 2,
		},
		EvidenceUnits: []tool.EvidenceUnit{{
			SourceKind: "dependency", Target: "svc-a",
			Sections: []string{"outbound:svc-a->svc-b:http|Client.call"},
		}},
	}

	report, err := projectReportWithEvidence(result, "knowledge.service.trace", "rep-1", nil)
	if err != nil {
		t.Fatalf("projectReportWithEvidence: %v", err)
	}
	if report.Flow == nil {
		t.Fatal("salvaged report dropped the rebuilt flow")
	}
	if report.Flow.Origin != agentapi.FlowOriginEvidenceFallback {
		t.Fatalf("flow origin = %q, want %q", report.Flow.Origin, agentapi.FlowOriginEvidenceFallback)
	}
}

// A report the investigator produced normally keeps an unset origin, so the
// merge treats it as authored.
func TestSucceededChildReportLeavesFlowOriginUnset(t *testing.T) {
	output := []byte(`{
		"focus":"code",
		"summary":"Traced the flow.",
		"findings":[],
		"gaps":[],
		"covered_evidence_goals":[],
		"unresolved_evidence_goals":[],
		"flow":{
			"subject":"orders",
			"status":"complete",
			"nodes":[
				{"id":"svc-a","label":"Order API","kind":"service"},
				{"id":"svc-b","label":"Order Worker","kind":"worker"}
			],
			"edges":[{"from":"svc-a","to":"svc-b","protocol":"http","sync_mode":"sync","evidence_state":"inferred"}],
			"open_hops":[],
			"confidence":"high"
		}
	}`)
	result := agentapi.RunResult{
		RunID:  "run_child_ok",
		Status: agentapi.RunSucceeded,
		Output: output,
	}

	report, err := projectReportWithEvidence(result, "knowledge.service.trace", "rep-2", nil)
	if err != nil {
		t.Fatalf("projectReportWithEvidence: %v", err)
	}
	if report.Flow == nil {
		t.Fatal("authored flow was dropped")
	}
	if report.Flow.Origin == agentapi.FlowOriginEvidenceFallback {
		t.Fatal("authored flow was marked as an evidence fallback")
	}
}
