package catalog

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/scope"
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
	definitions, err := DefaultReviewers(settings, 7)
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
	definitions, err := DefaultInvestigators(settings, 11)
	if err != nil {
		t.Fatal(err)
	}
	wantTools := map[string][]string{
		"investigator.code":    {"search_code", "get_symbol", "trace_calls", "list_apis"},
		"investigator.runtime": {"get_service", "trace_deps", "list_apis", "trace_calls"},
		"investigator.docs":    {"get_service", "search_runbooks", "check_docs"},
		"investigator.web":     {"web_search"},
		"investigator.memory":  {},
		"delegation.verifier":  {},
		"synthesizer":          {},
	}
	wantIDs := []string{
		"investigator.code", "investigator.runtime", "investigator.docs",
		"investigator.web", "investigator.memory", "delegation.verifier",
		"synthesizer",
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
		if strings.HasPrefix(definition.ID, "investigator.") &&
			definition.InputSchema.ID != "task.contract" {
			t.Fatalf("definition %q input schema = %+v", definition.ID, definition.InputSchema)
		}
	}
	synthesizer := definitions[len(definitions)-1]
	if synthesizer.InputSchema.ID != "investigation.verified_bundle" ||
		synthesizer.OutputSchema.ID != "investigation.answer" ||
		synthesizer.Prompt.Version != "investigation-synthesis-v4" ||
		!strings.Contains(synthesizer.Prompt.System, `"supported_claims"`) ||
		!strings.Contains(synthesizer.Prompt.System, `"partial_claims"`) ||
		!strings.Contains(synthesizer.Prompt.System, `"unsupported_claims"`) ||
		!strings.Contains(synthesizer.Prompt.System, `"omissions"`) ||
		strings.Contains(synthesizer.Prompt.System, `"handoffs[].payload"`) ||
		strings.Contains(synthesizer.Prompt.System, `"unavailable_tasks"`) ||
		len(synthesizer.Tools.VisibleToolIDs) != 0 || !synthesizer.Tools.RestrictVisible {
		t.Fatalf("synthesizer contract = %+v", synthesizer)
	}
	verifier := definitions[len(definitions)-2]
	if verifier.InputSchema.ID != "delegation.verification.request" ||
		verifier.OutputSchema.ID != "delegation.verification.result" ||
		verifier.Prompt.Version != "delegation-verification-v1" ||
		!strings.Contains(verifier.Prompt.System, `"unresolved"`) ||
		len(verifier.Tools.VisibleToolIDs) != 0 ||
		!verifier.Tools.RestrictVisible {
		t.Fatalf("delegation verifier contract = %+v", verifier)
	}
}

func TestDefaultCapabilitiesPinAgentContracts(t *testing.T) {
	settings := &config.PlatformSettings{
		LLMProvider: "openai", LLMModel: "investigation-model",
		LLMAnswerMaxTokens: 2048, LLMContextWindow: 32000,
		AgentTimeout: config.Duration(time.Minute), AgentMaxSteps: 4,
	}
	definitions, err := DefaultInvestigators(settings, 12)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := DefaultCapabilities(definitions, 12)
	if err != nil {
		t.Fatal(err)
	}
	wantAgents := map[string]string{
		"knowledge.code.inspect":   "investigator.code",
		"knowledge.service.trace":  "investigator.runtime",
		"knowledge.docs.verify":    "investigator.docs",
		"knowledge.web.research":   "investigator.web",
		"knowledge.memory.recall":  "investigator.memory",
		"evidence.semantic.verify": "delegation.verifier",
		"evidence.synthesize":      "synthesizer",
	}
	wantFacets := map[string][]string{
		"knowledge.code.inspect": {
			"entrypoint", "core_flow", "data_and_state", "external_dependency",
		},
		"knowledge.service.trace": {
			"system_boundary", "external_dependency", "runtime_and_operations",
		},
		"knowledge.docs.verify":    canonicalFacetValues(),
		"knowledge.web.research":   canonicalFacetValues(),
		"knowledge.memory.recall":  canonicalFacetValues(),
		"evidence.semantic.verify": nil,
		"evidence.synthesize":      nil,
	}
	wantFreshness := map[string]agentapi.FreshnessPolicy{
		"knowledge.code.inspect":   agentapi.FreshnessStable,
		"knowledge.service.trace":  agentapi.FreshnessStable,
		"knowledge.docs.verify":    agentapi.FreshnessStable,
		"knowledge.web.research":   agentapi.FreshnessCurrent,
		"knowledge.memory.recall":  agentapi.FreshnessCurrent,
		"evidence.semantic.verify": agentapi.FreshnessStable,
		"evidence.synthesize":      agentapi.FreshnessStable,
	}
	byAgent := make(map[string]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		byAgent[definition.ID] = definition
	}
	if len(capabilities) != len(wantAgents) {
		t.Fatalf("capabilities = %d, want %d", len(capabilities), len(wantAgents))
	}
	for _, capability := range capabilities {
		agentID, ok := wantAgents[capability.ID]
		if !ok || capability.Version != 12 || !capability.Enabled ||
			!capability.RetrySafe ||
			capability.SideEffects != agentapi.SideEffectNone ||
			capability.MaxConcurrency != 3 ||
			capability.Freshness != wantFreshness[capability.ID] ||
			capability.Agent != (agentapi.DefinitionRef{ID: agentID, Version: 12}) ||
			!slices.Equal(capability.PermissionScope, []string{"knowledge.read"}) {
			t.Fatalf("capability = %+v", capability)
		}
		definition := byAgent[agentID]
		if capability.InputSchema != definition.InputSchema ||
			capability.OutputSchema != definition.OutputSchema ||
			!slices.Equal(capability.ToolIDs, definition.Tools.VisibleToolIDs) ||
			!slices.Equal(capability.InputFacets, wantFacets[capability.ID]) {
			t.Fatalf(
				"capability %q does not match agent %q: %+v / %+v",
				capability.ID,
				agentID,
				capability,
				definition,
			)
		}
	}

	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatal(err)
	}
	registry := agentapi.NewCapabilityRegistry(schemas, agents)
	if err := registry.Publish(capabilities); err != nil {
		t.Fatalf("publish default capabilities: %v", err)
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
	definition.Permissions.Scopes = []string{scope.FeatureDelivery}
	err := catalog.Publish([]agentapi.Definition{definition})
	if err == nil || !strings.Contains(err.Error(), "not supported by the agent runtime") {
		t.Fatalf("Publish error = %v, want non-runtime scope rejection", err)
	}
}
