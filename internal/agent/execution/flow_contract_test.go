package execution

import (
	"context"
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
