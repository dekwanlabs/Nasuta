package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	EvidenceRiskGateID       = "evidence.risk"
	EvidenceRiskPassDecision = "pass"
)

const (
	riskReasonEvidenceConflict       = "evidence_conflict"
	riskReasonHighRiskPartialSupport = "high_risk_partial_support"
)

// RiskGateEvaluator blocks synthesis when verified evidence needs user judgment.
type RiskGateEvaluator struct{}

func (RiskGateEvaluator) Evaluate(
	_ context.Context,
	request NodeRequest,
) (GateDecision, error) {
	if len(request.Inputs) != 1 {
		return GateDecision{}, fmt.Errorf(
			"evidence risk gate %q requires exactly one verified evidence input",
			request.Node.ID,
		)
	}
	source := request.Inputs[0]
	var verified verifiedEvidenceView
	if err := json.Unmarshal(source.Payload, &verified); err != nil {
		return GateDecision{}, fmt.Errorf(
			"decode evidence risk gate %q input: %w",
			request.Node.ID,
			err,
		)
	}
	decision := GateDecision{
		SubjectHash: source.ContentHash,
		Decision:    EvidenceRiskPassDecision,
	}
	if len(verified.EvidenceConflicts) > 0 {
		decision.Decision = string(StopNeedsClarification)
		decision.ReasonCodes = append(
			decision.ReasonCodes,
			riskReasonEvidenceConflict,
		)
	}
	for _, claim := range verified.PartialClaims {
		if !claim.HighRisk {
			continue
		}
		decision.Decision = string(StopNeedsClarification)
		if len(decision.ReasonCodes) == 0 ||
			decision.ReasonCodes[len(decision.ReasonCodes)-1] !=
				riskReasonHighRiskPartialSupport {
			decision.ReasonCodes = append(
				decision.ReasonCodes,
				riskReasonHighRiskPartialSupport,
			)
		}
		decision.FindingIDs = append(
			decision.FindingIDs,
			claim.ProducerNodeID+":"+strconv.Itoa(claim.FindingIndex),
		)
	}
	return decision, nil
}
