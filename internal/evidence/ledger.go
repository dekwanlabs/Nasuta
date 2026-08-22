package evidence

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/dekwanlabs/nasuta/tool"
)

// Key is the canonical identity of one independently coverable evidence unit.
type Key struct {
	SourceKind string `json:"source_kind"`
	Target     string `json:"target"`
	Section    string `json:"section,omitempty"`
	Version    string `json:"version,omitempty"`
	TimeRange  string `json:"time_range,omitempty"`
}

// String returns a stable identity suitable for deterministic ordering.
func (key Key) String() string {
	return key.SourceKind + "\x00" + key.Target + "\x00" + key.Section + "\x00" + key.Version + "\x00" + key.TimeRange
}

// Handle is the stable citation token for one canonical evidence identity.
func (key Key) Handle() string {
	digest := sha256.Sum256([]byte("evidence-handle-v1\x00" + key.String()))
	return "ev_" + hex.EncodeToString(digest[:])
}

// UnitHandle returns the stable citation token for one single-section unit.
func UnitHandle(unit tool.EvidenceUnit) (string, bool) {
	key, ok := UnitKey(unit)
	if !ok {
		return "", false
	}
	return key.Handle(), true
}

// Conflict records two authoritative versions observed for one identity.
type Conflict struct {
	Key            Key               `json:"key"`
	Current        tool.EvidenceUnit `json:"current"`
	Incoming       tool.EvidenceUnit `json:"incoming"`
	CurrentOrigin  string            `json:"current_origin,omitempty"`
	IncomingOrigin string            `json:"incoming_origin,omitempty"`
}

type item struct {
	unit   tool.EvidenceUnit
	origin string
	index  int
}

// Ledger merges evidence by canonical identity while preserving source hashes.
type Ledger struct {
	items          map[Key]item
	order          []Key
	conflictHashes map[string]struct{}
}

// New seeds a run-local ledger from already trusted evidence.
func New(units []tool.EvidenceUnit, origin string) *Ledger {
	ledger := &Ledger{
		items:          make(map[Key]item, len(units)),
		conflictHashes: make(map[string]struct{}),
	}
	ledger.Add(units, origin)
	return ledger
}

// Add merges evidence and returns conflicts first observed by this call.
func (ledger *Ledger) Add(units []tool.EvidenceUnit, origin string) []Conflict {
	if ledger == nil {
		return nil
	}
	var conflicts []Conflict
	for _, unit := range Expand(units) {
		key, ok := UnitKey(unit)
		if !ok {
			continue
		}
		incoming := item{unit: unit, origin: origin}
		current, exists := ledger.items[key]
		if !exists {
			incoming.index = len(ledger.order)
			ledger.items[key] = incoming
			ledger.order = append(ledger.order, key)
			continue
		}
		if hashesConflict(current.unit.ContentHash, incoming.unit.ContentHash) {
			fingerprint := conflictFingerprint(key, current.unit.ContentHash, incoming.unit.ContentHash)
			if _, duplicate := ledger.conflictHashes[fingerprint]; duplicate {
				continue
			}
			ledger.conflictHashes[fingerprint] = struct{}{}
			conflicts = append(conflicts, Conflict{
				Key: key, Current: CloneUnit(current.unit), Incoming: CloneUnit(incoming.unit),
				CurrentOrigin: current.origin, IncomingOrigin: origin,
			})
			continue
		}
		if strongerCoverage(incoming.unit.Coverage, current.unit.Coverage) {
			incoming.index = current.index
			ledger.items[key] = incoming
		}
	}
	return conflicts
}

// RememberConflicts prevents a propagated conflict from being reported again.
func (ledger *Ledger) RememberConflicts(conflicts []Conflict) {
	if ledger == nil {
		return
	}
	for _, conflict := range conflicts {
		ledger.conflictHashes[ConflictFingerprint(conflict)] = struct{}{}
	}
}

// ConflictFingerprint identifies one version pair independently of order and origins.
func ConflictFingerprint(conflict Conflict) string {
	return conflictFingerprint(
		conflict.Key, conflict.Current.ContentHash, conflict.Incoming.ContentHash,
	)
}

