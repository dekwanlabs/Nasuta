package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func validFlow() *agentapi.FlowIR {
	return &agentapi.FlowIR{
		Subject:    "订单创建",
		Status:     "complete",
		Confidence: "high",
		Nodes: []agentapi.FlowNode{
			{ID: "api", Label: "订单 API", Kind: "service"},
			{ID: "worker", Label: "订单处理器", Kind: "worker"},
		},
		Edges: []agentapi.FlowEdge{{
			From: "api", To: "worker", Protocol: "HTTP", SyncMode: "sync",
			EvidenceState: "verified", EvidenceRefs: []string{"ev-edge"},
		}},
		OpenHops: []string{"外部支付"},
	}
}

func TestCanonicalFlowAnswerEmitsOneBlockPerFlow(t *testing.T) {
	flowA := validFlow()
	flowA.Subject = "RGB 灯效"
	flowB := validFlow()
	flowB.Subject = "消息中心"
	answer := canonicalFlowAnswer("```mermaid\nflowchart LR\n fake --> invented\n```\n\n模型说明", []*agentapi.FlowIR{flowA, flowB})
	if strings.Contains(answer, "invented") {
		t.Fatalf("model diagram survived: %s", answer)
	}
	if count := strings.Count(answer, "```flowir\n"); count != 2 {
		t.Fatalf("expected 2 flowir blocks, got %d: %s", count, answer)
	}
	if !strings.Contains(answer, "模型说明") {
		t.Fatalf("prose missing: %s", answer)
	}
	for _, part := range strings.Split(answer, "```flowir\n")[1:] {
		body := part[:strings.Index(part, "```")]
		var decoded agentapi.FlowIR
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("flowir body is not valid JSON: %v", err)
		}
	}
}

func TestCanonicalFlowAnswerPassesThroughWhenAllFlowsInvalid(t *testing.T) {
	flow := validFlow()
	flow.Edges[0].EvidenceRefs = nil // verified edge without evidence is invalid
	candidate := "模型说明\n\n```mermaid\nflowchart LR\n a --> injected\n```"
	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flow})
	if answer != candidate {
		t.Fatalf("expected verbatim pass-through, got: %s", answer)
	}
}

func TestValidateRenderableFlowIRRejectsInvalidTypedData(t *testing.T) {
	// verified edge without evidence refs
	bad := validFlow()
	bad.Edges[0].EvidenceRefs = nil
	if violations := validateRenderableFlowIR(bad); len(violations) == 0 {
		t.Fatal("verified edge without evidence passed validation")
	}

	// edge referencing an unknown endpoint
	unknown := validFlow()
	unknown.Edges[0].To = "missing"
	if violations := validateRenderableFlowIR(unknown); len(violations) == 0 {
		t.Fatal("edge with unknown endpoint passed validation")
	}

	// duplicate node id
	duplicate := validFlow()
	duplicate.Edges = nil
	duplicate.Nodes[1].ID = "api"
	if violations := validateRenderableFlowIR(duplicate); len(violations) == 0 {
		t.Fatal("duplicate node id passed validation")
	}

	// canonical flow is clean
	if violations := validateRenderableFlowIR(validFlow()); len(violations) != 0 {
		t.Fatalf("canonical violations = %v", violations)
	}
}

func TestFlowsContextIsCopied(t *testing.T) {
	flow := validFlow()
	flow.Nodes = []agentapi.FlowNode{{ID: "a", Label: "A", Kind: "service"}}
	ctx := withFlows(context.Background(), []*agentapi.FlowIR{flow})
	flow.Nodes[0].Label = "changed"
	got := flowsFromContext(ctx)
	if got == nil || len(got) != 1 || got[0].Nodes[0].Label != "A" {
		t.Fatalf("context flows = %#v", got)
	}
	got[0].Nodes[0].Label = "mutated"
	if flowsFromContext(ctx)[0].Nodes[0].Label != "A" {
		t.Fatal("flowsFromContext returned shared state")
	}
}
