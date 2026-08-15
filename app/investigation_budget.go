package app

import (
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/workflow"
)

func investigationBudgets(
	definitions []agentapi.Definition,
) (workflow.Budgets, error) {
	byID := make(map[string]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		if _, exists := byID[definition.ID]; exists {
			return workflow.Budgets{}, fmt.Errorf(
				"investigation agent definition %q is duplicated",
				definition.ID,
			)
		}
		byID[definition.ID] = definition
	}
	investigatorBudget := func(id string, requireTools bool) (workflow.NodeBudget, error) {
		definition, ok := byID[id]
		if !ok {
			return workflow.NodeBudget{}, fmt.Errorf(
				"investigation agent definition %q is required",
				id,
			)
		}
		if requireTools && len(definition.Tools.VisibleToolIDs) == 0 {
			return workflow.NodeBudget{}, fmt.Errorf(
				"investigation agent definition %q requires visible tools",
				id,
			)
		}
		maxToolCalls := int64(0)
		if requireTools {
			maxToolCalls = int64(definition.Budget.MaxSteps)
		}
		return agentNodeBudget(definition, maxToolCalls)
	}
	synthesizer, ok := byID["synthesizer"]
	if !ok {
		return workflow.Budgets{}, fmt.Errorf(
			"investigation agent definition %q is required",
			"synthesizer",
		)
	}
	if !synthesizer.Tools.RestrictVisible || len(synthesizer.Tools.VisibleToolIDs) != 0 {
		return workflow.Budgets{}, fmt.Errorf(
			"investigation synthesizer must explicitly disable tools",
		)
	}
	code, err := investigatorBudget("investigator.code", true)
	if err != nil {
		return workflow.Budgets{}, err
	}
	runtime, err := investigatorBudget("investigator.runtime", true)
	if err != nil {
		return workflow.Budgets{}, err
	}
	docs, err := investigatorBudget("investigator.docs", true)
	if err != nil {
		return workflow.Budgets{}, err
	}
	web, err := investigatorBudget("investigator.web", true)
	if err != nil {
		return workflow.Budgets{}, err
	}
	memory, err := investigatorBudget("investigator.memory", false)
	if err != nil {
		return workflow.Budgets{}, err
	}
	synthesis, err := agentNodeBudget(synthesizer, 0)
	if err != nil {
		return workflow.Budgets{}, err
	}
	return workflow.Budgets{
		Code: code, Runtime: runtime, Docs: docs, Web: web, Memory: memory,
		Synthesizer: synthesis,
	}, nil
}

func agentNodeBudget(
	definition agentapi.Definition,
	maxToolCalls int64,
) (workflow.NodeBudget, error) {
	ceiling, err := execution.UsageCeiling(
		definition.Budget,
		definition.Model,
	)
	if err != nil {
		return workflow.NodeBudget{}, fmt.Errorf(
			"calculate investigation agent %q budget: %w",
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
