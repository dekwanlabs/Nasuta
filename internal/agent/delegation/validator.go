package delegation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const (
	ReasonCriticalExplicitConflict     = "critical_explicit_conflict"
	ReasonCriticalStructuredConflict   = "critical_structured_conflict"
	ReasonUnstructuredCrossReportMerge = "unstructured_cross_report_merge"
	ReasonMissingCriticalCitation      = "missing_critical_citation"
	ReasonUnknownClaimComparator       = "unknown_claim_comparator"
	ReasonReportTruncated              = "report_truncated"
	ReasonHighRiskPolicy               = "high_risk_policy"
)

type ValidationLimits struct {
	MaxReportBytes   int
	MaxFindings      int
	MaxConflicts     int
	MaxUncertainties int
}

type Validator struct {
	claims *ClaimRegistry
	limits ValidationLimits
}

func NewValidator(claims *ClaimRegistry, limits ValidationLimits) *Validator {
	if claims == nil {
		claims = NewClaimRegistry()
	}
	if limits.MaxReportBytes <= 0 {
		limits.MaxReportBytes = 32 * 1024
	}
	if limits.MaxFindings <= 0 {
		limits.MaxFindings = 20
	}
	if limits.MaxConflicts <= 0 {
		limits.MaxConflicts = 10
	}
	if limits.MaxUncertainties <= 0 {
		limits.MaxUncertainties = 10
	}
	return &Validator{claims: claims, limits: limits}
}

type claimEntry struct {
	reportIndex int
	finding     agentapi.DelegationFinding
	fullID      string
	key         string
	value       json.RawMessage
	comparator  agentapi.ClaimComparator
}

func (validator *Validator) Validate(
	ctx context.Context,
	reports []agentapi.DelegationReport,
	evidence map[string]tool.EvidenceUnit,
	highRisk bool,
) (agentapi.DelegationValidation, error) {
	validation := agentapi.DelegationValidation{}
	reasons := map[string]struct{}{}
	claimIDs := make(map[string]struct{})
	claimHasCitation := make(map[string]bool)
	var findings, cited, structured int
	var entries []claimEntry
	unstructuredByReport := make(map[int]bool)

	for reportIndex, report := range reports {
		if err := validator.validateReport(report); err != nil {
			return validation, fmt.Errorf("report %d: %w", reportIndex, err)
		}
		if report.ReportID != "" {
			validation.ReportIDs = append(validation.ReportIDs, report.ReportID)
		}
		if reportWasTruncated(report) {
			reasons[ReasonReportTruncated] = struct{}{}
		}
		for _, finding := range report.Findings {
			findings++
			fullID := report.ReportID + "/" + finding.ID
			claimIDs[fullID] = struct{}{}
			validCitations := 0
			for _, citation := range finding.Citations {
				unit, ok := evidence[citation]
				if ok && unit.ContentHash != "" {
					validCitations++
				}
			}
			if validCitations > 0 {
				cited++
				claimHasCitation[fullID] = true
			} else if finding.Critical {
				reasons[ReasonMissingCriticalCitation] = struct{}{}
			}
			if finding.StructuredClaim == nil {
				unstructuredByReport[reportIndex] = true
				continue
			}
			structured++
			entry, known, err := validator.claimEntry(
				reportIndex,
				fullID,
				finding,
			)
			if err != nil {
				return validation, err
			}
			if !known {
				if finding.Critical {
					reasons[ReasonUnknownClaimComparator] = struct{}{}
				}
				continue
			}
			entries = append(entries, entry)
		}
	}
	if findings > 0 {
		validation.CitationCoverage = float64(cited) / float64(findings)
		validation.StructuredClaimCoverage = float64(structured) / float64(findings)
	}

	explicit, err := explicitConflicts(reports, claimIDs, claimHasCitation)
	if err != nil {
		return validation, err
	}
	validation.Conflicts = append(validation.Conflicts, explicit...)
	for _, conflict := range explicit {
		if conflict.Critical {
			reasons[ReasonCriticalExplicitConflict] = struct{}{}
		}
	}
	structuredConflicts, err := structuredConflicts(ctx, entries)
	if err != nil {
		return validation, err
	}
	validation.Conflicts = append(validation.Conflicts, structuredConflicts...)
	for _, conflict := range structuredConflicts {
		if conflict.Critical {
			reasons[ReasonCriticalStructuredConflict] = struct{}{}
		}
	}

	if len(reports) > 1 && len(unstructuredByReport) > 0 {
		validation.UnverifiedSemanticOverlap = true
		reasons[ReasonUnstructuredCrossReportMerge] = struct{}{}
	}
	if highRisk {
		reasons[ReasonHighRiskPolicy] = struct{}{}
	}
	validation.HasConflicts = len(validation.Conflicts) > 0
	validation.VerificationReasons = sortedKeys(reasons)
	validation.RequiresVerification = len(validation.VerificationReasons) > 0
	return validation, nil
}

