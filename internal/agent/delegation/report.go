package delegation

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
)

const reportTruncatedUncertainty = "report truncated to the configured delegation limit"

const (
	stableIDHashLength = 24
	childRunIDPrefix   = "run_child_"
	reportIDPrefix     = "report_"
)

type investigationOutput struct {
	Focus           string                 `json:"focus"`
	Summary         string                 `json:"summary"`
	Findings        []investigationFinding `json:"findings"`
	Gaps            []string               `json:"gaps"`
	CoveredGoals    []string               `json:"covered_goals"`
	UnresolvedGoals []string               `json:"unresolved_goals"`
}

type investigationFinding struct {
	Claim      string                  `json:"claim"`
	GoalIDs    []string                `json:"goal_ids"`
	Evidence   []investigationEvidence `json:"evidence"`
	Confidence float64                 `json:"confidence"`
}

type investigationEvidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

func projectReport(
	result agentapi.RunResult,
	capability string,
	reportID string,
) (agentapi.DelegationReport, error) {
	report := agentapi.DelegationReport{
		RunID: result.RunID, ReportID: reportID, Capability: capability,
		Completeness: agentapi.DelegationIncomplete,
		Usage:        publicDelegationUsage(result),
	}
	if result.Error != nil {
		report.Error = cloneRunError(result.Error)
	}
	if result.Status != agentapi.RunSucceeded {
		report.Status = ProjectStatus(StatusFacts{
			Admitted: true, Settled: true, RunStatus: result.Status,
			ErrorCode:    runErrorCode(result.Error),
			Completeness: agentapi.DelegationIncomplete,
		})
		return report, nil
	}
	var output investigationOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		return report, fmt.Errorf("decode investigation report: %w", err)
	}
	report.Summary = strings.TrimSpace(output.Summary)
	report.Uncertainties = appendUniqueStrings(
		nil,
		append(output.Gaps, output.UnresolvedGoals...)...,
	)
	report.Findings = make([]agentapi.DelegationFinding, 0, len(output.Findings))
	for index, finding := range output.Findings {
		citations := make([]string, 0, len(finding.Evidence))
		for _, evidence := range finding.Evidence {
			if reference := strings.TrimSpace(evidence.Reference); reference != "" {
				citations = append(citations, reference)
			}
		}
		report.Findings = append(report.Findings, agentapi.DelegationFinding{
			ID:         stableID("claim", reportID, fmt.Sprintf("%d", index)),
			Statement:  strings.TrimSpace(finding.Claim),
			Confidence: confidence(finding.Confidence),
			Citations:  appendUniqueStrings(nil, citations...),
			Facets:     appendUniqueStrings(nil, finding.GoalIDs...),
		})
	}
	report.Completeness = agentapi.DelegationComplete
	if result.Evidence.Status == "partial" ||
		result.Evidence.Status == "unavailable" ||
		len(report.Uncertainties) > 0 {
		report.Completeness = agentapi.DelegationIncomplete
	}
	report.Status = ProjectStatus(StatusFacts{
		Admitted: true, Settled: true, RunStatus: result.Status,
		EvidenceStatus: result.Evidence.Status,
		Completeness:   report.Completeness,
	})
	return report, nil
}

func boundReport(
	report agentapi.DelegationReport,
	maxTokens int64,
) agentapi.DelegationReport {
	if maxTokens <= 0 {
		return report
	}
	maxBytes := reportByteLimit(maxTokens)
	if reportFits(report, maxBytes) {
		return report
	}
	report.Completeness = agentapi.DelegationIncomplete
	if report.Status == agentapi.DelegationCompleted {
		report.Status = agentapi.DelegationPartial
	}
	report.Uncertainties = appendUniqueStrings(
		[]string{reportTruncatedUncertainty},
		report.Uncertainties...,
	)
	for len(report.Findings) > 0 {
		if reportFits(report, maxBytes) {
			return report
		}
		report.Findings = report.Findings[:len(report.Findings)-1]
	}
	for len(report.Conflicts) > 0 {
		if reportFits(report, maxBytes) {
			return report
		}
		report.Conflicts = report.Conflicts[:len(report.Conflicts)-1]
	}
	for len(report.Uncertainties) > 1 {
		if reportFits(report, maxBytes) {
			return report
		}
		report.Uncertainties = report.Uncertainties[:len(report.Uncertainties)-1]
	}
	if fitReportText(&report, maxBytes, &report.Summary) {
		return report
	}
	if report.Error != nil {
		if fitReportText(&report, maxBytes, &report.Error.Message) {
			return report
		}
		if fitReportText(&report, maxBytes, &report.Error.Code) {
			return report
		}
		report.Error = nil
		if reportFits(report, maxBytes) {
			return report
		}
	}
	report.Usage.ReasoningTokens = 0
	report.Usage.CostMicros = 0
	if reportFits(report, maxBytes) {
		return report
	}
	report.Usage = agentapi.DelegationUsage{}
	if reportFits(report, maxBytes) {
		return report
	}
	return report
}

func minimumBoundedReportTokens() int64 {
	report := agentapi.DelegationReport{
		RunID:         childRunIDPrefix + strings.Repeat("0", stableIDHashLength),
		ReportID:      reportIDPrefix + strings.Repeat("0", stableIDHashLength),
		Capability:    strings.Repeat("a", agentapi.MaxCapabilityIDBytes),
		Status:        agentapi.DelegationInterrupted,
		Completeness:  agentapi.DelegationIncomplete,
		Uncertainties: []string{reportTruncatedUncertainty},
	}
	raw, _ := json.Marshal(report)
	return int64((len(raw) + 3) / 4)
}

func reportByteLimit(maxTokens int64) int64 {
	if maxTokens > math.MaxInt64/4 {
		return math.MaxInt64
	}
	return maxTokens * 4
}

func reportFits(report agentapi.DelegationReport, maxBytes int64) bool {
	raw, err := json.Marshal(report)
	return err == nil && int64(len(raw)) <= maxBytes
}

func fitReportText(
	report *agentapi.DelegationReport,
	maxBytes int64,
	field *string,
) bool {
	original := *field
	*field = ""
	if !reportFits(*report, maxBytes) {
		return false
	}
	best := ""
	low, high := 1, len(original)
	for low <= high {
		mid := low + (high-low)/2
		candidate := truncateText(original, mid)
		*field = candidate
		if reportFits(*report, maxBytes) {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	*field = best
	return true
}

func reportWasTruncated(report agentapi.DelegationReport) bool {
	for _, uncertainty := range report.Uncertainties {
		if uncertainty == reportTruncatedUncertainty {
			return true
		}
	}
	return false
}

func publicDelegationUsage(result agentapi.RunResult) agentapi.DelegationUsage {
	return agentapi.DelegationUsage{
		ToolCalls:   int64(result.Evidence.ToolCallCount),
		InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens,
		ReasoningTokens: result.Usage.ReasoningTokens,
		TotalTokens:     result.Usage.TotalTokens,
		CostMicros:      result.Usage.CostMicros,
	}
}

func confidence(value float64) agentapi.DelegationConfidence {
	switch {
	case value < 0.4:
		return agentapi.DelegationConfidenceLow
	case value < 0.75:
		return agentapi.DelegationConfidenceMedium
	default:
		return agentapi.DelegationConfidenceHigh
	}
}

func cloneRunError(value *agentapi.RunError) *agentapi.RunError {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func runErrorCode(value *agentapi.RunError) string {
	if value == nil {
		return ""
	}
	return value.Code
}
