package delivery

import (
	"fmt"
	"sort"
	"time"
)

const (
	reasonRequiredAssignmentIncomplete = "required_assignment_incomplete"
	reasonOptionalReviewerIncomplete   = "optional_reviewer_incomplete"
	reasonSubjectMismatch              = "subject_hash_mismatch"
	reasonCoverageIncomplete           = "required_category_coverage_incomplete"
	reasonBlockingFinding              = "blocking_finding"
	reasonUnsupportedBlockingFinding   = "unsupported_blocking_finding"
	reasonFindingSeverityConflict      = "finding_severity_conflict"
	reasonValidationFailed             = "validation_failed"
	reasonValidationNotConfigured      = "validation_not_configured"
)

type findingGateGroup struct {
	ids         []string
	blocking    bool
	nonBlocking bool
}

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
	adjudications := append([]ReviewAdjudication(nil), evaluation.Adjudications...)
	sort.Slice(assignments, func(i, j int) bool { return assignments[i].ID < assignments[j].ID })
	sort.Slice(reports, func(i, j int) bool { return reports[i].AssignmentID < reports[j].AssignmentID })
	sort.Slice(adjudications, func(i, j int) bool { return adjudications[i].Fingerprint < adjudications[j].Fingerprint })

	reportByAssignment := make(map[string]ReviewReport, len(reports))
	reportHashes := make([]string, 0, len(reports))
	for _, report := range reports {
		if report.RoundID != round.ID || report.SubjectHash != round.Subject.ContentHash {
			return newGateResult(
				round, policy, GateFailed, []string{reasonSubjectMismatch},
				nil, nil, nil, reportHashes, nil, evaluatedAt,
			)
		}
		if _, duplicate := reportByAssignment[report.AssignmentID]; duplicate {
			return ReviewGateResult{}, fmt.Errorf("duplicate report for assignment %q: %w", report.AssignmentID, ErrConflict)
		}
		reportByAssignment[report.AssignmentID] = report
		reportHashes = append(reportHashes, report.ContentHash)
	}
	sort.Strings(reportHashes)

	reasons := canonicalStrings(evaluation.SubjectReasonCodes)
	coverage := make(map[string]struct{}, len(policy.RequiredCategories))
	blocking := canonicalStrings(evaluation.SubjectBlockingIDs)
	unsupported := make([]string, 0)
	findingGroups := make(map[string]*findingGateGroup)
	resolved := activeResolutions(evaluation.Resolutions, round.Subject.ContentHash, evaluatedAt)
	blockingSeverities := make(map[Severity]struct{}, len(policy.BlockingSeverities))
	for _, severity := range policy.BlockingSeverities {
		blockingSeverities[severity] = struct{}{}
	}
	assignmentByReviewer := make(map[string]struct{}, len(assignments))
	optionalReviewerGaps := make([]string, 0)

	for _, assignment := range assignments {
		assignmentByReviewer[assignment.ReviewerID] = struct{}{}
		report, hasReport := reportByAssignment[assignment.ID]
		if assignment.Required && (!successfulReviewAssignment(assignment.Status) || !hasReport) {
			reasons = appendUnique(reasons, reasonRequiredAssignmentIncomplete)
			continue
		}
		if !successfulReviewAssignment(assignment.Status) || !hasReport {
			optionalReviewerGaps = append(
				optionalReviewerGaps,
				"reviewer:"+assignment.ReviewerID,
			)
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
			_, blocks := blockingSeverities[finding.Severity]
			supported := findingSupported(finding)
			if finding.Fingerprint != "" && supported {
				group := findingGroups[finding.Fingerprint]
				if group == nil {
					group = &findingGateGroup{}
					findingGroups[finding.Fingerprint] = group
				}
				group.ids = append(group.ids, finding.ID)
				group.blocking = group.blocking || blocks
				group.nonBlocking = group.nonBlocking || !blocks
			}
			if !blocks {
				continue
			}
			if supported {
				blocking = append(blocking, finding.ID)
			} else {
				unsupported = append(unsupported, finding.ID)
			}
		}
	}
	for _, reviewer := range policy.Reviewers {
		if _, assigned := assignmentByReviewer[reviewer.ID]; assigned {
			continue
		}
		if reviewer.Required {
			reasons = appendUnique(reasons, reasonRequiredAssignmentIncomplete)
			continue
		}
		optionalReviewerGaps = appendUnique(
			optionalReviewerGaps,
			"reviewer:"+reviewer.ID,
		)
	}

	gaps := append([]string(nil), evaluation.SubjectCoverageGaps...)
	for _, category := range policy.RequiredCategories {
		if _, covered := coverage[category]; !covered {
			gaps = append(gaps, category)
		}
	}
	gaps = append(gaps, optionalReviewerGaps...)
	gaps = canonicalStrings(gaps)
	conflicts := make([]string, 0)
	adjudicationHashes := make([]string, 0, len(adjudications))
	adjudicationByFingerprint := make(map[string]ReviewAdjudication, len(adjudications))
	for _, adjudication := range adjudications {
		prepared, err := PrepareReviewAdjudication(adjudication)
		if err != nil {
			return ReviewGateResult{}, err
		}
		group := findingGroups[prepared.Fingerprint]
		if policy.Adjudicator == nil || group == nil ||
			!group.blocking || !group.nonBlocking ||
			prepared.RoundID != round.ID ||
			prepared.SubjectHash != round.Subject.ContentHash ||
			prepared.PolicyHash != policy.ContentHash ||
			prepared.Agent != policy.Adjudicator.Agent ||
			prepared.DefinitionHash != policy.Adjudicator.DefinitionHash ||
			!equalCanonicalStrings(prepared.FindingIDs, group.ids) {
			return ReviewGateResult{}, fmt.Errorf("review adjudication does not match its conflict group: %w", ErrConflict)
		}
		if _, duplicate := adjudicationByFingerprint[prepared.Fingerprint]; duplicate {
			return ReviewGateResult{}, fmt.Errorf("duplicate adjudication for fingerprint %q: %w", prepared.Fingerprint, ErrConflict)
		}
		adjudicationByFingerprint[prepared.Fingerprint] = prepared
		adjudicationHashes = append(adjudicationHashes, prepared.ContentHash)
	}
	sort.Strings(adjudicationHashes)
	for fingerprint, group := range findingGroups {
		if group.blocking && group.nonBlocking {
			if adjudication, ok := adjudicationByFingerprint[fingerprint]; ok &&
				adjudication.Decision == AdjudicationConfirmed {
				continue
			}
			conflicts = append(conflicts, group.ids...)
		}
	}
	conflicts = canonicalStrings(conflicts)
	sort.Strings(blocking)
	sort.Strings(unsupported)
	sort.Strings(gaps)

	switch {
	case containsString(reasons, reasonRequiredAssignmentIncomplete):
		return newGateResult(
			round, policy, GateIncomplete, reasons, append(blocking, unsupported...),
			nil, gaps, reportHashes, adjudicationHashes, evaluatedAt,
		)
	case len(optionalReviewerGaps) > 0 &&
		policy.OptionalReviewerAction == OptionalReviewerHumanRequired:
		reasons = appendUnique(reasons, reasonOptionalReviewerIncomplete)
		return newGateResult(
			round, policy, GateHumanRequired, reasons, append(blocking, unsupported...),
			nil, gaps, reportHashes, adjudicationHashes, evaluatedAt,
		)
	case len(gaps) > 0:
		if len(optionalReviewerGaps) > 0 {
			reasons = appendUnique(reasons, reasonOptionalReviewerIncomplete)
		}
		if len(gaps) > len(optionalReviewerGaps) {
			reasons = appendUnique(reasons, reasonCoverageIncomplete)
		}
		return newGateResult(
			round, policy, GateIncomplete, reasons, append(blocking, unsupported...),
			nil, gaps, reportHashes, adjudicationHashes, evaluatedAt,
		)
	case len(unsupported) > 0:
		reasons = append(reasons, reasonUnsupportedBlockingFinding)
		return newGateResult(
			round, policy, GateHumanRequired, reasons, unsupported,
			nil, nil, reportHashes, adjudicationHashes, evaluatedAt,
		)
	case len(conflicts) > 0:
		reasons = append(reasons, reasonFindingSeverityConflict)
		return newGateResult(
			round, policy, GateHumanRequired, reasons, blocking,
			conflicts, nil, reportHashes, adjudicationHashes, evaluatedAt,
		)
	case len(blocking) > 0:
		reasons = append(reasons, reasonBlockingFinding)
		return newGateResult(
			round, policy, GateRevise, reasons, blocking,
			nil, nil, reportHashes, adjudicationHashes, evaluatedAt,
		)
	default:
		return newGateResult(
			round, policy, GatePass, reasons, nil,
			nil, nil, reportHashes, adjudicationHashes, evaluatedAt,
		)
	}
}

