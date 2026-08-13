package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	platformagent "github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/memory"
)

func TestDefaultAgentDefinitionsShareOneSettingsVersion(t *testing.T) {
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, 9)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{
		"qa.answerer",
		"review.architecture",
		"review.security",
		"review.reliability",
		"review.adjudicator",
		"investigator.code",
		"investigator.runtime",
		"investigator.docs",
		"synthesizer",
	}
	if len(definitions) != len(wantIDs) {
		t.Fatalf("definitions = %d, want %d", len(definitions), len(wantIDs))
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	catalog := catalog.New(schemas)
	if err := catalog.Publish(definitions); err != nil {
		t.Fatal(err)
	}
	if catalog.Revision() != 1 {
		t.Fatalf("catalog revision = %d, want one atomic publication", catalog.Revision())
	}
	for index, definition := range definitions {
		if definition.ID != wantIDs[index] || definition.Version != 9 ||
			definition.ContentHash == "" {
			t.Fatalf("definition %d = %+v", index, definition)
		}
		if index == 0 && !definition.Tools.AllowWrite {
			t.Fatalf("QA definition did not expose the platform write ceiling: %+v", definition)
		}
		if index > 0 && definition.Tools.AllowWrite {
			t.Fatalf("reviewer definition %q permits writes", definition.ID)
		}
		resolved, err := catalog.Resolve(agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resolved.ContentHash != definition.ContentHash {
			t.Fatalf("resolved definition %q hash = %q", definition.ID, resolved.ContentHash)
		}
	}
}

func TestDelegatedInvestigationBudgetPolicyUsesImmutableAgentDefinitions(t *testing.T) {
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, 9)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := delegatedInvestigationBudgetPolicy(definitions)
	if err != nil {
		t.Fatal(err)
	}
	for name, budget := range map[string]workflow.NodeBudget{
		"code": policy.Code, "runtime": policy.Runtime, "docs": policy.Docs,
	} {
		if budget.MaxInputTokens <= 0 || budget.MaxOutputTokens <= 0 ||
			budget.MaxTotalTokens <= 0 || budget.MaxToolCalls != int64(settings.AgentMaxSteps) ||
			budget.MaxCostMicros != 0 {
			t.Fatalf("%s budget = %+v", name, budget)
		}
	}
	if policy.Synthesizer.MaxInputTokens <= 0 ||
		policy.Synthesizer.MaxOutputTokens <= 0 ||
		policy.Synthesizer.MaxTotalTokens <= 0 ||
		policy.Synthesizer.MaxToolCalls != 0 ||
		policy.Synthesizer.MaxCostMicros != 0 {
		t.Fatalf("synthesizer budget = %+v", policy.Synthesizer)
	}

	settings.LLMInputPriceMicrosPerMillionTokens = 2_000_000
	settings.LLMOutputPriceMicrosPerMillionTokens = 6_000_000
	pricedDefinitions, err := defaultAgentDefinitions(settings, 10)
	if err != nil {
		t.Fatal(err)
	}
	priced, err := delegatedInvestigationBudgetPolicy(pricedDefinitions)
	if err != nil {
		t.Fatal(err)
	}
	if priced.Code.MaxCostMicros <= 0 ||
		priced.Runtime.MaxCostMicros <= 0 ||
		priced.Docs.MaxCostMicros <= 0 ||
		priced.Synthesizer.MaxCostMicros <= 0 {
		t.Fatalf("priced budgets = %+v", priced)
	}
}

