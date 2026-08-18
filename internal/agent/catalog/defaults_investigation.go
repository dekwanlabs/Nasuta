package catalog

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

// DefaultInvestigators builds the fixed read-only delegated investigation panel.
func DefaultInvestigators(settings *config.PlatformSettings, version int64) ([]agentapi.Definition, error) {
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
			tools:   []string{"get_service", "trace_deps", "list_apis", "trace_calls"},
		},
		{
			id: "investigator.docs", name: "Documentation Investigator", focus: "docs",
			purpose: "Investigate runbooks, system documentation, and documentation coverage.",
			tools:   []string{"get_service", "search_runbooks", "check_docs"},
		},
		{
			id: "investigator.web", name: "Web Research Investigator", focus: "web",
			purpose: "Investigate current public evidence through the configured web provider.",
			tools:   []string{"web_search"},
		},
		{
			id: "investigator.memory", name: "Memory Recall Investigator", focus: "memory",
			purpose: "Evaluate bounded recalled memory already admitted by the task contract.",
		},
	}
	definitions := make([]agentapi.Definition, 0, len(specs)+2)
	for _, spec := range specs {
		maxToolCalls := int64(0)
		if len(spec.tools) > 0 {
			maxToolCalls = settings.AgentMaxToolCalls
		}
		rolePrompt := prompts.MustRender(prompts.AgentCatalogInvestigator, struct {
			Focus string
		}{Focus: spec.focus})
		definition, err := agentapi.Prepare(agentapi.Definition{
			ID: spec.id, Version: version, DisplayName: spec.name, Purpose: spec.purpose,
			Prompt: agentapi.PromptSpec{
				System: reportPrompt(spec.focus, rolePrompt), Version: "investigation-report-v1",
			},
			InputSchema:  agentapi.SchemaRef{ID: "task.contract", Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: "investigation.report", Version: 1},
			Model: agentapi.ModelPolicy{
				Provider: settings.LLMProvider, Model: settings.LLMModel,
				MaxOutputTokens:                   settings.LLMAnswerMaxTokens,
				InputPriceMicrosPerMillionTokens:  settings.LLMInputPriceMicrosPerMillionTokens,
				OutputPriceMicrosPerMillionTokens: settings.LLMOutputPriceMicrosPerMillionTokens,
			},
			Tools: agentapi.ToolPolicy{
				VisibleToolIDs: append([]string(nil), spec.tools...), RestrictVisible: true,
			},
			Budget: agentapi.BudgetPolicy{
				Timeout:            time.Duration(settings.AgentTimeout),
				MaxSteps:           settings.AgentMaxSteps,
				MaxToolCalls:       maxToolCalls,
				ContextTokens:      settings.LLMContextWindow,
				MaxToolResultBytes: 24 * 1024,
				MaxContinueRounds:  1,
			},
			Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		})
		if err != nil {
			return nil, fmt.Errorf("prepare investigator %q: %w", spec.id, err)
		}
		definitions = append(definitions, definition)
	}
	verifier, err := agentapi.Prepare(agentapi.Definition{
		ID: "delegation.verifier", Version: version,
		DisplayName: "Delegation Evidence Verifier",
		Purpose:     "Resolve bounded semantic claim conflicts using only cited evidence.",
		Prompt: agentapi.PromptSpec{
			System:  prompts.Text(prompts.AgentCatalogDelegationVerifier),
			Version: "delegation-verification-v1",
		},
		InputSchema: agentapi.SchemaRef{
			ID: "delegation.verification.request", Version: 1,
		},
		OutputSchema: agentapi.SchemaRef{
			ID: "delegation.verification.result", Version: 1,
		},
		Model: agentapi.ModelPolicy{
			Provider: settings.LLMProvider, Model: settings.LLMModel,
			MaxOutputTokens:                   settings.LLMAnswerMaxTokens,
			InputPriceMicrosPerMillionTokens:  settings.LLMInputPriceMicrosPerMillionTokens,
			OutputPriceMicrosPerMillionTokens: settings.LLMOutputPriceMicrosPerMillionTokens,
		},
		Tools: agentapi.ToolPolicy{
			VisibleToolIDs: []string{}, RestrictVisible: true,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout:           time.Duration(settings.AgentTimeout),
			MaxSteps:          settings.AgentMaxSteps,
			ContextTokens:     settings.LLMContextWindow,
			MaxContinueRounds: settings.LLMMaxContinueRounds,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		return nil, fmt.Errorf("prepare delegation verifier: %w", err)
	}
	definitions = append(definitions, verifier)
	synthesizer, err := agentapi.Prepare(agentapi.Definition{
		ID: "synthesizer", Version: version, DisplayName: "Evidence Synthesizer",
		Purpose: "Synthesize delegated investigation handoffs without gathering new evidence.",
		Prompt: agentapi.PromptSpec{
			System:  prompts.Text(prompts.AgentCatalogSynthesizer),
			Version: "investigation-synthesis-v6",
		},
		InputSchema:  agentapi.SchemaRef{ID: "investigation.verified_bundle", Version: 2},
		OutputSchema: agentapi.SchemaRef{ID: "investigation.answer", Version: 3},
		Model: agentapi.ModelPolicy{
			Provider: settings.LLMProvider, Model: settings.LLMModel,
			MaxOutputTokens:                   settings.LLMAnswerMaxTokens,
			InputPriceMicrosPerMillionTokens:  settings.LLMInputPriceMicrosPerMillionTokens,
			OutputPriceMicrosPerMillionTokens: settings.LLMOutputPriceMicrosPerMillionTokens,
		},
		Tools: agentapi.ToolPolicy{VisibleToolIDs: []string{}, RestrictVisible: true},
		Budget: agentapi.BudgetPolicy{
			Timeout:           time.Duration(settings.AgentTimeout),
			MaxSteps:          settings.AgentMaxSteps,
			ContextTokens:     settings.LLMContextWindow,
			MaxContinueRounds: settings.LLMMaxContinueRounds,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		return nil, fmt.Errorf("prepare synthesizer: %w", err)
	}
	return append(definitions, synthesizer), nil
}

func reportPrompt(focus, rolePrompt string) string {
	return prompts.MustRender(prompts.AgentCatalogInvestigationReport, struct {
		Focus      string
		RolePrompt string
	}{
		Focus:      focus,
		RolePrompt: rolePrompt,
	})
}
