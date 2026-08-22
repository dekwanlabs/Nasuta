package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/tool"
)

type findingView struct {
	Claim      string                `json:"claim"`
	EntityIDs  []string              `json:"entity_ids,omitempty"`
	GoalIDs    []string              `json:"goal_ids"`
	Evidence   []findingEvidenceView `json:"evidence"`
	Confidence float64               `json:"confidence"`
}

type findingEvidenceView struct {
	Kind       string                     `json:"kind"`
	Reference  string                     `json:"reference"`
	Summary    string                     `json:"summary"`
	EvidenceID string                     `json:"evidence_id,omitempty"`
	Identity   *agentapi.EvidenceIdentity `json:"identity,omitempty"`
}

type reportView struct {
	Findings        []findingView `json:"findings"`
	Gaps            []string      `json:"gaps"`
	CoveredGoals    []string      `json:"covered_goals"`
	UnresolvedGoals []string      `json:"unresolved_goals"`
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
	EntityIDs          []string                    `json:"-"`
	Evidence           []findingEvidenceView       `json:"evidence"`
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

type subjectCoverageView struct {
	EntityID      string   `json:"entity_id"`
	CoveredFacets []string `json:"covered_facets"`
	MissingFacets []string `json:"missing_facets"`
	Sources       []string `json:"sources"`
	Complete      bool     `json:"complete"`
}

type omissionView struct {
	Claims            int `json:"claims"`
	Goals             int `json:"goals"`
	Limitations       int `json:"limitations"`
	EvidenceUnits     int `json:"evidence_units"`
	EvidenceConflicts int `json:"evidence_conflicts"`
}

type verifiedEvidenceView struct {
	SupportedClaims   []verifiedClaimView         `json:"supported_claims"`
	PartialClaims     []verifiedClaimView         `json:"partial_claims"`
	UnsupportedClaims []unsupportedClaimView      `json:"unsupported_claims"`
	PartialGoals      []string                    `json:"partial_goals"`
	UnresolvedGoals   []string                    `json:"unresolved_goals"`
	Limitations       []string                    `json:"limitations"`
	LimitationsDetail *limitationsDetailRef       `json:"limitations_detail,omitempty"`
	EvidenceUnits     []tool.EvidenceUnit         `json:"evidence_units"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts"`
	SubjectCoverage   []subjectCoverageView       `json:"subject_coverage,omitempty"`
	Verification      verificationView            `json:"verification"`
	Completeness      Completeness                `json:"completeness"`
	// Omissions makes payload compaction visible to the synthesizer.
	Omissions omissionView `json:"omissions"`
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

// verifyEvidence verifies the findings joined from completed investigation nodes.
// The verifier applies the Workflow byte budget before preparing the handoff.
// Returned payloads have already passed the declared output Schema.
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

// verifyBundle binds reported claims back to canonical evidence identities.
// Unsupported or conflicting claims remain visible as verification outcomes.
// Only surviving evidence can contribute to the verified handoff.
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
	rawLimitations := make([]rawLimitation, 0)
	rawLimitationIndex := 0
	appendLegacyLimitation := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, duplicate := seenLimitations[value]; duplicate {
			return
		}
		seenLimitations[value] = struct{}{}
		limitations = append(limitations, value)
	}
	appendLimitation := func(value string) {
		appendLegacyLimitation(value)
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		rawLimitations = append(rawLimitations, rawLimitation{
			Text: value, FirstSeen: rawLimitationIndex,
		})
		rawLimitationIndex++
	}
	appendLimitationFor := func(value, producerNodeID string, evidenceRefs []string) {
		appendLegacyLimitation(value)
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		rawLimitations = append(rawLimitations, rawLimitation{
			Text: value, ProducerNodeIDs: []string{producerNodeID},
			EvidenceRefs: append([]string(nil), evidenceRefs...), FirstSeen: rawLimitationIndex,
		})
		rawLimitationIndex++
	}
	evidenceIndex := newEvidenceIndex(source.EvidenceUnits)

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
				appendLimitationFor(fmt.Sprintf(
					"Finding %d from investigation node %q was excluded because its evidence did not match the canonical ledger.",
					index,
					handoff.ProducerNodeID,
				), handoff.ProducerNodeID, nil)
				continue
			}
			if supportFacts.unboundCount > 0 {
				appendLimitationFor(fmt.Sprintf(
					"Finding %d from investigation node %q omitted %d evidence reference(s) that did not match the canonical ledger.",
					index,
					handoff.ProducerNodeID,
					supportFacts.unboundCount,
				), handoff.ProducerNodeID, evidenceReferences(boundEvidence))
			}
			support := claimSupported
			if supportFacts.incompleteCoverage ||
				findingHighRisk &&
					supportFacts.minimumTrustTier <
						input.node.Verifier.HighRiskMinimumTrustTier {
				support = claimPartial
				appendLimitationFor(fmt.Sprintf(
					"Finding %d from investigation node %q has partial evidence support.",
					index,
					handoff.ProducerNodeID,
				), handoff.ProducerNodeID, evidenceReferences(boundEvidence))
			}
			claim := verifiedClaimView{
				ProducerNodeID:     handoff.ProducerNodeID,
				FindingIndex:       index,
				Claim:              finding.Claim,
				GoalIDs:            append([]string(nil), finding.GoalIDs...),
				EntityIDs:          append([]string(nil), finding.EntityIDs...),
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
			appendLimitationFor(gap, handoff.ProducerNodeID, nil)
		}
	}
	for _, task := range ledger.UnavailableTasks {
		appendLimitationFor(fmt.Sprintf(
			"Investigation task %q was unavailable.",
			task.ProducerNodeID,
		), task.ProducerNodeID, nil)
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
	subjectCoverage, subjectEvidenceInsufficient := verifySubjectCoverage(
		input.node.Verifier.SubjectRequirements,
		claims,
		partialClaims,
		appendLimitation,
	)
	completeness := verifiedCompleteness(
		len(required),
		len(required)-len(unresolved),
		len(supported),
		source.Completeness,
	)
	completeness = subjectCompleteness(completeness, subjectCoverage)
	stopReason := stopForVerification(
		completeness,
		ledger,
		subjectEvidenceInsufficient,
	)
	stopReason = compatibleVerificationStopReason(
		input.node.OutputSchema,
		stopReason,
	)
	evidenceUnits := evidence.CloneUnits(source.EvidenceUnits)
	if evidenceUnits == nil {
		evidenceUnits = []tool.EvidenceUnit{}
	}
	evidenceConflicts := cloneConflicts(source.EvidenceConflicts)
	if evidenceConflicts == nil {
		evidenceConflicts = []agentapi.EvidenceConflict{}
	}
	var limitationsDetail *limitationsDetailRef
	var artifacts []WorkflowArtifact
	if input.node.OutputSchema.Version >= 2 {
		normalized, err := normalizeLimitations(input.workflowRunID, rawLimitations)
		if err != nil {
			return verificationRunOutput{}, err
		}
		limitations = normalized.Primary
		limitationsDetail = &normalized.Ref
		artifacts = []WorkflowArtifact{normalized.Detail}
	}
	view := verifiedEvidenceView{
		SupportedClaims:   claims,
		PartialClaims:     partialClaims,
		UnsupportedClaims: unsupportedClaims,
		PartialGoals:      partialGoals,
		UnresolvedGoals:   unresolved,
		Limitations:       limitations,
		LimitationsDetail: limitationsDetail,
		EvidenceUnits:     evidenceUnits,
		EvidenceConflicts: evidenceConflicts,
		SubjectCoverage:   subjectCoverage,
		Verification: verificationView{
			Decision: completeness, StopReason: stopReason,
		},
		Completeness: completeness,
	}
	view, err := trimVerifiedEvidence(
		view,
		input.node.Verifier.MaxPayloadTokens,
		required,
		evidenceIdentityKeySet(ledger.BaselineEvidenceIdentities),
	)
	if err != nil {
		return verificationRunOutput{}, fmt.Errorf(
			"bound verifier node %q evidence view: %w",
			input.node.ID,
			err,
		)
	}
	payload, err := json.Marshal(view)
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
		Artifacts:         artifacts,
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

type verifiedSlotKind uint8

const (
	verifiedSupportedSlot verifiedSlotKind = iota
	verifiedPartialSlot
	verifiedUnsupportedSlot
	verifiedPartialGoalSlot
	verifiedUnresolvedGoalSlot
	verifiedLimitationSlot
	verifiedConflictSlot
)

type verifiedSlot struct {
	kind  verifiedSlotKind
	index int
}

// trimVerifiedEvidence keeps the highest-value verified data within one payload budget.
// It preserves source order inside each category and leaves the full ledger untouched.
// A deterministic omission ledger tells synthesis which verified data was excluded.
func trimVerifiedEvidence(
	view verifiedEvidenceView,
	maxTokens int,
	required map[string]struct{},
	protectedEvidence map[evidence.Key]struct{},
) (verifiedEvidenceView, error) {
	if maxTokens <= 0 {
		return view, nil
	}
	view.Omissions = omissionView{}
	if verifiedViewTokens(view) <= maxTokens {
		return view, nil
	}
	slots := verifiedSlots(view, required)
	minimum := viewAtVerifiedSlot(view, slots, 0, protectedEvidence)
	if verifiedViewTokens(minimum) > maxTokens {
		return verifiedEvidenceView{}, fmt.Errorf(
			"minimum verified evidence view exceeds %d tokens",
			maxTokens,
		)
	}
	low, high := 0, len(slots)
	for low < high {
		middle := low + (high-low+1)/2
		candidate := viewAtVerifiedSlot(
			view,
			slots,
			middle,
			protectedEvidence,
		)
		if verifiedViewTokens(candidate) <= maxTokens {
			low = middle
			continue
		}
		high = middle - 1
	}
	return viewAtVerifiedSlot(view, slots, low, protectedEvidence), nil
}

func verifiedSlots(
	view verifiedEvidenceView,
	required map[string]struct{},
) []verifiedSlot {
	slots := make([]verifiedSlot, 0,
		len(view.SupportedClaims)+len(view.PartialClaims)+
			len(view.UnsupportedClaims)+len(view.PartialGoals)+
			len(view.UnresolvedGoals)+len(view.Limitations)+
			len(view.EvidenceConflicts),
	)
	for index, claim := range view.SupportedClaims {
		if claim.HighRisk || claimHasGoal(claim, required) {
			slots = append(slots, verifiedSlot{
				kind: verifiedSupportedSlot, index: index,
			})
		}
	}
	for index, claim := range view.SupportedClaims {
		if claim.HighRisk || claimHasGoal(claim, required) {
			continue
		}
		slots = append(slots, verifiedSlot{
			kind: verifiedSupportedSlot, index: index,
		})
	}
	for index := range view.PartialClaims {
		slots = append(slots, verifiedSlot{
			kind: verifiedPartialSlot, index: index,
		})
	}
	for index := range view.PartialGoals {
		slots = append(slots, verifiedSlot{
			kind: verifiedPartialGoalSlot, index: index,
		})
	}
	for index := range view.UnresolvedGoals {
		slots = append(slots, verifiedSlot{
			kind: verifiedUnresolvedGoalSlot, index: index,
		})
	}
	for index := range view.Limitations {
		slots = append(slots, verifiedSlot{
			kind: verifiedLimitationSlot, index: index,
		})
	}
	for index := range view.UnsupportedClaims {
		slots = append(slots, verifiedSlot{
			kind: verifiedUnsupportedSlot, index: index,
		})
	}
	for index := range view.EvidenceConflicts {
		slots = append(slots, verifiedSlot{
			kind: verifiedConflictSlot, index: index,
		})
	}
	return slots
}

func claimHasGoal(claim verifiedClaimView, required map[string]struct{}) bool {
	for _, goal := range claim.GoalIDs {
		if _, ok := required[goal]; ok {
			return true
		}
	}
	return false
}

func viewAtVerifiedSlot(
	full verifiedEvidenceView,
	slots []verifiedSlot,
	count int,
	protectedEvidence map[evidence.Key]struct{},
) verifiedEvidenceView {
	view := verifiedEvidenceView{
		SupportedClaims:   []verifiedClaimView{},
		PartialClaims:     []verifiedClaimView{},
		UnsupportedClaims: []unsupportedClaimView{},
		PartialGoals:      []string{},
		UnresolvedGoals:   []string{},
		Limitations:       []string{},
		LimitationsDetail: full.LimitationsDetail,
		EvidenceUnits:     []tool.EvidenceUnit{},
		EvidenceConflicts: []agentapi.EvidenceConflict{},
		SubjectCoverage:   append([]subjectCoverageView(nil), full.SubjectCoverage...),
		Verification:      full.Verification,
		Completeness:      full.Completeness,
	}
	selectedEvidence := cloneEvidenceKeySet(protectedEvidence)
	for _, slot := range slots[:min(count, len(slots))] {
		switch slot.kind {
		case verifiedSupportedSlot:
			claim := full.SupportedClaims[slot.index]
			view.SupportedClaims = append(view.SupportedClaims, claim)
			addClaimEvidence(selectedEvidence, claim)
		case verifiedPartialSlot:
			claim := full.PartialClaims[slot.index]
			view.PartialClaims = append(view.PartialClaims, claim)
			addClaimEvidence(selectedEvidence, claim)
		case verifiedUnsupportedSlot:
			view.UnsupportedClaims = append(
				view.UnsupportedClaims,
				full.UnsupportedClaims[slot.index],
			)
		case verifiedPartialGoalSlot:
			view.PartialGoals = append(
				view.PartialGoals,
				full.PartialGoals[slot.index],
			)
		case verifiedUnresolvedGoalSlot:
			view.UnresolvedGoals = append(
				view.UnresolvedGoals,
				full.UnresolvedGoals[slot.index],
			)
		case verifiedLimitationSlot:
			view.Limitations = append(
				view.Limitations,
				full.Limitations[slot.index],
			)
		case verifiedConflictSlot:
			view.EvidenceConflicts = append(
				view.EvidenceConflicts,
				full.EvidenceConflicts[slot.index],
			)
		}
	}
	view.EvidenceUnits = selectedEvidenceUnits(full.EvidenceUnits, selectedEvidence)
	view.Omissions = omissionView{
		Claims: len(full.SupportedClaims) + len(full.PartialClaims) +
			len(full.UnsupportedClaims) - len(view.SupportedClaims) -
			len(view.PartialClaims) - len(view.UnsupportedClaims),
		Goals: len(full.PartialGoals) + len(full.UnresolvedGoals) -
			len(view.PartialGoals) - len(view.UnresolvedGoals),
		Limitations:   len(full.Limitations) - len(view.Limitations),
		EvidenceUnits: len(full.EvidenceUnits) - len(view.EvidenceUnits),
		EvidenceConflicts: len(full.EvidenceConflicts) -
			len(view.EvidenceConflicts),
	}
	return view
}

func addClaimEvidence(
	selected map[evidence.Key]struct{},
	claim verifiedClaimView,
) {
	for _, identity := range claim.EvidenceIdentities {
		selected[keyFromIdentity(identity)] = struct{}{}
	}
}

func selectedEvidenceUnits(
	units []tool.EvidenceUnit,
	selected map[evidence.Key]struct{},
) []tool.EvidenceUnit {
	if len(selected) == 0 {
		return []tool.EvidenceUnit{}
	}
	out := make([]tool.EvidenceUnit, 0, len(selected))
	for _, unit := range units {
		matched := false
		sections := unit.Sections
		if len(sections) == 0 {
			sections = []string{""}
		}
		for _, section := range sections {
			key := evidence.Key{
				SourceKind: unit.SourceKind,
				Target:     unit.Target,
				Section:    section,
				Version:    unit.Version,
				TimeRange:  unit.TimeRange,
			}
			if _, ok := selected[key]; ok {
				matched = true
				break
			}
		}
		if matched {
			out = append(out, evidence.CloneUnit(unit))
		}
	}
	return out
}

func evidenceReferences(items []findingEvidenceView) []string {
	refs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Reference != "" {
			refs = append(refs, item.Reference)
		}
	}
	return unionStrings(nil, refs)
}

