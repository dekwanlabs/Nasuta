package featuredelivery

import (
	"fmt"
	"sort"
	"time"
)

const (
	reasonRequiredAssignmentIncomplete = "required_assignment_incomplete"
	reasonSubjectMismatch              = "subject_hash_mismatch"
	reasonCoverageIncomplete           = "required_category_coverage_incomplete"
	reasonBlockingFinding              = "blocking_finding"
	reasonUnsupportedBlockingFinding   = "unsupported_blocking_finding"
)

// EvaluateReviewGate derives one stable result from immutable review facts.
func EvaluateReviewGate(evaluation ReviewEvaluation, evaluatedAt time.Time) (ReviewGateResult, error) {
	round := evaluation.Round
	policy, err := PrepareReviewPolicy(evaluation.Policy)
	if err != nil {
		return ReviewGateResult{}, err
	}
	if round.Status != RoundEvaluating || round.Subject.ContentHash == "" ||
		round.PolicyID != policy.ID || round.PolicyVersion != policy.Version ||
		round.PolicyHash != policy.ContentHash || round.Subject.Kind != policy.SubjectKind {
		return ReviewGateResult{}, fmt.Errorf("review round and policy do not match: %w", ErrConflict)
	}

	assignments := append([]ReviewAssignment(nil), evaluation.Assignments...)
	reports := append([]ReviewReport(nil), evaluation.Reports...)
	sort.Slice(assignments, func(i, j int) bool { return assignments[i].ID < assignments[j].ID })
	sort.Slice(reports, func(i, j int) bool { return reports[i].AssignmentID < reports[j].AssignmentID })

	reportByAssignment := make(map[string]ReviewReport, len(reports))
	reportHashes := make([]string, 0, len(reports))
	for _, report := range reports {
		if report.RoundID != round.ID || report.SubjectHash != round.Subject.ContentHash {
			return newGateResult(round, policy, GateFailed, []string{reasonSubjectMismatch}, nil, nil, nil, reportHashes, evaluatedAt)
		}
		if _, duplicate := reportByAssignment[report.AssignmentID]; duplicate {
			return ReviewGateResult{}, fmt.Errorf("duplicate report for assignment %q: %w", report.AssignmentID, ErrConflict)
		}
		reportByAssignment[report.AssignmentID] = report
		reportHashes = append(reportHashes, report.ContentHash)
	}
	sort.Strings(reportHashes)

	reasons := make([]string, 0, 3)
	coverage := make(map[string]struct{}, len(policy.RequiredCategories))
	blocking := make([]string, 0)
	unsupported := make([]string, 0)
	resolved := activeResolutions(evaluation.Resolutions, round.Subject.ContentHash, evaluatedAt)
	blockingSeverities := make(map[Severity]struct{}, len(policy.BlockingSeverities))
	for _, severity := range policy.BlockingSeverities {
		blockingSeverities[severity] = struct{}{}
	}

	for _, assignment := range assignments {
		report, hasReport := reportByAssignment[assignment.ID]
		if assignment.Required && (assignment.Status != AssignmentSucceeded || !hasReport) {
			reasons = appendUnique(reasons, reasonRequiredAssignmentIncomplete)
			continue
		}
		if assignment.Status != AssignmentSucceeded || !hasReport {
			continue
		}
		for _, item := range report.Coverage {
			if item.Covered {
				coverage[item.Category] = struct{}{}
			}
		}
		for _, finding := range report.Findings {
			if _, done := resolved[finding.ID]; done {
				continue
			}
			if _, blocks := blockingSeverities[finding.Severity]; !blocks {
				continue
			}
			if findingSupported(finding) {
				blocking = append(blocking, finding.ID)
			} else {
				unsupported = append(unsupported, finding.ID)
			}
		}
	}

	gaps := make([]string, 0)
	for _, category := range policy.RequiredCategories {
		if _, covered := coverage[category]; !covered {
			gaps = append(gaps, category)
		}
	}
	sort.Strings(blocking)
	sort.Strings(unsupported)
	sort.Strings(gaps)

	switch {
	case containsString(reasons, reasonRequiredAssignmentIncomplete):
		return newGateResult(round, policy, GateIncomplete, reasons, append(blocking, unsupported...), nil, gaps, reportHashes, evaluatedAt)
	case len(gaps) > 0:
		reasons = append(reasons, reasonCoverageIncomplete)
		return newGateResult(round, policy, GateIncomplete, reasons, append(blocking, unsupported...), nil, gaps, reportHashes, evaluatedAt)
	case len(unsupported) > 0:
		reasons = append(reasons, reasonUnsupportedBlockingFinding)
		return newGateResult(round, policy, GateHumanRequired, reasons, unsupported, nil, nil, reportHashes, evaluatedAt)
	case len(blocking) > 0:
		reasons = append(reasons, reasonBlockingFinding)
		return newGateResult(round, policy, GateRevise, reasons, blocking, nil, nil, reportHashes, evaluatedAt)
	default:
		return newGateResult(round, policy, GatePass, reasons, nil, nil, nil, reportHashes, evaluatedAt)
	}
}

func newGateResult(
	round ReviewRound,
	policy ReviewPolicy,
	decision GateDecision,
	reasons, blocking, conflicts, gaps, reportHashes []string,
	evaluatedAt time.Time,
) (ReviewGateResult, error) {
	result := ReviewGateResult{
		RoundID: round.ID, SubjectHash: round.Subject.ContentHash, Decision: decision,
		ReasonCodes: canonicalStrings(reasons), BlockingIDs: canonicalStrings(blocking),
		ConflictIDs: canonicalStrings(conflicts), CoverageGaps: canonicalStrings(gaps),
		PolicyHash: policy.ContentHash, ReportHashes: canonicalStrings(reportHashes),
		CreatedAt: evaluatedAt,
	}
	hashInput := result
	hashInput.ID = ""
	hashInput.ContentHash = ""
	hashInput.CreatedAt = time.Time{}
	hash, err := hashJSON(hashInput)
	if err != nil {
		return ReviewGateResult{}, fmt.Errorf("hash review gate result: %w", err)
	}
	result.ContentHash = hash
	result.ID = "gate_" + hash[:24]
	return result, nil
}

func activeResolutions(resolutions []FindingResolution, subjectHash string, at time.Time) map[string]struct{} {
	active := make(map[string]struct{}, len(resolutions))
	for _, resolution := range resolutions {
		if resolution.SubjectHash != subjectHash || resolution.FindingID == "" {
			continue
		}
		if resolution.ExpiresAt != nil && !resolution.ExpiresAt.After(at) {
			continue
		}
		switch resolution.Resolution {
		case ResolutionWaived, ResolutionInvalidated:
			active[resolution.FindingID] = struct{}{}
		}
	}
	return active
}

func findingSupported(finding Finding) bool {
	if len(finding.Evidence) == 0 {
		return false
	}
	for _, evidence := range finding.Evidence {
		if evidence.Ref == "" || evidence.Hash == "" {
			return false
		}
	}
	return true
}

func appendUnique(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	return append(values, value)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
