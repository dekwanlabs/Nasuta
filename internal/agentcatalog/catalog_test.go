package agentcatalog

import (
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func testDefinition(version int64, prompt string) agentapi.Definition {
	return agentapi.Definition{
		ID: "qa.answerer", Version: version,
		Prompt:       agentapi.PromptSpec{System: prompt, Version: "1"},
		InputSchema:  agentapi.SchemaRef{ID: "qa.request", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "qa.answer", Version: 1},
		Model:        agentapi.ModelPolicy{Provider: "openai", Model: "model", MaxOutputTokens: 10},
		Budget:       agentapi.BudgetPolicy{Timeout: time.Second, MaxSteps: 1, ContextTokens: 100},
	}
}

func TestCatalogPinsPublishedVersions(t *testing.T) {
	catalog := New()
	if err := catalog.Replace([]agentapi.Definition{
		testDefinition(1, "first"), testDefinition(2, "second"),
	}); err != nil {
		t.Fatal(err)
	}
	latest, err := catalog.Resolve(agentapi.DefinitionRef{ID: "qa.answerer"})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 2 {
		t.Fatalf("latest version = %d", latest.Version)
	}
	latest.Prompt.System = "mutated"
	pinned, err := catalog.Resolve(agentapi.DefinitionRef{ID: "qa.answerer", Version: 2})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Prompt.System != "second" {
		t.Fatal("resolved definition mutated catalog state")
	}
}

func TestCatalogPublishesAtomically(t *testing.T) {
	catalog := New()
	if err := catalog.Replace([]agentapi.Definition{testDefinition(1, "first")}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				definition, err := catalog.Resolve(agentapi.DefinitionRef{ID: "qa.answerer"})
				if err != nil {
					t.Error(err)
					return
				}
				if definition.Version != 1 && definition.Version != 2 {
					t.Errorf("observed partial version %d", definition.Version)
					return
				}
			}
		}()
	}
	if err := catalog.Replace([]agentapi.Definition{testDefinition(2, "second")}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

func TestCatalogRejectsPublishedVersionMutation(t *testing.T) {
	catalog := New()
	if err := catalog.Publish([]agentapi.Definition{testDefinition(1, "first")}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Publish([]agentapi.Definition{testDefinition(1, "changed")}); err == nil {
		t.Fatal("catalog accepted mutation of a published version")
	}
}
