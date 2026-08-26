package qa

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/tool"
)

const defaultInvestigationMaxRounds = 3

// InvestigationRoundContext is the coordinator's bounded progress cursor.
// It is derived from durable child facts and does not need its own state table.
type InvestigationRoundContext struct {
	ParentRunID             string
	Objective               string
	Round                   int
	MaxRounds               int
	CoveredEvidenceGoals    []string
	UnresolvedEvidenceGoals []string
	PreviousWorkflowRunID   string
	RemainingBudget         int64
}

// ShouldContinue reports whether another bounded child workflow is eligible.
func ShouldContinue(context InvestigationRoundContext) bool {
	maxRounds := context.MaxRounds
	if maxRounds <= 0 {
		maxRounds = defaultInvestigationMaxRounds
	}
	unresolved := context.UnresolvedEvidenceGoals
	return context.Round > 0 &&
		context.Round < maxRounds &&
		len(unresolved) > 0 &&
		context.RemainingBudget >= 0
}

// NextRound derives the next coordinator cursor from one verified result.
func NextRound(
	context InvestigationRoundContext,
	result InvestigationResult,
) InvestigationRoundContext {
	next := context
	next.Round++
	next.CoveredEvidenceGoals = appendUniqueStrings(
		append([]string(nil), context.CoveredEvidenceGoals...),
		result.PartialEvidenceGoals...,
	)
	for _, claim := range result.SupportedClaims {
		next.CoveredEvidenceGoals = appendUniqueStrings(next.CoveredEvidenceGoals, claim.EvidenceGoalIDs...)
	}
	next.UnresolvedEvidenceGoals = uniqueStrings(result.UnresolvedEvidenceGoals)
	if len(next.UnresolvedEvidenceGoals) == 0 {
		next.UnresolvedEvidenceGoals = uniqueStrings(result.PartialEvidenceGoals)
	}
	return next
}

// continuationContract narrows a child contract to the goals that remain
// unresolved. A mismatched verifier goal is not safe to reinterpret as the
// original full contract, so it disables continuation instead.
func continuationContract(
	previous TaskContract,
	result InvestigationResult,
) (TaskContract, bool) {
	unresolved := uniqueStrings(result.UnresolvedEvidenceGoals)
	if len(unresolved) == 0 {
		return TaskContract{}, false
	}
	allowed := make(map[string]struct{}, len(previous.EvidenceGoals))
	for _, goal := range previous.EvidenceGoals {
		allowed[goal.ID] = struct{}{}
	}
	for _, goalID := range unresolved {
		if _, ok := allowed[goalID]; !ok {
			return TaskContract{}, false
		}
	}

	next := cloneTaskContract(previous)
	next.EvidenceGoals = make([]EvidenceGoal, 0, len(unresolved))
	unresolvedSet := make(map[string]struct{}, len(unresolved))
	for _, goalID := range unresolved {
		unresolvedSet[goalID] = struct{}{}
	}
	for _, goal := range previous.EvidenceGoals {
		if _, ok := allowed[goal.ID]; !ok {
			continue
		}
		if _, ok := unresolvedSet[goal.ID]; !ok {
			continue
		}
		next.EvidenceGoals = append(next.EvidenceGoals, cloneEvidenceGoal(goal))
	}
	if len(next.EvidenceGoals) == 0 {
		return TaskContract{}, false
	}

	// Evidence gaps do not identify investigation deliverables. New results
	// therefore preserve all admitted investigation goals while removing
	// dependency edges that could refer to evidence already covered.
	if result.PartialInvestigationGoals != nil || result.UnresolvedInvestigationGoals != nil {
		allowedInvestigation := make(map[string]struct{}, len(result.PartialInvestigationGoals)+len(result.UnresolvedInvestigationGoals))
		for _, goalID := range append(append([]string(nil), result.PartialInvestigationGoals...), result.UnresolvedInvestigationGoals...) {
			if goalID = strings.TrimSpace(goalID); goalID != "" {
				allowedInvestigation[goalID] = struct{}{}
			}
		}
		next.InvestigationGoals = next.InvestigationGoals[:0]
		for _, goal := range previous.InvestigationGoals {
			if _, ok := allowedInvestigation[goal.ID]; !ok {
				continue
			}
			goal.DependsOn = []string{}
			next.InvestigationGoals = append(next.InvestigationGoals, goal)
		}
	} else {
		for index := range next.InvestigationGoals {
			next.InvestigationGoals[index].DependsOn = []string{}
		}
	}
	next.TaskEvidenceAssignments = nil
	return next, true
}