func verifiedViewTokens(view verifiedEvidenceView) int {
	payload, err := json.Marshal(view)
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return tooloutput.EstimateTokens(string(payload))
}

func verifySubjectCoverage(
	requirements []SubjectRequirement,
	supportedClaims []verifiedClaimView,
	partialClaims []verifiedClaimView,
	appendLimitation func(string),
) ([]subjectCoverageView, bool) {
	if len(requirements) == 0 {
		return nil, false
	}
	claims := make([]verifiedClaimView, 0, len(supportedClaims)+len(partialClaims))
	claims = append(claims, supportedClaims...)
	claims = append(claims, partialClaims...)
	claimEntities := make([]map[string]struct{}, len(claims))
	for index, claim := range claims {
		claimEntities[index] = stringSet(claim.EntityIDs)
	}
	coverage := make([]subjectCoverageView, 0, len(requirements))
	insufficient := false
	for _, requirement := range requirements {
		terms := subjectEntityTerms(requirement)
		requiredFacets := stringSet(requirement.RequiredFacets)
		coveredFacets := make(map[string]struct{}, len(requirement.RequiredFacets))
		sources := make(map[agentapi.EvidenceSource]struct{}, len(requirement.RequiredSources))
		for claimIndex, claim := range claims {
			matched, matchedSources := subjectClaimSources(
				claim, claimEntities[claimIndex], requirement.EntityID, terms,
			)
			if !matched {
				continue
			}
			for _, facet := range claim.GoalIDs {
				if _, required := requiredFacets[facet]; required {
					coveredFacets[facet] = struct{}{}
				}
			}
			for source := range matchedSources {
				sources[source] = struct{}{}
			}
		}
		covered := orderedSetValues(requirement.RequiredFacets, coveredFacets)
		missingFacets := missingSetValues(requirement.RequiredFacets, coveredFacets)
		missingSources := missingEvidenceSources(requirement.RequiredSources, sources)
		availableSources := evidenceSourceStrings(sources)
		complete := len(missingFacets) == 0 && len(missingSources) == 0
		coverage = append(coverage, subjectCoverageView{
			EntityID: requirement.EntityID, CoveredFacets: covered,
			MissingFacets: missingFacets, Sources: availableSources,
			Complete: complete,
		})
		if complete {
			continue
		}
		insufficient = true
		clauses := make([]string, 0, 2)
		if len(missingFacets) > 0 {
			clauses = append(clauses, "facets: "+strings.Join(missingFacets, ", "))
		}
		if len(missingSources) > 0 {
			clauses = append(clauses, "sources: "+strings.Join(missingSources, ", "))
		}
		appendLimitation(fmt.Sprintf(
			"Required entity %q is missing evidence for %s.",
			requirement.EntityID,
			strings.Join(clauses, "; "),
		))
	}
	return coverage, insufficient
}

