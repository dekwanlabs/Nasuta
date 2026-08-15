package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"
)

func TestPrepareDefinitionIsDeterministicAndDetached(t *testing.T) {
	definition := Definition{
		ID: "qa.answerer", Version: 1,
		Prompt:       PromptSpec{System: "Answer with evidence.", Version: "1"},
		InputSchema:  SchemaRef{ID: "qa.request", Version: 1},
		OutputSchema: SchemaRef{ID: "qa.answer", Version: 1},
		Model: ModelPolicy{
			Provider: "openai", Model: "model", MaxOutputTokens: 1024,
			Parameters: map[string]any{"temperature": float64(0)},
		},
		Tools:       ToolPolicy{VisibleToolIDs: []string{"search_code"}},
		Budget:      BudgetPolicy{Timeout: time.Minute, MaxSteps: 5, ContextTokens: 32000},
		Permissions: PermissionPolicy{Scopes: []string{"knowledge.read"}},
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
	priced := definition
	priced.Model.InputPriceMicrosPerMillionTokens = 10
	priced.Model.OutputPriceMicrosPerMillionTokens = 20
	preparedPriced, err := Prepare(priced)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash == preparedPriced.ContentHash {
		t.Fatal("model prices did not change the definition content hash")
	}
	continued := definition
	continued.Budget.MaxContinueRounds = 1
	preparedContinued, err := Prepare(continued)
	if err != nil {
		t.Fatal(err)
	}
	if first.ContentHash == preparedContinued.ContentHash {
		t.Fatal("continuation policy did not change the definition content hash")
	}
	definition.Tools.VisibleToolIDs[0] = "changed"
	if first.Tools.VisibleToolIDs[0] != "search_code" {
		t.Fatal("prepared definition retained caller-owned tool slice")
	}
	definition.Model.Parameters["temperature"] = float64(1)
	if first.Model.Parameters["temperature"] != float64(0) {
		t.Fatal("prepared definition retained caller-owned model parameters")
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

func TestPrepareDefinitionAcceptsHashCreatedBeforeContinuationPolicy(t *testing.T) {
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
	legacy := struct {
		ID            string           `json:"id"`
		Version       int64            `json:"version"`
		DisplayName   string           `json:"display_name"`
		Purpose       string           `json:"purpose"`
		Prompt        PromptSpec       `json:"prompt"`
		InputSchema   SchemaRef        `json:"input_schema"`
		OutputSchema  SchemaRef        `json:"output_schema"`
		Model         ModelPolicy      `json:"model"`
		Tools         ToolPolicy       `json:"tools"`
		Budget        legacyBudget     `json:"budget"`
		Permissions   PermissionPolicy `json:"permissions"`
		FailurePolicy FailurePolicy    `json:"failure_policy"`
		ContentHash   string           `json:"content_hash"`
	}{
		ID: definition.ID, Version: definition.Version,
		DisplayName: definition.DisplayName, Purpose: definition.Purpose,
		Prompt: definition.Prompt, InputSchema: definition.InputSchema,
		OutputSchema: definition.OutputSchema, Model: definition.Model,
		Tools: definition.Tools,
		Budget: legacyBudget{
			Timeout:       definition.Budget.Timeout,
			MaxSteps:      definition.Budget.MaxSteps,
			ContextTokens: definition.Budget.ContextTokens,
		},
		Permissions: definition.Permissions, FailurePolicy: definition.FailurePolicy,
	}
	payload, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	definition.ContentHash = hex.EncodeToString(sum[:])

	prepared, err := Prepare(definition)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ContentHash != definition.ContentHash {
		t.Fatalf("content hash = %q, want legacy hash %q", prepared.ContentHash, definition.ContentHash)
	}
}

type legacyBudget struct {
	Timeout       time.Duration `json:"timeout"`
	MaxSteps      int           `json:"max_steps"`
	ContextTokens int           `json:"context_tokens"`
}
