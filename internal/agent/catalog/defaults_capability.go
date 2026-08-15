package catalog

import (
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
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
		inputFacets          []string
		freshness            agentapi.FreshnessPolicy
	}{
		{
			id: "knowledge.code.inspect", agentID: "investigator.code",
			purpose: "Inspect source implementation, symbols, APIs, and call paths.",
			inputFacets: []string{
				"implementation",
				"entrypoint",
				"core_flow",
				"data_and_state",
			},
			freshness: agentapi.FreshnessStable,
		},
		{
			id: "knowledge.service.trace", agentID: "investigator.runtime",
			purpose: "Trace service topology, dependencies, APIs, and runtime entrypoints.",
			inputFacets: []string{
				"service.topology",
				"system_boundary",
				"external_dependency",
				"runtime_and_operations",
			},
			freshness: agentapi.FreshnessStable,
		},
		{
			id: "knowledge.docs.verify", agentID: "investigator.docs",
			purpose: "Verify runbooks, system documentation, and documentation coverage.",
			inputFacets: []string{
				"documentation",
				"business_domain",
			},
			freshness: agentapi.FreshnessStable,
		},
		{
			id: "knowledge.web.research", agentID: "investigator.web",
			purpose: "Research current public evidence through the configured web provider.",
			inputFacets: []string{
				"implementation",
				"entrypoint",
				"core_flow",
				"data_and_state",
				"service.topology",
				"system_boundary",
				"external_dependency",
				"runtime_and_operations",
				"documentation",
				"business_domain",
			},
			freshness: agentapi.FreshnessCurrent,
		},
		{
			id: "knowledge.memory.recall", agentID: "investigator.memory",
			purpose: "Evaluate bounded recalled memory admitted by the task contract.",
			inputFacets: []string{
				"implementation",
				"entrypoint",
				"core_flow",
				"data_and_state",
				"service.topology",
				"system_boundary",
				"external_dependency",
				"runtime_and_operations",
				"documentation",
				"business_domain",
			},
			freshness: agentapi.FreshnessCurrent,
		},
		{
			id: "evidence.synthesize", agentID: "synthesizer",
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
		capabilities = append(capabilities, agentapi.Capability{
			ID:           specification.id,
			Version:      version,
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
