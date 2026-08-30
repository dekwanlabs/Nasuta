package execution

import (
	"context"
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

func TestRemindStructuredLastStepOnlyWhenToolsAreClosed(t *testing.T) {
	agent := &Agent{cfg: Config{StructuredOutput: true}}
	state := &compiledLoop{
		ctx:       context.Background(),
		tools:     []llm.ToolDef{{Type: "function", Function: llm.ToolFunctionDef{Name: "search_runbooks"}}},
		stepLimit: 4,
		messages:  []llm.Message{{Role: "user", Content: "查文档"}},
	}
	agent.remindStructuredLastStep(state, 3)
	if state.structuredLastStepReminded || len(state.messages) != 1 {
		t.Fatalf("reminded before tools closed: reminded=%t messages=%d", state.structuredLastStepReminded, len(state.messages))
	}
	agent.remindStructuredLastStep(state, 4)
	if !state.structuredLastStepReminded || len(state.messages) != 2 {
		t.Fatalf("last step reminder missing: reminded=%t messages=%d", state.structuredLastStepReminded, len(state.messages))
	}
	if state.messages[1].Content != structuredLastStepInstruction {
		t.Fatalf("last step reminder = %q", state.messages[1].Content)
	}
	agent.remindStructuredLastStep(state, 4)
	if len(state.messages) != 2 {
		t.Fatalf("last step reminder appended twice: %d", len(state.messages))
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