func cloneTaskContract(contract TaskContract) TaskContract {
	contract.Entities = append([]EntityRef(nil), contract.Entities...)
	for index := range contract.Entities {
		contract.Entities[index].Aliases = append(
			[]string(nil), contract.Entities[index].Aliases...,
		)
	}
	contract.InvestigationGoals = append(
		[]InvestigationGoal(nil), contract.InvestigationGoals...,
	)
	for index := range contract.InvestigationGoals {
		contract.InvestigationGoals[index].DependsOn = append(
			[]string(nil), contract.InvestigationGoals[index].DependsOn...,
		)
	}
	contract.EvidenceGoals = append([]EvidenceGoal(nil), contract.EvidenceGoals...)
	for index := range contract.EvidenceGoals {
		contract.EvidenceGoals[index] = cloneEvidenceGoal(contract.EvidenceGoals[index])
	}
	contract.TaskEvidenceAssignments = append(
		[]TaskEvidenceAssignment(nil), contract.TaskEvidenceAssignments...,
	)
	contract.Context.ConversationRefs = append(
		[]ConversationRef(nil), contract.Context.ConversationRefs...,
	)
	contract.Context.SeedMaterial = cloneContextBlocks(contract.Context.SeedMaterial)
	if contract.Context.TimeRange != nil {
		timeRange := *contract.Context.TimeRange
		contract.Context.TimeRange = &timeRange
	}
	return contract
}

func cloneEvidenceGoal(goal EvidenceGoal) EvidenceGoal {
	goal.Facets = append([]string(nil), goal.Facets...)
	goal.Sources = append([]agentapi.EvidenceSource(nil), goal.Sources...)
	goal.RequiredSources = append(
		[]agentapi.EvidenceSource(nil), goal.RequiredSources...,
	)
	return goal
}

// StableRoundWorkflowID gives retries of the same round the same child ID.
func StableRoundWorkflowID(parentRunID string, round int) string {
	parentRunID = strings.TrimSpace(parentRunID)
	if parentRunID == "" {
		parentRunID = "parent"
	}
	slug := make([]byte, 0, len(parentRunID))
	lastSeparator := false
	for index := 0; index < len(parentRunID); index++ {
		value := parentRunID[index]
		valid := value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
		if valid {
			if value >= 'A' && value <= 'Z' {
				value += 'a' - 'A'
			}
			slug = append(slug, value)
			lastSeparator = false
			continue
		}
		if !lastSeparator {
			slug = append(slug, '_')
			lastSeparator = true
		}
	}
	for len(slug) > 0 && slug[len(slug)-1] == '_' {
		slug = slug[:len(slug)-1]
	}
	if len(slug) == 0 {
		slug = []byte("parent")
	}
	if slug[0] >= '0' && slug[0] <= '9' {
		slug = append([]byte("p_"), slug...)
	}
	if len(slug) > 48 {
		digest := sha256.Sum256([]byte(parentRunID))
		slug = append(slug[:32], '_')
		slug = append(slug, hex.EncodeToString(digest[:])[:12]...)
	}
	if round <= 0 {
		round = 1
	}
	return "workflow_" + string(slug) + "_round_" + strconv.Itoa(round)
}

// EvidenceIdentity returns the canonical identity used across rounds.
func EvidenceIdentity(unit tool.EvidenceUnit) (agentapi.EvidenceIdentity, bool) {
	key, ok := evidence.UnitKey(unit)
	if !ok {
		return agentapi.EvidenceIdentity{}, false
	}
	return agentapi.EvidenceIdentity{
		SourceKind: key.SourceKind,
		Target:     key.Target,
		Section:    key.Section,
		Version:    key.Version,
		TimeRange:  key.TimeRange,
	}, true
}