func subjectEntityTerms(requirement SubjectRequirement) []string {
	terms := make([]string, 0, len(requirement.Aliases)+2)
	terms = appendProjectionEntityTerm(terms, requirement.EntityID)
	terms = appendProjectionEntityTerm(terms, requirement.Label)
	for _, alias := range requirement.Aliases {
		terms = appendProjectionEntityTerm(terms, alias)
	}
	return terms
}

func subjectClaimSources(
	claim verifiedClaimView,
	claimEntities map[string]struct{},
	entityID string,
	entityTerms []string,
) (bool, map[agentapi.EvidenceSource]struct{}) {
	sources := make(map[agentapi.EvidenceSource]struct{})
	_, explicitlyMatched := claimEntities[entityID]
	matched := false
	for _, identity := range claim.EvidenceIdentities {
		if !explicitlyMatched && !subjectIdentityMatches(identity, entityTerms) {
			continue
		}
		matched = true
		for _, source := range evidenceSourcesForIdentity(claim.ProducerNodeID, identity.SourceKind) {
			sources[source] = struct{}{}
		}
	}
	return matched, sources
}

func subjectIdentityMatches(
	identity agentapi.EvidenceIdentity,
	entityTerms []string,
) bool {
	if len(entityTerms) == 0 {
		return false
	}
	haystack := canonicalProjectionEntityText(identity.Target + " " + identity.Section)
	for _, term := range entityTerms {
		if strings.Contains(haystack, term) {
			return true
		}
	}
	return false
}

