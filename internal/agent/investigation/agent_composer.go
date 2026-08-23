package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	synthesizerRunIDSuffix     = ":synthesize"
	limitationsNormalization   = "limitations-v1"
	maxDisplayedLimitations    = 10
	unsupportedReasonCode      = "canonical_evidence_unbound"
	fallbackProducerNodeID     = "evidence.verify"
	fallbackEvidenceSourceKind = "unknown"
	fallbackEvidenceTarget     = "unknown"
)

// AgentComposer runs the synthesizer agent definition to turn a verified report
// into a user-readable answer. It is the real implementation of Composer; the
// deterministic renderer remains the fallback when composition is unavailable.
type AgentComposer struct {
	Runtime     agentapi.Runtime
	Definitions agentapi.DefinitionResolver
	// ComposerDefinition overrides the default synthesizer definition. A zero
	// Version resolves the catalog default.
	ComposerDefinition agentapi.DefinitionRef
}

func (composer AgentComposer) Compose(
	ctx context.Context,
	contract InvestigationContract,
	report InvestigationReport,
) (AnswerDraft, error) {
	if composer.Runtime == nil {
		return AnswerDraft{}, fmt.Errorf("agent runtime is required")
	}
	if composer.Definitions == nil {
		return AnswerDraft{}, fmt.Errorf("agent definition resolver is required")
	}
	ref := composer.ComposerDefinition
	if ref.ID == "" {
		ref.ID = defaultComposerDefinitionID
	}
	definition, err := composer.Definitions.Resolve(ref)
	if err != nil {
		return AnswerDraft{}, fmt.Errorf("resolve synthesizer definition %q: %w", ref.ID, err)
	}
	bundle, err := marshalVerifiedBundle(contract, report)
	if err != nil {
		return AnswerDraft{}, err
	}
	objective, err := synthesisObjectiveBlock(contract)
	if err != nil {
		return AnswerDraft{}, err
	}
	request := agentapi.RunRequest{
		RunID:          contract.ID + synthesizerRunIDSuffix,
		Agent:          agentapi.DefinitionRef{ID: definition.ID, Version: definition.Version},
		DefinitionHash: definition.ContentHash,
		Input:          bundle,
		Context:        []agentapi.ContextBlock{objective},
		Permissions:    definition.Permissions,
		ToolScope: agentapi.ToolScope{
			RestrictVisible: true,
			VisibleToolIDs:  append([]string(nil), definition.Tools.VisibleToolIDs...),
		},
		Policy: agentapi.RunPolicy{EvidenceSeeded: true},
		Limits: agentapi.RunLimits{
			MaxSteps:     definition.Budget.MaxSteps,
			MaxToolCalls: definition.Budget.MaxToolCalls,
		},
	}
	if definition.Budget.Timeout > 0 {
		request.Limits.Deadline = time.Now().UTC().Add(definition.Budget.Timeout)
	}
	result, err := composer.Runtime.Run(ctx, request)
	if err != nil {
		return AnswerDraft{}, err
	}
	return projectAnswer(result)
}

func projectAnswer(result agentapi.RunResult) (AnswerDraft, error) {
	if result.Error != nil {
		return AnswerDraft{}, fmt.Errorf("synthesizer run %q failed: %s", result.RunID, result.Error.Message)
	}
	if result.Status != agentapi.RunSucceeded {
		return AnswerDraft{}, fmt.Errorf("synthesizer run %q has status %q", result.RunID, result.Status)
	}
	var answer struct {
		Answer string `json:"answer"`
	}
	if len(result.Output) == 0 {
		return AnswerDraft{}, fmt.Errorf("synthesizer run %q produced no output", result.RunID)
	}
	if err := json.Unmarshal(result.Output, &answer); err != nil {
		return AnswerDraft{}, fmt.Errorf("decode synthesizer answer: %w", err)
	}
	if strings.TrimSpace(answer.Answer) == "" {
		return AnswerDraft{}, fmt.Errorf("synthesizer run %q produced an empty answer", result.RunID)
	}
	return AnswerDraft{Text: answer.Answer}, nil
}

