package catalog

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
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

func testCatalog(t *testing.T) *Catalog {
	t.Helper()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(DefaultSchemas()); err != nil {
		t.Fatalf("publish schemas: %v", err)
	}
	return New(schemas)
}

func TestCatalogPinsPublishedVersions(t *testing.T) {
	catalog := testCatalog(t)
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
	catalog := testCatalog(t)
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
	catalog := testCatalog(t)
	if err := catalog.Publish([]agentapi.Definition{testDefinition(1, "first")}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Publish([]agentapi.Definition{testDefinition(1, "changed")}); err == nil {
		t.Fatal("catalog accepted mutation of a published version")
	}
}

func TestDefaultReviewersArePinnedReadOnlyDefinitions(t *testing.T) {
	settings := &config.PlatformSettings{
		LLMProvider: "openai", LLMModel: "review-model",
		LLMAnswerMaxTokens: 2048, LLMContextWindow: 32000,
		AgentTimeout: config.Duration(time.Minute), AgentMaxSteps: 4,
	}
	definitions, err := DefaultReviewersVersion(settings, 7)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{
		"review.architecture",
		"review.security",
		"review.reliability",
		"review.adjudicator",
	}
	if len(definitions) != len(wantIDs) {
		t.Fatalf("definitions = %d, want %d", len(definitions), len(wantIDs))
	}
	for index, definition := range definitions {
		if definition.ID != wantIDs[index] || definition.Version != 7 ||
			definition.ContentHash == "" || definition.Model.Provider != "openai" ||
			definition.Model.Model != "review-model" || definition.Tools.AllowWrite ||
			len(definition.Permissions.Scopes) != 1 ||
			definition.Permissions.Scopes[0] != "knowledge.read" {
			t.Fatalf("definition %d = %+v", index, definition)
		}
	}
	if definitions[0].ContentHash == definitions[1].ContentHash {
		t.Fatal("reviewer roles produced identical immutable definitions")
	}
	adjudicator := definitions[len(definitions)-1]
	if adjudicator.InputSchema.ID != "review.adjudication.request" ||
		adjudicator.OutputSchema.ID != "review.adjudication" {
		t.Fatalf("adjudicator schemas = %+v / %+v", adjudicator.InputSchema, adjudicator.OutputSchema)
	}
}

func TestDefaultInvestigatorsArePinnedReadOnlyDefinitions(t *testing.T) {
	settings := &config.PlatformSettings{
		LLMProvider: "openai", LLMModel: "investigation-model",
		LLMAnswerMaxTokens: 2048, LLMContextWindow: 32000,
		AgentTimeout: config.Duration(time.Minute), AgentMaxSteps: 4,
	}
	definitions, err := DefaultInvestigatorsVersion(settings, 11)
	if err != nil {
		t.Fatal(err)
	}
	wantTools := map[string][]string{
		"investigator.code":    {"search_code", "get_symbol", "trace_calls", "list_apis"},
		"investigator.runtime": {"get_service", "trace_deps", "list_apis", "trace_calls"},
		"investigator.docs":    {"get_service", "search_runbooks", "check_docs"},
		"synthesizer":          {},
	}
	wantIDs := []string{
		"investigator.code", "investigator.runtime", "investigator.docs", "synthesizer",
	}
	if len(definitions) != len(wantIDs) {
		t.Fatalf("definitions = %d, want %d", len(definitions), len(wantIDs))
	}
	for index, definition := range definitions {
		if definition.ID != wantIDs[index] || definition.Version != 11 ||
			definition.ContentHash == "" || definition.Tools.AllowWrite ||
			!definition.Tools.RestrictVisible ||
			len(definition.Permissions.Scopes) != 1 ||
			definition.Permissions.Scopes[0] != "knowledge.read" {
			t.Fatalf("definition %d = %+v", index, definition)
		}
		if !slices.Equal(definition.Tools.VisibleToolIDs, wantTools[definition.ID]) {
			t.Fatalf("definition %q tools = %v, want %v", definition.ID, definition.Tools.VisibleToolIDs, wantTools[definition.ID])
		}
	}
	synthesizer := definitions[len(definitions)-1]
	if synthesizer.InputSchema.ID != "investigation.bundle" ||
		synthesizer.OutputSchema.ID != "investigation.answer" ||
		len(synthesizer.Tools.VisibleToolIDs) != 0 || !synthesizer.Tools.RestrictVisible {
		t.Fatalf("synthesizer contract = %+v", synthesizer)
	}
}

func TestCatalogRejectsUnknownDefinitionSchemas(t *testing.T) {
	catalog := testCatalog(t)
	definition := testDefinition(1, "first")
	definition.OutputSchema = agentapi.SchemaRef{ID: "qa.missing", Version: 1}
	if err := catalog.Publish([]agentapi.Definition{definition}); err == nil {
		t.Fatal("catalog accepted an unknown output schema")
	}
}

func TestCatalogRejectsNonRuntimePermissionScope(t *testing.T) {
	catalog := testCatalog(t)
	definition := testDefinition(1, "first")
	definition.Permissions.Scopes = []string{platformscope.FeatureDelivery}
	err := catalog.Publish([]agentapi.Definition{definition})
	if err == nil || !strings.Contains(err.Error(), "not supported by the agent runtime") {
		t.Fatalf("Publish error = %v, want non-runtime scope rejection", err)
	}
}
