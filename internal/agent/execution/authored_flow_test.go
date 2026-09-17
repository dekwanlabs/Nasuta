package execution

import (
	"context"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/tool"
)

// seededEvidence returns the evidence this run observed plus one unit's handle,
// so a test flow can cite evidence the run really saw.
func seededEvidence(t *testing.T) ([]tool.EvidenceUnit, string) {
	t.Helper()
	unit := tool.EvidenceUnit{SourceKind: "code", Target: "repos/hsds/HSRecipe.java"}
	handle, ok := evidence.UnitHandle(unit)
	if !ok {
		t.Fatal("evidence handle unavailable for the seeded unit")
	}
	return []tool.EvidenceUnit{unit}, handle
}

// renderAnswer runs the one entry point both the answer turn and the forced
// conclusion use, so these tests exercise the path production takes.
func renderAnswer(t *testing.T, serverFlows []*agentapi.FlowIR, observed []tool.EvidenceUnit, content string) string {
	t.Helper()
	agent := &Agent{observer: NoopObserver()}
	result := agent.enforceFlowContract(
		withFlows(context.Background(), serverFlows, observed),
		nil,
		&llm.ChatStreamResult{Content: content},
		agentapi.RunOutputContract{},
		0,
		nil,
	)
	return result.Content
}

func authoredFlowAnswer(handle string) string {
	// Deliberately padded identifiers and a padded subject: the renderer refuses
	// a non-canonical flow, so canonicalization must trim before validating.
	return "菜谱读取共有两跳。\n\n```flowir\n" +
		`{"subject":" 菜谱读取 ","status":"partial",` +
		`"nodes":[{"id":" api ","label":"网关入口","kind":"service"},{"id":"svc","label":"菜谱中台","kind":"service"}],` +
		`"edges":[{"from":" api ","to":"svc","evidence_state":"verified","evidence_refs":["` + handle + `"]}],` +
		`"open_hops":["写链路未确认"],"confidence":"medium"}` +
		"\n```\n"
}

// A run with no delegated child still gets a diagram: the fence the model wrote
// is canonicalized in place of the raw JSON, and the surrounding prose survives.
func TestAuthoredFlowIsCanonicalizedWithoutServerFlows(t *testing.T) {
	observed, handle := seededEvidence(t)
	answer := authoredFlowAnswer(handle)

	prose, adopted := canonicalizeAuthoredFences(t.Context(), answer, observed)
	if strings.Contains(prose, "flowir") || strings.Contains(prose, `"nodes"`) {
		t.Fatalf("authored fence survived canonicalization: %q", prose)
	}
	if len(adopted) != 1 {
		t.Fatalf("adopted flows = %d, want 1", len(adopted))
	}
	flow := adopted[0]
	if flow.Subject != "菜谱读取" {
		t.Fatalf("subject = %q, want trimmed 菜谱读取", flow.Subject)
	}
	if flow.Nodes[0].Label != "网关入口" || flow.Nodes[1].Label != "菜谱中台" {
		t.Fatalf("node labels were not preserved: %#v", flow.Nodes)
	}
	// The merge owns node identity, so the assertion is that the edge still
	// lands on declared nodes rather than on the padded ids the model wrote.
	declared := map[string]struct{}{flow.Nodes[0].ID: {}, flow.Nodes[1].ID: {}}
	for _, edge := range flow.Edges {
		if _, ok := declared[edge.From]; !ok {
			t.Fatalf("edge from %q is not a declared node: %#v", edge.From, flow)
		}
		if _, ok := declared[edge.To]; !ok {
			t.Fatalf("edge to %q is not a declared node: %#v", edge.To, flow)
		}
	}
	if got := flow.Edges[0].EvidenceRefs; len(got) != 1 || got[0] != handle {
		t.Fatalf("evidence refs = %v, want [%s]", got, handle)
	}
	if flow.Edges[0].EvidenceState != "verified" {
		t.Fatalf("evidence state = %q, want verified", flow.Edges[0].EvidenceState)
	}

	rendered := renderAnswer(t, nil, observed, answer)
	if count := strings.Count(rendered, "```flowir\n"); count != 1 {
		t.Fatalf("rendered flowir blocks = %d, want 1: %q", count, rendered)
	}
	if !strings.Contains(rendered, "菜谱读取共有两跳。") {
		t.Fatalf("rendered answer lost the model prose: %q", rendered)
	}
}