// verifiedBundleView is the JSON shape required by investigation.verified_bundle v2.
// Required fields are always emitted even when empty; subject_coverage stays optional.
type verifiedBundleView struct {
	SupportedClaims   []supportedClaimView        `json:"supported_claims"`
	PartialClaims     []supportedClaimView        `json:"partial_claims"`
	UnsupportedClaims []unsupportedClaimView      `json:"unsupported_claims"`
	PartialGoals      []string                    `json:"partial_goals"`
	UnresolvedGoals   []string                    `json:"unresolved_goals"`
	Limitations       []string                    `json:"limitations"`
	LimitationsDetail limitationsDetailRef        `json:"limitations_detail"`
	EvidenceUnits     []tool.EvidenceUnit         `json:"evidence_units"`
	EvidenceConflicts []agentapi.EvidenceConflict `json:"evidence_conflicts"`
	SubjectCoverage   []subjectCoverageView       `json:"subject_coverage,omitempty"`
	Verification      verificationView            `json:"verification"`
	Completeness      string                      `json:"completeness"`
	Omissions         omissionView                `json:"omissions"`
}

type supportedClaimView struct {
	ProducerNodeID     string                      `json:"producer_node_id"`
	FindingIndex       int                         `json:"finding_index"`
	Claim              string                      `json:"claim"`
	GoalIDs            []string                    `json:"goal_ids"`
	Evidence           []findingEvidenceView       `json:"evidence"`
	EvidenceIdentities []agentapi.EvidenceIdentity `json:"evidence_identities"`
	Confidence         float64                     `json:"confidence"`
	Support            string                      `json:"support"`
	HighRisk           bool                        `json:"high_risk"`
}

type unsupportedClaimView struct {
	ProducerNodeID string   `json:"producer_node_id"`
	FindingIndex   int      `json:"finding_index"`
	GoalIDs        []string `json:"goal_ids"`
	Support        string   `json:"support"`
	HighRisk       bool     `json:"high_risk"`
	ReasonCode     string   `json:"reason_code"`
}

type findingEvidenceView struct {
	Kind      string                     `json:"kind"`
	Reference string                     `json:"reference"`
	Summary   string                     `json:"summary"`
	Identity  *agentapi.EvidenceIdentity `json:"identity,omitempty"`
}