func evidenceSourcesForIdentity(
	producerNodeID string,
	sourceKind string,
) []agentapi.EvidenceSource {
	nodeID := strings.ToLower(strings.TrimSpace(producerNodeID))
	switch {
	case strings.Contains(nodeID, ".runtime"):
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime}
	case strings.Contains(nodeID, ".web"):
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceWeb}
	case strings.Contains(nodeID, ".memory"):
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceMemory}
	case strings.Contains(nodeID, ".code"), strings.Contains(nodeID, ".docs"),
		strings.Contains(nodeID, ".service"):
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
	}
	switch strings.ToLower(strings.TrimSpace(sourceKind)) {
	case "runtime":
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceRuntime}
	case "web", "external":
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceWeb}
	case "memory":
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceMemory}
	case "code", "codegraph", "service", "dependency", "runbook",
		"generated_doc", "doc", "docs":
		return []agentapi.EvidenceSource{agentapi.EvidenceSourceInternal}
	default:
		return nil
	}
}

func orderedSetValues(order []string, values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for _, value := range order {
		if _, ok := values[value]; ok {
			out = append(out, value)
		}
	}
	return out
}

func missingSetValues(required []string, covered map[string]struct{}) []string {
	out := make([]string, 0, len(required))
	for _, value := range required {
		if _, ok := covered[value]; !ok {
			out = append(out, value)
		}
	}
	return out
}

