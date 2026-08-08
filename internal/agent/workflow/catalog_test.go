package workflow

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestCatalogRetainsPublishedVersionsDuringConcurrentReads(t *testing.T) {
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	version1 := testWorkflow()
	if err := catalog.Publish([]WorkflowDefinition{version1}); err != nil {
		t.Fatal(err)
	}
	version2 := testWorkflow()
	version2.Version = 2
	version2.Purpose = "Run a revised independent review panel."

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				definition, err := catalog.Resolve(DefinitionRef{ID: version1.ID, Version: 1})
				if err != nil || definition.Version != 1 {
					t.Errorf("resolve version 1: definition=%#v err=%v", definition, err)
					return
				}
			}
		}()
	}
	if err := catalog.Publish([]WorkflowDefinition{version2}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	latest, err := catalog.Resolve(DefinitionRef{ID: version1.ID})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Version != 2 || catalog.Revision() != 2 {
		t.Fatalf("latest=%d revision=%d", latest.Version, catalog.Revision())
	}
}

func TestCatalogRejectsUnknownWorkflowSchema(t *testing.T) {
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	definition := testWorkflow()
	definition.OutputSchema.ID = "review.report.missing"
	if err := catalog.Publish([]WorkflowDefinition{definition}); err == nil {
		t.Fatal("catalog accepted an unknown workflow schema")
	}
}

func TestCatalogRejectsAgentSchemaMismatch(t *testing.T) {
	agents := testAgentDefinitions(t)
	definition := agents.definitions[agentapi.DefinitionRef{ID: "review.correctness", Version: 1}]
	definition.OutputSchema = agentapi.SchemaRef{ID: "other.input", Version: 1}
	definition.ContentHash = ""
	prepared, err := agentapi.Prepare(definition)
	if err != nil {
		t.Fatal(err)
	}
	agents.definitions[agentapi.DefinitionRef{ID: prepared.ID, Version: prepared.Version}] = prepared

	catalog := NewCatalog(testSchemaRegistry(t), agents)
	if err := catalog.Publish([]WorkflowDefinition{testWorkflow()}); err == nil {
		t.Fatal("catalog accepted an agent output incompatible with its node")
	}
}

func TestCatalogRejectsNodePermissionsOutsideAgentDefinition(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Permissions = agentapi.PermissionPolicy{
		Scopes: []string{"knowledge.read", "knowledge.write"},
	}
	definition.Nodes[0].Permissions = definition.Permissions
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	err := catalog.Publish([]WorkflowDefinition{definition})
	if err == nil || !strings.Contains(err.Error(), "exceed agent definition") {
		t.Fatalf("Publish error = %v, want agent permission rejection", err)
	}
}

func TestCatalogRequiresPinnedModelPricesForCostBudget(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Budget.MaxCostMicros = 100
	definition.Nodes[0].Budget.MaxCostMicros = 100
	agents := testAgentDefinitions(t)
	catalog := NewCatalog(testSchemaRegistry(t), agents)
	if err := catalog.Publish([]WorkflowDefinition{definition}); err == nil ||
		!strings.Contains(err.Error(), "model prices are required") {
		t.Fatalf("Publish error = %v, want model price validation", err)
	}

	ref := definition.Nodes[0].Agent
	agentDefinition := agents.definitions[ref]
	agentDefinition.ContentHash = ""
	agentDefinition.Model.InputPriceMicrosPerMillionTokens = 10
	agentDefinition.Model.OutputPriceMicrosPerMillionTokens = 20
	prepared, err := agentapi.Prepare(agentDefinition)
	if err != nil {
		t.Fatal(err)
	}
	agents.definitions[ref] = prepared
	if err := catalog.Publish([]WorkflowDefinition{definition}); err != nil {
		t.Fatalf("Publish priced workflow: %v", err)
	}
}

type testAgentResolver struct {
	definitions map[agentapi.DefinitionRef]agentapi.Definition
}

func (resolver *testAgentResolver) Resolve(ref agentapi.DefinitionRef) (agentapi.Definition, error) {
	definition, ok := resolver.definitions[ref]
	if !ok {
		return agentapi.Definition{}, fmt.Errorf("agent definition %q version %d not found", ref.ID, ref.Version)
	}
	return definition, nil
}

func testAgentDefinitions(t *testing.T) *testAgentResolver {
	t.Helper()
	definitions := make(map[agentapi.DefinitionRef]agentapi.Definition, 2)
	for _, id := range []string{"review.correctness", "review.security"} {
		definition, err := agentapi.Prepare(agentapi.Definition{
			ID: id, Version: 1, Purpose: "Review one subject.",
			Prompt:       agentapi.PromptSpec{System: "Review the input.", Version: "v1"},
			InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: "review.report", Version: 1},
			Model: agentapi.ModelPolicy{
				Provider: "openai", Model: "test", MaxOutputTokens: 256,
			},
			Budget: agentapi.BudgetPolicy{
				Timeout: time.Minute, MaxSteps: 2, ContextTokens: 4096,
			},
			Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		})
		if err != nil {
			t.Fatalf("prepare agent %q: %v", id, err)
		}
		definitions[agentapi.DefinitionRef{ID: id, Version: 1}] = definition
	}
	return &testAgentResolver{definitions: definitions}
}
