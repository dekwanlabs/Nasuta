package execution

import (
	"encoding/json"
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

type runEvidenceLedger struct {
	ledger    *evidence.Ledger
	conflicts []evidence.Conflict
}

func newRunEvidenceLedger(
	seed []tool.EvidenceUnit,
	propagatedConflicts []evidence.Conflict,
) *runEvidenceLedger {
	ledger := &runEvidenceLedger{ledger: evidence.New(nil, "")}
	ledger.remember(propagatedConflicts)
	ledger.add(seed, "seed")
	return ledger
}

func (ledger *runEvidenceLedger) add(units []tool.EvidenceUnit, origin string) []evidence.Conflict {
	if ledger == nil {
		return nil
	}
	conflicts := ledger.ledger.Add(units, origin)
	ledger.conflicts = append(ledger.conflicts, conflicts...)
	return conflicts
}

func (ledger *runEvidenceLedger) remember(conflicts []evidence.Conflict) {
	if ledger == nil || len(conflicts) == 0 {
		return
	}
	ledger.ledger.RememberConflicts(conflicts)
	seen := make(map[string]struct{}, len(ledger.conflicts)+len(conflicts))
	for _, conflict := range ledger.conflicts {
		seen[evidence.ConflictFingerprint(conflict)] = struct{}{}
	}
	for _, conflict := range conflicts {
		fingerprint := evidence.ConflictFingerprint(conflict)
		if _, duplicate := seen[fingerprint]; duplicate {
			continue
		}
		seen[fingerprint] = struct{}{}
		ledger.conflicts = append(ledger.conflicts, evidence.CloneConflict(conflict))
	}
}

func (ledger *runEvidenceLedger) fullyCovers(scope tool.EvidenceScope) ([]evidence.Key, bool) {
	if ledger == nil {
		return nil, false
	}
	return ledger.ledger.FullyCovers(scope)
}

func (ledger *runEvidenceLedger) snapshot() ([]tool.EvidenceUnit, []evidence.Conflict) {
	if ledger == nil {
		return nil, nil
	}
	return ledger.ledger.Units(), evidence.CloneConflicts(ledger.conflicts)
}

func cloneEvidenceUnits(units []tool.EvidenceUnit) []tool.EvidenceUnit {
	return evidence.CloneUnits(units)
}

type evidenceConflictVersion struct {
	ContentHash string `json:"content_hash"`
	Origin      string `json:"origin"`
}

type evidenceConflictNotice struct {
	Type     string                  `json:"type"`
	Identity evidence.Key            `json:"identity"`
	Current  evidenceConflictVersion `json:"current"`
	Incoming evidenceConflictVersion `json:"incoming"`
}

func marshalConflictNotices(conflicts []evidence.Conflict) ([]string, error) {
	notices := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		encoded, err := json.Marshal(evidenceConflictNotice{
			Type: "evidence_conflict", Identity: conflict.Key,
			Current: evidenceConflictVersion{
				ContentHash: conflict.Current.ContentHash, Origin: conflict.CurrentOrigin,
			},
			Incoming: evidenceConflictVersion{
				ContentHash: conflict.Incoming.ContentHash, Origin: conflict.IncomingOrigin,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal evidence conflict %q: %w", conflict.Key.String(), err)
		}
		notices = append(notices, string(encoded))
	}
	return notices, nil
}
