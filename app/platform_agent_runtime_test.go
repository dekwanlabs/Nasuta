package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	platformagent "github.com/dekwanlabs/nasuta/internal/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/catalog"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
	"github.com/dekwanlabs/nasuta/internal/indexing"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/tool"
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
		"investigator.web",
		"investigator.memory",
		"delegation.verifier",
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

func TestInvestigationBudgetsUsesImmutableAgentDefinitions(t *testing.T) {
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, 9)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := investigationBudgets(definitions)
	if err != nil {
		t.Fatal(err)
	}
	for name, budget := range map[string]workflow.NodeBudget{
		"code": policy.Code, "runtime": policy.Runtime, "docs": policy.Docs,
		"web": policy.Web,
	} {
		if budget.MaxInputTokens <= 0 || budget.MaxOutputTokens <= 0 ||
			budget.MaxTotalTokens <= 0 || budget.MaxToolCalls != settings.AgentMaxToolCalls ||
			budget.MaxCostMicros != 0 {
			t.Fatalf("%s budget = %+v", name, budget)
		}
	}
	if policy.Memory.MaxInputTokens <= 0 ||
		policy.Memory.MaxOutputTokens <= 0 ||
		policy.Memory.MaxTotalTokens <= 0 ||
		policy.Memory.MaxToolCalls != 0 ||
		policy.Memory.MaxCostMicros != 0 {
		t.Fatalf("memory budget = %+v", policy.Memory)
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
	priced, err := investigationBudgets(pricedDefinitions)
	if err != nil {
		t.Fatal(err)
	}
	if priced.Code.MaxCostMicros <= 0 ||
		priced.Runtime.MaxCostMicros <= 0 ||
		priced.Docs.MaxCostMicros <= 0 ||
		priced.Web.MaxCostMicros <= 0 ||
		priced.Memory.MaxCostMicros <= 0 ||
		priced.Synthesizer.MaxCostMicros <= 0 {
		t.Fatalf("priced budgets = %+v", priced)
	}
}

