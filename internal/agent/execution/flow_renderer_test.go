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

func TestCanonicalFlowAnswerInterleavesBlocksWithSubjectSections(t *testing.T) {
	flowA := validFlow()
	flowA.Subject = "订单创建"
	flowB := validFlow()
	flowB.Subject = "支付通知"
	candidate := "总体结论：两个模块通过服务端事件衔接。\n\n" +
		"## 1. 订单创建\n订单创建负责接收请求并创建订单。\n已确认链路：入口 → 订单服务。\n\n" +
		"## 2、支付通知\n支付通知负责向用户发送结果。\n已确认链路：订单服务 → 通知服务。\n\n" +
		"```mermaid\nflowchart LR\n  fake --> invented\n```"

	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flowA, flowB})
	if strings.Contains(answer, "invented") {
		t.Fatalf("model diagram survived: %s", answer)
	}
	firstFlow := strings.Index(answer, "```flowir\n")
	if firstFlow < 0 {
		t.Fatalf("first flow block missing: %s", answer)
	}
	secondFlow := strings.Index(answer[firstFlow+len("```flowir\n"):], "```flowir\n")
	if secondFlow < 0 {
		t.Fatalf("second flow block missing: %s", answer)
	}
	secondFlow += firstFlow + len("```flowir\n")
	secondHeading := strings.Index(answer, "## 2、支付通知")
	if secondHeading < 0 {
		t.Fatalf("second subject heading missing: %s", answer)
	}
	if firstFlow <= strings.Index(answer, "已确认链路：入口 → 订单服务。") || firstFlow >= secondHeading {
		t.Fatalf("订单创建 flow is not after its prose and before the next section: %s", answer)
	}
	if secondFlow <= strings.Index(answer, "已确认链路：订单服务 → 通知服务。") {
		t.Fatalf("支付通知 flow is not after its prose: %s", answer)
	}
	if strings.Index(answer, "## 2、支付通知") > secondFlow {
		t.Fatalf("支付通知 flow was placed before its subject section: %s", answer)
	}
}

func TestCanonicalFlowAnswerMatchesStandaloneNumberedSubjectLines(t *testing.T) {
	flowA := validFlow()
	flowA.Subject = "订单创建"
	flowB := validFlow()
	flowB.Subject = "支付通知"
	candidate := "总体结论\n\n1、订单创建\n订单创建文字\n\n2. 支付通知\n支付通知文字"

	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flowA, flowB})
	firstFlow := strings.Index(answer, "```flowir\n")
	secondFlow := strings.Index(answer[firstFlow+len("```flowir\n"):], "```flowir\n")
	if firstFlow < 0 || secondFlow < 0 {
		t.Fatalf("expected two interleaved flow blocks: %s", answer)
	}
	secondFlow += firstFlow + len("```flowir\n")
	secondSubject := strings.Index(answer, "2. 支付通知")
	if firstFlow <= strings.Index(answer, "订单创建文字") || firstFlow >= secondSubject {
		t.Fatalf("first standalone subject flow is misplaced: %s", answer)
	}
	if secondFlow <= strings.Index(answer, "支付通知文字") {
		t.Fatalf("second standalone subject flow is misplaced: %s", answer)
	}
}

func TestCanonicalFlowAnswerPairsBlocksBySubjectWhenFlowOrderDiffers(t *testing.T) {
	flowA := validFlow()
	flowA.Subject = "订单创建"
	flowB := validFlow()
	flowB.Subject = "支付通知"
	candidate := "## 1. 订单创建\n订单创建文字\n\n## 2. 支付通知\n支付通知文字"

	// The merged flow order must not be used as a positional assumption. The
	// server should pair each diagram with the subject named in its FlowIR.
	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flowB, flowA})
	orderFlow := strings.Index(answer, `"subject":"订单创建"`)
	paymentFlow := strings.Index(answer, `"subject":"支付通知"`)
	orderHeading := strings.Index(answer, "## 1. 订单创建")
	paymentHeading := strings.Index(answer, "## 2. 支付通知")
	if orderFlow < 0 || paymentFlow < 0 {
		t.Fatalf("flow subjects missing: %s", answer)
	}
	if orderFlow <= strings.Index(answer, "订单创建文字") || orderFlow >= paymentHeading {
		t.Fatalf("订单创建 diagram is not paired with its section: %s", answer)
	}
	if paymentFlow <= strings.Index(answer, "支付通知文字") {
		t.Fatalf("支付通知 diagram is not after its prose: %s", answer)
	}
	if orderHeading >= orderFlow || paymentHeading >= paymentFlow {
		t.Fatalf("diagram was placed before its section heading: %s", answer)
	}
}

