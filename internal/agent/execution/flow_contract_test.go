package execution

import (
	"context"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestFlowFallbackPreservesExactContractConservatively(t *testing.T) {
	contract := agentapi.RunOutputContract{
		Kind: "flow", RequireMermaid: true, Subjects: []string{"订单"}, MaxHops: 4,
	}
	state := &compiledLoop{
		ctx:    context.Background(),
		runCtx: context.Background(),
		runID:  "run-fallback",
		input:  Input{OutputContract: contract},
		result: &RunResult{Flow: &agentapi.FlowIR{
			Subject: "订单", Status: "partial", Confidence: "low",
			Nodes: []agentapi.FlowNode{
				{ID: "api", Label: "入口", Kind: "service"},
				{ID: "worker", Label: "处理", Kind: "worker"},
			},
			Edges: []agentapi.FlowEdge{{
				From: "api", To: "worker", Protocol: "HTTP", SyncMode: "sync",
				EvidenceState: "unresolved",
			}},
		}},
		answerContract: &exactAnswerContract{},
	}
	state.answerContract.Add(tool.AnswerContract{
		RequiredLiterals: []string{"report-1"},
		Delegations: []tool.DelegationAdoptionContract{{
			DelegationID: "delegation-1", ReportIDs: []string{"report-1"},
		}},
		Evidence: &tool.AnswerEvidenceContract{
			Claims: []tool.AnswerEvidenceClaim{{ClaimID: "claim-1", Decision: "unresolved"}},
			Edges: []tool.AnswerEvidenceEdge{{
				From: "api", To: "worker", Protocol: "HTTP", SyncMode: "sync", EvidenceState: "unresolved",
			}},
		},
	})
	agent := &Agent{observer: NoopObserver()}

	if !agent.useFlowFallback(state, "", context.DeadlineExceeded) {
		t.Fatal("useFlowFallback returned false")
	}
	if violations := ValidateFlowAnswer(state.result.Answer, contract); len(violations) != 0 {
		t.Fatalf("fallback violates flow contract: %v\n%s", violations, state.result.Answer)
	}
	if strings.Contains(state.result.Answer, delegationAdoptionMarkerPrefix) {
		t.Fatalf("internal adoption marker leaked into visible answer: %s", state.result.Answer)
	}
	if !strings.Contains(state.result.Answer, "report-1") || !strings.Contains(state.result.Answer, "unresolved") {
		t.Fatalf("fallback lost required uncertainty or literal: %s", state.result.Answer)
	}
	if state.result.Err != nil || len(state.result.DelegationAdoptions) != 1 ||
		state.result.DelegationAdoptions[0].Status != agentapi.DelegationNotAdopted {
		t.Fatalf("fallback outcome = %#v", state.result)
	}
}

func TestValidateFlowAnswerRequiresDiagramFirstAndProseAfter(t *testing.T) {
	contract := agentapi.RunOutputContract{
		Kind:           "flow",
		RequireMermaid: true,
		Subjects:       []string{"RGB 灯效", "消息中心"},
		MaxHops:        6,
	}
	answer := "" +
		"```mermaid\n" +
		"flowchart LR\n" +
		"  rgb[\"RGB 灯效\"] --> message[\"消息中心\"]\n" +
		"```\n\n" +
		"```mermaid\n" +
		"sequenceDiagram\n" +
		"  participant message as 消息中心\n" +
		"  message->>message: 消费事件\n" +
		"```\n\n" +
		"说明：第二跳的协议仍需结合证据确认。"

	if violations := ValidateFlowAnswer(answer, contract); len(violations) != 0 {
		t.Fatalf("valid flow answer rejected: %v", violations)
	}
}

func TestValidateFlowAnswerRejectsTextOnlyOrMisorderedAnswer(t *testing.T) {
	contract := agentapi.RunOutputContract{
		Kind:           "flow",
		RequireMermaid: true,
		Subjects:       []string{"RGB 灯效"},
		MaxHops:        2,
	}
	cases := []struct {
		name   string
		answer string
	}{
		{
			name:   "text only",
			answer: "RGB 灯效先调用服务，再投递消息。",
		},
		{
			name:   "prose before diagram",
			answer: "先说结论。\n\n```mermaid\nflowchart LR\n  rgb[\"RGB 灯效\"] --> done[\"完成\"]\n```\n\n说明。",
		},
		{
			name:   "missing prose",
			answer: "```mermaid\nflowchart LR\n  rgb[\"RGB 灯效\"] --> done[\"完成\"]\n```",
		},
		{
			name:   "hop limit",
			answer: "```mermaid\nflowchart LR\n  a --> b --> c --> d\n```\n\n说明。",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if violations := ValidateFlowAnswer(tc.answer, contract); len(violations) == 0 {
				t.Fatal("invalid flow answer was accepted")
			}
		})
	}
}

func TestDeterministicFlowFallbackRemainsExplicitlyUnresolved(t *testing.T) {
	contract := agentapi.RunOutputContract{
		Kind:           "flow",
		RequireMermaid: true,
		Subjects:       []string{"RGB 灯效", "消息中心"},
		MaxHops:        6,
	}
	fallback := deterministicFlowFallback(
		"RGB 灯效通过内部接口调用消息中心。\n\n```text\nraw draft\n```",
		contract,
	)
	if violations := ValidateFlowAnswer(fallback, contract); len(violations) != 0 {
		t.Fatalf("fallback violates flow contract: %v\n%s", violations, fallback)
	}
	if !strings.Contains(fallback, "待确认") ||
		!strings.Contains(fallback, "未通过流程图契约校验") ||
		strings.Contains(fallback, "```text") {
		t.Fatalf("fallback did not preserve explicit uncertainty safely:\n%s", fallback)
	}
}
