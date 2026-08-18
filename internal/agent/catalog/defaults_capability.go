package catalog

import (
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

// DefaultCapabilities binds the standard investigation agents to planner capabilities.
func DefaultCapabilities(
	definitions []agentapi.Definition,
	version int64,
) ([]agentapi.Capability, error) {
	if version <= 0 {
		return nil, fmt.Errorf("default investigation capability version must be positive")
	}
	byID := make(map[string]agentapi.Definition, len(definitions))
	for _, definition := range definitions {
		if _, duplicate := byID[definition.ID]; duplicate {
			return nil, fmt.Errorf(
				"default investigation agent definition %q is duplicated",
				definition.ID,
			)
		}
		byID[definition.ID] = definition
	}
	specifications := []struct {
		id, agentID, purpose string
		role                 agentapi.CapabilityRole
		inputFacets          []string
		freshness            agentapi.FreshnessPolicy
	}{
		{
			id: "knowledge.code.inspect", agentID: "investigator.code",
			role:    agentapi.RoleInvestigator,
			purpose: "Inspect source implementation, symbols, APIs, and call paths.",
			inputFacets: []string{
				string(domain.FacetEntrypoint),
				string(domain.FacetCoreFlow),
				string(domain.FacetDataAndState),
				string(domain.FacetExternalDependency),
			},
			freshness: agentapi.FreshnessStable,
		},
		{
			id: "knowledge.service.trace", agentID: "investigator.runtime",
			role:    agentapi.RoleInvestigator,
			purpose: "Trace service topology, dependencies, APIs, and runtime entrypoints.",
			inputFacets: []string{
				string(domain.FacetSystemBoundary),
				string(domain.FacetExternalDependency),
				string(domain.FacetRuntimeOperations),
			},
			freshness: agentapi.FreshnessStable,
		},
		{
			id: "knowledge.docs.verify", agentID: "investigator.docs",
			role:        agentapi.RoleInvestigator,
			purpose:     "Verify runbooks, system documentation, and documentation coverage.",
			inputFacets: canonicalFacetValues(),
			freshness:   agentapi.FreshnessStable,
		},
		{
			id: "knowledge.web.research", agentID: "investigator.web",
			role:    agentapi.RoleInvestigator,
			purpose: "Research current public evidence through the configured web provider.",
			inputFacets: []string{
				string(domain.FacetExternalDependency),
				string(domain.FacetBusinessDomain),
			},
			freshness: agentapi.FreshnessCurrent,
		},
		{
			id: "knowledge.memory.recall", agentID: "investigator.memory",
			role:    agentapi.RoleInvestigator,
			purpose: "Evaluate bounded recalled memory admitted by the task contract.",
			inputFacets: []string{
				string(domain.FacetBusinessDomain),
			},
			freshness: agentapi.FreshnessCurrent,
		},
		{
			id: "evidence.semantic.verify", agentID: "delegation.verifier",
			role:      agentapi.RoleVerifier,
			purpose:   "Resolve bounded semantic claim conflicts using cited evidence.",
			freshness: agentapi.FreshnessStable,
		},
		{
			id: "evidence.synthesize", agentID: "synthesizer",
			role:      agentapi.RoleSynthesizer,
			purpose:   "Synthesize admitted investigation evidence without gathering new evidence.",
			freshness: agentapi.FreshnessStable,
		},
	}
	capabilities := make([]agentapi.Capability, 0, len(specifications))
	for _, specification := range specifications {
		definition, ok := byID[specification.agentID]
		if !ok {
			return nil, fmt.Errorf(
				"default investigation agent definition %q is required",
				specification.agentID,
			)
		}
		if definition.Version != version {
			return nil, fmt.Errorf(
				"default investigation agent definition %q version %d does not match capability version %d",
				definition.ID,
				definition.Version,
				version,
			)
		}
		if definition.Tools.AllowWrite {
			return nil, fmt.Errorf(
				"default investigation agent definition %q must be read-only",
				definition.ID,
			)
		}
		if err := validateFacetValues(specification.inputFacets); err != nil {
			return nil, fmt.Errorf("default capability %q: %w", specification.id, err)
		}
		capabilities = append(capabilities, agentapi.Capability{
			ID:           specification.id,
			Version:      version,
			Role:         specification.role,
			Purpose:      specification.purpose,
			InputFacets:  append([]string(nil), specification.inputFacets...),
			InputSchema:  definition.InputSchema,
			OutputSchema: definition.OutputSchema,
			ToolIDs: append(
				[]string(nil),
				definition.Tools.VisibleToolIDs...,
			),
			PermissionScope: append(
				[]string(nil),
				definition.Permissions.Scopes...,
			),
			Freshness:      specification.freshness,
			SideEffects:    agentapi.SideEffectNone,
			RetrySafe:      true,
			MaxConcurrency: 3,
			Enabled:        true,
			Agent: agentapi.DefinitionRef{
				ID:      definition.ID,
				Version: definition.Version,
			},
		})
	}
	return capabilities, nil
}

func canonicalFacetValues() []string {
	specs := domain.FacetCatalog()
	values := make([]string, len(specs))
	for i, spec := range specs {
		values[i] = string(spec.ID)
	}
	return values
}

func validateFacetValues(values []string) error {
	facets := make([]domain.EvidenceFacet, len(values))
	for i, value := range values {
		facets[i] = domain.EvidenceFacet(value)
	}
	return domain.ValidateFacets(facets)
}
