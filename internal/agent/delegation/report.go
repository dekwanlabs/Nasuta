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
	CoveredGoals    []string               `json:"covered_evidence_goals"`
	UnresolvedGoals []string               `json:"unresolved_evidence_goals"`
}

type investigationFinding struct {
	Claim      string                  `json:"claim"`
	GoalIDs    []string                `json:"evidence_goal_ids"`
	Evidence   []investigationEvidence `json:"evidence"`
	Confidence float64                 `json:"confidence"`
}

func (finding *investigationFinding) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Claim           string                  `json:"claim"`
		GoalIDs         []string                `json:"goal_ids"`
		EvidenceGoalIDs []string                `json:"evidence_goal_ids"`
		Evidence        []investigationEvidence `json:"evidence"`
		Confidence      float64                 `json:"confidence"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	finding.Claim = wire.Claim
	finding.GoalIDs = firstNonEmptyStrings(wire.EvidenceGoalIDs, wire.GoalIDs)
	finding.Evidence = wire.Evidence
	finding.Confidence = wire.Confidence
	return nil
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
		if salvaged, ok := salvageCollectedChildReport(result, capability, reportID); ok {
			return salvaged, nil
		}
		report.Status = ProjectStatus(StatusFacts{
			Admitted: true, Settled: true, RunStatus: result.Status,
			ErrorCode:    runErrorCode(result.Error),
			Completeness: agentapi.DelegationIncomplete,
		})
		return report, nil
	}
	var output investigationOutput
	if err := decodeInvestigationOutput(result.Output, &output); err != nil {
		if salvaged, ok := salvageCollectedChildReport(result, capability, reportID); ok {
			return salvaged, nil
		}
		return report, fmt.Errorf("decode investigation report: %w", err)
	}
	report.Summary = strings.TrimSpace(output.Summary)
	report.Uncertainties = appendUniqueStrings(
		nil,
		append(output.Gaps, output.UnresolvedGoals...)...,
	)
	report.Findings = projectedFindings(output.Findings, reportID)
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

func salvageCollectedChildReport(
	result agentapi.RunResult,
	capability string,
	reportID string,
) (agentapi.DelegationReport, bool) {
	if !isRecoverableChildOutputFailure(result) || !childCollectedEvidence(result) {
		return agentapi.DelegationReport{}, false
	}
	report := agentapi.DelegationReport{
		RunID: result.RunID, ReportID: reportID, Capability: capability,
		Completeness: agentapi.DelegationIncomplete,
		Usage:        publicDelegationUsage(result),
		Summary:      "Evidence collection completed, but the investigator could not produce a schema-valid report.",
		Uncertainties: []string{
			"Report generation ended before a schema-valid investigation.report was produced.",
		},
	}
	var output investigationOutput
	if decodeInvestigationOutput(result.Output, &output) == nil &&
		strings.TrimSpace(output.Summary) != "" {
		report.Summary = strings.TrimSpace(output.Summary)
		report.Uncertainties = appendUniqueStrings(
			nil,
			append(output.Gaps, output.UnresolvedGoals...)...,
		)
		report.Findings = projectedFindings(output.Findings, reportID)
	}
	if len(report.Uncertainties) == 0 {
		report.Uncertainties = []string{
			"Report generation ended before a schema-valid investigation.report was produced.",
		}
	}
	report.Status = agentapi.DelegationPartial
	return report, true
}

func isRecoverableChildOutputFailure(result agentapi.RunResult) bool {
	if result.Status == agentapi.RunCancelled {
		return false
	}
	switch runErrorCode(result.Error) {
	case "", "invalid_output", "empty_output":
		return true
	default:
		return false
	}
}

func childCollectedEvidence(result agentapi.RunResult) bool {
	return result.Evidence.ToolCallCount > 0 ||
		result.Evidence.ResultCount > 0 ||
		len(result.EvidenceUnits) > 0 ||
		len(result.EvidenceObservations) > 0
}

func decodeInvestigationOutput(raw json.RawMessage, output *investigationOutput) error {
	if len(raw) == 0 || output == nil {
		return fmt.Errorf("investigation report is empty")
	}
	var wire struct {
		Focus                   string                 `json:"focus"`
		Summary                 string                 `json:"summary"`
		Findings                []investigationFinding `json:"findings"`
		Gaps                    []string               `json:"gaps"`
		CoveredGoals            []string               `json:"covered_goals"`
		CoveredEvidenceGoals    []string               `json:"covered_evidence_goals"`
		UnresolvedGoals         []string               `json:"unresolved_goals"`
		UnresolvedEvidenceGoals []string               `json:"unresolved_evidence_goals"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	output.Focus = wire.Focus
	output.Summary = wire.Summary
	output.Findings = wire.Findings
	output.Gaps = wire.Gaps
	output.CoveredGoals = firstNonEmptyStrings(wire.CoveredEvidenceGoals, wire.CoveredGoals)
	output.UnresolvedGoals = firstNonEmptyStrings(wire.UnresolvedEvidenceGoals, wire.UnresolvedGoals)
	return nil
}

func firstNonEmptyStrings(values ...[]string) []string {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

func projectedFindings(
	findings []investigationFinding,
	reportID string,
) []agentapi.DelegationFinding {
	projected := make([]agentapi.DelegationFinding, 0, len(findings))
	for index, finding := range findings {
		citations := make([]string, 0, len(finding.Evidence))
		for _, evidence := range finding.Evidence {
			if reference := strings.TrimSpace(evidence.Reference); reference != "" {
				citations = append(citations, reference)
			}
		}
		projected = append(projected, agentapi.DelegationFinding{
			ID:         stableID("claim", reportID, fmt.Sprintf("%d", index)),
			Statement:  strings.TrimSpace(finding.Claim),
			Confidence: confidence(finding.Confidence),
			Citations:  appendUniqueStrings(nil, citations...),
			Facets:     appendUniqueStrings(nil, finding.GoalIDs...),
		})
	}
	return projected
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
