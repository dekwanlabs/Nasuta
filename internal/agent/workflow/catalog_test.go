package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestCatalogRetainsPublishedVersionsDuringConcurrentReads(t *testing.T) {
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	version1 := testWorkflow()
	if err := catalog.Publish([]Definition{version1}); err != nil {
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
	if err := catalog.Publish([]Definition{version2}); err != nil {
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

func TestCatalogAttachStoreRestoresPreLimitExecutionBudget(t *testing.T) {
	schemas := testSchemaRegistry(t)
	definition := preLimitDefinition(t, schemas)
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createdAt := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)SELECT\s+definition_json,content_hash,active,is_default,created_by,created_at\s+FROM workflow_definitions WHERE is_default=1 ORDER BY id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"definition_json", "content_hash", "active", "is_default",
			"created_by", "created_at",
		}).AddRow(raw, definition.ContentHash, true, true, int64(7), createdAt))
	mock.ExpectQuery(`(?s)SELECT id,MAX\(version\)\s+FROM workflow_definitions GROUP BY id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "version",
		}).AddRow(definition.ID, definition.Version))
	mock.ExpectQuery(`(?s)SELECT\s+subject_id,rule_version,candidate_version,percentage_bps,salt,rule_hash,\s+active,created_by,created_at\s+FROM catalog_rollouts\s+WHERE catalog_kind='workflow'\s+ORDER BY subject_id`).
		WillReturnRows(sqlmock.NewRows([]string{
			"subject_id", "rule_version", "candidate_version", "percentage_bps",
			"salt", "rule_hash", "active", "created_by", "created_at",
		}))
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog(schemas, testAgentDefinitions(t))
	if err := catalog.AttachStore(context.Background(), store); err != nil {
		t.Fatalf("AttachStore: %v", err)
	}
	resolved, err := catalog.Resolve(DefinitionRef{
		ID: definition.ID, Version: definition.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.persistedWithoutExecutionLimits ||
		resolved.ContentHash != definition.ContentHash {
		t.Fatalf("resolved pre-limit workflow = %+v", resolved)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestCatalogRejectsUnknownWorkflowSchema(t *testing.T) {
	catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
	definition := testWorkflow()
	definition.OutputSchema.ID = "review.report.missing"
	if err := catalog.Publish([]Definition{definition}); err == nil {
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
	if err := catalog.Publish([]Definition{testWorkflow()}); err == nil {
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
	err := catalog.Publish([]Definition{definition})
	if err == nil || !strings.Contains(err.Error(), "exceed agent definition") {
		t.Fatalf("Publish error = %v, want agent permission rejection", err)
	}
}

func TestCatalogValidatesToolBudgetAgainstAgentCapability(t *testing.T) {
	t.Run("tool capable agent does not require legacy reservation", func(t *testing.T) {
		definition := singleNodeWorkflow()
		definition.Budget.MaxToolCalls = 2
		catalog := NewCatalog(testSchemaRegistry(t), testAgentDefinitions(t))
		err := catalog.Publish([]Definition{definition})
		if err != nil {
			t.Fatalf("Publish error = %v, want shared workflow budget semantics", err)
		}
	})

	t.Run("tool disabled agent requires zero reservation", func(t *testing.T) {
		definition := singleNodeWorkflow()
		definition.Budget.MaxToolCalls = 2
		definition.Nodes[0].Budget.MaxToolCalls = 2
		agents := testAgentDefinitions(t)
		ref := definition.Nodes[0].Agent
		agentDefinition := agents.definitions[ref]
		agentDefinition.ContentHash = ""
		agentDefinition.Tools = agentapi.ToolPolicy{RestrictVisible: true}
		prepared, err := agentapi.Prepare(agentDefinition)
		if err != nil {
			t.Fatal(err)
		}
		agents.definitions[ref] = prepared
		catalog := NewCatalog(testSchemaRegistry(t), agents)
		err = catalog.Publish([]Definition{definition})
		if err == nil || !strings.Contains(err.Error(), "tool budget must be zero") {
			t.Fatalf("Publish error = %v, want zero tool budget rejection", err)
		}

		definition.Nodes[0].Budget.MaxToolCalls = 0
		if err := catalog.Publish([]Definition{definition}); err != nil {
			t.Fatalf("Publish zero-tool workflow: %v", err)
		}
	})
}

func TestCatalogEnforcesComposerContract(t *testing.T) {
	base := composerWorkflowDefinition()
	validAgents := func(t *testing.T) *testAgentResolver {
		t.Helper()
		agents := testAgentDefinitions(t)
		addTestAgent(t, agents, testVerifierAgentDefinition(t))
		addTestAgent(t, agents, testComposerAgentDefinition(t))
		return agents
	}

	t.Run("accepts verified bundle to answer with no tools", func(t *testing.T) {
		catalog := NewCatalog(testSchemaRegistry(t), validAgents(t))
		if err := catalog.Publish([]Definition{base}); err != nil {
			t.Fatalf("Publish valid composer workflow: %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*Definition, *testAgentResolver)
		want   string
	}{
		{
			name: "node input schema",
			mutate: func(definition *Definition, _ *testAgentResolver) {
				definition.Nodes[1].InputSchema = agentapi.SchemaRef{ID: "review.report", Version: 1}
			},
			want: `composer node "compose" must consume`,
		},
		{
			name: "node output schema",
			mutate: func(definition *Definition, _ *testAgentResolver) {
				definition.Nodes[1].OutputSchema = agentapi.SchemaRef{ID: "review.report", Version: 1}
				definition.OutputSchema = definition.Nodes[1].OutputSchema
			},
			want: `composer node "compose" must produce`,
		},
		{
			name: "node tools",
			mutate: func(definition *Definition, _ *testAgentResolver) {
				definition.Nodes[1].RestrictVisibleTools = false
			},
			want: "must restrict visible tools and expose none",
		},
		{
			name: "agent input schema",
			mutate: func(_ *Definition, agents *testAgentResolver) {
				agent := agents.definitions[agentapi.DefinitionRef{ID: "synthesizer", Version: 1}]
				agent.InputSchema = agentapi.SchemaRef{ID: "review.subject", Version: 1}
				addTestAgent(t, agents, agent)
			},
			want: "agent input schema must be investigation.verified_bundle",
		},
		{
			name: "agent output schema",
			mutate: func(_ *Definition, agents *testAgentResolver) {
				agent := agents.definitions[agentapi.DefinitionRef{ID: "synthesizer", Version: 1}]
				agent.OutputSchema = agentapi.SchemaRef{ID: "review.report", Version: 1}
				addTestAgent(t, agents, agent)
			},
			want: "agent output schema must be investigation.answer",
		},
		{
			name: "agent tools",
			mutate: func(_ *Definition, agents *testAgentResolver) {
				agent := agents.definitions[agentapi.DefinitionRef{ID: "synthesizer", Version: 1}]
				agent.Tools.RestrictVisible = false
				addTestAgent(t, agents, agent)
			},
			want: "agent must restrict visible tools and expose none",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := base
			definition.Nodes = append([]NodeDefinition(nil), base.Nodes...)
			definition.Edges = append([]EdgeDefinition(nil), base.Edges...)
			agents := validAgents(t)
			test.mutate(&definition, agents)
			catalog := NewCatalog(testSchemaRegistry(t), agents)
			if err := catalog.Publish([]Definition{definition}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Publish error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func composerWorkflowDefinition() Definition {
	verified := agentapi.InvestigationVerifiedBundleSchemaRef()
	answer := agentapi.InvestigationAnswerSchemaRef()
	return Definition{
		ID: "investigation.composer.catalog", Version: 1,
		Purpose:      "Verify and synthesize an investigation.",
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: answer,
		Budget: Budget{
			MaxNodes: 2, MaxParallelism: 1, MaxRounds: 1, MaxDepth: 2,
			Timeout: time.Second, MaxHandoffBytes: 4096,
		},
		FailurePolicy: FailurePolicy{Mode: FailFast},
		Nodes: []NodeDefinition{
			{
				ID: "verify", Kind: NodeAgent,
				Agent:        agentapi.DefinitionRef{ID: "delegation.verifier", Version: 1},
				InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
				OutputSchema: verified, Timeout: time.Second,
			},
			{
				ID: "compose", Kind: NodeAgent,
				Agent:       agentapi.DefinitionRef{ID: "synthesizer", Version: 1},
				InputSchema: verified, OutputSchema: answer, Timeout: time.Second,
				RestrictVisibleTools: true,
			},
		},
		Edges: []EdgeDefinition{{From: "verify", To: "compose", Required: true}},
	}
}

func testVerifierAgentDefinition(t *testing.T) agentapi.Definition {
	t.Helper()
	definition, err := agentapi.Prepare(agentapi.Definition{
		ID: "delegation.verifier", Version: 1, Purpose: "Verify evidence.",
		Prompt:       agentapi.PromptSpec{System: "Verify the input.", Version: "v1"},
		InputSchema:  agentapi.SchemaRef{ID: "review.subject", Version: 1},
		OutputSchema: agentapi.InvestigationVerifiedBundleSchemaRef(),
		Model:        agentapi.ModelPolicy{Provider: "openai", Model: "test", MaxOutputTokens: 256},
		Tools:        agentapi.ToolPolicy{RestrictVisible: true},
		Budget:       agentapi.BudgetPolicy{Timeout: time.Minute, MaxSteps: 1, ContextTokens: 4096},
		Permissions:  agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		t.Fatalf("prepare verifier agent: %v", err)
	}
	return definition
}

func testComposerAgentDefinition(t *testing.T) agentapi.Definition {
	t.Helper()
	definition, err := agentapi.Prepare(agentapi.Definition{
		ID: "synthesizer", Version: 1, Purpose: "Synthesize verified evidence.",
		Prompt:       agentapi.PromptSpec{System: "Synthesize the input.", Version: "v1"},
		InputSchema:  agentapi.InvestigationVerifiedBundleSchemaRef(),
		OutputSchema: agentapi.InvestigationAnswerSchemaRef(),
		Model:        agentapi.ModelPolicy{Provider: "openai", Model: "test", MaxOutputTokens: 256},
		Tools:        agentapi.ToolPolicy{RestrictVisible: true},
		Budget:       agentapi.BudgetPolicy{Timeout: time.Minute, MaxSteps: 1, ContextTokens: 4096},
		Permissions:  agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		t.Fatalf("prepare composer agent: %v", err)
	}
	return definition
}

func addTestAgent(t *testing.T, agents *testAgentResolver, definition agentapi.Definition) {
	t.Helper()
	definition.ContentHash = ""
	prepared, err := agentapi.Prepare(definition)
	if err != nil {
		t.Fatalf("prepare test agent %q: %v", definition.ID, err)
	}
	agents.definitions[agentapi.DefinitionRef{ID: prepared.ID, Version: prepared.Version}] = prepared
}

func TestCatalogRequiresPinnedModelPricesForCostBudget(t *testing.T) {
	definition := singleNodeWorkflow()
	definition.Budget.MaxCostMicros = 100
	definition.Nodes[0].Budget.MaxCostMicros = 100
	agents := testAgentDefinitions(t)
	catalog := NewCatalog(testSchemaRegistry(t), agents)
	if err := catalog.Publish([]Definition{definition}); err == nil ||
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
	if err := catalog.Publish([]Definition{definition}); err != nil {
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
