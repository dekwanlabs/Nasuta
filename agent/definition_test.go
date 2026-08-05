package agent

import (
	"testing"
	"time"
)

func TestPrepareDefinitionIsDeterministicAndDetached(t *testing.T) {
	definition := Definition{
		ID: "qa.answerer", Version: 1,
		Prompt:       PromptSpec{System: "Answer with evidence.", Version: "1"},
		InputSchema:  SchemaRef{ID: "qa.request", Version: 1},
		OutputSchema: SchemaRef{ID: "qa.answer", Version: 1},
		Model:        ModelPolicy{Provider: "openai", Model: "model", MaxOutputTokens: 1024},
		Tools:        ToolPolicy{VisibleToolIDs: []string{"search_code"}},
		Budget:       BudgetPolicy{Timeout: time.Minute, MaxSteps: 5, ContextTokens: 32000},
		Permissions:  PermissionPolicy{Scopes: []string{"knowledge.read"}},
	}
	first, err := Prepare(definition)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(definition)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash == "" || first.ContentHash != second.ContentHash {
		t.Fatalf("content hash is not deterministic: %q %q", first.ContentHash, second.ContentHash)
	}
	definition.Tools.VisibleToolIDs[0] = "changed"
	if first.Tools.VisibleToolIDs[0] != "search_code" {
		t.Fatal("prepared definition retained caller-owned tool slice")
	}
}

func TestPrepareDefinitionRejectsHashMismatch(t *testing.T) {
	_, err := Prepare(Definition{
		ID: "qa.answerer", Version: 1,
		Prompt:       PromptSpec{System: "Answer.", Version: "1"},
		InputSchema:  SchemaRef{ID: "qa.request", Version: 1},
		OutputSchema: SchemaRef{ID: "qa.answer", Version: 1},
		Model:        ModelPolicy{Provider: "openai", Model: "model", MaxOutputTokens: 1},
		Budget:       BudgetPolicy{Timeout: time.Second, MaxSteps: 1, ContextTokens: 1},
		ContentHash:  "wrong",
	})
	if err == nil {
		t.Fatal("expected content hash mismatch")
	}
}
