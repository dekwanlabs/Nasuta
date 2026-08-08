package catalog

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

// DefaultInvestigatorsVersion builds the fixed read-only delegated investigation panel.
func DefaultInvestigatorsVersion(settings *config.PlatformSettings, version int64) ([]agentapi.Definition, error) {
	specs := []struct {
		id, name, purpose, focus string
		tools                    []string
	}{
		{
			id: "investigator.code", name: "Code Investigator", focus: "code",
			purpose: "Investigate source implementation, exact symbols, and call paths.",
			tools:   []string{"search_code", "get_symbol", "trace_calls", "list_apis"},
		},
		{
			id: "investigator.runtime", name: "Runtime Topology Investigator", focus: "runtime",
			purpose: "Investigate indexed service topology, dependencies, and exposed runtime entrypoints.",
			tools:   []string{"get_service", "trace_deps", "list_apis", "trace_calls", "index_stats"},
		},
		{
			id: "investigator.docs", name: "Documentation Investigator", focus: "docs",
			purpose: "Investigate runbooks, system documentation, and documentation coverage.",
			tools:   []string{"get_service", "search_runbooks", "check_docs"},
		},
	}
	definitions := make([]agentapi.Definition, 0, len(specs)+1)
	for _, spec := range specs {
		rolePrompt := prompts.MustRender(prompts.AgentCatalogInvestigator, struct {
			Focus string
		}{Focus: spec.focus})
		definition, err := agentapi.Prepare(agentapi.Definition{
			ID: spec.id, Version: version, DisplayName: spec.name, Purpose: spec.purpose,
			Prompt: agentapi.PromptSpec{
				System: investigationReportPrompt(spec.focus, rolePrompt), Version: "investigation-report-v1",
			},
			InputSchema:  agentapi.SchemaRef{ID: "investigation.request", Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: "investigation.report", Version: 1},
			Model: agentapi.ModelPolicy{
				Provider: settings.LLMProvider, Model: settings.LLMModel,
				MaxOutputTokens: settings.LLMAnswerMaxTokens,
			},
			Tools: agentapi.ToolPolicy{
				VisibleToolIDs: append([]string(nil), spec.tools...), RestrictVisible: true,
			},
			Budget: agentapi.BudgetPolicy{
				Timeout:       time.Duration(settings.AgentTimeout),
				MaxSteps:      settings.AgentMaxSteps,
				ContextTokens: settings.LLMContextWindow,
			},
			Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		})
		if err != nil {
			return nil, fmt.Errorf("prepare investigator %q: %w", spec.id, err)
		}
		definitions = append(definitions, definition)
	}
	synthesizer, err := agentapi.Prepare(agentapi.Definition{
		ID: "synthesizer", Version: version, DisplayName: "Evidence Synthesizer",
		Purpose: "Synthesize delegated investigation handoffs without gathering new evidence.",
		Prompt: agentapi.PromptSpec{
			System:  prompts.Text(prompts.AgentCatalogSynthesizer),
			Version: "investigation-synthesis-v1",
		},
		InputSchema:  agentapi.SchemaRef{ID: "investigation.bundle", Version: 1},
		OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 1},
		Model: agentapi.ModelPolicy{
			Provider: settings.LLMProvider, Model: settings.LLMModel,
			MaxOutputTokens: settings.LLMAnswerMaxTokens,
		},
		Tools: agentapi.ToolPolicy{VisibleToolIDs: []string{}, RestrictVisible: true},
		Budget: agentapi.BudgetPolicy{
			Timeout:       time.Duration(settings.AgentTimeout),
			MaxSteps:      settings.AgentMaxSteps,
			ContextTokens: settings.LLMContextWindow,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		return nil, fmt.Errorf("prepare synthesizer: %w", err)
	}
	return append(definitions, synthesizer), nil
}

func investigationReportPrompt(focus, rolePrompt string) string {
	return prompts.MustRender(prompts.AgentCatalogInvestigationReport, struct {
		Focus      string
		RolePrompt string
	}{
		Focus:      focus,
		RolePrompt: rolePrompt,
	})
}
