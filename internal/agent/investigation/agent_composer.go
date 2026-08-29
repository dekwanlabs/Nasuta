package investigation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
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
	// EvidenceContextBudget bounds evidence text entering the synthesizer.
	EvidenceContextBudget EvidenceContextBudget
	// Budget is the Composition share allocated from the frozen Investigation
	// Run budget. Input and aggregate-token limits are applied to the child Run;
	// output remains pinned by the synthesizer definition model policy.
	Budget BudgetVector
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
	bundle, err := marshalVerifiedBundleWithBudget(contract, report, composer.EvidenceContextBudget)
	if err != nil {
		return AnswerDraft{}, err
	}
	objective, err := synthesisObjectiveBlock(contract)
	if err != nil {
		return AnswerDraft{}, err
	}
	limits := runLimitsForBudget(composer.Budget, definition)
	if limits.Deadline.IsZero() && definition.Budget.Timeout > 0 {
		limits.Deadline = time.Now().UTC().Add(definition.Budget.Timeout)
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
		Policy: agentapi.RunPolicy{EvidenceSeeded: true, OutputMode: agentapi.RunOutputStandalone},
		Limits: limits,
	}
	result, err := composer.Runtime.Run(ctx, request)
	if err != nil {
		return AnswerDraft{}, err
	}
	return projectAnswer(result)
}

func projectAnswer(result agentapi.RunResult) (AnswerDraft, error) {
	usage := budgetVectorFromAgentUsage(result.Usage)
	if result.Error != nil {
		return AnswerDraft{Usage: usage}, fmt.Errorf("synthesizer run %q failed: %s", result.RunID, result.Error.Message)
	}
	if result.Status != agentapi.RunSucceeded {
		return AnswerDraft{Usage: usage}, fmt.Errorf("synthesizer run %q has status %q", result.RunID, result.Status)
	}
	var answer struct {
		Answer string `json:"answer"`
	}
	if len(result.Output) == 0 {
		return AnswerDraft{Usage: usage}, fmt.Errorf("synthesizer run %q produced no output", result.RunID)
	}
	if err := json.Unmarshal(result.Output, &answer); err != nil {
		return AnswerDraft{Usage: usage}, fmt.Errorf("decode synthesizer answer: %w", err)
	}
	if strings.TrimSpace(answer.Answer) == "" {
		return AnswerDraft{Usage: usage}, fmt.Errorf("synthesizer run %q produced an empty answer", result.RunID)
	}
	return AnswerDraft{Text: answer.Answer, Usage: usage}, nil
}

func budgetVectorFromAgentUsage(usage agentapi.Usage) BudgetVector {
	total := usage.TotalTokens
	if total == 0 && (usage.InputTokens > 0 || usage.OutputTokens > 0) {
		total = usage.InputTokens + usage.OutputTokens
	}
	return BudgetVector{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  total,
		CostMicros:   usage.CostMicros,
	}
}

// verifiedBundleView is the compact JSON handoff required by the synthesizer.
// Evidence bodies live once in evidence_lookup; claims carry only references.
type verifiedBundleView struct {
	SupportedClaims         []supportedClaimView           `json:"supported_claims"`
	PartialClaims           []supportedClaimView           `json:"partial_claims"`
	UnsupportedClaims       []unsupportedClaimView         `json:"unsupported_claims"`
	PartialEvidenceGoals    []string                       `json:"partial_evidence_goals"`
	UnresolvedEvidenceGoals []string                       `json:"unresolved_evidence_goals"`
	Limitations             []string                       `json:"limitations"`
	LimitationsDetail       limitationsDetailRef           `json:"limitations_detail"`
	EvidenceUnits           []tool.EvidenceUnit            `json:"evidence_units,omitempty"`
	EvidenceConflicts       []agentapi.EvidenceConflict    `json:"evidence_conflicts,omitempty"`
	SubjectCoverage         []subjectCoverageView          `json:"subject_coverage,omitempty"`
	Verification            verificationView               `json:"verification"`
	Completeness            string                         `json:"completeness"`
	Omissions               omissionView                   `json:"omissions"`
	EvidenceLookup          map[string]evidenceSummaryView `json:"evidence_lookup,omitempty"`
	EvidenceContext         evidenceContextView            `json:"evidence_context"`
	EvidenceOmissions       []evidenceOmissionView         `json:"evidence_omissions,omitempty"`
}

