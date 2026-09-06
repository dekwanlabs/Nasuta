package execution

import (
	"testing"

	"github.com/dekwanlabs/nasuta/internal/llm"
)

func toolDef(name string) llm.ToolDef {
	return llm.ToolDef{Type: "function", Function: llm.ToolFunctionDef{Name: name}}
}

func TestEffectiveToolsForStepRemovesDelegationToolsAfterSettlement(t *testing.T) {
	agent := &Agent{}
	state := &compiledLoop{
		tools: []llm.ToolDef{
			toolDef("delegate_investigation"),
			toolDef("delegation_status"),
			toolDef("search_runbooks"),
			toolDef("inspect_service"),
		},
		dispatchedDelegations: []string{"d-1"},
		settledDelegations:    map[string]bool{"d-1": true},
	}
	got := agent.effectiveToolsForStep(state)
	names := make([]string, 0, len(got))
	for _, def := range got {
		names = append(names, def.Function.Name)
	}
	want := "search_runbooks,inspect_service"
	if join := func() string {
		out := ""
		for i, n := range names {
			if i > 0 {
				out += ","
			}
			out += n
		}
		return out
	}(); join != want {
		t.Fatalf("tools = %v, want %s", names, want)
	}
}

func TestEffectiveToolsForStepKeepsDelegationToolsWhileUnsettled(t *testing.T) {
	agent := &Agent{}
	state := &compiledLoop{
		tools: []llm.ToolDef{
			toolDef("delegate_investigation"),
			toolDef("delegation_status"),
			toolDef("search_runbooks"),
		},
		dispatchedDelegations: []string{"d-1"},
		settledDelegations:    map[string]bool{},
	}
	got := agent.effectiveToolsForStep(state)
	if len(got) != 3 {
		t.Fatalf("tools = %d, want 3 (not settled yet)", len(got))
	}
}

func TestDelegationSettled(t *testing.T) {
	agent := &Agent{}
	cases := []struct {
		name       string
		dispatched []string
		settled    map[string]bool
		want       bool
	}{
		{name: "none dispatched", dispatched: nil, settled: nil, want: false},
		{name: "all settled", dispatched: []string{"a", "b"}, settled: map[string]bool{"a": true, "b": true}, want: true},
		{name: "partial settled", dispatched: []string{"a", "b"}, settled: map[string]bool{"a": true}, want: false},
	}
	for _, tc := range cases {
		state := &compiledLoop{dispatchedDelegations: tc.dispatched, settledDelegations: tc.settled}
		if got := agent.delegationSettled(state); got != tc.want {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