func TestCanonicalFlowAnswerPrefersLeadingSubjectOverMention(t *testing.T) {
	flowRGB := validFlow()
	flowRGB.Subject = "RGB灯效下发链路（种子证据实际覆盖；消息中心链路证据不足）"
	flowMessage := validFlow()
	flowMessage.Subject = "消息中心主链路"
	candidate := "## 1、RGB 灯效\nRGB 灯效的说明。\n\n## 2、消息中心\n消息中心的说明。"

	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flowRGB, flowMessage})
	rgbFlow := strings.Index(answer, `"subject":"RGB灯效下发链路（种子证据实际覆盖；消息中心链路证据不足）"`)
	messageFlow := strings.Index(answer, `"subject":"消息中心主链路"`)
	messageHeading := strings.Index(answer, "## 2、消息中心")
	if rgbFlow < 0 || messageFlow < 0 {
		t.Fatalf("flow subjects missing: %s", answer)
	}
	if rgbFlow <= strings.Index(answer, "RGB 灯效的说明。") || rgbFlow >= messageHeading {
		t.Fatalf("RGB diagram was not paired with the RGB section: %s", answer)
	}
	if messageFlow <= strings.Index(answer, "消息中心的说明。") {
		t.Fatalf("消息中心 diagram was not after its prose: %s", answer)
	}
}

func TestCanonicalFlowAnswerMatchesDecoratedSubjectHeading(t *testing.T) {
	flow := validFlow()
	flow.Subject = "订单创建"
	candidate := "总体结论\n\n### 1. 订单创建（主链路）\n订单创建的说明文字"

	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flow})
	flowIndex := strings.Index(answer, "```flowir\n")
	if flowIndex <= strings.Index(answer, "订单创建的说明文字") {
		t.Fatalf("decorated heading flow is not after its prose: %s", answer)
	}
}

func TestCanonicalFlowAnswerPutsLastSubjectBeforeTrailingSummary(t *testing.T) {
	flowTTS := validFlow()
	flowTTS.Subject = "TTS (语音合成) synthesis path"
	flowGeneric := validFlow()
	flowGeneric.Subject = "Evidence-derived flow"
	candidate := "结论先行：只有部分业务闭环。\n\n" +
		"## 1、RGB 灯效\nRGB 灯效的文字说明。\n\n" +
		"## 4、TTS\n只命中一处代理层实现。\n断点：对外入口与调用方链路均未验证。\n\n" +
		"**整体判断**：四个业务共用一套骨架，但仅 RGB 灯效验证到了下发。"

	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flowTTS, flowGeneric})
	ttsFlow := strings.Index(answer, `"subject":"TTS (语音合成) synthesis path"`)
	genericFlow := strings.Index(answer, `"subject":"Evidence-derived flow"`)
	summary := strings.Index(answer, "**整体判断**")
	if ttsFlow < 0 || genericFlow < 0 {
		t.Fatalf("flow subjects missing: %s", answer)
	}
	if ttsFlow <= strings.Index(answer, "断点：对外入口与调用方链路均未验证。") {
		t.Fatalf("TTS diagram is not after its prose: %s", answer)
	}
	if ttsFlow >= summary {
		t.Fatalf("TTS diagram was placed after the trailing summary: %s", answer)
	}
	if genericFlow <= summary {
		t.Fatalf("unmatched generic diagram should follow the trailing summary: %s", answer)
	}
}

func TestCanonicalFlowAnswerAppendsUnmatchedFlow(t *testing.T) {
	flow := validFlow()
	flow.Subject = "未出现在回答中的模块"
	candidate := "总体结论：回答没有提供该模块的独立 section。"
	answer := canonicalFlowAnswer(candidate, []*agentapi.FlowIR{flow})
	flowIndex := strings.Index(answer, "```flowir\n")
	if flowIndex < 0 {
		t.Fatalf("unmatched flow was dropped: %s", answer)
	}
	if flowIndex <= strings.Index(answer, candidate) {
		t.Fatalf("unmatched flow should be appended after prose: %s", answer)
	}
}

func TestCanonicalFlowAnswerRendersFlowWhenCandidateHasNoProse(t *testing.T) {
	flow := validFlow()
	flow.Subject = "订单创建"
	answer := canonicalFlowAnswer("```mermaid\nflowchart LR\n fake --> injected\n```", []*agentapi.FlowIR{flow})
	if strings.Contains(answer, "injected") {
		t.Fatalf("model diagram survived: %s", answer)
	}
	if !strings.Contains(answer, "```flowir\n") {
		t.Fatalf("authoritative flow missing: %s", answer)
	}
	if !strings.Contains(answer, "说明：流程图由服务端") {
		t.Fatalf("fallback prose missing: %s", answer)
	}
}