func (validator *Validator) validateReport(report agentapi.DelegationReport) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if len(raw) > validator.limits.MaxReportBytes {
		return fmt.Errorf("report exceeds %d bytes", validator.limits.MaxReportBytes)
	}
	switch report.Status {
	case agentapi.DelegationCompleted, agentapi.DelegationPartial,
		agentapi.DelegationFailed, agentapi.DelegationTimeout,
		agentapi.DelegationCancelled, agentapi.DelegationRejected,
		agentapi.DelegationInterrupted:
	default:
		return fmt.Errorf("invalid status %q", report.Status)
	}
	switch report.Completeness {
	case agentapi.DelegationComplete, agentapi.DelegationIncomplete:
	default:
		return fmt.Errorf("invalid completeness %q", report.Completeness)
	}
	if report.Status == agentapi.DelegationCompleted &&
		report.Completeness != agentapi.DelegationComplete {
		return fmt.Errorf("completed report must be complete")
	}
	if len(report.Findings) > validator.limits.MaxFindings {
		return fmt.Errorf("too many findings")
	}
	if len(report.Conflicts) > validator.limits.MaxConflicts {
		return fmt.Errorf("too many conflicts")
	}
	if len(report.Uncertainties) > validator.limits.MaxUncertainties {
		return fmt.Errorf("too many uncertainties")
	}
	for _, finding := range report.Findings {
		if strings.TrimSpace(finding.ID) == "" ||
			strings.TrimSpace(finding.Statement) == "" {
			return fmt.Errorf("finding id and statement are required")
		}
		if len(finding.Citations) == 0 {
			return fmt.Errorf("finding %q has no citations", finding.ID)
		}
		switch finding.Confidence {
		case agentapi.DelegationConfidenceLow,
			agentapi.DelegationConfidenceMedium,
			agentapi.DelegationConfidenceHigh:
		default:
			return fmt.Errorf("finding %q has invalid confidence", finding.ID)
		}
	}
	return nil
}

func (validator *Validator) claimEntry(
	reportIndex int,
	fullID string,
	finding agentapi.DelegationFinding,
) (claimEntry, bool, error) {
	claim := finding.StructuredClaim
	policy, comparator, ok := validator.claims.resolve(claim.Schema)
	if !ok {
		return claimEntry{}, false, nil
	}
	keyFields := make(map[string]any, len(policy.KeyFields)+2)
	keyFields["schema"] = schemaKey(policy.Schema)
	for _, field := range policy.KeyFields {
		switch field {
		case "subject":
			if strings.TrimSpace(claim.Subject) == "" {
				return claimEntry{}, false, fmt.Errorf(
					"structured claim %q has empty subject key",
					fullID,
				)
			}
			keyFields[field] = claim.Subject
		case "predicate":
			if strings.TrimSpace(claim.Predicate) == "" {
				return claimEntry{}, false, fmt.Errorf(
					"structured claim %q has empty predicate key",
					fullID,
				)
			}
			keyFields[field] = claim.Predicate
		}
	}
	scope := make(map[string]any, len(policy.ScopeFields))
	for _, field := range policy.ScopeFields {
		if value, exists := claim.Scope[field]; exists {
			scope[field] = value
		}
	}
	keyFields["scope"] = scope
	keyRaw, err := canonicalJSON(keyFields)
	if err != nil {
		return claimEntry{}, false, err
	}
	value, err := canonicalJSON(claim.Value)
	if err != nil {
		return claimEntry{}, false, err
	}
	return claimEntry{
		reportIndex: reportIndex,
		finding:     finding,
		fullID:      fullID,
		key:         string(keyRaw),
		value:       value,
		comparator:  comparator,
	}, true, nil
}

func explicitConflicts(
	reports []agentapi.DelegationReport,
	claimIDs map[string]struct{},
	claimHasCitation map[string]bool,
) ([]agentapi.DelegationValidationConflict, error) {
	seen := make(map[string]struct{})
	var out []agentapi.DelegationValidationConflict
	for _, report := range reports {
		for _, conflict := range report.Conflicts {
			ids := make([]string, len(conflict.ClaimIDs))
			for index, id := range conflict.ClaimIDs {
				if !strings.Contains(id, "/") {
					id = report.ReportID + "/" + id
				}
				if _, ok := claimIDs[id]; !ok {
					return nil, fmt.Errorf("explicit conflict references unknown claim %q", id)
				}
				if !claimHasCitation[id] {
					return nil, fmt.Errorf("explicit conflict references uncited claim %q", id)
				}
				ids[index] = id
			}
			sort.Strings(ids)
			key := conflict.Kind + "\x00" + strings.Join(ids, "\x00")
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, agentapi.DelegationValidationConflict{
				Kind: conflict.Kind, ClaimIDs: ids, Critical: conflict.Critical,
			})
		}
	}
	return out, nil
}

func structuredConflicts(
	ctx context.Context,
	entries []claimEntry,
) ([]agentapi.DelegationValidationConflict, error) {
	groups := make(map[string][]claimEntry)
	for _, entry := range entries {
		groups[entry.key] = append(groups[entry.key], entry)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out []agentapi.DelegationValidationConflict
	for _, key := range keys {
		group := groups[key]
		if len(group) < 2 {
			continue
		}
		var conflicting []string
		critical := false
		for left := 0; left < len(group); left++ {
			for right := left + 1; right < len(group); right++ {
				if group[left].reportIndex == group[right].reportIndex {
					continue
				}
				conflict, err := group[left].comparator.Conflicts(
					ctx,
					group[left].value,
					group[right].value,
				)
				if err != nil {
					return nil, err
				}
				if !conflict {
					continue
				}
				conflicting = append(
					conflicting,
					group[left].fullID,
					group[right].fullID,
				)
				critical = critical || group[left].finding.Critical ||
					group[right].finding.Critical
			}
		}
		conflicting = uniqueSorted(conflicting)
		if len(conflicting) == 0 {
			continue
		}
		sum := sha256.Sum256([]byte(key))
		out = append(out, agentapi.DelegationValidationConflict{
			Kind:     "structured_value_mismatch",
			ClaimKey: hex.EncodeToString(sum[:8]),
			ClaimIDs: conflicting,
			Critical: critical,
		})
	}
	return out, nil
}

func sortedKeys(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}