func missingEvidenceSources(
	required []agentapi.EvidenceSource,
	covered map[agentapi.EvidenceSource]struct{},
) []string {
	out := make([]string, 0, len(required))
	for _, source := range required {
		if _, ok := covered[source]; !ok {
			out = append(out, string(source))
		}
	}
	return out
}

func evidenceSourceStrings(values map[agentapi.EvidenceSource]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, string(value))
	}
	sort.Strings(out)
	return out
}

func subjectCompleteness(
	base Completeness,
	coverage []subjectCoverageView,
) Completeness {
	if len(coverage) == 0 {
		return base
	}
	allComplete := true
	coveredFacetCount := 0
	for _, subject := range coverage {
		allComplete = allComplete && subject.Complete
		coveredFacetCount += len(subject.CoveredFacets)
	}
	if allComplete {
		return base
	}
	if coveredFacetCount == 0 {
		return Unavailable
	}
	return Partial
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
		if inputCompleteness == Partial {
			return Partial
		}
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
	evidenceInsufficient ...bool,
) StopReason {
	insufficient := len(evidenceInsufficient) > 0 && evidenceInsufficient[0]
	if completeness == Complete {
		return StopRequiredGoalsCovered
	}
	if reason := unavailableStopReason(ledger.UnavailableTasks); reason != "" {
		return reason
	}
	if ledger.Convergence != nil {
		if ledger.Convergence.NewIdentityCount == 0 {
			return StopNoNewEvidence
		}
		if ledger.Convergence.MaxDuplicateRatio > 0 &&
			ledger.Convergence.DuplicateRatio > ledger.Convergence.MaxDuplicateRatio {
			return StopDuplicateEvidence
		}
	}
	if insufficient {
		return StopEvidenceInsufficient
	}
	return StopEvidenceInsufficient
}