// A flow may only claim what this run observed. An edge marked verified on
// handles the run never saw keeps its diagram but loses the claim: the refs are
// removed and the state is demoted, so the answer never presents an unsupported
// hop as verified.
func TestAuthoredFlowDemotesUnverifiableEdge(t *testing.T) {
	observed, _ := seededEvidence(t)
	answer := "仅一条推测连接。\n\n```flowir\n" +
		`{"subject":"菜谱","status":"partial",` +
		`"nodes":[{"id":"a","label":"入口","kind":"service"},{"id":"b","label":"中台","kind":"service"}],` +
		`"edges":[{"from":"a","to":"b","evidence_state":"verified","evidence_refs":["ev_0000000000000"]}],` +
		`"open_hops":[],"confidence":"high"}` +
		"\n```\n"

	prose, adopted := canonicalizeAuthoredFences(t.Context(), answer, observed)
	if len(adopted) != 1 {
		t.Fatalf("adopted flows = %d, want 1 (the diagram survives, demoted)", len(adopted))
	}
	edge := adopted[0].Edges[0]
	if edge.EvidenceState != "unresolved" {
		t.Fatalf("evidence state = %q, want unresolved", edge.EvidenceState)
	}
	if len(edge.EvidenceRefs) != 0 {
		t.Fatalf("unobserved evidence refs survived: %v", edge.EvidenceRefs)
	}
	if strings.Contains(prose, "ev_0000000000000") {
		t.Fatalf("raw model JSON leaked into the answer: %q", prose)
	}
	if !strings.Contains(prose, "仅一条推测连接。") {
		t.Fatalf("answer lost its prose: %q", prose)
	}
}

// A malformed block is not a flow at all: it is dropped and never reaches the
// answer, not even as raw text. This is what keeps a forced conclusion — whose
// stream is published before any caller can clean it — from leaking JSON.
func TestAuthoredFlowDropsMalformedBlock(t *testing.T) {
	observed, _ := seededEvidence(t)
	answer := "回答正文。\n\n```flowir\n{\"subject\":\"菜谱\",\"status\":\"sideways\"}\n```\n"

	rendered := renderAnswer(t, nil, observed, answer)
	if strings.Contains(rendered, "sideways") || strings.Contains(rendered, "flowir") {
		t.Fatalf("malformed block leaked into the answer: %q", rendered)
	}
	if !strings.Contains(rendered, "回答正文。") {
		t.Fatalf("answer lost its prose: %q", rendered)
	}
}

func TestFlowContractLeavesAnswersWithoutFlowFencesUntouched(t *testing.T) {
	observed, _ := seededEvidence(t)
	answer := "权威回答如下。\n\n```mermaid\nflowchart LR\n a --> b\n```\n"
	if got := renderAnswer(t, nil, observed, answer); got != answer {
		t.Fatalf("answer without a flowir fence was rewritten:\n got %q\nwant %q", got, answer)
	}
}

func TestStripAuthoredFlowBlocksKeepsOtherFencesAndUnterminatedFence(t *testing.T) {
	content := "前文\n```flowir\n{\"subject\":\"x\"}\n```\n中段\n```mermaid\nflowchart LR\n a --> b\n```\n"
	prose, bodies := stripAuthoredFlowBlocks(content)
	if len(bodies) != 1 || bodies[0] != `{"subject":"x"}` {
		t.Fatalf("bodies = %#v", bodies)
	}
	if !strings.Contains(prose, "```mermaid") || !strings.Contains(prose, "中段") {
		t.Fatalf("non-flowir markup was stripped: %q", prose)
	}
	if strings.Contains(prose, "flowir") {
		t.Fatalf("flowir fence survived: %q", prose)
	}

	truncated, bodies := stripAuthoredFlowBlocks("前文\n```flowir\n{\"subject\":\"x\"")
	if len(bodies) != 0 {
		t.Fatalf("unterminated fence produced bodies = %#v", bodies)
	}
	if !strings.Contains(truncated, "前文") {
		t.Fatalf("truncated answer lost its prose: %q", truncated)
	}
}

