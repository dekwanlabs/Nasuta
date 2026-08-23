package investigation

import (
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	DefaultTaskInputSchema  = "investigation.task.input"
	DefaultTaskOutputSchema = "investigation.task.output"
)

// RegisterCodeInvestigationTemplates installs the first-batch templates for a
// single code investigation chain. Tool argument binding is deliberately left to
// the planner; the templates pin capability, schema, cost, and executor boundaries.
func RegisterCodeInvestigationTemplates(catalog *TaskTemplateCatalog) error {
	if catalog == nil {
		return fmt.Errorf("task template catalog is required")
	}
	defs := []TaskTemplate{
		{
			ID: "workspace.resolve_entity", Version: 1,
			GoalKinds: []string{GoalKindEntrypoint, GoalKindCoreFlow}, SourceKinds: []string{"internal"}, Provides: []string{"entity"},
			ToolGrant:    []tool.ToolID{"get_service"},
			ToolCalls:    []ToolCallSpec{{ToolID: "get_service"}},
			InputSchema:  agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1},
			Executor:     ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "code.search", Version: 1,
			GoalKinds: []string{GoalKindEntrypoint, GoalKindCoreFlow}, SourceKinds: []string{"internal"}, RequiredInputs: []string{"entity"}, Provides: []string{"search_hits"},
			ToolGrant:    []tool.ToolID{"search_code"},
			ToolCalls:    []ToolCallSpec{{ToolID: "search_code"}},
			InputSchema:  agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1},
			Executor:     ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "code.inspect_symbol", Version: 1,
			GoalKinds: []string{GoalKindEntrypoint, GoalKindCoreFlow}, SourceKinds: []string{"internal"}, RequiredInputs: []string{"search_hits"}, Provides: []string{"symbol"},
			ToolGrant:    []tool.ToolID{"get_symbol"},
			ToolCalls:    []ToolCallSpec{{ToolID: "get_symbol"}},
			InputSchema:  agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1},
			Executor:     ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "code.trace_calls", Version: 1,
			GoalKinds: []string{GoalKindEntrypoint, GoalKindCoreFlow}, SourceKinds: []string{"internal"}, RequiredInputs: []string{"symbol"}, Provides: []string{"trace"},
			ToolGrant:    []tool.ToolID{"trace_calls"},
			ToolCalls:    []ToolCallSpec{{ToolID: "trace_calls"}},
			InputSchema:  agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1},
			Executor:     ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "evidence.verify", Version: 1,
			GoalKinds: []string{
				GoalKindSystemBoundary,
				GoalKindBusinessDomain,
				GoalKindEntrypoint,
				GoalKindCoreFlow,
				GoalKindDataAndState,
				GoalKindExternalDependency,
				GoalKindRuntimeOperations,
			},
			InputSchema:  agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1},
			Executor:     ExecutorVerifier, CostProfile: BudgetVector{}, Enabled: true,
		},
	}
	for _, def := range defs {
		if err := catalog.Register(def); err != nil {
			return err
		}
	}
	return nil
}

// RegisterFacetCoverageTemplates installs leaf evidence templates for the facet
// dimensions that the code chain does not cover. Each maps one read tool to one
// or more goal kinds so an overview contract can form candidates for every
// required facet instead of failing the whole plan with ErrCapabilityGap.
func RegisterFacetCoverageTemplates(catalog *TaskTemplateCatalog) error {
	if catalog == nil {
		return fmt.Errorf("task template catalog is required")
	}
	input := agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1}
	output := agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1}
	defs := []TaskTemplate{
		{
			ID: "workspace.resolve_boundary", Version: 1,
			GoalKinds:   []string{GoalKindSystemBoundary, GoalKindBusinessDomain},
			SourceKinds: []string{"internal"},
			ToolGrant:   []tool.ToolID{"get_service"},
			ToolCalls:   []ToolCallSpec{{ToolID: "get_service"}},
			InputSchema: input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "code.find_data_state", Version: 1,
			GoalKinds:   []string{GoalKindDataAndState},
			SourceKinds: []string{"internal"},
			ToolGrant:   []tool.ToolID{"search_code"},
			ToolCalls:   []ToolCallSpec{{ToolID: "search_code"}},
			InputSchema: input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "runtime.trace_dependencies", Version: 1,
			GoalKinds:      []string{GoalKindExternalDependency},
			SourceKinds:    []string{"runtime"},
			DiscoveryTypes: []string{"dependency"},
			ToolGrant:      []tool.ToolID{"trace_deps"},
			ToolCalls:      []ToolCallSpec{{ToolID: "trace_deps"}},
			InputSchema:    input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "docs.find_operations", Version: 1,
			GoalKinds:   []string{GoalKindRuntimeOperations},
			ToolGrant:   []tool.ToolID{"search_runbooks"},
			ToolCalls:   []ToolCallSpec{{ToolID: "search_runbooks"}},
			InputSchema: input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
	}
	for _, def := range defs {
		if err := catalog.Register(def); err != nil {
			return err
		}
	}
	return nil
}

// RegisterExploreTemplates installs the generic fallback leaf for questions that
// do not match a typed facet. It maps the evidence.explore goal to an investigator
// executor, so an unclassified question still gets a verifiable evidence path.
func RegisterExploreTemplates(catalog *TaskTemplateCatalog) error {
	if catalog == nil {
		return fmt.Errorf("task template catalog is required")
	}
	definition := TaskTemplate{
		ID:             "investigation.explore",
		Version:        1,
		GoalKinds:      []string{GoalKindExplore},
		DiscoveryTypes: []string{"entity", "dependency"},
		ToolGrant:      []tool.ToolID{"search_code"},
		InputSchema:    agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1},
		OutputSchema:   agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1},
		Executor:       ExecutorInvestigator,
		CostProfile:    BudgetVector{ToolCalls: 1},
		Enabled:        true,
	}
	return catalog.Register(definition)
}

