package investigation

import (
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/tool"
)

// ContractBuilder derives the immutable InvestigationContract from a question.
// Goal derivation is deterministic; it reuses the platform facet vocabulary and
// leaves entity/language inference to the QA query classifier that owns it.
type ContractBuilder struct {
	AllowedToolIDs   []tool.ToolID
	PrincipalToolIDs []tool.ToolID
	WorkspaceToolIDs []tool.ToolID
	MaxRounds        int
	MaxTasks         int
	BudgetProfile    string
	// ExtraGoals appends evidence goals beyond the canonical query facets, for
	// example an AI entrypoint goal when the classifier has no stable facet.
	ExtraGoals []EvidenceGoal
}

type ContractRequest struct {
	ID       string
	Question string
	Kind     domain.QueryKind
	Entities []string
}

func (builder ContractBuilder) Build(request ContractRequest) (InvestigationContract, error) {
	question := strings.TrimSpace(request.Question)
	if question == "" {
		return InvestigationContract{}, fmt.Errorf("%w: question is required", ErrPlanInvalid)
	}
	id := strings.TrimSpace(request.ID)
	if id == "" {
		id = "contract"
	}
	facets := domain.RequiredFacetsFor(request.Kind)
	goals := make([]EvidenceGoal, 0, len(facets)+len(builder.ExtraGoals))
	for _, facet := range facets {
		value := string(facet)
		goals = append(goals, EvidenceGoal{
			ID:          value,
			Kind:        value,
			Description: facetDescription(facet),
			Facets:      []string{value},
			Required:    true,
		})
	}
	if len(facets) == 0 {
		// Unclassified questions still need a verifiable evidence goal. The
		// generic explore goal lets the planner fall back to the
		// investigation.explore template instead of failing with an empty plan.
		goals = append(goals, EvidenceGoal{
			ID:          "explore",
			Kind:        GoalKindExplore,
			Description: "collect and verify evidence that answers the question",
			Facets:      []string{GoalKindExplore},
			Required:    true,
		})
	}
	goals = append(goals, builder.ExtraGoals...)
	if err := validateContractGoals(goals); err != nil {
		return InvestigationContract{}, err
	}
	return InvestigationContract{
		ID:               id,
		Version:          InvestigationContractVersion,
		Entities:         append([]string(nil), request.Entities...),
		Question:         question,
		EvidenceGoals:    goals,
		AllowedToolIDs:   append([]tool.ToolID(nil), builder.AllowedToolIDs...),
		PrincipalToolIDs: append([]tool.ToolID(nil), builder.PrincipalToolIDs...),
		WorkspaceToolIDs: append([]tool.ToolID(nil), builder.WorkspaceToolIDs...),
		MaxRounds:        builder.MaxRounds,
		MaxTasks:         builder.MaxTasks,
		BudgetProfile:    builder.BudgetProfile,
		CreatedAt:        time.Now().UTC(),
	}, nil
}

func facetDescription(facet domain.EvidenceFacet) string {
	for _, spec := range domain.FacetCatalog() {
		if spec.ID == facet {
			return spec.Description
		}
	}
	return string(facet)
}

func validateContractGoals(goals []EvidenceGoal) error {
	seen := make(map[string]struct{}, len(goals))
	for _, goal := range goals {
		id := strings.TrimSpace(goal.ID)
		if id == "" || strings.TrimSpace(goal.Kind) == "" {
			return fmt.Errorf("%w: goal id and kind are required", ErrPlanInvalid)
		}
		if len(goal.Facets) == 0 {
			return fmt.Errorf("%w: goal %q facets are required", ErrPlanInvalid, id)
		}
		for _, facet := range goal.Facets {
			if strings.TrimSpace(facet) == "" {
				return fmt.Errorf("%w: goal %q contains an empty facet", ErrPlanInvalid, id)
			}
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("%w: goal %q is duplicated", ErrPlanInvalid, id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