// NewEvidenceRatio measures genuinely new canonical units in the current round.
func NewEvidenceRatio(previous, current []tool.EvidenceUnit) float64 {
	baseline := evidence.New(previous, "previous")
	baselineSet := evidenceUnitSet(baseline)
	if len(current) == 0 {
		return 0
	}
	newUnits := 0
	seen := make(map[string]struct{}, len(current))
	for _, unit := range evidence.Expand(current) {
		key, ok := evidence.UnitKey(unit)
		if !ok {
			continue
		}
		identity := key.String()
		if _, exists := seen[identity]; exists {
			continue
		}
		seen[identity] = struct{}{}
		if _, exists := baselineSet[key]; !exists {
			newUnits++
		}
	}
	uniqueCurrent := len(seen)
	if uniqueCurrent == 0 {
		return 0
	}
	return float64(newUnits) / float64(uniqueCurrent)
}

func evidenceUnitSet(ledger *evidence.Ledger) map[evidence.Key]struct{} {
	units := ledger.Units()
	set := make(map[evidence.Key]struct{}, len(units))
	for _, unit := range units {
		if key, ok := evidence.UnitKey(unit); ok {
			set[key] = struct{}{}
		}
	}
	return set
}

// MergeRoundResult joins historical and current verified facts by stable IDs.
func MergeRoundResult(previous, current InvestigationResult) InvestigationResult {
	merged := cloneInvestigationResult(previous)
	if strings.TrimSpace(current.Answer) != "" {
		merged.Answer = current.Answer
	}
	merged.Citations = mergeCitations(merged.Citations, current.Citations)
	merged.Limitations = uniqueStrings(append(merged.Limitations, current.Limitations...))
	merged.SupportedClaims = mergeClaims(merged.SupportedClaims, current.SupportedClaims)
	merged.PartialClaims = mergeClaims(merged.PartialClaims, current.PartialClaims)
	merged.UnsupportedClaims = mergeUnsupportedClaims(
		merged.UnsupportedClaims,
		current.UnsupportedClaims,
	)
	if current.PartialEvidenceGoals != nil {
		merged.PartialEvidenceGoals = uniqueStrings(current.PartialEvidenceGoals)
	}
	if current.UnresolvedEvidenceGoals != nil {
		merged.UnresolvedEvidenceGoals = uniqueStrings(current.UnresolvedEvidenceGoals)
	}
	if current.PartialInvestigationGoals != nil {
		merged.PartialInvestigationGoals = uniqueStrings(current.PartialInvestigationGoals)
	}
	if current.UnresolvedInvestigationGoals != nil {
		merged.UnresolvedInvestigationGoals = uniqueStrings(current.UnresolvedInvestigationGoals)
	}
	ledger := evidence.New(previous.EvidenceUnits, "previous")
	conflicts := append([]agentapi.EvidenceConflict(nil), previous.EvidenceConflicts...)
	for _, conflict := range ledger.Add(current.EvidenceUnits, "current") {
		conflicts = appendUniqueEvidenceConflict(conflicts, conflictToAPI(conflict))
	}
	merged.EvidenceUnits = ledger.Units()
	for _, conflict := range current.EvidenceConflicts {
		conflicts = appendUniqueEvidenceConflict(conflicts, conflict)
	}
	merged.EvidenceConflicts = conflicts
	copyNonEmptyResultMetadata(&merged, current)
	return merged
}

func cloneInvestigationResult(result InvestigationResult) InvestigationResult {
	result.Citations = append([]InvestigationCitation(nil), result.Citations...)
	result.Limitations = append([]string(nil), result.Limitations...)
	result.SupportedClaims = cloneClaims(result.SupportedClaims)
	result.PartialClaims = cloneClaims(result.PartialClaims)
	result.UnsupportedClaims = append([]InvestigationUnsupportedClaim(nil), result.UnsupportedClaims...)
	result.PartialInvestigationGoals = append([]string(nil), result.PartialInvestigationGoals...)
	result.UnresolvedInvestigationGoals = append([]string(nil), result.UnresolvedInvestigationGoals...)
	result.PartialEvidenceGoals = append([]string(nil), result.PartialEvidenceGoals...)
	result.UnresolvedEvidenceGoals = append([]string(nil), result.UnresolvedEvidenceGoals...)
	result.EvidenceUnits = evidence.CloneUnits(result.EvidenceUnits)
	result.EvidenceConflicts = append([]agentapi.EvidenceConflict(nil), result.EvidenceConflicts...)
	return result
}

