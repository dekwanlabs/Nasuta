package execution

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestMergeDelegatedFlowsIsIdempotent(t *testing.T) {
	agent := &Agent{observer: NoopObserver()}
	state := &compiledLoop{
		ctx:    context.Background(),
		runID:  "run-merge",
		result: &RunResult{},
		delegatedFlows: []agentapi.FlowIR{{
			Subject: "菜谱", Status: "partial", Confidence: "low",
			Nodes: []agentapi.FlowNode{{ID: "api", Label: "入口", Kind: "service"}},
		}},
	}
	agent.mergeDelegatedFlows(state)
	if state.result.Flow == nil {
		t.Fatal("first merge did not populate result.Flow")
	}
	first := state.result.Flow
	agent.mergeDelegatedFlows(state)
	if state.result.Flow != first {
		t.Fatal("second merge mutated an already-merged flow")
	}
}

func TestMergeDelegatedFlowsNoopWhenEmpty(t *testing.T) {
	agent := &Agent{observer: NoopObserver()}
	state := &compiledLoop{ctx: context.Background(), runID: "run-empty", result: &RunResult{}}
	agent.mergeDelegatedFlows(state)
	if state.result.Flow != nil {
		t.Fatalf("empty delegated flows produced flow = %#v", state.result.Flow)
	}
}

func TestDeterministicConclusionInstallsNonEmptyAnswer(t *testing.T) {
	agent := &Agent{observer: NoopObserver()}
	contract := &exactAnswerContract{}
	contract.Add(tool.AnswerContract{
		Delegations: []tool.DelegationAdoptionContract{{
			DelegationID: "del-1",
		}},
	})
	state := &compiledLoop{
		ctx:            context.Background(),
		runID:          "run-deterministic",
		input:          Input{Question: "RGB 与消息中心如何交互？"},
		result:         &RunResult{},
		answerContract: contract,
	}
	state.result.Evidence.ResultCount = 3
	state.result.Evidence.PartialResultCount = 1

	if !agent.installDeterministicConclusion(state, errors.New("model budget exhausted")) {
		t.Fatal("installDeterministicConclusion did not install a fallback answer")
	}
	if strings.TrimSpace(state.result.Answer) == "" {
		t.Fatal("fallback answer is empty")
	}
	if !state.result.ForcedConclusion || !state.result.Evidence.ForcedConclusion {
		t.Fatalf("fallback did not mark forced conclusion: %+v", state.result)
	}
	if state.result.Err != nil {
		t.Fatalf("fallback left Err set: %v", state.result.Err)
	}
	if !strings.Contains(state.result.Answer, "RGB 与消息中心如何交互？") {
		t.Fatalf("fallback answer missing question: %q", state.result.Answer)
	}
	// The active answer contract must be consumed server-side: no unknown
	// adoption remains after the fallback installs conservative metadata.
	for _, adoption := range state.result.DelegationAdoptions {
		if adoption.Status == agentapi.DelegationUnknown {
			t.Fatalf("fallback left unknown adoption: %#v", state.result.DelegationAdoptions)
		}
	}
}

func TestDeterministicConclusionStructuredFallbackIsSchemaValidJSON(t *testing.T) {
	agent := &Agent{cfg: Config{StructuredOutput: true}}
	state := &compiledLoop{
		ctx:    context.Background(),
		runID:  "run-structured-fallback",
		input: Input{
			Question: "Execute this JSON input against output schema investigation.report version 1.",
			OriginalRequest: &agentapi.RunRequest{
				Agent: agentapi.DefinitionRef{ID: "investigator.docs"},
			},
		},
		result: &RunResult{},
	}
	if !agent.installDeterministicConclusion(state, errors.New("model budget exhausted")) {
		t.Fatal("installDeterministicConclusion did not install a fallback answer")
	}
	answer := strings.TrimSpace(state.result.Answer)
	if !json.Valid([]byte(answer)) {
		t.Fatalf("structured fallback is not valid JSON: %q", answer)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(answer), &report); err != nil {
		t.Fatalf("decode fallback: %v", err)
	}
	for _, field := range []string{"focus", "summary", "findings", "gaps", "covered_evidence_goals", "unresolved_evidence_goals"} {
		if _, ok := report[field]; !ok {
			t.Fatalf("fallback missing report field %q: %v", field, report)
		}
	}
	if report["focus"] != "docs" {
		t.Fatalf("focus = %v, want docs", report["focus"])
	}
	for _, leaked := range []string{"objective", "capability", "delegation_id", "parent_run_id", "task_index", "parent_question_summary", "focus_facets", "evidence_refs", "output_kind"} {
		if _, ok := report[leaked]; ok {
			t.Fatalf("fallback leaked task-contract field %q: %v", leaked, report)
		}
	}
	// The task question (which embeds the task contract) must not be echoed.
	if strings.Contains(answer, "Execute this JSON input") {
		t.Fatalf("fallback echoed the task question: %q", answer)
	}
}

func TestDeterministicConclusionProseDerivesFromEvidence(t *testing.T) {
	state := &compiledLoop{
		input:  Input{Question: "菜谱和 TTS 是否相关？"},
		result: &RunResult{},
	}
	state.result.Evidence.ResultCount = 2
	state.result.Evidence.ToolFailureCount = 1
	prose := deterministicConclusionProse(state)
	for _, want := range []string{
		"菜谱和 TTS 是否相关？",
		"2 条完整结果",
		"1 次工具调用失败",
	} {
		if !strings.Contains(prose, want) {
			t.Fatalf("prose missing %q: %q", want, prose)
		}
	}
}
