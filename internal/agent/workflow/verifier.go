package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

type findingView struct {
	Claim      string                      `json:"claim"`
	GoalIDs    []string                    `json:"goal_ids"`
	Evidence   []findingEvidenceView `json:"evidence"`
	Confidence float64                     `json:"confidence"`
}

type findingEvidenceView struct {
	Kind      string                     `json:"kind"`
	Reference string                     `json:"reference"`
	Summary   string                     `json:"summary"`
	Identity  *agentapi.EvidenceIdentity `json:"identity,omitempty"`
}

type reportView struct {
	Findings        []findingView `json:"findings"`
	Gaps            []string                   `json:"gaps"`
	CoveredGoals    []string                   `json:"covered_goals"`
	UnresolvedGoals []string                   `json:"unresolved_goals"`
}

type claimSupport string

const (
	claimSupported   claimSupport = "supported"
	claimPartial     claimSupport = "partial"
	claimUnsupported claimSupport = "unsupported"
)

type verifiedClaimView struct {
	ProducerNodeID     string                      `json:"producer_node_id"`
	FindingIndex       int                         `json:"finding_index"`
	Claim              string                      `json:"claim"`
	GoalIDs            []string                    `json:"goal_ids"`
	Evidence           []findingEvidenceView `json:"evidence"`
	EvidenceIdentities []agentapi.EvidenceIdentity `json:"evidence_identities"`
	Confidence         float64                     `json:"confidence"`
	Support            claimSupport                `json:"support"`
	HighRisk           bool                        `json:"high_risk"`
}

type unsupportedClaimView struct {
	ProducerNodeID string       `json:"producer_node_id"`
	FindingIndex   int          `json:"finding_index"`
	GoalIDs        []string     `json:"goal_ids"`
	Support        claimSupport `json:"support"`
	HighRisk       bool         `json:"high_risk"`
	ReasonCode     string       `json:"reason_code"`
}

type verificationView struct {
	Decision   Completeness `json:"decision"`
	StopReason StopReason   `json:"stop_reason"`
}

type verifiedEvidenceView struct {
	SupportedClaims   []verifiedClaimView         `json:"supported_claims"`
	PartialClaims     []verifiedClaimView         `json:"partial_claims"`
	UnsupportedClaims []unsupportedClaimView      `json:"unsupported_claims"`
	PartialGoals      []string                    `json:"partial_goals"`
	UnresolvedGoals   []string                    `json:"unresolved_goals"`
	Limitations       []string                    `json:"limitations"`
	EvidenceUnits     []tool.EvidenceUnit         `json:"evidence_units"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts"`
	Verification      verificationView    `json:"verification"`
	Completeness      Completeness                `json:"completeness"`
}

type verificationRunInput struct {
	workflowRunID string
	node          NodeDefinition
	inputs        []Handoff
	maxBytes      int64
	schemas       *agentapi.SchemaRegistry
}

type verificationRunOutput struct {
	handoff             Handoff
	decision            string
	stopReason          StopReason
	supportedClaimCount int
	partialClaimCount   int
	unsupportedCount    int
	unresolvedGoalCount int
	conflictCount       int
}

var verificationTraceSpec = runtrace.Spec[
	verificationRunInput,
	verificationRunOutput,
]{
	Operation: "verification.completed",
	Node:      "verification.completed",
	Input: func(input verificationRunInput) map[string]any {
		requiredGoalCount := 0
		if input.node.Verifier != nil {
			requiredGoalCount = len(input.node.Verifier.RequiredGoals)
		}
		return map[string]any{
			"node_id":             input.node.ID,
			"input_count":         len(input.inputs),
			"required_goal_count": requiredGoalCount,
		}
	},
	Output: func(
		_ verificationRunInput,
		output verificationRunOutput,
		err error,
	) map[string]any {
		fields := map[string]any{
			"decision":                output.decision,
			"stop_reason":             output.stopReason,
			"supported_claim_count":   output.supportedClaimCount,
			"partial_claim_count":     output.partialClaimCount,
			"unsupported_claim_count": output.unsupportedCount,
			"unresolved_goal_count":   output.unresolvedGoalCount,
			"conflict_count":          output.conflictCount,
		}
		if err != nil {
			fields["error"] = err.Error()
		}
		return fields
	},
	Status: func(output verificationRunOutput, err error) string {
		if err != nil {
			return "failed"
		}
		if output.decision == string(Partial) ||
			output.decision == string(Unavailable) {
			return "degraded"
		}
		return "completed"
	},
}