func cloneClaims(claims []InvestigationClaim) []InvestigationClaim {
	if len(claims) == 0 {
		return nil
	}
	out := make([]InvestigationClaim, len(claims))
	for index, claim := range claims {
		out[index] = claim
		out[index].EvidenceGoalIDs = append([]string(nil), claim.EvidenceGoalIDs...)
		out[index].EntityIDs = append([]string(nil), claim.EntityIDs...)
		out[index].Evidence = append([]InvestigationEvidence(nil), claim.Evidence...)
		out[index].EvidenceIdentities = append([]agentapi.EvidenceIdentity(nil), claim.EvidenceIdentities...)
	}
	return out
}

func mergeClaims(left, right []InvestigationClaim) []InvestigationClaim {
	out := cloneClaims(left)
	indexes := make(map[string]int, len(out)+len(right))
	for index, claim := range out {
		indexes[claimKey(claim)] = index
	}
	for _, claim := range right {
		key := claimKey(claim)
		if index, exists := indexes[key]; exists {
			out[index] = mergeClaim(out[index], claim)
			continue
		}
		indexes[key] = len(out)
		out = append(out, cloneClaims([]InvestigationClaim{claim})[0])
	}
	return out
}

func mergeClaim(previous, current InvestigationClaim) InvestigationClaim {
	merged := current
	if strings.TrimSpace(merged.ProducerNodeID) == "" {
		merged.ProducerNodeID = previous.ProducerNodeID
	}
	if merged.FindingIndex == 0 && previous.FindingIndex != 0 {
		merged.FindingIndex = previous.FindingIndex
	}
	if strings.TrimSpace(merged.Claim) == "" {
		merged.Claim = previous.Claim
	}
	if len(merged.EvidenceGoalIDs) == 0 {
		merged.EvidenceGoalIDs = append([]string(nil), previous.EvidenceGoalIDs...)
	} else {
		merged.EvidenceGoalIDs = appendUniqueStrings(previous.EvidenceGoalIDs, merged.EvidenceGoalIDs...)
	}
	if len(merged.EntityIDs) == 0 {
		merged.EntityIDs = append([]string(nil), previous.EntityIDs...)
	} else {
		merged.EntityIDs = appendUniqueStrings(previous.EntityIDs, merged.EntityIDs...)
	}
	merged.Evidence = mergeClaimEvidence(previous.Evidence, merged.Evidence)
	if len(merged.EvidenceIdentities) == 0 {
		merged.EvidenceIdentities = append(
			[]agentapi.EvidenceIdentity(nil), previous.EvidenceIdentities...,
		)
	} else {
		merged.EvidenceIdentities = mergeEvidenceIdentities(
			previous.EvidenceIdentities, merged.EvidenceIdentities,
		)
	}
	merged.HighRisk = previous.HighRisk || merged.HighRisk
	if strings.TrimSpace(merged.Support) == "" {
		merged.Support = previous.Support
	}
	return merged
}

func mergeClaimEvidence(
	left, right []InvestigationEvidence,
) []InvestigationEvidence {
	out := append([]InvestigationEvidence(nil), left...)
	seen := make(map[string]struct{}, len(left)+len(right))
	for _, item := range left {
		seen[investigationEvidenceKey(item)] = struct{}{}
	}
	for _, item := range right {
		key := investigationEvidenceKey(item)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

func investigationEvidenceKey(item InvestigationEvidence) string {
	return strings.TrimSpace(item.Kind) + "\x00" +
		strings.TrimSpace(item.Reference)
}

func mergeEvidenceIdentities(
	left, right []agentapi.EvidenceIdentity,
) []agentapi.EvidenceIdentity {
	out := append([]agentapi.EvidenceIdentity(nil), left...)
	seen := make(map[string]struct{}, len(left)+len(right))
	for _, identity := range left {
		seen[evidenceIdentityKey(identity)] = struct{}{}
	}
	for _, identity := range right {
		key := evidenceIdentityKey(identity)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, identity)
	}
	return out
}

func evidenceIdentityKey(identity agentapi.EvidenceIdentity) string {
	return identity.SourceKind + "\x00" + identity.Target + "\x00" +
		identity.Section + "\x00" + identity.Version + "\x00" + identity.TimeRange
}

func claimKey(claim InvestigationClaim) string {
	if claim.ProducerNodeID != "" || claim.FindingIndex != 0 {
		return fmt.Sprintf("%s\x00%d", claim.ProducerNodeID, claim.FindingIndex)
	}
	return "claim\x00" + strings.TrimSpace(claim.Claim)
}

func mergeUnsupportedClaims(left, right []InvestigationUnsupportedClaim) []InvestigationUnsupportedClaim {
	out := append([]InvestigationUnsupportedClaim(nil), left...)
	seen := make(map[string]struct{}, len(out)+len(right))
	for _, claim := range out {
		seen[unsupportedClaimKey(claim)] = struct{}{}
	}
	for _, claim := range right {
		key := unsupportedClaimKey(claim)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, claim)
	}
	return out
}

