package catalog

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

const (
	verifierOutputMinimum = 4096
	investigatorMaxSteps  = 6
	convergenceMaxSteps   = 1
	roleMaxContinueRounds = 1
)

// DefaultInvestigators builds the fixed read-only delegated investigation panel.
func DefaultInvestigators(settings *config.PlatformSettings, version int64) ([]agentapi.Definition, error) {
	verifierOutput := delegationRoleOutputBudget(
		settings.LLMAnswerMaxTokens,
		verifierOutputMinimum,
	)
	investigatorSteps := boundedRoleLimit(settings.AgentMaxSteps, investigatorMaxSteps)
	convergenceSteps := boundedRoleLimit(settings.AgentMaxSteps, convergenceMaxSteps)
	continueRounds := boundedRoleLimit(
		settings.LLMMaxContinueRounds,
		roleMaxContinueRounds,
	)
	specs := []struct {
		id, name, purpose, focus string
	}{
		{
			id: "investigator.code", name: "Code Investigator", focus: "code",
			purpose: "Investigate source implementation, exact symbols, and call paths.",
		},
		{
			id: "investigator.runtime", name: "Runtime Topology Investigator", focus: "runtime",
			purpose: "Investigate indexed service topology, dependencies, and exposed runtime entrypoints.",
		},
		{
			id: "investigator.docs", name: "Documentation Investigator", focus: "docs",
			purpose: "Investigate runbooks, system documentation, and documentation coverage.",
		},
		{
			id: "investigator.web", name: "Web Research Investigator", focus: "web",
			purpose: "Investigate current public evidence through the configured web provider.",
		},
		{
			id: "investigator.memory", name: "Memory Recall Investigator", focus: "memory",
			purpose: "Evaluate bounded recalled memory already admitted by the task contract.",
		},
	}
	definitions := make([]agentapi.Definition, 0, len(specs)+2)
	for _, spec := range specs {
		rolePrompt := prompts.MustRender(prompts.AgentCatalogInvestigator, struct {
			Focus string
		}{Focus: spec.focus})
		definition, err := agentapi.Prepare(agentapi.Definition{
			ID: spec.id, Version: version, DisplayName: spec.name, Purpose: spec.purpose,
			Prompt: agentapi.PromptSpec{
				System: reportPrompt(spec.focus, rolePrompt), Version: "investigation-report-v3",
			},
			InputSchema:  agentapi.TaskContractSchemaRef(),
			OutputSchema: agentapi.InvestigationReportSchemaRef(),
			Model: agentapi.ModelPolicy{
				Provider: settings.LLMProvider, Model: settings.LLMModel,
				MaxOutputTokens:                   settings.LLMAnswerMaxTokens,
				InputPriceMicrosPerMillionTokens:  settings.LLMInputPriceMicrosPerMillionTokens,
				OutputPriceMicrosPerMillionTokens: settings.LLMOutputPriceMicrosPerMillionTokens,
			},
			// Investigators are not tool-allowlisted: each one gets the full
			// read-only tool set so it can span code, topology, docs, and call
			// paths within one subject instead of being locked to one dimension.
			// Divergence is bounded by MaxToolCalls, not by a tool allowlist.
			Tools: agentapi.ToolPolicy{},
			Budget: agentapi.BudgetPolicy{
				Timeout:            time.Duration(settings.AgentTimeout),
				MaxSteps:           investigatorSteps,
				MaxToolCalls:       settings.AgentMaxToolCalls,
				ContextTokens:      settings.LLMContextWindow,
				MaxToolResultBytes: 24 * 1024,
				MaxContinueRounds:  continueRounds,
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
			Version: "delegation-verification-v2",
		},
		InputSchema: agentapi.SchemaRef{
			ID: "delegation.verification.request", Version: 1,
		},
		OutputSchema: agentapi.SchemaRef{
			ID: "delegation.verification.result", Version: 1,
		},
		Model: agentapi.ModelPolicy{
			Provider: settings.LLMProvider, Model: settings.LLMModel,
			MaxOutputTokens:                   verifierOutput,
			InputPriceMicrosPerMillionTokens:  settings.LLMInputPriceMicrosPerMillionTokens,
			OutputPriceMicrosPerMillionTokens: settings.LLMOutputPriceMicrosPerMillionTokens,
		},
		Tools: agentapi.ToolPolicy{
			VisibleToolIDs: []string{}, RestrictVisible: true,
		},
		Budget: agentapi.BudgetPolicy{
			Timeout:           time.Duration(settings.AgentTimeout),
			MaxSteps:          convergenceSteps,
			ContextTokens:     settings.LLMContextWindow,
			MaxContinueRounds: continueRounds,
		},
		Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
	})
	if err != nil {
		return nil, fmt.Errorf("prepare delegation verifier: %w", err)
	}
	definitions = append(definitions, verifier)
	return definitions, nil
}

// delegationRoleOutputBudget derives a bounded verifier model output cap from
// the shared answer budget. Small answer budgets retain the role-specific floor
// required for structured handoffs.
func delegationRoleOutputBudget(global, minimum int) int {
	if global <= 0 {
		return minimum
	}
	budget := global / 10
	if budget < minimum {
		return minimum
	}
	return budget
}

func boundedRoleLimit(global, maximum int) int {
	if global <= 0 || global <= maximum {
		return global
	}
	return maximum
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
