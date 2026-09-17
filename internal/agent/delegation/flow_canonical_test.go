package delegation

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

func canonicalFlowEvidenceIndex(t *testing.T) (map[string]tool.EvidenceUnit, string) {
	t.Helper()
	unit := tool.EvidenceUnit{SourceKind: "runbook", Target: "doc-flow"}
	handle, ok := evidence.UnitHandle(unit)
	if !ok {
		t.Fatal("evidence handle unavailable for the seeded unit")
	}
	return AddEvidenceUnits(nil, []tool.EvidenceUnit{unit}), handle
}

func TestCanonicalFlowIRTrimsAndNormalizesEvidence(t *testing.T) {
	index, handle := canonicalFlowEvidenceIndex(t)
	flow, err := CanonicalFlowIR(&agentapi.FlowIR{
		Subject: " 菜谱 ", Status: "partial", Confidence: "medium",
		Nodes: []agentapi.FlowNode{
			{ID: " a ", Label: " 入口 ", Kind: " service "},
			{ID: "b", Label: "中台", Kind: "service"},
		},
		Edges: []agentapi.FlowEdge{{
			From: " a ", To: "b", EvidenceState: "verified",
			EvidenceRefs: []string{handle, "ev_fffffffffffff"},
		}},
		OpenHops: []string{" 写链路 "},
	}, index)
	if err != nil {
		t.Fatalf("CanonicalFlowIR: %v", err)
	}
	if flow.Subject != "菜谱" || flow.Nodes[0].ID != "a" || flow.Nodes[0].Label != "入口" || flow.Nodes[0].Kind != "service" {
		t.Fatalf("flow was not canonicalized: %#v", flow)
	}
	if flow.Edges[0].From != "a" {
		t.Fatalf("edge endpoint = %q, want a", flow.Edges[0].From)
	}
	if got := flow.Edges[0].EvidenceRefs; len(got) != 1 || got[0] != handle {
		t.Fatalf("evidence refs = %v, want [%s]", got, handle)
	}
	if flow.OpenHops[0] != "写链路" {
		t.Fatalf("open hops = %v", flow.OpenHops)
	}
}

func TestCanonicalFlowIRDemotesEdgeWithoutObservedEvidence(t *testing.T) {
	index, _ := canonicalFlowEvidenceIndex(t)
	flow, err := CanonicalFlowIR(&agentapi.FlowIR{
		Subject: "菜谱", Status: "partial", Confidence: "high",
		Nodes: []agentapi.FlowNode{
			{ID: "a", Label: "入口", Kind: "service"},
			{ID: "b", Label: "中台", Kind: "service"},
		},
		Edges: []agentapi.FlowEdge{{
			From: "a", To: "b", EvidenceState: "verified",
			EvidenceRefs: []string{"ev_0000000000000"},
		}},
	}, index)
	if err != nil {
		t.Fatalf("CanonicalFlowIR: %v", err)
	}
	if flow.Edges[0].EvidenceState != "unresolved" || len(flow.Edges[0].EvidenceRefs) != 0 {
		t.Fatalf("unobserved evidence was not demoted: %#v", flow.Edges[0])
	}
}

func TestCanonicalFlowIRRejectsUnusableFlow(t *testing.T) {
	index, _ := canonicalFlowEvidenceIndex(t)
	cases := map[string]*agentapi.FlowIR{
		"nil": nil,
		"missing subject": {
			Status: "partial", Confidence: "medium",
		},
		"unknown status": {
			Subject: "菜谱", Status: "sideways", Confidence: "medium",
		},
		"edge on undeclared node": {
			Subject: "菜谱", Status: "partial", Confidence: "medium",
			Nodes: []agentapi.FlowNode{{ID: "a", Label: "入口", Kind: "service"}},
			Edges: []agentapi.FlowEdge{{From: "a", To: "ghost", EvidenceState: "inferred"}},
		},
	}
	for name, candidate := range cases {
		if _, err := CanonicalFlowIR(candidate, index); err == nil {
			t.Errorf("CanonicalFlowIR(%s) accepted an unusable flow", name)
		}
	}
}