func (orchestrator *Orchestrator) verifyEvidence(
	ctx context.Context,
	workflowRunID string,
	node NodeDefinition,
	inputs []Handoff,
	maxBytes int64,
) (Handoff, error) {
	ctx = runtrace.WithCorrelation(ctx, runtrace.Correlation{WorkflowNodeID: node.ID})
	output, err := runtrace.Invoke(
		ctx,
		verificationTraceSpec,
		verificationRunInput{
			workflowRunID: workflowRunID,
			node:          node,
			inputs:        inputs,
			maxBytes:      maxBytes,
			schemas:       orchestrator.schemas,
		},
		func(
			_ context.Context,
			input verificationRunInput,
		) (verificationRunOutput, error) {
			return verifyBundle(input)
		},
	)
	return output.handoff, err
}

func verifyBundle(
	input verificationRunInput,
) (verificationRunOutput, error) {
	if input.node.Verifier == nil {
		return verificationRunOutput{}, fmt.Errorf(
			"verifier node %q has no policy",
			input.node.ID,
		)
	}
	if len(input.inputs) != 1 {
		return verificationRunOutput{}, fmt.Errorf(
			"verifier node %q requires exactly one evidence bundle",
			input.node.ID,
		)
	}
	source := input.inputs[0]
	var ledger ledgerView
	if err := json.Unmarshal(source.Payload, &ledger); err != nil {
		return verificationRunOutput{}, fmt.Errorf(
			"decode verifier node %q evidence bundle: %w",
			input.node.ID,
			err,
		)
	}
	if input.node.Verifier.RejectEvidenceConflicts &&
		len(ledger.EvidenceConflicts) > 0 {
		return verificationRunOutput{
				decision:      "reject",
				stopReason:    StopVerificationFailed,
				conflictCount: len(ledger.EvidenceConflicts),
			}, rejectionError{
				nodeID: input.node.ID,
				count:  len(ledger.EvidenceConflicts),
			}
	}

	required := make(map[string]struct{}, len(input.node.Verifier.RequiredGoals))
	for _, goal := range input.node.Verifier.RequiredGoals {
		required[goal] = struct{}{}
	}
	highRisk := make(map[string]struct{}, len(input.node.Verifier.HighRiskGoals))
	for _, goal := range input.node.Verifier.HighRiskGoals {
		highRisk[goal] = struct{}{}
	}
	supported := make(map[string]struct{}, len(required))
	covered := make(map[string]struct{}, len(required))
	claims := make([]verifiedClaimView, 0)
	partialClaims := make([]verifiedClaimView, 0)
	unsupportedClaims := make([]unsupportedClaimView, 0)
	limitations := make([]string, 0)
	seenLimitations := make(map[string]struct{})
	evidenceIndex := newEvidenceIndex(source.EvidenceUnits)
	appendLimitation := func(value string) {
		if value == "" {
			return
		}
		if _, duplicate := seenLimitations[value]; duplicate {
			return
		}
		seenLimitations[value] = struct{}{}
		limitations = append(limitations, value)
	}

	for _, handoff := range ledger.Handoffs {
		var report reportView
		if err := json.Unmarshal(handoff.Payload, &report); err != nil {
			return verificationRunOutput{}, fmt.Errorf(
				"decode verifier input from %q: %w",
				handoff.ProducerNodeID,
				err,
			)
		}
		for index, finding := range report.Findings {
			boundEvidence, identities, supportFacts := evidenceIndex.bind(
				finding.Evidence,
			)
			findingHighRisk := hasTrackedGoal(finding.GoalIDs, highRisk)
			if len(identities) == 0 {
				unsupportedClaims = append(unsupportedClaims, unsupportedClaimView{
					ProducerNodeID: handoff.ProducerNodeID,
					FindingIndex:   index,
					GoalIDs:        append([]string(nil), finding.GoalIDs...),
					Support:        claimUnsupported,
					HighRisk:       findingHighRisk,
					ReasonCode:     "canonical_evidence_unbound",
				})
				appendLimitation(fmt.Sprintf(
					"Finding %d from investigation node %q was excluded because its evidence did not match the canonical ledger.",
					index,
					handoff.ProducerNodeID,
				))
				continue
			}
			if supportFacts.unboundCount > 0 {
				appendLimitation(fmt.Sprintf(
					"Finding %d from investigation node %q omitted %d evidence reference(s) that did not match the canonical ledger.",
					index,
					handoff.ProducerNodeID,
					supportFacts.unboundCount,
				))
			}
			support := claimSupported
			if supportFacts.incompleteCoverage ||
				findingHighRisk &&
					supportFacts.minimumTrustTier <
						input.node.Verifier.HighRiskMinimumTrustTier {
				support = claimPartial
				appendLimitation(fmt.Sprintf(
					"Finding %d from investigation node %q has partial evidence support.",
					index,
					handoff.ProducerNodeID,
				))
			}
			claim := verifiedClaimView{
				ProducerNodeID:     handoff.ProducerNodeID,
				FindingIndex:       index,
				Claim:              finding.Claim,
				GoalIDs:            append([]string(nil), finding.GoalIDs...),
				Evidence:           boundEvidence,
				EvidenceIdentities: identities,
				Confidence:         finding.Confidence,
				Support:            support,
				HighRisk:           findingHighRisk,
			}
			if support == claimSupported {
				claims = append(claims, claim)
			} else {
				partialClaims = append(partialClaims, claim)
			}
			for _, goal := range finding.GoalIDs {
				if _, tracked := required[goal]; tracked {
					covered[goal] = struct{}{}
					if support == claimSupported {
						supported[goal] = struct{}{}
					}
				}
			}
		}
		for _, gap := range report.Gaps {
			appendLimitation(gap)
		}
	}
	for _, task := range ledger.UnavailableTasks {
		appendLimitation(fmt.Sprintf(
			"Investigation task %q was unavailable.",
			task.ProducerNodeID,
		))
	}

	unresolved := make([]string, 0, len(required))
	partialGoals := make([]string, 0, len(required))
	for _, goal := range input.node.Verifier.RequiredGoals {
		if _, fullySupported := supported[goal]; fullySupported {
			continue
		}
		if _, partlyCovered := covered[goal]; partlyCovered {
			partialGoals = append(partialGoals, goal)
			appendLimitation(fmt.Sprintf(
				"Required evidence goal %q has only partial support.",
				goal,
			))
			continue
		}
		unresolved = append(unresolved, goal)
		appendLimitation(fmt.Sprintf(
			"Required evidence goal %q remains unresolved.",
			goal,
		))
	}
	completeness := verifiedCompleteness(
		len(required),
		len(required)-len(unresolved),
		len(supported),
		source.Completeness,
	)
	stopReason := stopForVerification(completeness, ledger)
	evidenceUnits := evidence.CloneUnits(source.EvidenceUnits)
	if evidenceUnits == nil {
		evidenceUnits = []tool.EvidenceUnit{}
	}
	evidenceConflicts := cloneConflicts(source.EvidenceConflicts)
	if evidenceConflicts == nil {
		evidenceConflicts = []agentapi.EvidenceConflict{}
	}
	payload, err := json.Marshal(verifiedEvidenceView{
		SupportedClaims:   claims,
		PartialClaims:     partialClaims,
		UnsupportedClaims: unsupportedClaims,
		PartialGoals:      partialGoals,
		UnresolvedGoals:   unresolved,
		Limitations:       limitations,
		EvidenceUnits:     evidenceUnits,
		EvidenceConflicts: evidenceConflicts,
		Verification: verificationView{
			Decision: completeness, StopReason: stopReason,
		},
		Completeness: completeness,
	})
	if err != nil {
		return verificationRunOutput{}, fmt.Errorf(
			"marshal verifier node %q evidence view: %w",
			input.node.ID,
			err,
		)
	}
	handoff, err := PrepareHandoff(Handoff{
		WorkflowRunID:     input.workflowRunID,
		ProducerNodeID:    input.node.ID,
		Schema:            input.node.OutputSchema,
		Payload:           payload,
		References:        append([]agentapi.Reference(nil), source.References...),
		EvidenceUnits:     evidenceUnits,
		EvidenceConflicts: evidenceConflicts,
		Completeness:      completeness,
	}, input.maxBytes, input.schemas)
	if err != nil {
		return verificationRunOutput{}, err
	}
	return verificationRunOutput{
		handoff:             handoff,
		decision:            string(completeness),
		stopReason:          stopReason,
		supportedClaimCount: len(claims),
		partialClaimCount:   len(partialClaims),
		unsupportedCount:    len(unsupportedClaims),
		unresolvedGoalCount: len(unresolved),
		conflictCount:       len(ledger.EvidenceConflicts),
	}, nil
}

