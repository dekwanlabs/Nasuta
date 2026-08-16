package delegation

import (
	"context"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

func TestValidatorRejectsOversizedAndInvalidReports(t *testing.T) {
	validator := NewValidator(nil, ValidationLimits{
		MaxReportBytes: 512,
		MaxFindings:    1,
	})
	report := validReport("report-1", "claim-1", "ev-1")
	report.Status = agentapi.DelegationCompleted
	report.Completeness = agentapi.DelegationIncomplete
	if _, err := validator.Validate(context.Background(), []agentapi.DelegationReport{report}, evidenceLedger(), false); err == nil {
		t.Fatal("Validate accepted completed partial report")
	}

	report = validReport("report-1", "claim-1", "ev-1")
	report.Summary = strings.Repeat("x", 1024)
	if _, err := validator.Validate(context.Background(), []agentapi.DelegationReport{report}, evidenceLedger(), false); err == nil {
		t.Fatal("Validate accepted oversized report")
	}
}

func TestValidatorCalculatesCitationCoverageAndMissingCriticalCitation(t *testing.T) {
	validator := NewValidator(nil, ValidationLimits{})
	report := validReport("report-1", "claim-1", "ev-1")
	report.Findings = append(report.Findings, agentapi.DelegationFinding{
		ID: "claim-2", Statement: "missing", Confidence: agentapi.DelegationConfidenceLow,
		Citations: []string{"ev-missing"}, Critical: true,
	})
	validation, err := validator.Validate(
		context.Background(),
		[]agentapi.DelegationReport{report},
		evidenceLedger(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if validation.CitationCoverage != 0.5 {
		t.Fatalf("citation coverage = %v, want 0.5", validation.CitationCoverage)
	}
	if !containsReason(validation.VerificationReasons, ReasonMissingCriticalCitation) {
		t.Fatalf("verification reasons = %v", validation.VerificationReasons)
	}
}

func TestValidatorAggregatesExplicitConflicts(t *testing.T) {
	validator := NewValidator(nil, ValidationLimits{})
	report := validReport("report-1", "claim-1", "ev-1")
	report.Conflicts = []agentapi.DelegationConflict{{
		Kind: "source_mismatch", ClaimIDs: []string{"claim-1"}, Critical: true,
	}}
	validation, err := validator.Validate(
		context.Background(),
		[]agentapi.DelegationReport{report},
		evidenceLedger(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.HasConflicts ||
		!containsReason(validation.VerificationReasons, ReasonCriticalExplicitConflict) {
		t.Fatalf("validation = %#v", validation)
	}
}

func TestValidatorDetectsRegisteredStructuredConflict(t *testing.T) {
	claims := NewClaimRegistry()
	if err := claims.Publish([]agentapi.ClaimPolicy{{
		Schema:       agentapi.SchemaRef{ID: "knowledge.code.assertion", Version: 1},
		ComparatorID: ComparatorBooleanAssertion,
		KeyFields:    []string{"subject", "predicate"},
		ScopeFields:  []string{"revision"},
	}}); err != nil {
		t.Fatal(err)
	}
	validator := NewValidator(claims, ValidationLimits{})
	left := validReport("report-1", "claim-1", "ev-1")
	right := validReport("report-2", "claim-2", "ev-2")
	left.Findings[0].Critical = true
	left.Findings[0].StructuredClaim = structuredClaim(true)
	right.Findings[0].StructuredClaim = structuredClaim(false)

	validation, err := validator.Validate(
		context.Background(),
		[]agentapi.DelegationReport{left, right},
		evidenceLedger(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(validation.Conflicts) != 1 ||
		!containsReason(validation.VerificationReasons, ReasonCriticalStructuredConflict) {
		t.Fatalf("validation = %#v", validation)
	}
}

func TestValidatorUsesClaimPolicyKeyFields(t *testing.T) {
	claims := NewClaimRegistry()
	if err := claims.Publish([]agentapi.ClaimPolicy{{
		Schema:       agentapi.SchemaRef{ID: "knowledge.code.assertion", Version: 1},
		ComparatorID: ComparatorBooleanAssertion,
		KeyFields:    []string{"subject"},
		ScopeFields:  []string{"revision"},
	}}); err != nil {
		t.Fatal(err)
	}
	validator := NewValidator(claims, ValidationLimits{})
	left := validReport("report-1", "claim-1", "ev-1")
	right := validReport("report-2", "claim-2", "ev-2")
	left.Findings[0].StructuredClaim = structuredClaim(true)
	right.Findings[0].StructuredClaim = structuredClaim(false)
	right.Findings[0].StructuredClaim.Predicate = "different_predicate"

	validation, err := validator.Validate(
		context.Background(),
		[]agentapi.DelegationReport{left, right},
		evidenceLedger(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(validation.Conflicts) != 1 {
		t.Fatalf("conflicts = %#v, want one policy-keyed conflict", validation.Conflicts)
	}
}

func TestClaimRegistryRejectsUnsupportedKeyField(t *testing.T) {
	claims := NewClaimRegistry()
	err := claims.Publish([]agentapi.ClaimPolicy{{
		Schema:       agentapi.SchemaRef{ID: "knowledge.code.assertion", Version: 1},
		ComparatorID: ComparatorBooleanAssertion,
		KeyFields:    []string{"value"},
	}})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("Publish error = %v, want unsupported key field", err)
	}
}

func TestValidatorDoesNotTreatOrdinaryPartialReportAsTruncated(t *testing.T) {
	validator := NewValidator(nil, ValidationLimits{})
	report := validReport("report-1", "claim-1", "ev-1")
	report.Status = agentapi.DelegationPartial
	report.Completeness = agentapi.DelegationIncomplete
	report.Uncertainties = []string{"source evidence was partial"}

	validation, err := validator.Validate(
		context.Background(),
		[]agentapi.DelegationReport{report},
		evidenceLedger(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if containsReason(validation.VerificationReasons, ReasonReportTruncated) {
		t.Fatalf("verification reasons = %v, ordinary partial report was marked truncated",
			validation.VerificationReasons)
	}
}

func TestValidatorRequiresVerificationForUnknownComparatorAndFreeTextOverlap(t *testing.T) {
	validator := NewValidator(nil, ValidationLimits{})
	left := validReport("report-1", "claim-1", "ev-1")
	right := validReport("report-2", "claim-2", "ev-2")
	left.Findings[0].Critical = true
	left.Findings[0].StructuredClaim = structuredClaim(true)

	validation, err := validator.Validate(
		context.Background(),
		[]agentapi.DelegationReport{left, right},
		evidenceLedger(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !validation.UnverifiedSemanticOverlap ||
		!containsReason(validation.VerificationReasons, ReasonUnknownClaimComparator) ||
		!containsReason(validation.VerificationReasons, ReasonUnstructuredCrossReportMerge) {
		t.Fatalf("validation = %#v", validation)
	}
}

func validReport(reportID, claimID, citation string) agentapi.DelegationReport {
	return agentapi.DelegationReport{
		ReportID: reportID, Capability: "knowledge.code.inspect",
		Status: agentapi.DelegationCompleted, Completeness: agentapi.DelegationComplete,
		Summary: "summary",
		Findings: []agentapi.DelegationFinding{{
			ID: claimID, Statement: "statement",
			Confidence: agentapi.DelegationConfidenceHigh,
			Citations:  []string{citation},
		}},
	}
}

func structuredClaim(value bool) *agentapi.StructuredClaim {
	return &agentapi.StructuredClaim{
		Schema: "knowledge.code.assertion@1", Subject: "symbol:UpdateStatus",
		Predicate: "reachable_null_dereference", Value: value,
		Scope: map[string]any{"revision": "abc"},
	}
}

func evidenceLedger() map[string]tool.EvidenceUnit {
	return map[string]tool.EvidenceUnit{
		"ev-1": {SourceKind: "code", Target: "a.go", ContentHash: "hash-1"},
		"ev-2": {SourceKind: "code", Target: "b.go", ContentHash: "hash-2"},
	}
}

func containsReason(reasons []string, target string) bool {
	for _, reason := range reasons {
		if reason == target {
			return true
		}
	}
	return false
}
