package catalog

import (
	"fmt"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/prompts"
)

// DefaultReviewersVersion builds the isolated read-only review agents shipped by Nasuta.
func DefaultReviewersVersion(settings *config.PlatformSettings, version int64) ([]agentapi.Definition, error) {
	specs := []struct {
		id, name, purpose, focus, inputSchema, outputSchema, promptVersion string
	}{
		{
			id: "review.architecture", name: "Architecture Reviewer",
			purpose:     "Review system boundaries, dependency direction, contracts, and evolvability.",
			focus:       "Focus on architecture, compatibility, ownership boundaries, coupling, and unsupported lifecycle complexity.",
			inputSchema: "review.request", outputSchema: "review.report", promptVersion: "review-report-v1",
		},
		{
			id: "review.security", name: "Security Reviewer",
			purpose:     "Review trust boundaries, authorization, secrets, input handling, and abuse resistance.",
			focus:       "Focus on authentication, authorization, data exposure, injection, secret handling, and privilege escalation.",
			inputSchema: "review.request", outputSchema: "review.report", promptVersion: "review-report-v1",
		},
		{
			id: "review.reliability", name: "Reliability Reviewer",
			purpose:     "Review failure handling, concurrency, persistence, recovery, and operability.",
			focus:       "Focus on data integrity, concurrency, retries, cancellation, recovery, bounded resource use, and observability.",
			inputSchema: "review.request", outputSchema: "review.report", promptVersion: "review-report-v1",
		},
		{
			id: "review.adjudicator", name: "Review Adjudicator",
			purpose:     "Resolve one evidence-backed review conflict without approving or modifying the subject.",
			focus:       "adjudicator",
			inputSchema: "review.adjudication.request", outputSchema: "review.adjudication",
			promptVersion: "review-adjudication-v1",
		},
	}
	definitions := make([]agentapi.Definition, 0, len(specs))
	for _, spec := range specs {
		definition, err := agentapi.Prepare(agentapi.Definition{
			ID: spec.id, Version: version, DisplayName: spec.name, Purpose: spec.purpose,
			Prompt: agentapi.PromptSpec{
				System:  reviewAgentPrompt(spec.focus),
				Version: spec.promptVersion,
			},
			InputSchema:  agentapi.SchemaRef{ID: spec.inputSchema, Version: 1},
			OutputSchema: agentapi.SchemaRef{ID: spec.outputSchema, Version: 1},
			Model: agentapi.ModelPolicy{
				Provider: settings.LLMProvider, Model: settings.LLMModel,
				MaxOutputTokens: settings.LLMAnswerMaxTokens,
			},
			Budget: agentapi.BudgetPolicy{
				Timeout:       time.Duration(settings.AgentTimeout),
				MaxSteps:      settings.AgentMaxSteps,
				ContextTokens: settings.LLMContextWindow,
			},
			Permissions: agentapi.PermissionPolicy{Scopes: []string{"knowledge.read"}},
		})
		if err != nil {
			return nil, fmt.Errorf("prepare reviewer %q: %w", spec.id, err)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func reviewAgentPrompt(focus string) string {
	if focus == "adjudicator" {
		return prompts.Text(prompts.AgentCatalogAdjudicator)
	}
	return prompts.MustRender(prompts.AgentCatalogReviewer, struct {
		Focus string
	}{Focus: focus})
}
