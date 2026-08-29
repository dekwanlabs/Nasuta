package investigation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

// ErrOpaqueEvidence marks model or adapter output that contains only an internal identifier.
var ErrOpaqueEvidence = errors.New("evidence content is an opaque identifier")

// EvidenceLedger admits normalized evidence once and preserves conflicting units.
type EvidenceLedger struct {
	mu    sync.RWMutex
	items map[string]EvidenceUnit
}

func NewEvidenceLedger() *EvidenceLedger {
	return &EvidenceLedger{items: make(map[string]EvidenceUnit)}
}

// NewEvidenceLedgerFrom restores a persisted evidence snapshot. The units are
// already normalized and must be inserted without re-validating their content.
func NewEvidenceLedgerFrom(units []EvidenceUnit) *EvidenceLedger {
	ledger := NewEvidenceLedger()
	for _, unit := range units {
		ledger.items[unit.ID] = cloneEvidence(unit)
	}
	return ledger
}

func (ledger *EvidenceLedger) Admit(taskID string, candidate EvidenceCandidate) (EvidenceUnit, bool, error) {
	if ledger == nil {
		return EvidenceUnit{}, false, fmt.Errorf("evidence ledger is required")
	}
	unit, err := normalizeEvidence(taskID, candidate)
	if err != nil {
		return EvidenceUnit{}, false, err
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if existing, ok := ledger.items[unit.ID]; ok {
		return cloneEvidence(existing), false, nil
	}
	ledger.items[unit.ID] = unit
	return cloneEvidence(unit), true, nil
}

// AdmitSeed admits caller-provided identity-only evidence. Unlike Admit, it
// does not require content because seed evidence is already canonicalized at
// the QA boundary and carries only stable identity for downstream references.
func (ledger *EvidenceLedger) AdmitSeed(taskID string, unit EvidenceUnit) (EvidenceUnit, bool, error) {
	if ledger == nil {
		return EvidenceUnit{}, false, fmt.Errorf("evidence ledger is required")
	}
	normalized, err := normalizeSeedEvidence(taskID, unit)
	if err != nil {
		return EvidenceUnit{}, false, err
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if existing, ok := ledger.items[normalized.ID]; ok {
		return cloneEvidence(existing), false, nil
	}
	ledger.items[normalized.ID] = normalized
	return cloneEvidence(normalized), true, nil
}

func (ledger *EvidenceLedger) Get(id string) (EvidenceUnit, bool) {
	if ledger == nil {
		return EvidenceUnit{}, false
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	unit, ok := ledger.items[id]
	return cloneEvidence(unit), ok
}

func (ledger *EvidenceLedger) All() []EvidenceUnit {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	ids := make([]string, 0, len(ledger.items))
	for id := range ledger.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]EvidenceUnit, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneEvidence(ledger.items[id]))
	}
	return out
}