func TestDecodeAuthoredFlowCarriesSectionOrder(t *testing.T) {
	body := `{"subject":"菜谱","status":"partial","confidence":"low",` +
		`"nodes":[{"id":"a","label":"入口","kind":"service"}],"order":2}`
	flow, err := decodeAuthoredFlow(body)
	if err != nil {
		t.Fatalf("decodeAuthoredFlow: %v", err)
	}
	if flow.Order != 2 {
		t.Fatalf("order = %d, want 2", flow.Order)
	}

	// An absent or nonsensical number claims no section, so placement stays on
	// subject matching rather than anchoring to a section never named.
	for _, raw := range []string{`"order":0`, `"order":-1`} {
		body := `{"subject":"菜谱","status":"partial","confidence":"low",` +
			`"nodes":[{"id":"a","label":"入口","kind":"service"}],` + raw + `}`
		flow, err := decodeAuthoredFlow(body)
		if err != nil {
			t.Fatalf("decodeAuthoredFlow(%s): %v", raw, err)
		}
		if flow.Order != 0 {
			t.Fatalf("%s produced order %d, want 0", raw, flow.Order)
		}
	}

	// Unknown keys stay fatal: order is a declared field, anything else is a
	// model invention and must not be silently ignored.
	if _, err := decodeAuthoredFlow(`{"subject":"菜谱","status":"partial","confidence":"low","section":1}`); err == nil {
		t.Fatal("decodeAuthoredFlow accepted an unknown field")
	}
}

// The section number in the fence is what anchors the diagram, so a block whose
// order names a numbered bold section lands there instead of at the tail.
func TestAuthoredFlowKeepsSectionOrderForPlacement(t *testing.T) {
	observed, handle := seededEvidence(t)
	answer := "开头\n\n**1、菜谱读取**\n\n正文一\n\n**2、向量检索**\n\n正文二\n\n```flowir\n" +
		`{"subject":"菜谱读取链路","status":"partial","confidence":"medium","order":1,` +
		`"nodes":[{"id":"api","label":"网关入口","kind":"service"},{"id":"svc","label":"菜谱中台","kind":"service"}],` +
		`"edges":[{"from":"api","to":"svc","evidence_state":"verified","evidence_refs":["` + handle + `"]}]}` +
		"\n```\n"

	rendered := renderAnswer(t, nil, observed, answer)
	block := strings.Index(rendered, "```flowir\n")
	sectionTwo := strings.Index(rendered, "**2、向量检索**")
	if block < 0 || sectionTwo < 0 || block > sectionTwo {
		t.Fatalf("diagram was not anchored to section 1: %q", rendered)
	}
}

// A delegated answer renders the child flows it already merged, and an authored
// fence for a subject a child did not cover joins the same set.
func TestFlowContractMergesChildAndAuthoredFlows(t *testing.T) {
	observed, handle := seededEvidence(t)
	child := &agentapi.FlowIR{
		EntityID: "rgb-effect", Subject: "RGB 灯效端到端", Status: "partial", Confidence: "medium",
		Order: 1,
		Nodes: []agentapi.FlowNode{
			{ID: "app", Label: "App 入口", Kind: "service"},
			{ID: "shadow", Label: "设备影子", Kind: "service"},
		},
		Edges: []agentapi.FlowEdge{{
			From: "app", To: "shadow", EvidenceState: "verified", EvidenceRefs: []string{handle},
		}},
	}
	answer := "开头\n\n**1、RGB 灯效**\n\n正文一\n\n**2、菜谱读取**\n\n正文二\n\n```flowir\n" +
		`{"subject":"菜谱读取链路","status":"partial","confidence":"medium","order":2,` +
		`"nodes":[{"id":"api","label":"网关入口","kind":"service"},{"id":"svc","label":"菜谱中台","kind":"service"}],` +
		`"edges":[{"from":"api","to":"svc","evidence_state":"verified","evidence_refs":["` + handle + `"]}]}` +
		"\n```\n"

	rendered := renderAnswer(t, []*agentapi.FlowIR{child}, observed, answer)
	if got := strings.Count(rendered, "```flowir\n"); got != 2 {
		t.Fatalf("rendered flowir blocks = %d, want one per subject: %q", got, rendered)
	}
	if strings.Index(rendered, "App 入口") > strings.Index(rendered, "**2、菜谱读取**") {
		t.Fatalf("child flow was not anchored to its own section: %q", rendered)
	}
	if strings.Index(rendered, "网关入口") < strings.Index(rendered, "正文二") {
		t.Fatalf("authored flow was not anchored to its own section: %q", rendered)
	}
}
