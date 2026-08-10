package execution

import "github.com/dekwanlabs/nasuta/tool"

type evidenceKey struct {
	sourceKind string
	target     string
	section    string
	version    string
	timeRange  string
}

type evidenceLedgerItem struct {
	key           evidenceKey
	coverage      tool.EvidenceCoverage
	contentHash   string
	trustTier     int
	evidenceClass string
	tokenCost     int
	origin        string
}

type runEvidenceLedger struct {
	items     map[evidenceKey]evidenceLedgerItem
	conflicts map[evidenceKey][]evidenceLedgerItem
}

func newRunEvidenceLedger(seed []tool.EvidenceUnit) *runEvidenceLedger {
	ledger := &runEvidenceLedger{
		items: make(map[evidenceKey]evidenceLedgerItem, len(seed)),
	}
	ledger.add(seed, "seed")
	return ledger
}

func (ledger *runEvidenceLedger) add(units []tool.EvidenceUnit, origin string) {
	if ledger == nil {
		return
	}
	for _, unit := range units {
		sections := unit.Sections
		if len(sections) == 0 {
			sections = []string{""}
		}
		for _, section := range sections {
			key := evidenceKey{
				sourceKind: unit.SourceKind,
				target:     unit.Target,
				section:    section,
				version:    unit.Version,
				timeRange:  unit.TimeRange,
			}
			if key.sourceKind == "" || key.target == "" {
				continue
			}
			next := evidenceLedgerItem{
				key: key, coverage: unit.Coverage, contentHash: unit.ContentHash,
				trustTier: unit.TrustTier, evidenceClass: unit.EvidenceClass,
				tokenCost: unit.TokenCost, origin: origin,
			}
			current, exists := ledger.items[key]
			if exists && current.coverage.Complete && !next.coverage.Complete {
				continue
			}
			if exists && current.contentHash == next.contentHash {
				if next.coverage.Complete {
					current.coverage = next.coverage
					ledger.items[key] = current
				}
				continue
			}
			if exists && current.contentHash != "" && next.contentHash != "" {
				if ledger.conflicts == nil {
					ledger.conflicts = make(map[evidenceKey][]evidenceLedgerItem)
				}
				ledger.conflicts[key] = append(ledger.conflicts[key], next)
				continue
			}
			ledger.items[key] = next
		}
	}
}

func (ledger *runEvidenceLedger) fullyCovers(scope tool.EvidenceScope) ([]evidenceKey, bool) {
	if ledger == nil || scope.SourceKind == "" || scope.Target == "" {
		return nil, false
	}
	targetKey := evidenceKey{
		sourceKind: scope.SourceKind, target: scope.Target,
		version: scope.Version, timeRange: scope.TimeRange,
	}
	if item, exists := ledger.items[targetKey]; exists && item.coverage.Complete {
		return []evidenceKey{targetKey}, true
	}
	if len(scope.Sections) == 0 {
		return nil, false
	}
	keys := make([]evidenceKey, 0, len(scope.Sections))
	for _, section := range scope.Sections {
		key := evidenceKey{
			sourceKind: scope.SourceKind, target: scope.Target, section: section,
			version: scope.Version, timeRange: scope.TimeRange,
		}
		if _, exists := ledger.items[key]; !exists {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]tool.EvidenceUnit, len(units))
	for i, unit := range units {
		out[i] = unit
		out[i].Sections = append([]string(nil), unit.Sections...)
		out[i].Facets = append([]string(nil), unit.Facets...)
	}
	return out
}
