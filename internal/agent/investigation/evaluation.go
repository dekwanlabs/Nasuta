package investigation

import (
	"fmt"
	"strings"
)

// EvaluationResult is a deterministic regression gate for one delivered run. It
// checks delivery text, evidence traceability, and required-goal coverage.
type EvaluationResult struct {
	RunID                string
	NonEmptyDelivery     bool
	ClaimsTraceable      bool
	RequiredGoalsCovered bool
	Failures             []string
}

// EvaluateDelivery applies the P0 non-empty and traceability invariants to one
// delivery snapshot. It is deliberately provider-independent.
func EvaluateDelivery(run InvestigationRun) EvaluationResult {
	result := EvaluationResult{RunID: run.ID}
	if run.Delivery == nil {
		result.Failures = append(result.Failures, "delivery is missing")
		return result
	}
	result.NonEmptyDelivery = strings.TrimSpace(run.Delivery.Text) != ""
	if !result.NonEmptyDelivery {
		result.Failures = append(result.Failures, "delivery text is empty")
	}
	evidenceIDs := make(map[string]struct{}, len(run.Report.Evidence))
	for _, unit := range run.Report.Evidence {
		evidenceIDs[unit.ID] = struct{}{}
	}
	result.ClaimsTraceable = true
	for _, claim := range run.Report.Claims {
		if claim.Status == ClaimSupported && len(claim.EvidenceRefs) == 0 {
			result.ClaimsTraceable = false
			result.Failures = append(result.Failures, fmt.Sprintf("claim %q has no evidence", claim.ID))
		}
		for _, ref := range claim.EvidenceRefs {
			if _, ok := evidenceIDs[ref.EvidenceID]; !ok {
				result.ClaimsTraceable = false
				result.Failures = append(result.Failures, fmt.Sprintf("claim %q references missing evidence %q", claim.ID, ref.EvidenceID))
			}
		}
	}
	result.RequiredGoalsCovered = true
	coverageByGoal := make(map[string]GoalCoverageStatus, len(run.Report.Coverage))
	for _, coverage := range run.Report.Coverage {
		coverageByGoal[coverage.GoalID] = coverage.Status
	}
	for _, goal := range run.Contract.EvidenceGoals {
		if goal.Required && coverageByGoal[goal.ID] != GoalCovered {
			result.RequiredGoalsCovered = false
			result.Failures = append(result.Failures, fmt.Sprintf("required goal %q is not covered", goal.ID))
		}
	}
	return result
}

// Pass reports whether all deterministic invariants held.
func (result EvaluationResult) Pass() bool {
	return result.NonEmptyDelivery && result.ClaimsTraceable && result.RequiredGoalsCovered
}
