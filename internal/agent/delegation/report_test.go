package delegation

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
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

func flowReport(subject string) *agentapi.FlowIR {
	return &agentapi.FlowIR{
		Subject:    subject,
		Status:     "partial",
		Confidence: "medium",
		Nodes: []agentapi.FlowNode{
			{ID: "api", Label: "订单 API", Kind: "service"},
			{ID: "worker", Label: "订单处理器", Kind: "worker"},
			{ID: "db", Label: "订单库", Kind: "database"},
		},
		Edges: []agentapi.FlowEdge{
			{From: "api", To: "worker", Protocol: "HTTP", SyncMode: "sync", EvidenceRefs: []string{"ev-1"}, EvidenceState: "verified"},
			{From: "worker", To: "db", Protocol: "SQL", SyncMode: "async", EvidenceRefs: []string{"ev-2"}, EvidenceState: "verified"},
		},
		OpenHops: []string{"worker -> external payment"},
	}
}

func TestProjectReportProjectsValidFlow(t *testing.T) {
	flow := flowReport("订单创建")
	output, err := json.Marshal(investigationOutput{Summary: "订单创建流程", Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	report, err := projectReport(agentapi.RunResult{
		RunID: "child-flow", Status: agentapi.RunSucceeded, Output: output,
		Evidence: agentapi.EvidenceSummary{Status: "complete"},
	}, "knowledge.code.inspect", "report-flow")
	if err != nil {
		t.Fatal(err)
	}
	if report.Flow == nil || report.Flow.Subject != "订单创建" {
		t.Fatalf("flow = %#v", report.Flow)
	}
	if len(report.Flow.Nodes) != 3 || len(report.Flow.Edges) != 2 || len(report.Flow.OpenHops) != 1 {
		t.Fatalf("flow cardinality = %#v", report.Flow)
	}
	if len(report.Uncertainties) != 0 {
		t.Fatalf("unexpected uncertainties = %v", report.Uncertainties)
	}
	// Projection must not share nested slices with the decoded child object.
	flow.Nodes[0].EvidenceRefs = []string{"mutated"}
	flow.Edges[0].EvidenceRefs[0] = "mutated"
	flow.OpenHops[0] = "mutated"
	if report.Flow.Nodes[0].EvidenceRefs != nil || report.Flow.Edges[0].EvidenceRefs[0] != "ev-1" || report.Flow.OpenHops[0] != "worker -> external payment" {
		t.Fatalf("projected flow aliases child data: %#v", report.Flow)
	}
}

func TestProjectReportWithEvidenceDowngradesUnknownVerifiedFlowRefs(t *testing.T) {
	flow := flowReport("订单创建")
	flow.Edges[0].EvidenceRefs = []string{"missing-ref"}
	output, err := json.Marshal(investigationOutput{Summary: "订单创建流程", Flow: flow})
	if err != nil {
		t.Fatal(err)
	}
	report, err := projectReportWithEvidence(
		agentapi.RunResult{RunID: "child-flow", Status: agentapi.RunSucceeded, Output: output,
			Evidence: agentapi.EvidenceSummary{Status: "complete"}},
		"knowledge.code.inspect", "report-flow",
		map[string]tool.EvidenceUnit{"ev-1": {SourceKind: "code", Target: "orders.go"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if report.Flow == nil {
		t.Fatal("flow was discarded instead of being downgraded")
	}
	if report.Flow.Edges[0].EvidenceState != "unresolved" || len(report.Flow.Edges[0].EvidenceRefs) != 0 {
		t.Fatalf("unknown verified edge refs were retained: %#v", report.Flow.Edges[0])
	}
}

func TestProjectReportOmitsInvalidFlowAndMarksUncertainty(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*agentapi.FlowIR)
	}{
		{name: "verified edge without evidence", mutate: func(flow *agentapi.FlowIR) {
			flow.Edges[0].EvidenceRefs = nil
		}},
		{name: "unknown node", mutate: func(flow *agentapi.FlowIR) {
			flow.Edges[0].To = "missing"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flow := flowReport("订单创建")
			tc.mutate(flow)
			output, err := json.Marshal(investigationOutput{Summary: "订单创建流程", Flow: flow})
			if err != nil {
				t.Fatal(err)
			}
			report, err := projectReport(agentapi.RunResult{
				RunID: "child-flow", Status: agentapi.RunSucceeded, Output: output,
				Evidence: agentapi.EvidenceSummary{Status: "complete"},
			}, "knowledge.code.inspect", "report-flow")
			if err != nil {
				t.Fatal(err)
			}
			if report.Flow != nil {
				t.Fatalf("invalid flow was projected: %#v", report.Flow)
			}
			if !containsString(report.Uncertainties, invalidFlowUncertainty) {
				t.Fatalf("uncertainties = %v", report.Uncertainties)
			}
		})
	}
}

func TestBoundReportShrinksFlowBeforeFindings(t *testing.T) {
	report := validReport("report-flow", "claim-1", "ev-1")
	report.Summary = strings.Repeat("summary ", 80)
	report.Flow = flowReport("订单创建")
	maxTokens := int64(0)
	for tokens := int64(1); tokens < 1000; tokens++ {
		candidate := boundReport(report, tokens)
		if candidate.Flow != nil && len(candidate.Flow.Edges) < len(report.Flow.Edges) {
			maxTokens = tokens
			break
		}
	}
	if maxTokens == 0 {
		t.Fatal("could not find a budget that exercises flow shrinking")
	}
	bounded := boundReport(report, maxTokens)
	raw, err := json.Marshal(bounded)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) > reportByteLimit(maxTokens) {
		t.Fatalf("bounded report is %d bytes, want <= %d", len(raw), reportByteLimit(maxTokens))
	}
	if bounded.Flow == nil {
		t.Fatal("flow should be retained when shrinking can satisfy the budget")
	}
	if len(bounded.Flow.Edges) >= len(report.Flow.Edges) {
		t.Fatalf("flow was not shrunk: %#v", bounded.Flow)
	}
}

func TestCloneFlowIRDeepCopiesSlices(t *testing.T) {
	original := flowReport("订单创建")
	cloned := cloneFlowIR(original)
	if cloned == original {
		t.Fatal("clone returned the original pointer")
	}
	cloned.Nodes[0].EvidenceRefs = []string{"ev-node"}
	cloned.Edges[0].EvidenceRefs[0] = "ev-edge"
	cloned.OpenHops[0] = "changed"
	if len(original.Nodes[0].EvidenceRefs) != 0 || original.Edges[0].EvidenceRefs[0] != "ev-1" || original.OpenHops[0] != "worker -> external payment" {
		t.Fatalf("clone shares nested slices: original = %#v", original)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
