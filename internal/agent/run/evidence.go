package run

import (
	"crypto/sha256"
	"encoding/hex"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const EvidenceLedgerArtifactKind = "evidence_ledger"

var EvidenceLedgerArtifactSchema = agentapi.SchemaRef{
	ID: "agent.evidence_ledger", Version: 1,
}

func evidenceLedgerArtifactID(runID string) string {
	sum := sha256.Sum256([]byte(
		"agent_evidence_ledger\x00" + runID,
	))
	return "artifact_" + hex.EncodeToString(sum[:12])
}