func newGateResult(
	round ReviewRound,
	policy ReviewPolicy,
	decision GateDecision,
	reasons, blocking, conflicts, gaps, reportHashes, adjudicationHashes []string,
	evaluatedAt time.Time,
) (ReviewGateResult, error) {
	result := ReviewGateResult{
		RoundID: round.ID, SubjectHash: round.Subject.ContentHash, Decision: decision,
		ReasonCodes: canonicalStrings(reasons), BlockingIDs: canonicalStrings(blocking),
		ConflictIDs: canonicalStrings(conflicts), CoverageGaps: canonicalStrings(gaps),
		PolicyHash: policy.ContentHash, ReportHashes: canonicalStrings(reportHashes),
		AdjudicationHashes: canonicalStrings(adjudicationHashes),
		CreatedAt:          evaluatedAt,
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

func equalCanonicalStrings(left, right []string) bool {
	left = canonicalStrings(left)
	right = canonicalStrings(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func activeResolutions(resolutions []FindingResolution, subjectHash string, at time.Time) map[string]struct{} {
	latest := latestFindingResolutions(resolutions, subjectHash)
	active := make(map[string]struct{}, len(latest))
	for findingID, resolution := range latest {
		if resolution.ExpiresAt != nil && !resolution.ExpiresAt.After(at) {
			continue
		}
		switch resolution.Resolution {
		case ResolutionFixed, ResolutionWaived, ResolutionInvalidated, ResolutionSuperseded:
			active[findingID] = struct{}{}
		}
	}
	return active
}

func latestFindingResolutions(
	resolutions []FindingResolution,
	subjectHash string,
) map[string]FindingResolution {
	latest := make(map[string]FindingResolution, len(resolutions))
	for _, resolution := range resolutions {
		if resolution.SubjectHash != subjectHash || resolution.FindingID == "" {
			continue
		}
		current, ok := latest[resolution.FindingID]
		if ok && (resolution.CreatedAt.Before(current.CreatedAt) ||
			(resolution.CreatedAt.Equal(current.CreatedAt) && resolution.ID <= current.ID)) {
			continue
		}
		latest[resolution.FindingID] = resolution
	}
	return latest
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
