package app

import (
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
)

func delegatedInvestigationBudgetPolicy(
	definitions []agentapi.Definition,
) (workflow.DelegatedInvestigationBudgetPolicy, error) {
	byID := make(map[string]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		if _, exists := byID[definition.ID]; exists {
			return workflow.DelegatedInvestigationBudgetPolicy{}, fmt.Errorf(
				"delegated investigation agent definition %q is duplicated",
				definition.ID,
			)
		}
		byID[definition.ID] = definition
	}
	investigatorBudget := func(id string) (workflow.NodeBudget, error) {
		definition, ok := byID[id]
		if !ok {
			return workflow.NodeBudget{}, fmt.Errorf(
				"delegated investigation agent definition %q is required",
				id,
			)
		}
		if len(definition.Tools.VisibleToolIDs) == 0 {
			return workflow.NodeBudget{}, fmt.Errorf(
				"delegated investigation agent definition %q requires visible tools",
				id,
			)
		}
		return delegatedAgentNodeBudget(definition, int64(definition.Budget.MaxSteps))
	}
	synthesizer, ok := byID["synthesizer"]
	if !ok {
		return workflow.DelegatedInvestigationBudgetPolicy{}, fmt.Errorf(
			"delegated investigation agent definition %q is required",
			"synthesizer",
		)
	}
	if !synthesizer.Tools.RestrictVisible || len(synthesizer.Tools.VisibleToolIDs) != 0 {
		return workflow.DelegatedInvestigationBudgetPolicy{}, fmt.Errorf(
			"delegated investigation synthesizer must explicitly disable tools",
		)
	}
	code, err := investigatorBudget("investigator.code")
	if err != nil {
		return workflow.DelegatedInvestigationBudgetPolicy{}, err
	}
	runtime, err := investigatorBudget("investigator.runtime")
	if err != nil {
		return workflow.DelegatedInvestigationBudgetPolicy{}, err
	}
	docs, err := investigatorBudget("investigator.docs")
	if err != nil {
		return workflow.DelegatedInvestigationBudgetPolicy{}, err
	}
	synthesis, err := delegatedAgentNodeBudget(synthesizer, 0)
	if err != nil {
		return workflow.DelegatedInvestigationBudgetPolicy{}, err
	}
	return workflow.DelegatedInvestigationBudgetPolicy{
		Code: code, Runtime: runtime, Docs: docs, Synthesizer: synthesis,
	}, nil
}

func delegatedAgentNodeBudget(
	definition agentapi.Definition,
	maxToolCalls int64,
) (workflow.NodeBudget, error) {
	ceiling, err := execution.CalculateModelUsageCeiling(
		definition.Budget,
		definition.Model,
	)
	if err != nil {
		return workflow.NodeBudget{}, fmt.Errorf(
			"calculate delegated investigation agent %q budget: %w",
			definition.ID,
			err,
		)
	}
	return workflow.NodeBudget{
		MaxInputTokens: ceiling.InputTokens, MaxOutputTokens: ceiling.OutputTokens,
		MaxTotalTokens: ceiling.TotalTokens, MaxToolCalls: maxToolCalls,
		MaxCostMicros: ceiling.CostMicros,
	}, nil
}
