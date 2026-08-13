package execution

import (
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestCalculateModelUsageCeilingIncludesAllBoundedGenerationPaths(t *testing.T) {
	ceiling, err := CalculateModelUsageCeiling(
		agentapi.BudgetPolicy{
			Timeout: time.Minute, MaxSteps: 4, ContextTokens: 32000,
			MaxContinueRounds: 2,
		},
		agentapi.ModelPolicy{
			Provider: "openai", Model: "model", MaxOutputTokens: 2048,
			InputPriceMicrosPerMillionTokens:  2_000_000,
			OutputPriceMicrosPerMillionTokens: 6_000_000,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	const calls int64 = 4 + 5*(2+1)
	if ceiling.Calls != calls ||
		ceiling.InputTokens != calls*32000 ||
		ceiling.OutputTokens != calls*2048 ||
		ceiling.TotalTokens != calls*(32000+2048) {
		t.Fatalf("ceiling = %+v", ceiling)
	}
	perCallCost := int64(32000*2 + 2048*6)
	if ceiling.CostMicros != calls*perCallCost {
		t.Fatalf("cost = %d, want %d", ceiling.CostMicros, calls*perCallCost)
	}
}

func TestCalculateModelUsageCeilingRejectsIncompletePricing(t *testing.T) {
	_, err := CalculateModelUsageCeiling(
		agentapi.BudgetPolicy{
			Timeout: time.Minute, MaxSteps: 1, ContextTokens: 32000,
		},
		agentapi.ModelPolicy{
			Provider: "openai", Model: "model", MaxOutputTokens: 1024,
			InputPriceMicrosPerMillionTokens: 1,
		},
	)
	if err == nil {
		t.Fatal("incomplete model pricing was accepted")
	}
}
