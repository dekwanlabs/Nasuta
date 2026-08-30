package delegation

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

func TestProjectReportSalvagesInvalidOutputWhenToolsSucceeded(t *testing.T) {
	report, err := projectReport(agentapi.RunResult{
		RunID:  "run_child_1",
		Status: agentapi.RunFailed,
		Error:  &agentapi.RunError{Code: "invalid_output", Message: "not investigation.report"},
		Evidence: agentapi.EvidenceSummary{
			Status:        "partial",
			ToolCallCount: 6,
			ResultCount:   6,
		},
	}, "knowledge.docs.verify", "report-docs")
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != agentapi.DelegationPartial ||
		report.Completeness != agentapi.DelegationIncomplete ||
		report.Error != nil ||
		report.Summary == "" {
		t.Fatalf("salvaged report = %+v", report)
	}
}

func TestProjectReportKeepsHardFailuresFailed(t *testing.T) {
	report, err := projectReport(agentapi.RunResult{
		RunID:  "run_child_2",
		Status: agentapi.RunFailed,
		Error:  &agentapi.RunError{Code: ErrorChildTimeout, Message: "deadline"},
		Evidence: agentapi.EvidenceSummary{
			ToolCallCount: 2,
			ResultCount:   2,
		},
	}, "knowledge.docs.verify", "report-timeout")
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != agentapi.DelegationTimeout {
		t.Fatalf("status = %s, want timeout", report.Status)
	}
}

func TestProjectReportIncludesAuthoritativeToolCalls(t *testing.T) {
	output, err := json.Marshal(investigationOutput{
		Summary: "verified",
		Findings: []investigationFinding{{
			Claim:      "the path is reachable",
			GoalIDs:    []string{"core_flow"},
			Confidence: 0.9,
			Evidence: []investigationEvidence{{
				Reference: "ev-1",
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := projectReport(agentapi.RunResult{
		RunID:  "child-1",
		Status: agentapi.RunSucceeded,
		Output: output,
		Evidence: agentapi.EvidenceSummary{
			Status:        "complete",
			ToolCallCount: 3,
		},
		Usage: agentapi.Usage{
			InputTokens: 11, OutputTokens: 7, TotalTokens: 18,
		},
	}, "knowledge.code.inspect", "report-1")
	if err != nil {
		t.Fatal(err)
	}
	if report.Usage.ToolCalls != 3 ||
		report.Usage.InputTokens != 11 ||
		report.Usage.OutputTokens != 7 ||
		report.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v", report.Usage)
	}
}

func TestBoundReportMarksOnlyActualTruncation(t *testing.T) {
	report := validReport("report-1", "claim-1", "ev-1")
	report.Summary = strings.Repeat("summary ", 200)
	bounded := boundReport(report, 128)
	if bounded.Status != agentapi.DelegationPartial ||
		bounded.Completeness != agentapi.DelegationIncomplete ||
		!reportWasTruncated(bounded) {
		t.Fatalf("bounded report = %+v", bounded)
	}

	validation, err := NewValidator(nil, ValidationLimits{}).Validate(
		t.Context(),
		[]agentapi.DelegationReport{bounded},
		evidenceLedger(),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !containsReason(validation.VerificationReasons, ReasonReportTruncated) {
		t.Fatalf("verification reasons = %v", validation.VerificationReasons)
	}
}

func TestBoundReportStrictlyHonorsMinimumBudget(t *testing.T) {
	report := validReport(
		"report_"+strings.Repeat("1", stableIDHashLength),
		"claim-1",
		"ev-1",
	)
	report.RunID = "run_child_" + strings.Repeat("2", stableIDHashLength)
	report.Capability = strings.Repeat("a", agentapi.MaxCapabilityIDBytes)
	report.Summary = strings.Repeat("summary ", 200)
	report.Uncertainties = []string{
		strings.Repeat("uncertain ", 100),
		strings.Repeat("unknown ", 100),
	}
	report.Error = &agentapi.RunError{
		Code:    strings.Repeat("error_", 100),
		Message: strings.Repeat("message ", 200),
	}
	report.Usage = agentapi.DelegationUsage{
		ToolCalls: math.MaxInt64, InputTokens: math.MaxInt64,
		OutputTokens: math.MaxInt64, ReasoningTokens: math.MaxInt64,
		TotalTokens: math.MaxInt64, CostMicros: math.MaxInt64,
	}
	wantRunID := report.RunID
	wantReportID := report.ReportID
	wantCapability := report.Capability

	maxTokens := minimumBoundedReportTokens()
	bounded := boundReport(report, maxTokens)
	raw, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := int64(len(raw)), maxTokens*4; got > want {
		t.Fatalf("bounded report is %d bytes, want <= %d: %s", got, want, raw)
	}
	if bounded.Status != agentapi.DelegationPartial ||
		bounded.Completeness != agentapi.DelegationIncomplete ||
		!reportWasTruncated(bounded) {
		t.Fatalf("bounded report = %+v", bounded)
	}
	if bounded.RunID != wantRunID ||
		bounded.ReportID != wantReportID ||
		bounded.Capability != wantCapability {
		t.Fatalf("bounded report lost identity: %+v", bounded)
	}
}

func TestBoundReportStrictlyHonorsMinimumBudgetForLongestStatus(t *testing.T) {
	report := agentapi.DelegationReport{
		RunID:         "run_child_" + strings.Repeat("3", stableIDHashLength),
		ReportID:      "report_" + strings.Repeat("4", stableIDHashLength),
		Capability:    strings.Repeat("a", agentapi.MaxCapabilityIDBytes),
		Status:        agentapi.DelegationInterrupted,
		Completeness:  agentapi.DelegationIncomplete,
		Summary:       strings.Repeat("summary ", 200),
		Uncertainties: []string{strings.Repeat("uncertain ", 100)},
	}
	wantRunID := report.RunID
	wantReportID := report.ReportID
	wantCapability := report.Capability

	maxTokens := minimumBoundedReportTokens()
	bounded := boundReport(report, maxTokens)
	raw, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := int64(len(raw)), maxTokens*4; got > want {
		t.Fatalf("bounded report is %d bytes, want <= %d: %s", got, want, raw)
	}
	if bounded.Status != agentapi.DelegationInterrupted ||
		!reportWasTruncated(bounded) {
		t.Fatalf("bounded report = %+v", bounded)
	}
	if bounded.RunID != wantRunID ||
		bounded.ReportID != wantReportID ||
		bounded.Capability != wantCapability {
		t.Fatalf("bounded report lost identity: %+v", bounded)
	}
}