type supportedClaimView struct {
	ProducerNodeID  string                `json:"producer_node_id"`
	FindingIndex    int                   `json:"finding_index"`
	Claim           string                `json:"claim"`
	EvidenceGoalIDs []string              `json:"evidence_goal_ids"`
	Evidence        []findingEvidenceView `json:"evidence"`
	Confidence      float64               `json:"confidence"`
	Support         string                `json:"support"`
	HighRisk        bool                  `json:"high_risk"`
}

type unsupportedClaimView struct {
	ProducerNodeID  string   `json:"producer_node_id"`
	FindingIndex    int      `json:"finding_index"`
	EvidenceGoalIDs []string `json:"evidence_goal_ids"`
	Support         string   `json:"support"`
	HighRisk        bool     `json:"high_risk"`
	ReasonCode      string   `json:"reason_code"`
}

type findingEvidenceView struct {
	EvidenceID string `json:"evidence_id"`
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
	return marshalVerifiedBundleWithBudget(contract, report, EvidenceContextBudget{})
}

func marshalVerifiedBundleWithBudget(
	contract InvestigationContract,
	report InvestigationReport,
	budget EvidenceContextBudget,
) (json.RawMessage, error) {
	originalEvidence := append([]EvidenceUnit(nil), report.Evidence...)
	report = PruneUnreferencedEvidence(report)
	goalByClaim := indexGoalByClaim(report.Coverage)
	required := requiredGoalIDs(contract)
	evidenceContext := buildEvidenceContext(report.Evidence, report.Claims, contract, budget)
	prunedOmissions := prunedEvidenceOmissions(originalEvidence, report.Evidence)
	evidenceOmissions := append([]evidenceOmissionView(nil), prunedOmissions...)
	knownEvidence := indexEvidence(report.Evidence)

	supported := make([]supportedClaimView, 0)
	partial := make([]supportedClaimView, 0)
	unsupported := make([]unsupportedClaimView, 0)
	findingIndex := 0
	for _, claim := range report.Claims {
		goalIDs := claimGoalIDs(claim, goalByClaim, required)
		claim.EvidenceRefs, evidenceOmissions = boundedEvidenceRefs(
			claim.EvidenceRefs,
			evidenceContext.lookup,
			knownEvidence,
			evidenceOmissions,
		)
		if claim.Status != ClaimRejected && len(claim.EvidenceRefs) == 0 {
			unsupported = append(unsupported, unsupportedClaim(claim, goalIDs, findingIndex))
			findingIndex++
			continue
		}
		switch claim.Status {
		case ClaimSupported:
			supported = append(supported, supportedClaim(claim, goalIDs, findingIndex, "supported"))
		case ClaimRejected:
			unsupported = append(unsupported, unsupportedClaim(claim, goalIDs, findingIndex))
		default:
			partial = append(partial, supportedClaim(claim, goalIDs, findingIndex, "partial"))
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
		SupportedClaims:         supported,
		PartialClaims:           partial,
		UnsupportedClaims:       unsupported,
		PartialEvidenceGoals:    partialGoals,
		UnresolvedEvidenceGoals: unresolvedGoals,
		Limitations:             limitations.displayed,
		LimitationsDetail: limitationsDetailRef{
			ArtifactID:           limitationArtifactID(contract.ID),
			TotalCount:           limitations.total,
			DisplayedCount:       len(limitations.displayed),
			OmittedCount:         limitations.total - len(limitations.displayed),
			NormalizationVersion: limitationsNormalization,
		},
		EvidenceConflicts: append([]agentapi.EvidenceConflict(nil), report.EvidenceConflicts...),
		Verification: verificationView{
			Decision:   decision,
			StopReason: stopReason(decision),
		},
		Completeness:      decision,
		Omissions:         omissionView{EvidenceUnits: len(prunedOmissions)},
		EvidenceLookup:    evidenceContext.lookup,
		EvidenceContext:   evidenceContext.context,
		EvidenceOmissions: appendEvidenceOmissions(evidenceOmissions, evidenceContext.omissions...),
	}
	payload, err := marshalBoundedVerifiedBundle(&view, budget)
	if err != nil {
		return nil, fmt.Errorf("marshal verified bundle: %w", err)
	}
	return payload, nil
}

func boundedEvidenceRefs(
	refs []EvidenceRef,
	lookup map[string]evidenceSummaryView,
	known map[string]EvidenceUnit,
	omissions []evidenceOmissionView,
) ([]EvidenceRef, []evidenceOmissionView) {
	bounded := make([]EvidenceRef, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		id := strings.TrimSpace(ref.EvidenceID)
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		if _, available := lookup[id]; available {
			ref.EvidenceID = id
			bounded = append(bounded, ref)
			continue
		}
		reason := "evidence_context_budget"
		if _, exists := known[id]; !exists {
			reason = "evidence_not_found"
		}
		omissions = appendEvidenceOmission(omissions, evidenceOmissionView{EvidenceID: id, Reason: reason})
	}
	return bounded, omissions
}

func appendEvidenceOmissions(existing []evidenceOmissionView, omissions ...evidenceOmissionView) []evidenceOmissionView {
	for _, omission := range omissions {
		existing = appendEvidenceOmission(existing, omission)
	}
	return existing
}

func prunedEvidenceOmissions(original, pruned []EvidenceUnit) []evidenceOmissionView {
	kept := make(map[string]struct{}, len(pruned))
	for _, unit := range pruned {
		kept[unit.ID] = struct{}{}
	}
	omitted := make([]evidenceOmissionView, 0)
	for _, unit := range original {
		if _, ok := kept[unit.ID]; ok {
			continue
		}
		omitted = append(omitted, evidenceOmissionView{
			EvidenceID: unit.ID,
			Reason:     "unreferenced_evidence",
		})
	}
	return omitted
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
	required := make(map[string]struct{}, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
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
) supportedClaimView {
	evidence := make([]findingEvidenceView, 0, len(claim.EvidenceRefs))
	seenEvidence := make(map[string]struct{}, len(claim.EvidenceRefs))
	for _, ref := range claim.EvidenceRefs {
		if strings.TrimSpace(ref.EvidenceID) == "" {
			continue
		}
		if _, duplicate := seenEvidence[ref.EvidenceID]; duplicate {
			continue
		}
		seenEvidence[ref.EvidenceID] = struct{}{}
		evidence = append(evidence, findingEvidenceView{EvidenceID: ref.EvidenceID})
	}
	confidence := claim.Confidence
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}
	return supportedClaimView{
		ProducerNodeID:  firstNonEmpty(claim.VerifierTaskID, fallbackProducerNodeID),
		FindingIndex:    findingIndex,
		Claim:           claim.Text,
		EvidenceGoalIDs: append([]string(nil), goalIDs...),
		Evidence:        evidence,
		Confidence:      confidence,
		Support:         support,
		HighRisk:        false,
	}
}

func marshalBoundedVerifiedBundle(view *verifiedBundleView, budget EvidenceContextBudget) (json.RawMessage, error) {
	_, _, maxBundleTokens := budget.effective()
	payload, err := json.Marshal(view)
	if err != nil {
		return nil, err
	}
	if maxBundleTokens <= 0 || tooloutput.EstimateTokens(string(payload)) <= int(maxBundleTokens) {
		return payload, nil
	}

	for target := 128; target >= 8; target /= 2 {
		if shrinkEvidenceSummaries(view.EvidenceLookup, target) {
			payload, err = json.Marshal(view)
			if err != nil {
				return nil, err
			}
			if tooloutput.EstimateTokens(string(payload)) <= int(maxBundleTokens) {
				return payload, nil
			}
		}
	}

	view.EvidenceConflicts = nil
	view.SubjectCoverage = nil
	payload, err = json.Marshal(view)
	if err != nil {
		return nil, err
	}
	if tooloutput.EstimateTokens(string(payload)) <= int(maxBundleTokens) {
		return payload, nil
	}

	for tooloutput.EstimateTokens(string(payload)) > int(maxBundleTokens) {
		progressed := false
		if trimBundleClaimText(view, 64) {
			progressed = true
		} else if removeEvidenceForBundleBudget(view) {
			progressed = true
		} else if trimBundleClaimText(view, 32) {
			progressed = true
		} else if dropLastBundleClaim(view) {
			progressed = true
		}
		if !progressed {
			break
		}
		payload, err = json.Marshal(view)
		if err != nil {
			return nil, err
		}
	}
	if tooloutput.EstimateTokens(string(payload)) > int(maxBundleTokens) {
		return nil, fmt.Errorf("verified bundle requires %d tokens, budget is %d", tooloutput.EstimateTokens(string(payload)), maxBundleTokens)
	}
	return payload, nil
}

func shrinkEvidenceSummaries(lookup map[string]evidenceSummaryView, maxTokens int) bool {
	changed := false
	for id, evidence := range lookup {
		shrunk := tooloutput.TruncateContent(evidence.Summary, maxTokens)
		if shrunk == evidence.Summary {
			continue
		}
		evidence.Summary = shrunk
		lookup[id] = evidence
		changed = true
	}
	return changed
}

func removeEvidenceForBundleBudget(view *verifiedBundleView) bool {
	ids := make([]string, 0, len(view.EvidenceLookup))
	for id := range view.EvidenceLookup {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if bundleEvidenceReferenced(view, id) {
			continue
		}
		delete(view.EvidenceLookup, id)
		view.EvidenceOmissions = appendEvidenceOmission(view.EvidenceOmissions, evidenceOmissionView{EvidenceID: id, Reason: "bundle_budget"})
		return true
	}
	if len(ids) == 0 {
		return false
	}
	id := ids[len(ids)-1]
	delete(view.EvidenceLookup, id)
	view.EvidenceOmissions = appendEvidenceOmission(view.EvidenceOmissions, evidenceOmissionView{
		EvidenceID: id,
		Reason:     "bundle_budget",
	})
	filterBundleEvidence(view)
	return true
}

func bundleEvidenceReferenced(view *verifiedBundleView, evidenceID string) bool {
	for _, claim := range view.SupportedClaims {
		for _, ref := range claim.Evidence {
			if ref.EvidenceID == evidenceID {
				return true
			}
		}
	}
	for _, claim := range view.PartialClaims {
		for _, ref := range claim.Evidence {
			if ref.EvidenceID == evidenceID {
				return true
			}
		}
	}
	return false
}

func filterBundleEvidence(view *verifiedBundleView) {
	filterClaims := func(claims []supportedClaimView) []supportedClaimView {
		filtered := claims[:0]
		for _, claim := range claims {
			refs := claim.Evidence[:0]
			for _, ref := range claim.Evidence {
				if _, ok := view.EvidenceLookup[ref.EvidenceID]; ok {
					refs = append(refs, ref)
				}
			}
			if len(refs) == 0 {
				view.Omissions.Claims++
				continue
			}
			claim.Evidence = refs
			filtered = append(filtered, claim)
		}
		return filtered
	}
	view.SupportedClaims = filterClaims(view.SupportedClaims)
	view.PartialClaims = filterClaims(view.PartialClaims)
}

func appendEvidenceOmission(existing []evidenceOmissionView, omission evidenceOmissionView) []evidenceOmissionView {
	for _, item := range existing {
		if item.EvidenceID == omission.EvidenceID {
			return existing
		}
	}
	return append(existing, omission)
}

func trimBundleClaimText(view *verifiedBundleView, maxTokens int) bool {
	changed := false
	for index := range view.SupportedClaims {
		claim := &view.SupportedClaims[index]
		trimmed := tooloutput.TruncateContent(claim.Claim, maxTokens)
		if trimmed != claim.Claim {
			claim.Claim = trimmed
			changed = true
		}
	}
	for index := range view.PartialClaims {
		claim := &view.PartialClaims[index]
		trimmed := tooloutput.TruncateContent(claim.Claim, maxTokens)
		if trimmed != claim.Claim {
			claim.Claim = trimmed
			changed = true
		}
	}
	return changed
}

func dropLastBundleClaim(view *verifiedBundleView) bool {
	if len(view.PartialClaims) > 0 {
		view.PartialClaims = view.PartialClaims[:len(view.PartialClaims)-1]
		view.Omissions.Claims++
		return true
	}
	if len(view.SupportedClaims) > 0 {
		view.SupportedClaims = view.SupportedClaims[:len(view.SupportedClaims)-1]
		view.Omissions.Claims++
		return true
	}
	if len(view.UnsupportedClaims) > 0 {
		view.UnsupportedClaims = view.UnsupportedClaims[:len(view.UnsupportedClaims)-1]
		view.Omissions.Claims++
		return true
	}
	return false
}

func unsupportedClaim(claim VerifiedClaim, goalIDs []string, findingIndex int) unsupportedClaimView {
	return unsupportedClaimView{
		ProducerNodeID:  firstNonEmpty(claim.VerifierTaskID, fallbackProducerNodeID),
		FindingIndex:    findingIndex,
		EvidenceGoalIDs: append([]string(nil), goalIDs...),
		Support:         "unsupported",
		HighRisk:        false,
		ReasonCode:      unsupportedReasonCode,
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
	goals := make([]synthesisGoalView, 0, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
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