func verifiedCompleteness(
	requiredGoalCount int,
	coveredGoalCount int,
	fullySupportedGoalCount int,
	inputCompleteness Completeness,
) Completeness {
	if requiredGoalCount == 0 {
		return inputCompleteness
	}
	switch {
	case fullySupportedGoalCount == requiredGoalCount:
		return Complete
	case coveredGoalCount == 0:
		return Unavailable
	default:
		return Partial
	}
}

func hasTrackedGoal(goals []string, tracked map[string]struct{}) bool {
	for _, goal := range goals {
		if _, ok := tracked[goal]; ok {
			return true
		}
	}
	return false
}

func stopForCompleteness(completeness Completeness) StopReason {
	switch completeness {
	case Complete:
		return StopRequiredGoalsCovered
	default:
		return StopCapabilityUnavailable
	}
}

func stopForVerification(
	completeness Completeness,
	ledger ledgerView,
) StopReason {
	if completeness == Complete {
		return StopRequiredGoalsCovered
	}
	if reason := unavailableStopReason(ledger.UnavailableTasks); reason != "" {
		return reason
	}
	if ledger.Convergence == nil {
		return StopCapabilityUnavailable
	}
	if ledger.Convergence.NewIdentityCount == 0 {
		return StopNoNewEvidence
	}
	if ledger.Convergence.MaxDuplicateRatio > 0 &&
		ledger.Convergence.DuplicateRatio > ledger.Convergence.MaxDuplicateRatio {
		return StopDuplicateEvidence
	}
	return StopCapabilityUnavailable
}