func TestInvestigationFlowCompilesPublishedCapabilities(t *testing.T) {
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
	values, err := catalog.DefaultCapabilities(definitions, version)
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
	budgets, err := investigationBudgets(definitions)
	if err != nil {
		t.Fatal(err)
	}
	workflowDefinition, err := platform.defaultInvestigationFlow(
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

func TestInvestigationFlowForGoalsPinsRequestedVersion(t *testing.T) {
	const requestedVersion int64 = 14
	settings := enabledAgentSettings()
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := catalog.New(schemas)
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	for _, version := range []int64{requestedVersion, requestedVersion + 1} {
		definitions, err := defaultAgentDefinitions(settings, version)
		if err != nil {
			t.Fatal(err)
		}
		if err := agents.Publish(definitions); err != nil {
			t.Fatal(err)
		}
		values, err := catalog.DefaultCapabilities(
			definitions,
			version,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := capabilities.Publish(values); err != nil {
			t.Fatal(err)
		}
	}
	platform := &Platform{
		agents: agentRuntime{
			schemas: schemas, catalog: agents, capabilities: capabilities,
		},
	}
	goals := []platformagent.EvidenceGoal{
		{ID: "core_flow", Facet: "core_flow", Required: true},
		{ID: "entrypoint", Facet: "entrypoint", Required: true},
	}
	definition, err := platform.investigationFlow(
		t.Context(),
		requestedVersion,
		goals,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if definition.ID == workflow.FlowID ||
		definition.Version != requestedVersion ||
		len(definition.Nodes) != 5 {
		t.Fatalf("request workflow = %+v", definition)
	}
	nodes := make(map[string]workflow.NodeDefinition, len(definition.Nodes))
	for _, node := range definition.Nodes {
		nodes[node.ID] = node
		if node.Kind != workflow.NodeAgent {
			continue
		}
		if node.Agent.Version != requestedVersion ||
			node.Capability.Version != requestedVersion {
			t.Fatalf("node %q version binding = %+v", node.ID, node)
		}
	}
	verifier := nodes["evidence.verify"]
	if verifier.Kind != workflow.NodeVerifier ||
		verifier.InputSchema.ID != "investigation.bundle" ||
		verifier.OutputSchema.ID != "investigation.verified_bundle" {
		t.Fatalf("request verifier = %+v", verifier)
	}
	subjectDefinition, _, err := platform.investigationFlowWithEvidenceSubjects(
		t.Context(),
		requestedVersion,
		goals,
		[]workflow.SubjectRequirement{{
			EntityID: "our_agent", RequiredFacets: []string{"core_flow"},
		}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	var subjectVerifier workflow.NodeDefinition
	for _, node := range subjectDefinition.Nodes {
		if node.ID == "evidence.verify" {
			subjectVerifier = node
			break
		}
	}
	if subjectVerifier.Verifier == nil ||
		len(subjectVerifier.Verifier.SubjectRequirements) != 1 ||
		subjectVerifier.Verifier.SubjectRequirements[0].EntityID != "our_agent" {
		t.Fatalf("subject verifier = %+v", subjectVerifier.Verifier)
	}
	riskGate := nodes["evidence.risk"]
	if riskGate.Kind != workflow.NodeGate ||
		riskGate.Gate == nil ||
		riskGate.Gate.ID != workflow.EvidenceRiskGateID ||
		!riskGate.Gate.ForwardInput {
		t.Fatalf("request risk gate = %+v", riskGate)
	}
	again, err := platform.investigationFlow(
		t.Context(),
		requestedVersion,
		[]platformagent.EvidenceGoal{
			{ID: "entrypoint", Facet: "entrypoint", Required: true},
			{ID: "core_flow", Facet: "core_flow", Required: true},
		},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != definition.ID || again.ContentHash != definition.ContentHash {
		t.Fatalf(
			"equivalent request workflows differ: %q/%q and %q/%q",
			definition.ID,
			definition.ContentHash,
			again.ID,
			again.ContentHash,
		)
	}

	workflows := workflow.NewCatalog(schemas, agents)
	if err := workflows.Publish([]workflow.Definition{definition}); err != nil {
		t.Fatal(err)
	}
	resolved, err := workflows.Resolve(workflow.DefinitionRef{
		ID: definition.ID, Version: definition.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ContentHash != definition.ContentHash {
		t.Fatalf("resolved request workflow hash = %q", resolved.ContentHash)
	}
}

func TestInvestigationFlowBindsObserveAgent(t *testing.T) {
	const version int64 = 16
	settings := enabledAgentSettings()
	definitions, err := defaultAgentDefinitions(settings, version)
	if err != nil {
		t.Fatal(err)
	}
	observerSettings := *settings
	observerSettings.LLMContextWindow = 12000
	observer, observeCapability := observeCatalog(t, &observerSettings, version)
	definitions = append(definitions, observer)
	schemas := agentapi.NewSchemaRegistry()
	if err := schemas.Publish(catalog.DefaultSchemas()); err != nil {
		t.Fatal(err)
	}
	agents := catalog.New(schemas)
	if err := agents.Publish(definitions); err != nil {
		t.Fatal(err)
	}
	capabilities := agentapi.NewCapabilityRegistry(schemas, agents)
	values, err := catalog.DefaultCapabilities(definitions, version)
	if err != nil {
		t.Fatal(err)
	}
	values = append(values, observeCapability)
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	platform := &Platform{
		agents: agentRuntime{
			schemas: schemas, catalog: agents, capabilities: capabilities,
		},
	}
	definition, payloadBudget, err := platform.investigationFlowWithBudget(
		t.Context(),
		version,
		[]platformagent.EvidenceGoal{{
			ID: "core_flow", Facet: "core_flow", Required: true,
			Sources:         []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime},
			Freshness:       agentapi.FreshnessBoundedLive,
			MinimumCoverage: 1,
		}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	observerBudget, err := agentPayloadBudget(observer)
	if err != nil {
		t.Fatal(err)
	}
	if payloadBudget != observerBudget {
		t.Fatalf("investigator payload budget = %d, want %d", payloadBudget, observerBudget)
	}
	if len(definition.Nodes) != 5 {
		t.Fatalf("runtime observation workflow = %+v", definition)
	}
	var observeNode workflow.NodeDefinition
	for _, node := range definition.Nodes {
		if node.ID == "investigate.observe" {
			observeNode = node
			break
		}
	}
	if observeNode.ID == "" ||
		observeNode.Agent.ID != observer.ID ||
		observeNode.Agent.Version != version ||
		observeNode.Capability.ID != observeCapability.ID ||
		observeNode.Capability.Version != version ||
		observeNode.Budget.MaxToolCalls != settings.AgentMaxToolCalls ||
		!observeNode.Optional {
		t.Fatalf("runtime observation node = %+v", observeNode)
	}
}

func TestInvestigationFlowUsesPlanAndFallsBackOnRejection(t *testing.T) {
	const version int64 = 17
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
	values, err := catalog.DefaultCapabilities(definitions, version)
	if err != nil {
		t.Fatal(err)
	}
	if err := capabilities.Publish(values); err != nil {
		t.Fatal(err)
	}
	platform := &Platform{
		agents: agentRuntime{
			schemas: schemas, catalog: agents, capabilities: capabilities,
		},
	}
	goals := []platformagent.EvidenceGoal{
		{ID: "core_flow", Facet: "core_flow", Required: true},
		{
			ID: "external_dependency", Facet: "external_dependency",
			Required: true,
		},
	}
	report := agentapi.SchemaRef{ID: "investigation.report", Version: 1}
	answer := agentapi.SchemaRef{ID: "investigation.answer", Version: 3}
	proposal := agentapi.TaskGraphProposal{
		Tasks: []agentapi.TaskSpec{
			{
				ID: "planner.code", Purpose: "Inspect the requested core flow.",
				Capability: "knowledge.code.inspect", RequiredFacets: []string{"core_flow"},
				OutputSchema: report, Optional: true, MaxAttempts: 2,
			},
			{
				ID: "planner.runtime", Purpose: "Trace the requested external dependencies.",
				Capability:     "knowledge.service.trace",
				RequiredFacets: []string{"external_dependency"},
				OutputSchema:   report, Optional: true, MaxAttempts: 2,
			},
			{
				ID: "synthesize", Purpose: "Synthesize the available evidence.",
				Capability: "evidence.synthesize", OutputSchema: answer, MaxAttempts: 2,
			},
		},
		Edges: []agentapi.TaskEdge{
			{From: "planner.code", To: "synthesize"},
			{From: "planner.runtime", To: "synthesize"},
		},
	}
	definition, err := platform.investigationFlow(
		t.Context(),
		version,
		goals,
		&proposal,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(definition.ID, ".plan.") ||
		!hasWorkflowNode(definition, "planner.code") ||
		!hasWorkflowNode(definition, "planner.runtime") {
		t.Fatalf("planner workflow = %+v", definition)
	}

	rejected := proposal
	rejected.Tasks = append([]agentapi.TaskSpec(nil), proposal.Tasks...)
	rejected.Tasks[0].OutputSchema = answer
	fallback, err := platform.investigationFlow(
		t.Context(),
		version,
		goals,
		&rejected,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fallback.ID, ".plan.") ||
		!hasWorkflowNode(fallback, "investigate.code") ||
		!hasWorkflowNode(fallback, "investigate.runtime") ||
		hasWorkflowNode(fallback, "planner.code") {
		t.Fatalf("fallback workflow = %+v", fallback)
	}

	alternative, err := platform.investigationFlow(
		t.Context(),
		version,
		[]platformagent.EvidenceGoal{{
			ID: "core_flow", Facet: "core_flow", Required: true,
			Sources: []agentapi.EvidenceSource{
				agentapi.EvidenceSourceInternal,
				agentapi.EvidenceSourceRuntime,
			},
		}},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWorkflowNode(alternative, "investigate.code") ||
		hasWorkflowNode(alternative, "investigate.observe") {
		t.Fatalf("alternative-source workflow = %+v", alternative)
	}
}

func hasWorkflowNode(definition workflow.Definition, id string) bool {
	for _, node := range definition.Nodes {
		if node.ID == id {
			return true
		}
	}
	return false
}

func TestInvestigationFlowFallsBackWithoutCapabilityRegistry(t *testing.T) {
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
	budgets, err := investigationBudgets(definitions)
	if err != nil {
		t.Fatal(err)
	}
	workflowDefinition, err := platform.defaultInvestigationFlow(
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

	qa, definitions, capabilities, bindings, runtime, err := platform.buildQARuntime(
		settings,
		nil,
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if qa.QA != nil || len(definitions) != 0 ||
		len(capabilities) != 0 || len(bindings) != 0 || runtime != nil {
		t.Fatalf(
			"disabled runtime = (qa=%p definitions=%d capabilities=%d bindings=%d runtime=%v)",
			qa.QA,
			len(definitions),
			len(capabilities),
			len(bindings),
			runtime,
		)
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

			runtime, definitions, capabilities, bindings, definitionRuntime, err := platform.buildQARuntime(
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
			if len(capabilities) != 0 {
				t.Fatalf("runtime without extension produced %d extension capabilities", len(capabilities))
			}
			if !test.wantRuntime && len(bindings) != 0 {
				t.Fatalf("disabled LLM produced %d workflow bindings", len(bindings))
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
			canceller, cancelOK := runtime.InvestigationCanceller.(*platformagent.QACoordinator)
			reconciler, reconcileOK := runtime.InvestigationReconciler.(*platformagent.QACoordinator)
			if !cancelOK || !reconcileOK || canceller != reconciler {
				t.Fatal("cancellation and recovery use different coordinators")
			}
		})
	}
}

func TestRebuildQARuntimeReusesPublishedCatalogOnStartup(t *testing.T) {
	const (
		version    int64 = 7
		maxVersion int64 = 9
	)
	settings := enabledAgentSettings()
	platform := qaRuntimeTestPlatform(t)
	platform.settings = settings
	platform.index = &indexing.Service{}
	platform.registry = tool.NewRegistry()
	platform.reads = tool.NewReadRegistry(platform.registry)
	platform.agents.capabilities = agentapi.NewCapabilityRegistry(
		platform.agents.schemas,
		platform.agents.catalog,
	)
	platform.flow.bindings = workflow.NewWorkflowBindingRegistry(
		platform.agents.capabilities,
		platform.flow.catalog,
	)

	definitions, err := defaultAgentDefinitions(settings, version)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := platform.prepareQACatalogSnapshot(
		settings,
		version,
		definitions,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.agents.catalog.Publish(snapshot.definitions); err != nil {
		t.Fatal(err)
	}
	if err := platform.flow.catalog.Publish(
		[]workflow.Definition{snapshot.workflowDefinition},
	); err != nil {
		t.Fatal(err)
	}
	agentRevision := platform.agents.catalog.Revision()
	workflowRevision := platform.flow.catalog.Revision()
	platform.agents.maxVersion = maxVersion

	if err := platform.rebuildQARuntimeLocked(settings, nil); err != nil {
		t.Fatal(err)
	}

	if platform.agents.version != version ||
		platform.agents.maxVersion != maxVersion {
		t.Fatalf(
			"catalog versions = active:%d max:%d, want active:%d max:%d",
			platform.agents.version,
			platform.agents.maxVersion,
			version,
			maxVersion,
		)
	}
	if next := platform.nextQACatalogVersion(); next != maxVersion+1 {
		t.Fatalf("next catalog version = %d, want %d", next, maxVersion+1)
	}
	if platform.agents.catalog.Revision() != agentRevision {
		t.Fatalf(
			"agent catalog revision = %d, want unchanged %d",
			platform.agents.catalog.Revision(),
			agentRevision,
		)
	}
	if platform.flow.catalog.Revision() != workflowRevision {
		t.Fatalf(
			"workflow catalog revision = %d, want unchanged %d",
			platform.flow.catalog.Revision(),
			workflowRevision,
		)
	}
	if platform.agents.capabilities.Revision() == 0 {
		t.Fatal("startup did not restore the in-memory capability registry")
	}
}

func TestRebuildQARuntimeDoesNotReuseCatalogWhenSynthesizerContractChanges(t *testing.T) {
	const (
		staleVersion int64 = 7
		maxVersion   int64 = 9
	)
	settings := enabledAgentSettings()
	platform := qaRuntimeTestPlatform(t)
	platform.settings = settings
	platform.index = &indexing.Service{}
	platform.registry = tool.NewRegistry()
	platform.reads = tool.NewReadRegistry(platform.registry)
	platform.agents.capabilities = agentapi.NewCapabilityRegistry(
		platform.agents.schemas,
		platform.agents.catalog,
	)
	platform.flow.bindings = workflow.NewWorkflowBindingRegistry(
		platform.agents.capabilities,
		platform.flow.catalog,
	)

	definitions, err := defaultAgentDefinitions(settings, staleVersion)
	if err != nil {
		t.Fatal(err)
	}
	foundSynthesizer := false
	for index := range definitions {
		if definitions[index].ID != "synthesizer" {
			continue
		}
		foundSynthesizer = true
		definitions[index].Prompt.Version = "investigation-synthesis-v5"
		definitions[index].OutputSchema = agentapi.SchemaRef{
			ID: "investigation.answer", Version: 3,
		}
		definitions[index].ContentHash = ""
		definitions[index], err = agentapi.Prepare(definitions[index])
		if err != nil {
			t.Fatal(err)
		}
	}
	if !foundSynthesizer {
		t.Fatal("default catalog did not contain synthesizer")
	}
	staleSnapshot, err := platform.prepareQACatalogSnapshot(
		settings,
		staleVersion,
		definitions,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := platform.agents.catalog.Publish(staleSnapshot.definitions); err != nil {
		t.Fatal(err)
	}
	if err := platform.flow.catalog.Publish(
		[]workflow.Definition{staleSnapshot.workflowDefinition},
	); err != nil {
		t.Fatal(err)
	}
	platform.agents.maxVersion = maxVersion

	if err := platform.rebuildQARuntimeLocked(settings, nil); err != nil {
		t.Fatal(err)
	}

	wantVersion := maxVersion + 1
	if platform.agents.version != wantVersion ||
		platform.agents.maxVersion != wantVersion {
		t.Fatalf(
			"catalog versions = active:%d max:%d, want %d",
			platform.agents.version,
			platform.agents.maxVersion,
			wantVersion,
		)
	}
	synthesizer, err := platform.agents.catalog.Resolve(agentapi.DefinitionRef{
		ID: "synthesizer", Version: wantVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if synthesizer.OutputSchema != (agentapi.SchemaRef{
		ID: "investigation.answer", Version: 3,
	}) || synthesizer.Prompt.Version != "investigation-synthesis-v6" {
		t.Fatalf("published synthesizer contract = %+v", synthesizer)
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
	if !platform.flow.service.Available() {
		t.Fatal("workflow runtime was not assembled")
	}
	if err := platform.configureAgentWorkflowRuntime(nil); err != nil {
		t.Fatal(err)
	}
	if platform.flow.service.Available() {
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
			runs:     agentrun.Bind(db),
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

func observeCatalog(
	t *testing.T,
	settings *config.PlatformSettings,
	version int64,
) (agentapi.Definition, agentapi.Capability) {
	t.Helper()
	definition, err := agentapi.Prepare(agentapi.Definition{
		ID: "investigator.observe", Version: version,
		DisplayName: "Live Runtime Investigator",
		Purpose:     "Investigate bounded live runtime evidence.",
		Prompt: agentapi.PromptSpec{
			System: "Investigate runtime evidence and return the required report.", Version: "1",
		},
		InputSchema:  agentapi.SchemaRef{ID: "task.contract", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "investigation.report", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: settings.LLMProvider, Model: settings.LLMModel,
			MaxOutputTokens: settings.LLMAnswerMaxTokens,
		},
		Tools: agentapi.ToolPolicy{
			VisibleToolIDs: []string{"observe_logs"}, RestrictVisible: true,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout:       time.Duration(settings.AgentTimeout),
			MaxSteps:      settings.AgentMaxSteps,
			MaxToolCalls:  settings.AgentMaxToolCalls,
			ContextTokens: settings.LLMContextWindow,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return definition, agentapi.Capability{
		ID: "knowledge.runtime.observe", Version: version,
		Role:            agentapi.RoleInvestigator,
		Purpose:         "Investigate bounded live runtime evidence.",
		InputFacets:     []string{"core_flow"},
		InputSchema:     definition.InputSchema,
		OutputSchema:    definition.OutputSchema,
		ToolIDs:         []string{"observe_logs"},
		PermissionScope: []string{"knowledge.read"},
		Freshness:       agentapi.FreshnessBoundedLive,
		SideEffects:     agentapi.SideEffectNone,
		RetrySafe:       true,
		MaxConcurrency:  3,
		Enabled:         true,
		Agent: agentapi.DefinitionRef{
			ID: definition.ID, Version: definition.Version,
		},
	}
}
