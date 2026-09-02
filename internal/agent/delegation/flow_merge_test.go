package delegation

import (
	"encoding/json"
	"reflect"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func mergeTestFlow(subject, apiID, workerID, state string, refs []string) agentapi.FlowIR {
	return agentapi.FlowIR{
		Subject: subject, Status: "complete", Confidence: "high",
		Nodes: []agentapi.FlowNode{
			{ID: apiID, Label: "Order API", Kind: "service"},
			{ID: workerID, Label: "Order Worker", Kind: "worker"},
		},
		Edges: []agentapi.FlowEdge{{
			From: apiID, To: workerID, Protocol: "HTTP", SyncMode: "sync",
			EvidenceRefs: refs, EvidenceState: state,
		}},
	}
}

func TestMergeFlowIRsCanonicalizesModelNodeIDs(t *testing.T) {
	merged, err := MergeFlowIRs([]agentapi.FlowIR{
		mergeTestFlow("orders", "a", "b", "verified", []string{"ev-2"}),
		mergeTestFlow("orders", "left", "right", "verified", []string{"ev-1", "ev-2"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Nodes) != 2 || len(merged.Edges) != 1 {
		t.Fatalf("merged = %#v", merged)
	}
	if merged.Nodes[0].ID == "a" || merged.Nodes[1].ID == "b" {
		t.Fatalf("model ids leaked: %#v", merged.Nodes)
	}
	if got := merged.Edges[0].EvidenceRefs; !reflect.DeepEqual(got, []string{"ev-1", "ev-2"}) {
		t.Fatalf("refs = %#v", got)
	}
}

func TestMergeFlowIRsPropagatesConservativeEvidenceState(t *testing.T) {
	tests := []struct{ name, left, right, want string }{
		{"inferred beats verified", "verified", "inferred", "inferred"},
		{"unresolved beats inferred", "inferred", "unresolved", "unresolved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merged, err := MergeFlowIRs([]agentapi.FlowIR{
				mergeTestFlow("orders", "a", "b", tt.left, []string{"ev-1"}),
				mergeTestFlow("orders", "left", "right", tt.right, []string{"ev-2"}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := merged.Edges[0].EvidenceState; got != tt.want {
				t.Fatalf("state = %q, want %q", got, tt.want)
			}
			if merged.Status != "partial" {
				t.Fatalf("status = %q", merged.Status)
			}
		})
	}
}

func TestMergeFlowIRsOpenHopsMakePartial(t *testing.T) {
	flow := mergeTestFlow("orders", "a", "b", "verified", []string{"ev-1"})
	flow.OpenHops = []string{"worker -> payment"}
	merged, err := MergeFlowIRs([]agentapi.FlowIR{flow})
	if err != nil {
		t.Fatal(err)
	}
	if merged.Status != "partial" || len(merged.OpenHops) != 1 {
		t.Fatalf("merged = %#v", merged)
	}
}

func TestMergeFlowIRsDeterministicAndDoesNotMutateInputs(t *testing.T) {
	first := mergeTestFlow("orders", "a", "b", "verified", []string{"ev-2", "ev-1"})
	// Keep the input valid while exercising merge-side sorting without changing it.
	first.Edges[0].EvidenceRefs = []string{"ev-2", "ev-1"}
	second := mergeTestFlow("payments", "x", "y", "verified", []string{"ev-3"})
	beforeFirst, _ := json.Marshal(first)
	beforeSecond, _ := json.Marshal(second)

	left, err := MergeFlowIRs([]agentapi.FlowIR{first, second})
	if err != nil {
		t.Fatal(err)
	}
	right, err := MergeFlowIRs([]agentapi.FlowIR{second, first})
	if err != nil {
		t.Fatal(err)
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	if string(leftJSON) != string(rightJSON) {
		t.Fatalf("merge is not deterministic:\n%s\n%s", leftJSON, rightJSON)
	}
	afterFirst, _ := json.Marshal(first)
	afterSecond, _ := json.Marshal(second)
	if string(beforeFirst) != string(afterFirst) || string(beforeSecond) != string(afterSecond) {
		t.Fatal("merge mutated input")
	}
	if left.Subject != "orders / payments" {
		t.Fatalf("subject = %q", left.Subject)
	}
}

func TestMergeFlowIRsKeepsSameNamedNodesIsolatedAcrossSubjects(t *testing.T) {
	orders := mergeTestFlow("orders", "a", "b", "verified", []string{"orders-edge"})
	payments := mergeTestFlow("payments", "x", "y", "verified", []string{"payments-edge"})
	merged, err := MergeFlowIRs([]agentapi.FlowIR{orders, payments})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Nodes) != 4 || len(merged.Edges) != 2 {
		t.Fatalf("cross-subject flow was merged: nodes=%d edges=%d flow=%#v", len(merged.Nodes), len(merged.Edges), merged)
	}
	if merged.Subject != "orders / payments" {
		t.Fatalf("subject = %q", merged.Subject)
	}
	for _, edge := range merged.Edges {
		if edge.From == edge.To {
			t.Fatalf("edge collapsed into self-loop: %#v", edge)
		}
		if len(edge.EvidenceRefs) != 1 {
			t.Fatalf("edge evidence refs = %#v", edge.EvidenceRefs)
		}
	}
	// Same labels are retained as separate canonical IDs; evidence cannot be
	// transferred between subject components by the merge operation.
	seen := map[string]struct{}{}
	for _, node := range merged.Nodes {
		if _, ok := seen[node.ID]; ok {
			t.Fatalf("duplicate canonical node id: %#v", merged.Nodes)
		}
		seen[node.ID] = struct{}{}
	}
}
