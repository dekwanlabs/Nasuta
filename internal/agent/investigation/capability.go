package investigation

import agentapi "github.com/dekwanlabs/nasuta/agent"

// Selection is the server-owned capability projection for one evidence source
// and facet pair.
type Selection struct {
	CapabilityID string
	NodeID       string
	Focus        string
}

// Select returns the only built-in investigator that can produce the requested
// evidence. Unsupported source and facet pairs have no selection.
func Select(source agentapi.EvidenceSource, facet string) (Selection, bool) {
	switch source {
	case agentapi.EvidenceSourceInternal:
		switch facet {
		case "entrypoint", "core_flow", "data_and_state":
			return Selection{
				CapabilityID: "knowledge.code.inspect",
				NodeID:       "investigate.code",
				Focus:        "code",
			}, true
		case "system_boundary", "external_dependency", "runtime_and_operations":
			return Selection{
				CapabilityID: "knowledge.service.trace",
				NodeID:       "investigate.runtime",
				Focus:        "runtime",
			}, true
		case "business_domain":
			return Selection{
				CapabilityID: "knowledge.docs.verify",
				NodeID:       "investigate.docs",
				Focus:        "documentation",
			}, true
		}
	case agentapi.EvidenceSourceWeb:
		switch facet {
		case "external_dependency", "business_domain":
			return Selection{
				CapabilityID: "knowledge.web.research",
				NodeID:       "investigate.web",
				Focus:        "web",
			}, true
		}
	case agentapi.EvidenceSourceMemory:
		if facet == "business_domain" {
			return Selection{
				CapabilityID: "knowledge.memory.recall",
				NodeID:       "investigate.memory",
				Focus:        "memory",
			}, true
		}
	case agentapi.EvidenceSourceRuntime:
		return Selection{
			CapabilityID: "knowledge.runtime.observe",
			NodeID:       "investigate.observe",
			Focus:        "runtime",
		}, true
	}
	return Selection{}, false
}