func unavailableStopReason(
	tasks []unavailableTaskView,
) StopReason {
	priority := map[StopReason]int{
		StopNeedsClarification:    4,
		StopNoAffordableTask:      3,
		StopBudgetExhausted:       2,
		StopCapabilityUnavailable: 1,
	}
	var selected StopReason
	for _, task := range tasks {
		reason := task.StopReason
		if reason == "" {
			reason = StopCapabilityUnavailable
		}
		if priority[reason] > priority[selected] {
			selected = reason
		}
	}
	return selected
}

type verificationIndex struct {
	byIdentity  map[evidence.Key]evidenceMatch
	byReference map[string][]evidenceMatch
}

type evidenceMatch struct {
	identity  agentapi.EvidenceIdentity
	coverage  tool.EvidenceCoverage
	trustTier int
}

type supportFacts struct {
	unboundCount       int
	incompleteCoverage bool
	minimumTrustTier   int
}

func newEvidenceIndex(
	units []tool.EvidenceUnit,
) verificationIndex {
	expanded := evidence.Expand(units)
	index := verificationIndex{
		byIdentity:  make(map[evidence.Key]evidenceMatch, len(expanded)),
		byReference: make(map[string][]evidenceMatch, len(expanded)),
	}
	for _, unit := range expanded {
		key, ok := evidence.UnitKey(unit)
		if !ok {
			continue
		}
		if _, duplicate := index.byIdentity[key]; duplicate {
			continue
		}
		identity := identityFromKey(key)
		match := evidenceMatch{
			identity: identity, coverage: unit.Coverage, trustTier: unit.TrustTier,
		}
		index.byIdentity[key] = match
		referenceKey := referenceKey(key.SourceKind, key.Target)
		index.byReference[referenceKey] = append(
			index.byReference[referenceKey],
			match,
		)
	}
	return index
}