// HasTask reports whether a task owns at least one admitted evidence unit.
// Ownership keeps a sibling task from satisfying another task's partial result.
func (ledger *EvidenceLedger) HasTask(taskID string) bool {
	if ledger == nil || strings.TrimSpace(taskID) == "" {
		return false
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	for _, unit := range ledger.items {
		if unit.TaskID == taskID {
			return true
		}
	}
	return false
}

// Conflicts preserves competing units with the same full evidence identity.
// Independent sources, versions, and time ranges remain separate provenance.
func (ledger *EvidenceLedger) Conflicts() []agentapi.EvidenceConflict {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	units := make([]EvidenceUnit, 0, len(ledger.items))
	for _, unit := range ledger.items {
		units = append(units, cloneEvidence(unit))
	}
	ledger.mu.RUnlock()
	sort.Slice(units, func(i, j int) bool { return units[i].ID < units[j].ID })

	byIdentity := make(map[string][]EvidenceUnit)
	for _, unit := range units {
		key := evidenceIdentityKey(unit)
		byIdentity[key] = append(byIdentity[key], unit)
	}
	identities := make([]string, 0, len(byIdentity))
	for identity := range byIdentity {
		identities = append(identities, identity)
	}
	sort.Strings(identities)

	conflicts := make([]agentapi.EvidenceConflict, 0)
	for _, identity := range identities {
		group := byIdentity[identity]
		if len(group) < 2 {
			continue
		}
		current := group[0]
		for _, incoming := range group[1:] {
			if current.ContentHash == incoming.ContentHash {
				continue
			}
			conflicts = append(conflicts, agentapi.EvidenceConflict{
				Identity: agentapi.EvidenceIdentity{
					SourceKind: current.SourceKind,
					Target:     current.Target,
					Section:    current.Section,
					Version:    current.Version,
					TimeRange:  current.TimeRange,
				},
				Current:        publicEvidenceUnit(current),
				Incoming:       publicEvidenceUnit(incoming),
				CurrentOrigin:  current.TaskID,
				IncomingOrigin: incoming.TaskID,
			})
		}
	}
	return conflicts
}

func evidenceIdentityKey(unit EvidenceUnit) string {
	return unit.SourceKind + "\x00" + unit.Target + "\x00" +
		unit.Section + "\x00" + unit.Version + "\x00" + unit.TimeRange
}

func publicEvidenceUnit(unit EvidenceUnit) tool.EvidenceUnit {
	sections := make([]string, 0, 1)
	if strings.TrimSpace(unit.Section) != "" {
		sections = append(sections, unit.Section)
	}
	return tool.EvidenceUnit{
		SourceKind:    unit.SourceKind,
		Target:        unit.Target,
		Sections:      sections,
		ContentHash:   unit.ContentHash,
		Facets:        normalizeFacets(unit.Facets),
		TrustTier:     unit.TrustTier,
		EvidenceClass: unit.EvidenceClass,
		Version:       unit.Version,
		TimeRange:     unit.TimeRange,
	}
}

func (ledger *EvidenceLedger) ValidateRef(ref EvidenceRef) error {
	if ledger == nil {
		return fmt.Errorf("%w: evidence ledger is required", ErrEvidenceReference)
	}
	if strings.TrimSpace(ref.EvidenceID) == "" {
		return fmt.Errorf("%w: evidence id is required", ErrEvidenceReference)
	}
	unit, ok := ledger.Get(ref.EvidenceID)
	if !ok {
		return fmt.Errorf("%w: evidence %q was not admitted", ErrEvidenceReference, ref.EvidenceID)
	}
	if ref.SourceKind != "" && ref.SourceKind != unit.SourceKind {
		return fmt.Errorf("%w: evidence %q source kind mismatch", ErrEvidenceReference, ref.EvidenceID)
	}
	if ref.Target != "" && ref.Target != unit.Target {
		return fmt.Errorf("%w: evidence %q target mismatch", ErrEvidenceReference, ref.EvidenceID)
	}
	if ref.Section != "" && ref.Section != unit.Section {
		return fmt.Errorf("%w: evidence %q section mismatch", ErrEvidenceReference, ref.EvidenceID)
	}
	if ref.Version != "" && ref.Version != unit.Version {
		return fmt.Errorf("%w: evidence %q version mismatch", ErrEvidenceReference, ref.EvidenceID)
	}
	if ref.TimeRange != "" && ref.TimeRange != unit.TimeRange {
		return fmt.Errorf("%w: evidence %q time range mismatch", ErrEvidenceReference, ref.EvidenceID)
	}
	if ref.ContentHash != "" && ref.ContentHash != unit.ContentHash {
		return fmt.Errorf("%w: evidence %q content hash mismatch", ErrEvidenceReference, ref.EvidenceID)
	}
	return nil
}

// ClaimLedger contains only verifier-owned claims. Composer never receives raw candidates.
type ClaimLedger struct {
	mu       sync.RWMutex
	claims   map[string]VerifiedClaim
	goals    map[string]EvidenceGoal
	evidence *EvidenceLedger
}

func NewClaimLedger(goals []EvidenceGoal, evidence *EvidenceLedger) *ClaimLedger {
	goalMap := make(map[string]EvidenceGoal, len(goals))
	for _, goal := range goals {
		goalMap[goal.ID] = goal
	}
	return &ClaimLedger{
		claims:   make(map[string]VerifiedClaim),
		goals:    goalMap,
		evidence: evidence,
	}
}

// NewClaimLedgerFrom restores a persisted claim snapshot. Claims are trusted
// because they were admitted by the verifier before persistence.
func NewClaimLedgerFrom(
	goals []EvidenceGoal,
	evidence *EvidenceLedger,
	claims []VerifiedClaim,
) *ClaimLedger {
	ledger := NewClaimLedger(goals, evidence)
	for _, claim := range claims {
		ledger.claims[claim.ID] = cloneClaim(claim)
	}
	return ledger
}

func (ledger *ClaimLedger) Admit(taskID string, candidate ClaimCandidate) (VerifiedClaim, bool, error) {
	if ledger == nil || ledger.evidence == nil {
		return VerifiedClaim{}, false, fmt.Errorf("claim and evidence ledgers are required")
	}
	candidate.GoalID = strings.TrimSpace(candidate.GoalID)
	candidate.Text = strings.TrimSpace(candidate.Text)
	if !isUserReadableClaimText(candidate.Text) {
		return VerifiedClaim{}, false, fmt.Errorf("%w: claim text is an opaque identifier", ErrEvidenceReference)
	}
	_, ok := ledger.goals[candidate.GoalID]
	if !ok {
		return VerifiedClaim{}, false, fmt.Errorf("%w: unknown goal %q", ErrEvidenceReference, candidate.GoalID)
	}
	if candidate.Text == "" {
		return VerifiedClaim{}, false, fmt.Errorf("%w: claim text is empty", ErrEvidenceReference)
	}
	switch candidate.Status {
	case ClaimSupported, ClaimPartial, ClaimConflicting:
	default:
		return VerifiedClaim{}, false, fmt.Errorf("%w: claim status %q is not verifier-owned", ErrEvidenceReference, candidate.Status)
	}
	if candidate.Confidence < 0 || candidate.Confidence > 1 || candidate.Confidence != candidate.Confidence {
		return VerifiedClaim{}, false, fmt.Errorf("%w: claim confidence must be between 0 and 1", ErrEvidenceReference)
	}
	normalizedEvidenceRefs, err := normalizeClaimRefs(ledger.evidence, candidate.EvidenceRefs)
	if err != nil {
		return VerifiedClaim{}, false, err
	}
	candidate.EvidenceRefs = normalizedEvidenceRefs
	normalizedConflictRefs, err := normalizeClaimRefs(ledger.evidence, candidate.ConflictRefs)
	if err != nil {
		return VerifiedClaim{}, false, err
	}
	candidate.ConflictRefs = normalizedConflictRefs
	if len(candidate.EvidenceRefs) == 0 {
		return VerifiedClaim{}, false, fmt.Errorf("%w: claim %q has no evidence", ErrEvidenceReference, candidate.Text)
	}
	if candidate.Status == ClaimSupported && (len(candidate.ConflictRefs) > 0 ||
		hasConflictingEvidence(ledger.evidence, append(append([]EvidenceRef(nil), candidate.EvidenceRefs...), candidate.ConflictRefs...))) {
		candidate.Status = ClaimConflicting
	}
	if candidate.Status == ClaimConflicting && len(candidate.ConflictRefs) == 0 {
		candidate.ConflictRefs = conflictingRefs(ledger.evidence, candidate.EvidenceRefs)
		if len(candidate.ConflictRefs) == 0 {
			return VerifiedClaim{}, false, fmt.Errorf("%w: conflicting claim %q has no conflicting evidence", ErrEvidenceReference, candidate.Text)
		}
	}
	claim := VerifiedClaim{
		ID:             claimID(candidate),
		GoalID:         candidate.GoalID,
		Text:           candidate.Text,
		Status:         candidate.Status,
		EvidenceRefs:   cloneEvidenceRefs(candidate.EvidenceRefs),
		Confidence:     candidate.Confidence,
		ConflictRefs:   cloneEvidenceRefs(candidate.ConflictRefs),
		VerifierTaskID: taskID,
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if existing, ok := ledger.claims[claim.ID]; ok {
		merged := mergeVerifiedClaims(ledger.evidence, existing, claim)
		ledger.claims[claim.ID] = merged
		return cloneClaim(merged), false, nil
	}
	ledger.claims[claim.ID] = claim
	return cloneClaim(claim), true, nil
}

func (ledger *ClaimLedger) All() []VerifiedClaim {
	if ledger == nil {
		return nil
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	ids := make([]string, 0, len(ledger.claims))
	for id := range ledger.claims {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]VerifiedClaim, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneClaim(ledger.claims[id]))
	}
	return out
}

// HasTask reports whether a task owns at least one admitted verifier claim.
func (ledger *ClaimLedger) HasTask(taskID string) bool {
	if ledger == nil || strings.TrimSpace(taskID) == "" {
		return false
	}
	ledger.mu.RLock()
	defer ledger.mu.RUnlock()
	for _, claim := range ledger.claims {
		if claim.VerifierTaskID == taskID {
			return true
		}
	}
	return false
}

func (ledger *ClaimLedger) Coverage() []GoalCoverage {
	if ledger == nil {
		return nil
	}
	claims := ledger.All()
	byGoal := make(map[string][]VerifiedClaim, len(ledger.goals))
	for _, claim := range claims {
		byGoal[claim.GoalID] = append(byGoal[claim.GoalID], claim)
	}
	goalIDs := make([]string, 0, len(ledger.goals))
	for id := range ledger.goals {
		goalIDs = append(goalIDs, id)
	}
	sort.Strings(goalIDs)
	coverage := make([]GoalCoverage, 0, len(goalIDs))
	for _, goalID := range goalIDs {
		goal := ledger.goals[goalID]
		goalClaims := byGoal[goalID]
		claimIDs := make([]string, 0, len(goalClaims))
		conflict := false
		partial := false
		supported := false
		coveredFacets := make(map[string]struct{})
		coveredSources := make(map[string]struct{})
		for _, claim := range goalClaims {
			claimIDs = append(claimIDs, claim.ID)
			if claim.Status == ClaimConflicting {
				conflict = true
			}
			if claim.Status == ClaimPartial {
				partial = true
			}
			if claim.Status == ClaimSupported {
				supported = true
			}
			for _, ref := range claim.EvidenceRefs {
				unit, ok := ledger.evidence.Get(ref.EvidenceID)
				if !ok {
					continue
				}
				coveredSources[unit.SourceKind] = struct{}{}
				for _, facet := range unit.Facets {
					if facet = strings.TrimSpace(facet); facet != "" {
						coveredFacets[facet] = struct{}{}
					}
				}
			}
		}
		missingFacets := missingStrings(goal.Facets, coveredFacets)
		missingSources := missingSourceStrings(goal.RequiredSources, coveredSources)
		status := GoalUnresolved
		if len(goalClaims) > 0 && (conflict || partial || len(missingFacets) > 0 || len(missingSources) > 0) {
			status = GoalPartial
		} else if supported {
			if goal.HighRisk && goal.MinimumCoverage > 0 && len(goalClaims) < goal.MinimumCoverage {
				status = GoalPartial
			} else {
				status = GoalCovered
			}
		}
		coverage = append(coverage, GoalCoverage{
			GoalID: goalID, Required: goal.Required, Status: status,
			ClaimIDs: claimIDs, MissingFacets: missingFacets, MissingSources: missingSources,
		})
	}
	return coverage
}

func missingStrings(required []string, covered map[string]struct{}) []string {
	missing := make([]string, 0, len(required))
	seen := make(map[string]struct{}, len(required))
	for _, value := range required {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		if _, ok := covered[value]; !ok {
			missing = append(missing, value)
		}
	}
	sort.Strings(missing)
	return missing
}

func missingSourceStrings(required []agentapi.EvidenceSource, covered map[string]struct{}) []string {
	values := make([]string, 0, len(required))
	for _, source := range required {
		values = append(values, string(source))
	}
	return missingStrings(values, covered)
}

func normalizeClaimRefs(evidence *EvidenceLedger, refs []EvidenceRef) ([]EvidenceRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	byID := make(map[string]EvidenceRef, len(refs))
	for _, ref := range refs {
		ref.EvidenceID = strings.TrimSpace(ref.EvidenceID)
		ref.SourceKind = strings.TrimSpace(ref.SourceKind)
		ref.Target = strings.TrimSpace(ref.Target)
		ref.Section = strings.TrimSpace(ref.Section)
		ref.Version = strings.TrimSpace(ref.Version)
		ref.TimeRange = strings.TrimSpace(ref.TimeRange)
		ref.ContentHash = strings.TrimSpace(ref.ContentHash)
		if err := evidence.ValidateRef(ref); err != nil {
			return nil, err
		}
		unit, ok := evidence.Get(ref.EvidenceID)
		if !ok {
			return nil, fmt.Errorf("%w: evidence %q was not admitted", ErrEvidenceReference, ref.EvidenceID)
		}
		// Persist the complete admitted identity so later merges cannot replace
		// rich provenance with a sparse reference for the same EvidenceID.
		ref.SourceKind = unit.SourceKind
		ref.Target = unit.Target
		ref.Section = unit.Section
		ref.Version = unit.Version
		ref.TimeRange = unit.TimeRange
		ref.ContentHash = unit.ContentHash
		byID[ref.EvidenceID] = ref
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]EvidenceRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out, nil
}

func hasConflictingEvidence(evidence *EvidenceLedger, refs []EvidenceRef) bool {
	if evidence == nil {
		return false
	}
	byIdentity := make(map[string]string)
	for _, ref := range refs {
		unit, ok := evidence.Get(ref.EvidenceID)
		if !ok {
			continue
		}
		identity := evidenceIdentityKey(unit)
		if hash, exists := byIdentity[identity]; exists && hash != unit.ContentHash {
			return true
		}
		byIdentity[identity] = unit.ContentHash
	}
	return false
}

func conflictingRefs(evidence *EvidenceLedger, refs []EvidenceRef) []EvidenceRef {
	if evidence == nil {
		return nil
	}
	byIdentity := make(map[string][]EvidenceRef)
	for _, ref := range refs {
		unit, ok := evidence.Get(ref.EvidenceID)
		if ok {
			byIdentity[evidenceIdentityKey(unit)] = append(byIdentity[evidenceIdentityKey(unit)], ref)
		}
	}
	out := make([]EvidenceRef, 0)
	for _, group := range byIdentity {
		hashes := make(map[string]struct{})
		for _, ref := range group {
			unit, _ := evidence.Get(ref.EvidenceID)
			hashes[unit.ContentHash] = struct{}{}
		}
		if len(hashes) > 1 {
			out = append(out, group...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EvidenceID < out[j].EvidenceID })
	return out
}

func mergeVerifiedClaims(evidence *EvidenceLedger, existing, incoming VerifiedClaim) VerifiedClaim {
	merged := existing
	merged.EvidenceRefs = mergeEvidenceRefs(existing.EvidenceRefs, incoming.EvidenceRefs)
	merged.ConflictRefs = mergeEvidenceRefs(existing.ConflictRefs, incoming.ConflictRefs)
	if claimStatusRank(incoming.Status) > claimStatusRank(merged.Status) {
		merged.Status = incoming.Status
	}
	if merged.Status == ClaimSupported && (len(merged.ConflictRefs) > 0 ||
		hasConflictingEvidence(evidence, append(append([]EvidenceRef(nil), merged.EvidenceRefs...), merged.ConflictRefs...))) {
		merged.Status = ClaimConflicting
	}
	if merged.Confidence == 0 || (incoming.Confidence > 0 && incoming.Confidence < merged.Confidence) {
		merged.Confidence = incoming.Confidence
	}
	return merged
}

func mergeEvidenceRefs(left, right []EvidenceRef) []EvidenceRef {
	byID := make(map[string]EvidenceRef, len(left)+len(right))
	for _, ref := range append(append([]EvidenceRef(nil), left...), right...) {
		ref.EvidenceID = strings.TrimSpace(ref.EvidenceID)
		if ref.EvidenceID == "" {
			continue
		}
		byID[ref.EvidenceID] = ref
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]EvidenceRef, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

func claimStatusRank(status ClaimStatus) int {
	switch status {
	case ClaimSupported:
		return 1
	case ClaimPartial:
		return 2
	case ClaimConflicting:
		return 3
	default:
		return 0
	}
}

func BuildReport(evidence *EvidenceLedger, claims *ClaimLedger, failures []RunFailure) InvestigationReport {
	report := InvestigationReport{}
	if evidence != nil {
		report.Evidence = evidence.All()
	}
	if claims != nil {
		report.Claims = claims.All()
		report.Coverage = claims.Coverage()
		if evidence != nil {
			report.EvidenceConflicts = evidence.Conflicts()
		}
		for _, coverage := range report.Coverage {
			if coverage.Status == GoalCovered {
				continue
			}
			report.Gaps = append(report.Gaps, EvidenceGap{
				GoalID:         coverage.GoalID,
				Reason:         gapReason(coverage.Status),
				MissingFacets:  append([]string(nil), coverage.MissingFacets...),
				MissingSources: append([]string(nil), coverage.MissingSources...),
			})
		}
	}
	report.Failures = append([]RunFailure(nil), failures...)
	return report
}

// PruneUnreferencedEvidence drops admitted evidence units that no claim cites.
// It keeps every traceability relationship intact while reducing the model
// context sent to downstream composition.
func PruneUnreferencedEvidence(report InvestigationReport) InvestigationReport {
	referenced := make(map[string]struct{}, len(report.Evidence))
	for _, claim := range report.Claims {
		for _, ref := range append(append([]EvidenceRef(nil), claim.EvidenceRefs...), claim.ConflictRefs...) {
			referenced[ref.EvidenceID] = struct{}{}
		}
	}
	if len(referenced) == 0 {
		report.Evidence = nil
		return report
	}
	evidence := make([]EvidenceUnit, 0, len(referenced))
	for _, unit := range report.Evidence {
		if _, ok := referenced[unit.ID]; ok {
			evidence = append(evidence, unit)
		}
	}
	report.Evidence = evidence
	return report
}

func normalizeFacets(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func normalizeEvidence(taskID string, candidate EvidenceCandidate) (EvidenceUnit, error) {
	candidate.SourceKind = strings.TrimSpace(candidate.SourceKind)
	candidate.Target = strings.TrimSpace(candidate.Target)
	candidate.Section = strings.TrimSpace(candidate.Section)
	candidate.Content = strings.TrimSpace(candidate.Content)
	candidate.ContentHash = strings.TrimSpace(candidate.ContentHash)
	candidate.Version = strings.TrimSpace(candidate.Version)
	candidate.TimeRange = strings.TrimSpace(candidate.TimeRange)
	if candidate.SourceKind == "" || candidate.Target == "" || candidate.Content == "" {
		return EvidenceUnit{}, fmt.Errorf("evidence source, target, and content are required")
	}
	if !isReadableEvidenceContent(candidate.Content) {
		return EvidenceUnit{}, fmt.Errorf("%w: content cannot be admitted as evidence", ErrOpaqueEvidence)
	}
	hash := sha256.Sum256([]byte(candidate.Content))
	computedHash := hex.EncodeToString(hash[:])
	if candidate.ContentHash == "" {
		candidate.ContentHash = computedHash
	} else if candidate.ContentHash != computedHash {
		return EvidenceUnit{}, fmt.Errorf("evidence content hash does not match content")
	}
	return EvidenceUnit{
		ID:            evidenceID(candidate),
		SourceKind:    candidate.SourceKind,
		Target:        candidate.Target,
		Section:       candidate.Section,
		Content:       candidate.Content,
		ContentHash:   candidate.ContentHash,
		Facets:        normalizeFacets(candidate.Facets),
		TrustTier:     candidate.TrustTier,
		EvidenceClass: candidate.EvidenceClass,
		Version:       candidate.Version,
		TimeRange:     candidate.TimeRange,
		TaskID:        taskID,
	}, nil
}

func evidenceCandidatesForTask(units []EvidenceUnit, taskID string) []EvidenceCandidate {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" || len(units) == 0 {
		return nil
	}
	candidates := make([]EvidenceCandidate, 0)
	for _, unit := range units {
		if unit.TaskID != taskID || !isReadableEvidenceContent(unit.Content) {
			continue
		}
		candidates = append(candidates, EvidenceCandidate{
			SourceKind:    unit.SourceKind,
			Target:        unit.Target,
			Section:       unit.Section,
			Content:       unit.Content,
			ContentHash:   unit.ContentHash,
			Facets:        append([]string(nil), unit.Facets...),
			TrustTier:     unit.TrustTier,
			EvidenceClass: unit.EvidenceClass,
			Version:       unit.Version,
			TimeRange:     unit.TimeRange,
		})
	}
	return candidates
}

func normalizeSeedEvidence(taskID string, unit EvidenceUnit) (EvidenceUnit, error) {
	unit.SourceKind = strings.TrimSpace(unit.SourceKind)
	unit.Target = strings.TrimSpace(unit.Target)
	unit.Section = strings.TrimSpace(unit.Section)
	unit.ContentHash = strings.TrimSpace(unit.ContentHash)
	unit.Version = strings.TrimSpace(unit.Version)
	unit.TimeRange = strings.TrimSpace(unit.TimeRange)
	unit.Content = ""
	if unit.SourceKind == "" || unit.Target == "" {
		return EvidenceUnit{}, fmt.Errorf("seed evidence source and target are required")
	}
	return EvidenceUnit{
		ID: evidenceID(EvidenceCandidate{
			SourceKind: unit.SourceKind, Target: unit.Target,
			Section: unit.Section, Version: unit.Version,
			TimeRange: unit.TimeRange, ContentHash: unit.ContentHash,
		}),
		SourceKind:    unit.SourceKind,
		Target:        unit.Target,
		Section:       unit.Section,
		ContentHash:   unit.ContentHash,
		Facets:        append([]string(nil), unit.Facets...),
		TrustTier:     unit.TrustTier,
		EvidenceClass: unit.EvidenceClass,
		Version:       unit.Version,
		TimeRange:     unit.TimeRange,
		TaskID:        taskID,
	}, nil
}

// UserReadableClaimText reports whether text may enter ClaimLedger or be
// treated as a user-visible finding. Tool payloads are rejected even when
// truncated so they cannot become verifier or composer input.
func UserReadableClaimText(text string) bool {
	return isUserReadableClaimText(text)
}

func isUserReadableClaimText(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || containsOpaqueIdentifier(text) {
		return false
	}
	// Adapter/tool payloads start with an object or array. Complete JSON is
	// rejected by json.Valid; truncated dumps such as `{"matches":[{"docId"`
	// are not valid JSON and must still be rejected. Natural-language claims
	// may mention JSON later in the sentence.
	if looksLikeMachineJSONPayload(text) {
		return false
	}
	return true
}

func looksLikeMachineJSONPayload(text string) bool {
	if text == "" {
		return false
	}
	switch text[0] {
	case '{', '[':
		return true
	default:
		return false
	}
}

// isReadableEvidenceContent rejects identity-only payloads while allowing
// authoritative text that also contains ordinary metadata such as evidence IDs.
func isReadableEvidenceContent(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || isOpaqueIdentifier(text) {
		return false
	}
	if json.Valid([]byte(text)) {
		var value any
		if err := json.Unmarshal([]byte(text), &value); err == nil {
			return hasReadableJSONValue(value)
		}
	}
	for _, token := range strings.Fields(text) {
		token = strings.Trim(token, `.,;:!?()[]{}\"'<>`)
		if token != "" && !isOpaqueIdentifier(token) {
			return true
		}
	}
	return false
}

func hasReadableJSONValue(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case string:
		return isReadableEvidenceContent(value)
	case []any:
		for _, item := range value {
			if hasReadableJSONValue(item) {
				return true
			}
		}
		return false
	case map[string]any:
		for _, item := range value {
			if hasReadableJSONValue(item) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func containsOpaqueIdentifier(text string) bool {
	for _, token := range strings.Fields(text) {
		token = strings.Trim(token, `.,;:!?()[]{}\"'<>`)
		if isOpaqueIdentifier(token) {
			return true
		}
	}
	return false
}

func isOpaqueIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if isHexString(value) && (len(value) == 64 || len(value) >= 32) {
		return true
	}
	for _, prefix := range []string{"evidence_", "claim_", "workflow_", "run_", "reservation_"} {
		if strings.HasPrefix(value, prefix) {
			suffix := strings.TrimPrefix(value, prefix)
			// Canonical evidence/claim handles are now 8 hex chars; keep a
			// floor low enough to catch them leaking into claim text while
			// still ignoring short human-readable words.
			if isHexString(suffix) && len(suffix) >= 8 {
				return true
			}
		}
	}
	if len(value) == 36 {
		for index, char := range value {
			if index == 8 || index == 13 || index == 18 || index == 23 {
				if char != '-' {
					return false
				}
				continue
			}
			if !isHexRune(char) {
				return false
			}
		}
		return true
	}
	return false
}

func isHexString(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !isHexRune(char) {
			return false
		}
	}
	return true
}

func isHexRune(char rune) bool {
	return (char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')
}

func gapReason(status GoalCoverageStatus) string {
	if status == GoalPartial {
		return "only partial or conflicting claims were verified"
	}
	return "no verified claim covers this goal"
}

func cloneEvidence(value EvidenceUnit) EvidenceUnit {
	value.Facets = append([]string(nil), value.Facets...)
	return value
}

func cloneClaim(value VerifiedClaim) VerifiedClaim {
	value.EvidenceRefs = cloneEvidenceRefs(value.EvidenceRefs)
	value.ConflictRefs = cloneEvidenceRefs(value.ConflictRefs)
	return value
}

func cloneEvidenceRefs(refs []EvidenceRef) []EvidenceRef {
	return append([]EvidenceRef(nil), refs...)
}
