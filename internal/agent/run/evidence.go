package run

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const EvidenceLedgerArtifactKind = "evidence_ledger"

var EvidenceLedgerArtifactSchema = agentapi.SchemaRef{
	ID: "agent.evidence_ledger", Version: 1,
}

// EvidenceReference preserves the exact model-visible ref used for a resolved
// authoritative evidence unit.
type EvidenceReference struct {
	Ref  string
	Unit tool.EvidenceUnit
}

// EvidenceReferenceID returns the stable handoff ref for one canonical
// evidence identity. Content integrity remains bound by the ledger hash.
func EvidenceReferenceID(unit tool.EvidenceUnit) string {
	sections := append([]string(nil), unit.Sections...)
	sort.Strings(sections)
	identity := struct {
		SourceKind string   `json:"source_kind"`
		Target     string   `json:"target"`
		Sections   []string `json:"sections,omitempty"`
		Version    string   `json:"version,omitempty"`
		TimeRange  string   `json:"time_range,omitempty"`
	}{
		SourceKind: unit.SourceKind,
		Target:     unit.Target,
		Sections:   sections,
		Version:    unit.Version,
		TimeRange:  unit.TimeRange,
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(append([]byte("evidence_ref\x00"), raw...))
	return "ev_" + hex.EncodeToString(sum[:12])
}

func evidenceLedgerArtifactID(runID string) string {
	sum := sha256.Sum256([]byte(
		"agent_evidence_ledger\x00" + runID,
	))
	return "artifact_" + hex.EncodeToString(sum[:12])
}
