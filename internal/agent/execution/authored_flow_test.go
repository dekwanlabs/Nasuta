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

// seededLedger returns a ledger holding one observed evidence unit plus that
// unit's handle, so a test flow can cite evidence this run really saw.
func seededLedger(t *testing.T) (*runEvidenceLedger, string) {
	t.Helper()
	unit := tool.EvidenceUnit{SourceKind: "code", Target: "repos/hsds/HSRecipe.java"}
	handle, ok := evidence.UnitHandle(unit)
	if !ok {
		t.Fatal("evidence handle unavailable for the seeded unit")
	}
	return newRunEvidenceLedger([]tool.EvidenceUnit{unit}, nil), handle
}

func authoredFlowAnswer(handle string) string {
	// Deliberately padded identifiers and a padded subject: the renderer refuses
	// a non-canonical flow, so adoption must trim before validating.
	return "菜谱读取共有两跳。\n\n```flowir\n" +
		`{"subject":" 菜谱读取 ","status":"partial",` +
		`"nodes":[{"id":" api ","label":"网关入口","kind":"service"},{"id":"svc","label":"菜谱中台","kind":"service"}],` +
		`"edges":[{"from":" api ","to":"svc","evidence_state":"verified","evidence_refs":["` + handle + `"]}],` +
		`"open_hops":["写链路未确认"],"confidence":"medium"}` +
		"\n```\n"
}

func TestAdoptAuthoredFlowsCanonicalizesAndStripsFence(t *testing.T) {
	ledger, handle := seededLedger(t)
	agent := &Agent{observer: NoopObserver()}
	state := &compiledLoop{
		ctx: context.Background(), runID: "run-authored",
		result: &RunResult{}, evidenceLedger: ledger,
	}

	prose := agent.adoptAuthoredFlows(state, authoredFlowAnswer(handle))
	if strings.Contains(prose, "flowir") || strings.Contains(prose, `"nodes"`) {
		t.Fatalf("authored fence survived adoption: %q", prose)
	}
	if len(state.result.Flows) != 1 {
		t.Fatalf("adopted flows = %d, want 1", len(state.result.Flows))
	}
	flow := state.result.Flows[0]
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

	rendered := agent.enforceFlowContract(
		withFlows(context.Background(), state.result.Flows),
		nil,
		&llm.ChatStreamResult{Content: prose},
		agentapi.RunOutputContract{},
		0,
		nil,
	)
	if count := strings.Count(rendered.Content, "```flowir\n"); count != 1 {
		t.Fatalf("rendered flowir blocks = %d, want 1: %q", count, rendered.Content)
	}
	if !strings.Contains(rendered.Content, "菜谱读取共有两跳。") {
		t.Fatalf("rendered answer lost the model prose: %q", rendered.Content)
	}
}

// A flow may only claim what this run observed. An edge marked verified on
// handles the run never saw keeps its diagram but loses the claim: the refs are
// removed and the state is demoted, so the answer never presents an unsupported
// hop as verified.
func TestAdoptAuthoredFlowsDemotesUnverifiableEdge(t *testing.T) {
	ledger, _ := seededLedger(t)
	agent := &Agent{observer: NoopObserver()}
	state := &compiledLoop{
		ctx: context.Background(), runID: "run-unverifiable",
		result: &RunResult{}, evidenceLedger: ledger,
	}
	answer := "仅一条推测连接。\n\n```flowir\n" +
		`{"subject":"菜谱","status":"partial",` +
		`"nodes":[{"id":"a","label":"入口","kind":"service"},{"id":"b","label":"中台","kind":"service"}],` +
		`"edges":[{"from":"a","to":"b","evidence_state":"verified","evidence_refs":["ev_0000000000000"]}],` +
		`"open_hops":[],"confidence":"high"}` +
		"\n```\n"

	prose := agent.adoptAuthoredFlows(state, answer)
	if len(state.result.Flows) != 1 {
		t.Fatalf("adopted flows = %d, want 1 (the diagram survives, demoted)", len(state.result.Flows))
	}
	edge := state.result.Flows[0].Edges[0]
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
// answer, not even as raw text.
func TestAdoptAuthoredFlowsDropsMalformedBlock(t *testing.T) {
	ledger, _ := seededLedger(t)
	agent := &Agent{observer: NoopObserver()}
	state := &compiledLoop{
		ctx: context.Background(), runID: "run-malformed",
		result: &RunResult{}, evidenceLedger: ledger,
	}
	answer := "回答正文。\n\n```flowir\n{\"subject\":\"菜谱\",\"status\":\"sideways\"}\n```\n"

	prose := agent.adoptAuthoredFlows(state, answer)
	if state.result.Flows != nil {
		t.Fatalf("malformed block was adopted: %#v", state.result.Flows)
	}
	if strings.Contains(prose, "sideways") || strings.Contains(prose, "flowir") {
		t.Fatalf("malformed block leaked into the answer: %q", prose)
	}
	if !strings.Contains(prose, "回答正文。") {
		t.Fatalf("answer lost its prose: %q", prose)
	}
}

func TestAdoptAuthoredFlowsLeavesAnswersWithoutFlowFencesUntouched(t *testing.T) {
	ledger, _ := seededLedger(t)
	agent := &Agent{observer: NoopObserver()}
	state := &compiledLoop{
		ctx: context.Background(), runID: "run-plain",
		result: &RunResult{}, evidenceLedger: ledger,
	}
	answer := "权威回答如下。\n\n```mermaid\nflowchart LR\n a --> b\n```\n"
	if got := agent.adoptAuthoredFlows(state, answer); got != answer {
		t.Fatalf("answer without a flowir fence was rewritten:\n got %q\nwant %q", got, answer)
	}
	if state.result.Flows != nil {
		t.Fatalf("flows = %#v, want none", state.result.Flows)
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
func TestAdoptAuthoredFlowsKeepsSectionOrderForPlacement(t *testing.T) {
	ledger, handle := seededLedger(t)
	agent := &Agent{observer: NoopObserver()}
	state := &compiledLoop{
		ctx: context.Background(), runID: "run-order",
		result: &RunResult{}, evidenceLedger: ledger,
	}
	answer := "开头\n\n**1、菜谱读取**\n\n正文一\n\n**2、向量检索**\n\n正文二\n\n```flowir\n" +
		`{"subject":"菜谱读取链路","status":"partial","confidence":"medium","order":1,` +
		`"nodes":[{"id":"api","label":"网关入口","kind":"service"},{"id":"svc","label":"菜谱中台","kind":"service"}],` +
		`"edges":[{"from":"api","to":"svc","evidence_state":"verified","evidence_refs":["` + handle + `"]}]}` +
		"\n```\n"

	prose := agent.adoptAuthoredFlows(state, answer)
	if len(state.result.Flows) != 1 || state.result.Flows[0].Order != 1 {
		t.Fatalf("adopted flows = %#v, want one flow with order 1", state.result.Flows)
	}
	rendered := agent.enforceFlowContract(
		withFlows(context.Background(), state.result.Flows),
		nil,
		&llm.ChatStreamResult{Content: prose},
		agentapi.RunOutputContract{},
		0,
		nil,
	)
	block := strings.Index(rendered.Content, "```flowir\n")
	sectionTwo := strings.Index(rendered.Content, "**2、向量检索**")
	if block < 0 || sectionTwo < 0 || block > sectionTwo {
		t.Fatalf("diagram was not anchored to section 1: %q", rendered.Content)
	}
}