// RegisterExtendedInvestigationTemplates installs the second-batch leaf
// templates for config, API, docs, and runtime investigations. They are
// independent direct-tool tasks so later rounds can select an alternative
// source without rebuilding a whole investigation chain.
func RegisterExtendedInvestigationTemplates(catalog *TaskTemplateCatalog) error {
	if catalog == nil {
		return fmt.Errorf("task template catalog is required")
	}
	input := agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1}
	output := agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1}
	defs := []TaskTemplate{
		{
			ID: "config.find_model_provider", Version: 1,
			GoalKinds:   []string{GoalKindExternalDependency, GoalKindSystemBoundary},
			SourceKinds: []string{"internal"},
			ToolGrant:   []tool.ToolID{"search_code"},
			ToolCalls:   []ToolCallSpec{{ToolID: "search_code"}},
			InputSchema: input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "api.list_external_endpoints", Version: 1,
			GoalKinds:      []string{GoalKindExternalDependency},
			SourceKinds:    []string{"internal"},
			DiscoveryTypes: []string{"dependency"},
			ToolGrant:      []tool.ToolID{"list_apis"},
			ToolCalls:      []ToolCallSpec{{ToolID: "list_apis"}},
			InputSchema:    input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "docs.find_ai_integration", Version: 1,
			GoalKinds:   []string{GoalKindBusinessDomain, GoalKindSystemBoundary},
			SourceKinds: []string{"internal"},
			ToolGrant:   []tool.ToolID{"check_docs"},
			ToolCalls:   []ToolCallSpec{{ToolID: "check_docs"}},
			InputSchema: input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "runtime.find_failure_path", Version: 1,
			GoalKinds:   []string{GoalKindRuntimeOperations},
			SourceKinds: []string{"runtime"},
			ToolGrant:   []tool.ToolID{"search_runbooks"},
			ToolCalls:   []ToolCallSpec{{ToolID: "search_runbooks"}},
			InputSchema: input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "code.find_ai_entrypoint", Version: 1,
			GoalKinds:      []string{GoalKindEntrypoint, GoalKindCoreFlow},
			SourceKinds:    []string{"internal"},
			DiscoveryTypes: []string{"entity"},
			ToolGrant:      []tool.ToolID{"search_code"},
			ToolCalls:      []ToolCallSpec{{ToolID: "search_code"}},
			InputSchema:    input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
		{
			ID: "code.trace_call_chain", Version: 1,
			GoalKinds:      []string{GoalKindCoreFlow, GoalKindExternalDependency},
			SourceKinds:    []string{"internal"},
			DiscoveryTypes: []string{"dependency"},
			ToolGrant:      []tool.ToolID{"trace_calls"},
			ToolCalls:      []ToolCallSpec{{ToolID: "trace_calls"}},
			InputSchema:    input, OutputSchema: output,
			Executor: ExecutorDirectTool, CostProfile: BudgetVector{ToolCalls: 1}, Enabled: true,
		},
	}
	for _, def := range defs {
		if err := catalog.Register(def); err != nil {
			return err
		}
	}
	return nil
}

// RegisterProposalInvestigatorTemplates installs the server-owned executor
// bindings used when a validated QA proposal selects a capability. They are
// excluded from generic candidate generation because proposal tasks already
// carry the planner's bounded graph and capability selection.
func RegisterProposalInvestigatorTemplates(catalog *TaskTemplateCatalog) error {
	if catalog == nil {
		return fmt.Errorf("task template catalog is required")
	}
	input := agentapi.SchemaRef{ID: DefaultTaskInputSchema, Version: 1}
	output := agentapi.SchemaRef{ID: DefaultTaskOutputSchema, Version: 1}
	capabilities := []string{
		"knowledge.code.inspect",
		"knowledge.service.trace",
		"knowledge.docs.verify",
		"knowledge.web.research",
		"knowledge.memory.recall",
		"knowledge.runtime.observe",
	}
	for _, capability := range capabilities {
		if err := catalog.Register(TaskTemplate{
			ID: "proposal." + strings.TrimPrefix(capability, "knowledge."), Version: 1,
			InputSchema: input, OutputSchema: output,
			Executor: ExecutorInvestigator, CostProfile: BudgetVector{ToolCalls: 1},
			MaxAttempts: 2, ProposalOnly: true, Enabled: true,
		}); err != nil {
			return err
		}
	}
	return nil
}

// RegisterDefaultInvestigationTemplates installs the code chain plus the facet
// coverage leaves and the generic explore fallback. It is the catalog used by the
// default  coordinator assembly.
func RegisterDefaultInvestigationTemplates(catalog *TaskTemplateCatalog) error {
	if err := RegisterCodeInvestigationTemplates(catalog); err != nil {
		return err
	}
	if err := RegisterFacetCoverageTemplates(catalog); err != nil {
		return err
	}
	if err := RegisterExtendedInvestigationTemplates(catalog); err != nil {
		return err
	}
	if err := RegisterProposalInvestigatorTemplates(catalog); err != nil {
		return err
	}
	return RegisterExploreTemplates(catalog)
}