func TestDelegatedInvestigationWorkflowCompilesPublishedCapabilities(t *testing.T) {
	const version int64 = 14
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, version)
	if err != nil {
		t.Fatal(err)
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := catalog.New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultInvestigationCapabilities(definitions, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	platform := &Platform{
		agents: agentRuntime{
			schemas:      schemas,
			catalog:      agents,
			capabilities: capabilities,
		},
	}
	budgets, err := delegatedInvestigationBudgetPolicy(definitions)
	if err != nil {
		t.Fatal(err)
	}
	workflowDefinition, err := platform.delegatedInvestigationWorkflow(
		version,
		time.Minute,
		budgets,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantCapabilities := map[string]string{
		"investigate.code":    "knowledge.code.inspect",
		"investigate.runtime": "knowledge.service.trace",
		"investigate.docs":    "knowledge.docs.verify",
		"synthesize":          "evidence.synthesize",
	}
	for _, node := range workflowDefinition.Nodes {
		wantID, ok := wantCapabilities[node.ID]
		if !ok {
			continue
		}
		if node.Capability.ID != wantID || node.Capability.Version != version ||
			node.CapabilityMaxConcurrency != 3 || !node.RestrictVisibleTools ||
			!node.RetrySafe {
			t.Fatalf("node %q capability binding = %+v", node.ID, node)
		}
	}
	if workflowDefinition.Nodes[0].Capability.ID == "" {
		t.Fatal("compiled investigation workflow did not bind capabilities")
	}
}

func TestDelegatedInvestigationWorkflowFallsBackWithoutCapabilityRegistry(t *testing.T) {
	const version int64 = 15
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, version)
	if err != nil {
		t.Fatal(err)
	}
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := catalog.New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatal(err)
	}
	platform := &Platform{
		agents: agentRuntime{
			schemas: schemas,
			catalog: agents,
		},
	}
	budgets, err := delegatedInvestigationBudgetPolicy(definitions)
	if err != nil {
		t.Fatal(err)
	}
	workflowDefinition, err := platform.delegatedInvestigationWorkflow(
		version,
		time.Minute,
		budgets,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range workflowDefinition.Nodes {
		if node.Capability.ID != "" || node.Capability.Version != 0 ||
			node.CapabilityMaxConcurrency != 0 {
			t.Fatalf("fallback node %q unexpectedly has capability binding: %+v", node.ID, node)
		}
	}
}

func TestBuildQARuntimeDisablesDefinitionRuntimeWithoutLLM(t *testing.T) {
	platform := &Platform{}
	settings := &config.PlatformSettings{}
	settings.Apply(nil)

	qa, definitions, runtime, err := platform.buildQARuntime(settings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if qa.QA != nil || len(definitions) != 0 || runtime != nil {
		t.Fatalf("disabled runtime = (qa=%p definitions=%d runtime=%v)", qa.QA, len(definitions), runtime)
	}
}

func TestBuildQARuntimeKeepsInvestigationControlWithAndWithoutLLM(t *testing.T) {
	for _, test := range []struct {
		name        string
		settings    *config.PlatformSettings
		wantQA      bool
		wantRuntime bool
	}{
		{
			name:     "LLM disabled",
			settings: disabledAgentSettings(),
		},
		{
			name:        "LLM enabled",
			settings:    enabledAgentSettings(),
			wantQA:      true,
			wantRuntime: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform := qaRuntimeTestPlatform(t)

			runtime, definitions, definitionRuntime, err := platform.buildQARuntime(
				test.settings,
				nil,
				1,
			)
			if err != nil {
				t.Fatal(err)
			}
			if (runtime.QA != nil) != test.wantQA ||
				(definitionRuntime != nil) != test.wantRuntime {
				t.Fatalf(
					"runtime = (qa=%p definition=%v)",
					runtime.QA,
					definitionRuntime,
				)
			}
			if test.wantRuntime && len(definitions) == 0 {
				t.Fatal("enabled LLM produced no definitions")
			}
			if !test.wantRuntime && len(definitions) != 0 {
				t.Fatalf("disabled LLM produced %d definitions", len(definitions))
			}
			if runtime.RunStore != platform.qa.runs ||
				runtime.Hub == nil ||
				runtime.InvestigationCanceller == nil ||
				runtime.InvestigationReconciler == nil {
				t.Fatalf(
					"investigation runtime = (store=%p hub=%p canceller=%v reconciler=%v)",
					runtime.RunStore,
					runtime.Hub,
					runtime.InvestigationCanceller,
					runtime.InvestigationReconciler,
				)
			}
			canceller, cancelOK := runtime.InvestigationCanceller.(*platformagent.InvestigationCoordinator)
			reconciler, reconcileOK := runtime.InvestigationReconciler.(*platformagent.InvestigationCoordinator)
			if !cancelOK || !reconcileOK || canceller != reconciler {
				t.Fatal("cancellation and recovery use different coordinators")
			}
		})
	}
}

func TestConfigureAgentWorkflowRuntimeTracksLLMAvailability(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := catalog.New(schemas)
	workflowCatalog := workflow.NewCatalog(schemas, agents)
	workflowStore, err := workflow.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	service, err := workflow.NewService(workflowCatalog, workflowStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	platform := &Platform{
		agents: agentRuntime{schemas: schemas, catalog: agents},
		flow: workflowRuntime{
			catalog: workflowCatalog,
			service: service,
		},
	}
	runtime := staticWorkflowAgentRuntime{}
	if err := platform.configureAgentWorkflowRuntime(runtime); err != nil {
		t.Fatal(err)
	}
	if !platform.flow.service.ExecutionAvailable() {
		t.Fatal("workflow runtime was not assembled")
	}
	if err := platform.configureAgentWorkflowRuntime(nil); err != nil {
		t.Fatal(err)
	}
	if platform.flow.service.ExecutionAvailable() {
		t.Fatal("workflow runtime remained enabled without an LLM runtime")
	}
}

type staticWorkflowAgentRuntime struct{}

func (staticWorkflowAgentRuntime) Run(
	context.Context,
	agentapi.RunRequest,
) (agentapi.RunResult, error) {
	return agentapi.RunResult{
		Status: agentapi.RunSucceeded,
		Output: json.RawMessage(`{"ok":true}`),
	}, nil
}

var _ agentapi.Runtime = staticWorkflowAgentRuntime{}

func qaRuntimeTestPlatform(t *testing.T) *Platform {
	t.Helper()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := catalog.New(schemas)
	workflowCatalog := workflow.NewCatalog(schemas, agents)
	workflowStore, err := workflow.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	workflowService, err := workflow.NewService(workflowCatalog, workflowStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Platform{
		db: db,
		agents: agentRuntime{
			schemas: schemas,
			catalog: agents,
		},
		qa: qaState{
			runs:     agentrun.BindStore(db),
			sessions: memory.NewSessionStore(db),
			memory:   memory.NewMemoryStore(db, nil, nil, 0),
		},
		flow: workflowRuntime{
			catalog: workflowCatalog,
			service: workflowService,
		},
	}
}

func disabledAgentSettings() *config.PlatformSettings {
	settings := &config.PlatformSettings{}
	settings.Apply(nil)
	return settings
}

func enabledAgentSettings() *config.PlatformSettings {
	settings := &config.PlatformSettings{
		LLMBaseURL: "http://llm.invalid", LLMAPIKey: "key",
		LLMProvider: "openai", LLMModel: "model",
		LLMMaxTokens: 2048, LLMAnswerMaxTokens: 2048,
		LLMConclusionMaxTokens: 2048, LLMContextWindow: 32000,
		AgentTimeout: config.Duration(2 * time.Minute), AgentMaxSteps: 4,
		AgentAnswerReserve: config.Duration(30 * time.Second),
	}
	settings.Apply(nil)
	return settings
}