// Units returns the merged evidence in first-observed identity order.
func (ledger *Ledger) Units() []tool.EvidenceUnit {
	if ledger == nil || len(ledger.order) == 0 {
		return nil
	}
	units := make([]tool.EvidenceUnit, 0, len(ledger.order))
	for _, key := range ledger.order {
		units = append(units, CloneUnit(ledger.items[key].unit))
	}
	return units
}

// FullyCovers reports whether complete evidence already satisfies the scope.
func (ledger *Ledger) FullyCovers(scope tool.EvidenceScope) ([]Key, bool) {
	if ledger == nil || scope.SourceKind == "" || scope.Target == "" {
		return nil, false
	}
	target := Key{
		SourceKind: scope.SourceKind, Target: scope.Target,
		Version: scope.Version, TimeRange: scope.TimeRange,
	}
	if current, exists := ledger.items[target]; exists && current.unit.Coverage.Complete {
		return []Key{target}, true
	}
	if len(scope.Sections) == 0 {
		return nil, false
	}
	keys := make([]Key, 0, len(scope.Sections))
	for _, section := range scope.Sections {
		key := target
		key.Section = section
		current, exists := ledger.items[key]
		if !exists || !current.unit.Coverage.Complete {
			return nil, false
		}
		keys = append(keys, key)
	}
	return keys, true
}

// UnitKey returns the canonical identity for a single-section unit.
func UnitKey(unit tool.EvidenceUnit) (Key, bool) {
	if unit.SourceKind == "" || unit.Target == "" {
		return Key{}, false
	}
	if len(unit.Sections) > 1 {
		return Key{}, false
	}
	key := Key{
		SourceKind: unit.SourceKind, Target: unit.Target,
		Version: unit.Version, TimeRange: unit.TimeRange,
	}
	if len(unit.Sections) == 1 {
		key.Section = unit.Sections[0]
	}
	return key, true
}

// Expand clones units so each returned unit owns one canonical section.
func Expand(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	count := 0
	for _, unit := range units {
		count += max(1, len(unit.Sections))
	}
	if count == 0 {
		return nil
	}
	out := make([]tool.EvidenceUnit, 0, count)
	for _, unit := range units {
		if len(unit.Sections) == 0 {
			out = append(out, CloneUnit(unit))
			continue
		}
		for _, section := range unit.Sections {
			item := CloneUnit(unit)
			item.Sections = []string{section}
			out = append(out, item)
		}
	}
	return out
}

// CloneUnits copies evidence-owned slices.
func CloneUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	if len(units) == 0 {
		return nil
	}
	out := make([]tool.EvidenceUnit, len(units))
	for index, unit := range units {
		out[index] = CloneUnit(unit)
	}
	return out
}

// CloneConflicts copies conflicts and their evidence-owned slices.
func CloneConflicts(conflicts []Conflict) []Conflict {
	if len(conflicts) == 0 {
		return nil
	}
	out := make([]Conflict, len(conflicts))
	for index, conflict := range conflicts {
		conflict.Current = CloneUnit(conflict.Current)
		conflict.Incoming = CloneUnit(conflict.Incoming)
		out[index] = conflict
	}
	return out
}

// CloneConflict copies one conflict and its evidence-owned slices.
func CloneConflict(conflict Conflict) Conflict {
	conflict.Current = CloneUnit(conflict.Current)
	conflict.Incoming = CloneUnit(conflict.Incoming)
	return conflict
}

// CloneUnit copies evidence-owned slices.
func CloneUnit(unit tool.EvidenceUnit) tool.EvidenceUnit {
	unit.Sections = append([]string(nil), unit.Sections...)
	unit.Facets = append([]string(nil), unit.Facets...)
	return unit
}

func hashesConflict(current, incoming string) bool {
	return current != "" && incoming != "" && current != incoming
}

func conflictFingerprint(key Key, currentHash, incomingHash string) string {
	if incomingHash < currentHash {
		currentHash, incomingHash = incomingHash, currentHash
	}
	return key.String() + "\x00" + currentHash + "\x00" + incomingHash
}

func strongerCoverage(candidate, current tool.EvidenceCoverage) bool {
	if candidate.Complete != current.Complete {
		return candidate.Complete
	}
	return candidate.Included > current.Included
}
