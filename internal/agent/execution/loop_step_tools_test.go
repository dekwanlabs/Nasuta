package execution

import (
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func TestToolsForStepReservesLastStructuredTurn(t *testing.T) {
	agent := &Agent{cfg: Config{StructuredOutput: true}}
	state := &compiledLoop{
		tools:     []llm.ToolDef{{Type: "function", Function: llm.ToolFunctionDef{Name: "search_runbooks"}}},
		stepLimit: 4,
	}
	if got := agent.toolsForStep(state, 3); len(got) != 1 {
		t.Fatalf("step 3 tools = %d, want search tools available", len(got))
	}
	if got := agent.toolsForStep(state, 4); got != nil {
		t.Fatalf("last structured step still offered tools: %+v", got)
	}
}

func TestToolsForStepKeepsToolsWhenUnstructured(t *testing.T) {
	agent := &Agent{cfg: Config{StructuredOutput: false}}
	state := &compiledLoop{
		tools:     []llm.ToolDef{{Type: "function", Function: llm.ToolFunctionDef{Name: "search_runbooks"}}},
		stepLimit: 4,
	}
	if got := agent.toolsForStep(state, 4); len(got) != 1 {
		t.Fatalf("unstructured last step tools = %d, want tools kept", len(got))
	}
}

func TestShouldForceConclusionForStructuredEvidenceWorker(t *testing.T) {
	agent := &Agent{cfg: Config{StructuredOutput: true}}
	state := &compiledLoop{
		input:  Input{OutputMode: agentapi.RunOutputEvidenceWorker},
		result: &RunResult{},
	}
	if !agent.shouldForceConclusion(state) {
		t.Fatal("structured investigator must force a report after tool-only turns")
	}
	agent.cfg.StructuredOutput = false
	if agent.shouldForceConclusion(state) {
		t.Fatal("unstructured evidence worker must not force a visible answer")
	}
}