func unsupportedClaimKey(claim InvestigationUnsupportedClaim) string {
	return fmt.Sprintf("%s\x00%d\x00%s", claim.ProducerNodeID, claim.FindingIndex, claim.ReasonCode)
}

func mergeCitations(left, right []InvestigationCitation) []InvestigationCitation {
	out := append([]InvestigationCitation(nil), left...)
	seen := make(map[string]struct{}, len(out)+len(right))
	for _, citation := range out {
		seen[citationKey(citation)] = struct{}{}
	}
	for _, citation := range right {
		key := citationKey(citation)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, citation)
	}
	return out
}

func citationKey(citation InvestigationCitation) string {
	parts := make([]string, 0, len(citation.Evidence)+1)
	parts = append(parts, strings.TrimSpace(citation.Claim))
	for _, item := range citation.Evidence {
		parts = append(parts, item.Kind+"\x00"+item.Reference)
	}
	return strings.Join(parts, "\x00")
}

func copyNonEmptyResultMetadata(target *InvestigationResult, source InvestigationResult) {
	if source.Verification != (InvestigationVerification{}) {
		target.Verification = source.Verification
	}
	for target.WorkflowCompleteness == "" && source.WorkflowCompleteness != "" {
		target.WorkflowCompleteness = source.WorkflowCompleteness
	}
	if source.ExecutionStatus != "" {
		target.ExecutionStatus = source.ExecutionStatus
	}
	if source.EvidenceStatus != "" {
		target.EvidenceStatus = source.EvidenceStatus
	}
	if source.ClaimStatus != "" {
		target.ClaimStatus = source.ClaimStatus
	}
	if source.VerificationStatus != "" {
		target.VerificationStatus = source.VerificationStatus
	}
	if source.FailureReason != "" {
		target.FailureReason = source.FailureReason
	}
	if source.Round > 0 {
		target.Round = source.Round
	}
	if source.BaseDepth > 0 {
		target.BaseDepth = source.BaseDepth
	}
	if source.StopReason != "" {
		target.StopReason = source.StopReason
	}
}

func conflictToAPI(conflict evidence.Conflict) agentapi.EvidenceConflict {
	identity, _ := EvidenceIdentity(conflict.Current)
	return agentapi.EvidenceConflict{
		Identity:       identity,
		Current:        conflict.Current,
		Incoming:       conflict.Incoming,
		CurrentOrigin:  conflict.CurrentOrigin,
		IncomingOrigin: conflict.IncomingOrigin,
	}
}

func appendUniqueEvidenceConflict(
	values []agentapi.EvidenceConflict,
	value agentapi.EvidenceConflict,
) []agentapi.EvidenceConflict {
	identity := value.Identity.SourceKind + "\x00" + value.Identity.Target + "\x00" +
		value.Identity.Section + "\x00" + value.Current.ContentHash + "\x00" + value.Incoming.ContentHash
	for _, existing := range values {
		existingKey := existing.Identity.SourceKind + "\x00" + existing.Identity.Target + "\x00" +
			existing.Identity.Section + "\x00" + existing.Current.ContentHash + "\x00" + existing.Incoming.ContentHash
		if existingKey == identity {
			return values
		}
	}
	return append(values, value)
}

func uniqueStrings(values []string) []string {
	return appendUniqueStrings(nil, values...)
}

func appendUniqueStrings(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	out := make([]string, 0, len(values)+len(additions))
	appendValue := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	for _, value := range values {
		appendValue(value)
	}
	for _, value := range additions {
		appendValue(value)
	}
	return out
}