func compatibleVerificationStopReason(
	ref agentapi.SchemaRef,
	reason StopReason,
) StopReason {
	if ref == (agentapi.SchemaRef{
		ID: "investigation.verified_bundle", Version: 1,
	}) && reason == StopEvidenceInsufficient {
		return StopCapabilityUnavailable
	}
	return reason
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
	byHandle    map[string]evidenceMatch
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
		byHandle:    make(map[string]evidenceMatch, len(expanded)),
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
		index.byHandle[key.Handle()] = match
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
	if item.EvidenceID != "" {
		match, ok := index.byHandle[item.EvidenceID]
		if !ok {
			return nil
		}
		return []evidenceMatch{match}
	}
	if item.Identity != nil {
		match, ok := index.byIdentity[keyFromIdentity(*item.Identity)]
		if !ok {
			return nil
		}
		return []evidenceMatch{match}
	}
	matches := index.byReference[referenceKey(item.Kind, item.Reference)]
	target, section, ok := parseEvidenceReference(item.Reference)
	if !ok {
		return matches
	}
	kind := normalizeEvidencePart(item.Kind)
	if section != "" {
		if match, exists := index.byIdentity[evidence.Key{
			SourceKind: kind, Target: target, Section: section,
		}]; exists {
			return []evidenceMatch{match}
		}
		return matches
	}
	if targetMatches := index.byReference[referenceKey(kind, target)]; len(targetMatches) > 0 {
		return targetMatches
	}
	return matches
}

func referenceKey(kind, reference string) string {
	return normalizeEvidencePart(kind) + "\x00" + strings.TrimSpace(reference)
}

func normalizeEvidencePart(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func parseEvidenceReference(reference string) (string, string, bool) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return "", "", false
	}
	for _, marker := range []string{"#L", ":L", " (L"} {
		index := strings.LastIndex(reference, marker)
		if index <= 0 {
			continue
		}
		target := strings.TrimSpace(reference[:index])
		sectionStart := index + 1
		if marker == " (L" {
			sectionStart = index + 2
		}
		section := strings.TrimSpace(reference[sectionStart:])
		section = strings.TrimSuffix(section, ")")
		if target == "" || !validEvidenceSection(section) {
			continue
		}
		return target, section, true
	}
	return reference, "", true
}

func validEvidenceSection(section string) bool {
	if len(section) < 2 || section[0] != 'L' {
		return false
	}
	for _, value := range section[1:] {
		if (value < '0' || value > '9') && value != '-' && value != 'L' {
			return false
		}
	}
	return true
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
	// The handle is an investigation transport detail; verified output carries
	// the resolved canonical identity instead.
	item.EvidenceID = ""
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
