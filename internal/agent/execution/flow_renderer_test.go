package execution

import (
	"context"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestRenderFlowIRIsDeterministicAndConservative(t *testing.T) {
	flow := &agentapi.FlowIR{
		Subject:    "订单创建",
		Status:     "partial",
		Confidence: "medium",
		Nodes: []agentapi.FlowNode{
			{ID: "bad id", Label: "订单 API", Kind: "service"},
			{ID: "worker", Label: "处理器\nworker", Kind: "worker"},
		},
		Edges: []agentapi.FlowEdge{
			{From: "bad id", To: "worker", Protocol: "HTTP", SyncMode: "sync", EvidenceState: "verified"},
			{From: "worker", To: "bad id", Protocol: "SQL", SyncMode: "async", EvidenceState: "unresolved"},
		},
		OpenHops: []string{"外部支付"},
	}
	first := RenderFlowIR(flow)
	second := RenderFlowIR(flow)
	if first != second {
		t.Fatal("renderer output is not deterministic")
	}
	if !strings.HasPrefix(first, "```mermaid\nflowchart LR\n") || !strings.HasSuffix(first, "```\n") {
		t.Fatalf("unexpected fence: %q", first)
	}
	if strings.Contains(first, "bad id[") || !strings.Contains(first, "-.->|\"SQL / async / unresolved\"|") {
		t.Fatalf("unsafe or unresolved edge rendering: %s", first)
	}
	if !strings.Contains(first, "待确认：外部支付") {
		t.Fatalf("open hop missing: %s", first)
	}
}

func TestCanonicalFlowAnswerReplacesModelDiagram(t *testing.T) {
	flow := &agentapi.FlowIR{
		Subject:    "订单",
		Status:     "complete",
		Confidence: "high",
		Nodes: []agentapi.FlowNode{
			{ID: "a", Label: "入口", Kind: "service"},
			{ID: "b", Label: "处理", Kind: "worker"},
		},
		Edges: []agentapi.FlowEdge{{From: "a", To: "b", Protocol: "HTTP", SyncMode: "sync", EvidenceState: "verified", EvidenceRefs: []string{"ev-1"}}},
	}
	answer := canonicalFlowAnswer("```mermaid\nflowchart LR\n fake --> invented\n```\n\n模型说明", flow)
	if strings.Contains(answer, "invented") || !strings.Contains(answer, "HTTP / sync / verified") || !strings.Contains(answer, "模型说明") {
		t.Fatalf("answer = %s", answer)
	}
}

func TestFlowIRContextIsCopied(t *testing.T) {
	flow := &agentapi.FlowIR{Subject: "x", Status: "complete", Confidence: "high", Nodes: []agentapi.FlowNode{{ID: "a", Label: "A", Kind: "service"}}}
	ctx := withFlowIR(context.Background(), flow)
	flow.Nodes[0].Label = "changed"
	got := flowIRFromContext(ctx)
	if got == nil || got.Nodes[0].Label != "A" {
		t.Fatalf("context flow = %#v", got)
	}
	got.Nodes[0].Label = "mutated"
	if flowIRFromContext(ctx).Nodes[0].Label != "A" {
		t.Fatal("flowIRFromContext returned shared state")
	}
}

func TestValidateRenderedFlowIRAcceptsOnlyCanonicalGraph(t *testing.T) {
	flow := &agentapi.FlowIR{
		Subject: "订单",
		Status:  "complete", Confidence: "high",
		Nodes: []agentapi.FlowNode{
			{ID: "api", Label: "订单 API", Kind: "service", EvidenceRefs: []string{"ev-api"}},
			{ID: "worker", Label: "订单处理器", Kind: "worker", EvidenceRefs: []string{"ev-worker"}},
		},
		Edges: []agentapi.FlowEdge{{
			From: "api", To: "worker", Protocol: "HTTP", SyncMode: "sync",
			EvidenceState: "verified", EvidenceRefs: []string{"ev-edge"},
		}},
	}
	canonical := RenderFlowIR(flow)
	if violations := ValidateRenderedFlowIR(flow, canonical); len(violations) != 0 {
		t.Fatalf("canonical violations = %v", violations)
	}

	injected := strings.Replace(canonical, "```\n", "    invented[\"模型注入节点\"]\n```\n", 1)
	if violations := ValidateRenderedFlowIR(flow, injected); len(violations) == 0 {
		t.Fatal("injected node passed canonical render validation")
	}

	unknownEdge := strings.Replace(canonical, "```\n", "    api --> invented\n```\n", 1)
	if violations := ValidateRenderedFlowIR(flow, unknownEdge); len(violations) == 0 {
		t.Fatal("unknown edge passed canonical render validation")
	}
}

func TestValidateRenderedFlowIRRejectsMarkerOrNodeOmission(t *testing.T) {
	flow := &agentapi.FlowIR{
		Subject: "支付",
		Status:  "partial", Confidence: "medium",
		Nodes: []agentapi.FlowNode{
			{ID: "api", Label: "支付 API", Kind: "service"},
			{ID: "queue", Label: "支付队列", Kind: "queue"},
		},
		Edges: []agentapi.FlowEdge{{
			From: "api", To: "queue", Protocol: "Kafka", SyncMode: "async", EvidenceState: "unresolved",
		}},
	}
	canonical := RenderFlowIR(flow)
	markerMutated := strings.Replace(canonical, "-.->", "-->", 1)
	if violations := ValidateRenderedFlowIR(flow, markerMutated); len(violations) == 0 {
		t.Fatal("unresolved edge rendered as verified line passed validation")
	}
	missingNode := strings.Replace(canonical, "    queue[\"queue: 支付队列\"]\n", "", 1)
	if violations := ValidateRenderedFlowIR(flow, missingNode); len(violations) == 0 {
		t.Fatal("missing canonical node passed validation")
	}
}

func TestValidateRenderedFlowIRRejectsInvalidTypedEdgesAndDuplicateNodes(t *testing.T) {
	verifiedWithoutEvidence := &agentapi.FlowIR{
		Subject: "订单", Status: "complete", Confidence: "high",
		Nodes: []agentapi.FlowNode{{ID: "a", Label: "A", Kind: "service"}, {ID: "b", Label: "B", Kind: "worker"}},
		Edges: []agentapi.FlowEdge{{From: "a", To: "b", Protocol: "HTTP", SyncMode: "sync", EvidenceState: "verified"}},
	}
	if violations := ValidateRenderedFlowIR(verifiedWithoutEvidence, RenderFlowIR(verifiedWithoutEvidence)); len(violations) == 0 {
		t.Fatal("verified edge without evidence passed validation")
	}

	unknownEndpoint := cloneExecutionFlow(verifiedWithoutEvidence)
	unknownEndpoint.Edges[0].EvidenceState = "unresolved"
	unknownEndpoint.Edges[0].To = "missing"
	if violations := ValidateRenderedFlowIR(unknownEndpoint, RenderFlowIR(unknownEndpoint)); len(violations) == 0 {
		t.Fatal("edge with unknown endpoint passed validation")
	}

	duplicateNode := cloneExecutionFlow(verifiedWithoutEvidence)
	duplicateNode.Edges = nil
	duplicateNode.Nodes[1].ID = "a"
	if violations := ValidateRenderedFlowIR(duplicateNode, RenderFlowIR(duplicateNode)); len(violations) == 0 {
		t.Fatal("duplicate node id passed validation")
	}
}

func TestCanonicalFlowAnswerDegradesInvalidFlowIR(t *testing.T) {
	flow := &agentapi.FlowIR{
		Subject: "订单", Status: "complete", Confidence: "high",
		Nodes: []agentapi.FlowNode{{ID: "a", Label: "A", Kind: "service"}, {ID: "b", Label: "B", Kind: "worker"}},
		Edges: []agentapi.FlowEdge{{From: "a", To: "b", Protocol: "HTTP", SyncMode: "sync", EvidenceState: "verified"}},
	}
	answer := canonicalFlowAnswer("```mermaid\nflowchart LR\n a --> injected\n```\n\n模型说明", flow)
	if strings.Contains(answer, "injected") || strings.Contains(answer, "HTTP / sync / verified") {
		t.Fatalf("invalid flow facts leaked into canonical answer: %s", answer)
	}
	if !strings.Contains(answer, "unknown / unknown / unresolved") || !strings.Contains(answer, "未通过服务端渲染质量门禁") {
		t.Fatalf("invalid flow was not visibly degraded: %s", answer)
	}
}
