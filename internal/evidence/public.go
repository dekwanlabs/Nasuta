package evidence

import (
	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

// PublicConflicts converts internal ledger conflicts to the stable agent API shape.
// Keeping this projection at the evidence boundary prevents each caller from
// reimplementing the same identity and clone semantics.
func PublicConflicts(conflicts []Conflict) []agentapi.EvidenceConflict {
	if len(conflicts) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceConflict, len(conflicts))
	for index, conflict := range conflicts {
		out[index] = agentapi.EvidenceConflict{
			Identity: agentapi.EvidenceIdentity{
				SourceKind: conflict.Key.SourceKind,
				Target:     conflict.Key.Target,
				Section:    conflict.Key.Section,
				Version:    conflict.Key.Version,
				TimeRange:  conflict.Key.TimeRange,
			},
			Current:        CloneUnit(conflict.Current),
			Incoming:       CloneUnit(conflict.Incoming),
			CurrentOrigin:  conflict.CurrentOrigin,
			IncomingOrigin: conflict.IncomingOrigin,
		}
	}
	return out
}

// IdentityMatches reports whether a single-section evidence unit matches the
// public identity, including the contract that an empty section means no
// section and a non-empty section means exactly one section.
func IdentityMatches(identity agentapi.EvidenceIdentity, unit tool.EvidenceUnit) bool {
	if identity.SourceKind != unit.SourceKind ||
		identity.Target != unit.Target ||
		identity.Version != unit.Version ||
		identity.TimeRange != unit.TimeRange {
		return false
	}
	if identity.Section == "" {
		return len(unit.Sections) == 0
	}
	return len(unit.Sections) == 1 && unit.Sections[0] == identity.Section
}