func (index verificationIndex) bind(
	items []findingEvidenceView,
) (
	[]findingEvidenceView,
	[]agentapi.EvidenceIdentity,
	supportFacts,
) {
	bound := make([]findingEvidenceView, 0, len(items))
	identities := make([]agentapi.EvidenceIdentity, 0, len(items))
	seen := make(map[evidence.Key]struct{}, len(items))
	facts := supportFacts{minimumTrustTier: 101}
	for _, item := range items {
		matches := index.match(item)
		if len(matches) == 0 {
			facts.unboundCount++
			facts.incompleteCoverage = true
			continue
		}
		bound = append(bound, cloneFindingEvidence(item))
		for _, match := range matches {
			key := keyFromIdentity(match.identity)
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			identities = append(identities, match.identity)
			if !match.coverage.Complete || match.coverage.Partial ||
				match.coverage.OmittedItems > 0 ||
				match.coverage.NextCursor != "" {
				facts.incompleteCoverage = true
			}
			if match.trustTier < facts.minimumTrustTier {
				facts.minimumTrustTier = match.trustTier
			}
		}
	}
	if len(identities) == 0 {
		facts.minimumTrustTier = 0
	}
	return bound, identities, facts
}

func (index verificationIndex) match(
	item findingEvidenceView,
) []evidenceMatch {
	if item.Identity != nil {
		match, ok := index.byIdentity[keyFromIdentity(*item.Identity)]
		if !ok {
			return nil
		}
		return []evidenceMatch{match}
	}
	return index.byReference[referenceKey(item.Kind, item.Reference)]
}

func referenceKey(kind, reference string) string {
	return kind + "\x00" + reference
}

func keyFromIdentity(identity agentapi.EvidenceIdentity) evidence.Key {
	return evidence.Key{
		SourceKind: identity.SourceKind,
		Target:     identity.Target,
		Section:    identity.Section,
		Version:    identity.Version,
		TimeRange:  identity.TimeRange,
	}
}

func identityFromKey(key evidence.Key) agentapi.EvidenceIdentity {
	return agentapi.EvidenceIdentity{
		SourceKind: key.SourceKind,
		Target:     key.Target,
		Section:    key.Section,
		Version:    key.Version,
		TimeRange:  key.TimeRange,
	}
}

func cloneFindingEvidence(
	item findingEvidenceView,
) findingEvidenceView {
	if item.Identity == nil {
		return item
	}
	identity := *item.Identity
	item.Identity = &identity
	return item
}

type rejectionError struct {
	nodeID string
	count  int
}

func (err rejectionError) Error() string {
	return fmt.Sprintf(
		"verifier %q rejected %d evidence conflict(s)",
		err.nodeID,
		err.count,
	)
}

func (err rejectionError) Is(target error) bool {
	return target == ErrEvidenceConflict
}