type verificationView struct {
	Decision   string `json:"decision"`
	StopReason string `json:"stop_reason"`
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

type limitationsDetailRef struct {
	ArtifactID           string `json:"artifact_id"`
	TotalCount           int    `json:"total_count"`
	DisplayedCount       int    `json:"displayed_count"`
	OmittedCount         int    `json:"omitted_count"`
	NormalizationVersion string `json:"normalization_version"`
}

func marshalVerifiedBundle(contract InvestigationContract, report InvestigationReport) (json.RawMessage, error) {
	report = PruneUnreferencedEvidence(report)
	evidenceByID := indexEvidence(report.Evidence)
	goalByClaim := indexGoalByClaim(report.Coverage)
	required := requiredGoalIDs(contract)

	supported := make([]supportedClaimView, 0)
	partial := make([]supportedClaimView, 0)
	unsupported := make([]unsupportedClaimView, 0)
	findingIndex := 0
	for _, claim := range report.Claims {
		goalIDs := claimGoalIDs(claim, goalByClaim, required)
		if claim.Status != ClaimRejected && len(claim.EvidenceRefs) == 0 {
			unsupported = append(unsupported, unsupportedClaim(claim, goalIDs, findingIndex))
			findingIndex++
			continue
		}
		switch claim.Status {
		case ClaimSupported:
			supported = append(supported, supportedClaim(claim, goalIDs, findingIndex, "supported", evidenceByID))
		case ClaimRejected:
			unsupported = append(unsupported, unsupportedClaim(claim, goalIDs, findingIndex))
		default:
			partial = append(partial, supportedClaim(claim, goalIDs, findingIndex, "partial", evidenceByID))
		}
		findingIndex++
	}

	partialGoals := make([]string, 0)
	unresolvedGoals := make([]string, 0)
	for _, coverage := range report.Coverage {
		if !goalRequired(coverage.GoalID, required) {
			continue
		}
		switch coverage.Status {
		case GoalPartial:
			partialGoals = append(partialGoals, coverage.GoalID)
		case GoalUnresolved:
			unresolvedGoals = append(unresolvedGoals, coverage.GoalID)
		}
	}

	limitations := reportLimitations(report)
	decision := verificationDecision(contract, report)
	view := verifiedBundleView{
		SupportedClaims:   supported,
		PartialClaims:     partial,
		UnsupportedClaims: unsupported,
		PartialGoals:      partialGoals,
		UnresolvedGoals:   unresolvedGoals,
		Limitations:       limitations.displayed,
		LimitationsDetail: limitationsDetailRef{
			ArtifactID:           limitationArtifactID(contract.ID),
			TotalCount:           limitations.total,
			DisplayedCount:       len(limitations.displayed),
			OmittedCount:         limitations.total - len(limitations.displayed),
			NormalizationVersion: limitationsNormalization,
		},
		EvidenceUnits:     publicEvidenceUnits(report.Evidence),
		EvidenceConflicts: append([]agentapi.EvidenceConflict(nil), report.EvidenceConflicts...),
		Verification: verificationView{
			Decision:   decision,
			StopReason: stopReason(decision),
		},
		Completeness: decision,
		Omissions:    omissionView{},
	}
	payload, err := json.Marshal(view)
	if err != nil {
		return nil, fmt.Errorf("marshal verified bundle: %w", err)
	}
	return payload, nil
}

func indexEvidence(units []EvidenceUnit) map[string]EvidenceUnit {
	index := make(map[string]EvidenceUnit, len(units))
	for _, unit := range units {
		index[unit.ID] = unit
	}
	return index
}

func indexGoalByClaim(coverage []GoalCoverage) map[string]string {
	index := make(map[string]string)
	for _, entry := range coverage {
		for _, claimID := range entry.ClaimIDs {
			index[claimID] = entry.GoalID
		}
	}
	return index
}

func requiredGoalIDs(contract InvestigationContract) map[string]struct{} {
	required := make(map[string]struct{}, len(contract.Goals))
	for _, goal := range contract.Goals {
		if goal.Required {
			required[goal.ID] = struct{}{}
		}
	}
	return required
}

func goalRequired(goalID string, required map[string]struct{}) bool {
	_, ok := required[goalID]
	return ok
}

func claimGoalIDs(claim VerifiedClaim, goalByClaim map[string]string, required map[string]struct{}) []string {
	if strings.TrimSpace(claim.GoalID) != "" {
		return []string{claim.GoalID}
	}
	if goalID := goalByClaim[claim.ID]; goalID != "" {
		return []string{goalID}
	}
	for goalID := range required {
		return []string{goalID}
	}
	return []string{claim.ID}
}

func supportedClaim(
	claim VerifiedClaim,
	goalIDs []string,
	findingIndex int,
	support string,
	evidenceByID map[string]EvidenceUnit,
) supportedClaimView {
	identities := make([]agentapi.EvidenceIdentity, 0, len(claim.EvidenceRefs))
	evidence := make([]findingEvidenceView, 0, len(claim.EvidenceRefs))
	seenIdentity := make(map[string]struct{}, len(claim.EvidenceRefs))
	for _, ref := range claim.EvidenceRefs {
		unit, ok := evidenceByID[ref.EvidenceID]
		if !ok {
			unit = EvidenceUnit{
				SourceKind: fallbackEvidenceSourceKind,
				Target:     fallbackEvidenceTarget,
				Content:    claim.Text,
			}
		}
		identity := agentapi.EvidenceIdentity{
			SourceKind: firstNonEmpty(unit.SourceKind, ref.SourceKind, fallbackEvidenceSourceKind),
			Target:     firstNonEmpty(unit.Target, ref.Target, fallbackEvidenceTarget),
			Section:    firstNonEmpty(unit.Section, ref.Section),
			Version:    unit.Version,
			TimeRange:  unit.TimeRange,
		}
		key := identity.SourceKind + "\x00" + identity.Target + "\x00" + identity.Section
		if _, duplicate := seenIdentity[key]; duplicate {
			continue
		}
		seenIdentity[key] = struct{}{}
		identities = append(identities, identity)
		evidence = append(evidence, findingEvidenceView{
			Kind:      identity.SourceKind,
			Reference: identity.Target,
			Summary:   firstNonEmpty(unit.Content, claim.Text),
			Identity:  &identity,
		})
	}
	confidence := claim.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return supportedClaimView{
		ProducerNodeID:     firstNonEmpty(claim.VerifierTaskID, fallbackProducerNodeID),
		FindingIndex:       findingIndex,
		Claim:              claim.Text,
		GoalIDs:            append([]string(nil), goalIDs...),
		Evidence:           evidence,
		EvidenceIdentities: identities,
		Confidence:         confidence,
		Support:            support,
		HighRisk:           false,
	}
}

func unsupportedClaim(claim VerifiedClaim, goalIDs []string, findingIndex int) unsupportedClaimView {
	return unsupportedClaimView{
		ProducerNodeID: firstNonEmpty(claim.VerifierTaskID, fallbackProducerNodeID),
		FindingIndex:   findingIndex,
		GoalIDs:        append([]string(nil), goalIDs...),
		Support:        "unsupported",
		HighRisk:       false,
		ReasonCode:     unsupportedReasonCode,
	}
}

func publicEvidenceUnits(units []EvidenceUnit) []tool.EvidenceUnit {
	out := make([]tool.EvidenceUnit, 0, len(units))
	for _, unit := range units {
		var sections []string
		if strings.TrimSpace(unit.Section) != "" {
			sections = []string{unit.Section}
		}
		out = append(out, tool.EvidenceUnit{
			SourceKind:    firstNonEmpty(unit.SourceKind, fallbackEvidenceSourceKind),
			Target:        firstNonEmpty(unit.Target, fallbackEvidenceTarget),
			Sections:      sections,
			ContentHash:   unit.ContentHash,
			Coverage:      tool.EvidenceCoverage{Complete: true},
			Facets:        append([]string(nil), unit.Facets...),
			TrustTier:     unit.TrustTier,
			EvidenceClass: unit.EvidenceClass,
			Version:       unit.Version,
			TimeRange:     unit.TimeRange,
		})
	}
	return out
}

type limitationSet struct {
	displayed []string
	total     int
}

func reportLimitations(report InvestigationReport) limitationSet {
	seen := make(map[string]struct{}, len(report.Gaps))
	displayed := make([]string, 0, minInt(len(report.Gaps), maxDisplayedLimitations))
	total := 0
	for _, gap := range report.Gaps {
		reason := strings.TrimSpace(gap.Reason)
		if reason == "" {
			reason = "no verified claim covers this goal"
		}
		details := make([]string, 0, 2)
		if len(gap.MissingFacets) > 0 {
			details = append(details, "missing facets: "+strings.Join(gap.MissingFacets, ", "))
		}
		if len(gap.MissingSources) > 0 {
			details = append(details, "missing sources: "+strings.Join(gap.MissingSources, ", "))
		}
		if len(details) > 0 {
			reason += "; " + strings.Join(details, "; ")
		}
		text := fmt.Sprintf("Goal %q is not fully covered: %s", gap.GoalID, reason)
		if _, duplicate := seen[text]; duplicate {
			continue
		}
		seen[text] = struct{}{}
		total++
		if len(displayed) < maxDisplayedLimitations {
			displayed = append(displayed, text)
		}
	}
	return limitationSet{displayed: displayed, total: total}
}

func verificationDecision(contract InvestigationContract, report InvestigationReport) string {
	switch deliveryStatus(contract, report) {
	case DeliverySucceeded:
		return "complete"
	case DeliveryPartial:
		return "partial"
	default:
		return "unavailable"
	}
}

func stopReason(decision string) string {
	switch decision {
	case "complete":
		return "required_goals_covered"
	case "partial":
		return "no_new_evidence"
	default:
		return "evidence_insufficient"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// limitationArtifactID mirrors the verifier's deterministic artifact id so the
// synthesizer input remains schema-valid without coupling to the workflow package.
func limitationArtifactID(runID string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + limitationsNormalization))
	bytes := sum[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("art_%s-%s-%s-%s-%s",
		hex.EncodeToString(bytes[0:4]),
		hex.EncodeToString(bytes[4:6]),
		hex.EncodeToString(bytes[6:8]),
		hex.EncodeToString(bytes[8:10]),
		hex.EncodeToString(bytes[10:16]),
	)
}

func synthesisObjectiveBlock(contract InvestigationContract) (agentapi.ContextBlock, error) {
	goals := make([]synthesisGoalView, 0, len(contract.Goals))
	for _, goal := range contract.Goals {
		objective := strings.TrimSpace(goal.Description)
		if objective == "" {
			objective = goal.Kind
		}
		goals = append(goals, synthesisGoalView{
			ID:                  goal.ID,
			Objective:           objective,
			IndependentlyUseful: goal.Required,
			DependsOn:           []string{},
		})
	}
	content, err := json.Marshal(synthesisObjectiveView{
		Objective:          contract.Question,
		InvestigationGoals: goals,
	})
	if err != nil {
		return agentapi.ContextBlock{}, fmt.Errorf("marshal synthesis objective: %w", err)
	}
	sum := sha256.Sum256(content)
	return agentapi.ContextBlock{
		Source:      "workflow.synthesis_objective",
		Title:       "Original investigation objective",
		Content:     string(content),
		Complete:    true,
		ContentHash: hex.EncodeToString(sum[:]),
	}, nil
}

type synthesisObjectiveView struct {
	Objective          string              `json:"objective"`
	InvestigationGoals []synthesisGoalView `json:"investigation_goals"`
}

type synthesisGoalView struct {
	ID                  string   `json:"id"`
	Objective           string   `json:"objective"`
	IndependentlyUseful bool     `json:"independently_useful"`
	DependsOn           []string `json:"depends_on"`
}
