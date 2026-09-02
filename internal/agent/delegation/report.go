package delegation

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/tool"
)

const reportTruncatedUncertainty = "report truncated to the configured delegation limit"
const invalidFlowUncertainty = "flow handoff was invalid and was omitted"

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
	Flow            *agentapi.FlowIR       `json:"flow,omitempty"`
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
	return projectReportWithEvidence(result, capability, reportID, nil)
}

func projectReportWithEvidence(
	result agentapi.RunResult,
	capability string,
	reportID string,
	evidenceIndex map[string]tool.EvidenceUnit,
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
		if salvaged, ok := salvageCollectedChildReport(result, capability, reportID, evidenceIndex); ok {
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
		if salvaged, ok := salvageCollectedChildReport(result, capability, reportID, evidenceIndex); ok {
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
	if output.Flow != nil {
		flow := cloneFlowIR(output.Flow)
		normalizeFlowEvidenceRefs(flow, evidenceIndex)
		if err := validateFlowIR(flow); err != nil {
			report.Uncertainties = appendUniqueStrings(report.Uncertainties, invalidFlowUncertainty)
		} else {
			report.Flow = flow
		}
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

func salvageCollectedChildReport(
	result agentapi.RunResult,
	capability string,
	reportID string,
	evidenceIndex map[string]tool.EvidenceUnit,
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
		if output.Flow != nil {
			flow := cloneFlowIR(output.Flow)
			normalizeFlowEvidenceRefs(flow, evidenceIndex)
			if err := validateFlowIR(flow); err != nil {
				report.Uncertainties = appendUniqueStrings(report.Uncertainties, invalidFlowUncertainty)
			} else {
				report.Flow = flow
			}
		}
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
		Flow                    *agentapi.FlowIR       `json:"flow"`
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
	output.Flow = cloneFlowIR(wire.Flow)
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
	// Flow is a pointer so a value copy of DelegationReport would otherwise
	// let the bounding pass mutate the caller's handoff in place.
	report.Flow = cloneFlowIR(report.Flow)
	if shrinkFlowToFit(&report, maxBytes) {
		return report
	}
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

const (
	maxFlowNodes          = 32
	maxFlowEdges          = 32
	maxFlowOpenHops       = 16
	maxFlowSubjectLen     = 256
	maxFlowNodeIDLen      = 128
	maxFlowNodeLabelLen   = 256
	maxFlowNodeKindLen    = 64
	maxFlowProtocolLen    = 128
	maxFlowOpenHopLen     = 512
	maxFlowEvidenceRefLen = 256
)

func normalizeFlowEvidenceRefs(flow *agentapi.FlowIR, evidenceIndex map[string]tool.EvidenceUnit) {
	if flow == nil || evidenceIndex == nil {
		return
	}
	for index := range flow.Nodes {
		flow.Nodes[index].EvidenceRefs = availableFlowEvidenceRefs(
			flow.Nodes[index].EvidenceRefs, evidenceIndex,
		)
	}
	for index := range flow.Edges {
		refs := availableFlowEvidenceRefs(flow.Edges[index].EvidenceRefs, evidenceIndex)
		if flow.Edges[index].EvidenceState == "verified" && len(refs) == 0 {
			flow.Edges[index].EvidenceState = "unresolved"
		}
		flow.Edges[index].EvidenceRefs = refs
	}
}

func availableFlowEvidenceRefs(
	references []string,
	evidenceIndex map[string]tool.EvidenceUnit,
) []string {
	if len(references) == 0 {
		return nil
	}
	available := make([]string, 0, len(references))
	for _, reference := range references {
		key := strings.TrimSpace(reference)
		if key == "" {
			continue
		}
		if _, ok := evidenceIndex[key]; ok {
			available = append(available, reference)
		}
	}
	return available
}

func validateFlowIR(flow *agentapi.FlowIR) error {
	if flow == nil {
		return nil
	}
	if strings.TrimSpace(flow.Subject) == "" || len(flow.Subject) > maxFlowSubjectLen {
		return fmt.Errorf("flow subject is required and must be at most %d bytes", maxFlowSubjectLen)
	}
	switch flow.Status {
	case "complete", "partial":
	default:
		return fmt.Errorf("flow status %q is invalid", flow.Status)
	}
	switch flow.Confidence {
	case "low", "medium", "high":
	default:
		return fmt.Errorf("flow confidence %q is invalid", flow.Confidence)
	}
	if len(flow.Nodes) > maxFlowNodes || len(flow.Edges) > maxFlowEdges || len(flow.OpenHops) > maxFlowOpenHops {
		return fmt.Errorf("flow exceeds bounded node, edge, or open-hop limit")
	}
	nodes := make(map[string]struct{}, len(flow.Nodes))
	for index, node := range flow.Nodes {
		if strings.TrimSpace(node.ID) == "" || len(node.ID) > maxFlowNodeIDLen || strings.TrimSpace(node.Label) == "" || len(node.Label) > maxFlowNodeLabelLen || strings.TrimSpace(node.Kind) == "" || len(node.Kind) > maxFlowNodeKindLen {
			return fmt.Errorf("flow node %d has invalid identity or label", index)
		}
		if _, exists := nodes[node.ID]; exists {
			return fmt.Errorf("flow node %q is duplicated", node.ID)
		}
		nodes[node.ID] = struct{}{}
		if err := validateFlowEvidenceRefs(node.EvidenceRefs); err != nil {
			return fmt.Errorf("flow node %q: %w", node.ID, err)
		}
	}
	for index, edge := range flow.Edges {
		if _, ok := nodes[edge.From]; !ok {
			return fmt.Errorf("flow edge %d references unknown from node %q", index, edge.From)
		}
		if _, ok := nodes[edge.To]; !ok {
			return fmt.Errorf("flow edge %d references unknown to node %q", index, edge.To)
		}
		if len(edge.Protocol) > maxFlowProtocolLen {
			return fmt.Errorf("flow edge %d protocol is too long", index)
		}
		switch edge.SyncMode {
		case "", "sync", "async", "unknown":
		default:
			return fmt.Errorf("flow edge %d sync_mode %q is invalid", index, edge.SyncMode)
		}
		switch edge.EvidenceState {
		case "verified", "inferred", "unresolved":
		default:
			return fmt.Errorf("flow edge %d evidence_state %q is invalid", index, edge.EvidenceState)
		}
		if edge.EvidenceState == "verified" && len(edge.EvidenceRefs) == 0 {
			return fmt.Errorf("flow edge %d marked verified without evidence_refs", index)
		}
		if err := validateFlowEvidenceRefs(edge.EvidenceRefs); err != nil {
			return fmt.Errorf("flow edge %d: %w", index, err)
		}
	}
	for index, hop := range flow.OpenHops {
		if strings.TrimSpace(hop) == "" || len(hop) > maxFlowOpenHopLen {
			return fmt.Errorf("flow open_hops[%d] is invalid", index)
		}
	}
	return nil
}

func validateFlowEvidenceRefs(references []string) error {
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		if strings.TrimSpace(reference) == "" || len(reference) > maxFlowEvidenceRefLen {
			return fmt.Errorf("evidence reference is empty or too long")
		}
		if _, exists := seen[reference]; exists {
			return fmt.Errorf("evidence references are duplicated")
		}
		seen[reference] = struct{}{}
	}
	return nil
}

func cloneFlowIR(flow *agentapi.FlowIR) *agentapi.FlowIR {
	if flow == nil {
		return nil
	}
	cloned := *flow
	cloned.Nodes = append([]agentapi.FlowNode(nil), flow.Nodes...)
	for index := range cloned.Nodes {
		cloned.Nodes[index].EvidenceRefs = append([]string(nil), flow.Nodes[index].EvidenceRefs...)
	}
	cloned.Edges = append([]agentapi.FlowEdge(nil), flow.Edges...)
	for index := range cloned.Edges {
		cloned.Edges[index].EvidenceRefs = append([]string(nil), flow.Edges[index].EvidenceRefs...)
	}
	cloned.OpenHops = append([]string(nil), flow.OpenHops...)
	return &cloned
}

func shrinkFlowToFit(report *agentapi.DelegationReport, maxBytes int64) bool {
	if report == nil || report.Flow == nil {
		return report != nil && reportFits(*report, maxBytes)
	}
	for report.Flow != nil {
		if len(report.Flow.Edges) > 0 {
			report.Flow.Edges = report.Flow.Edges[:len(report.Flow.Edges)-1]
		} else if len(report.Flow.Nodes) > 0 {
			nodeID := report.Flow.Nodes[len(report.Flow.Nodes)-1].ID
			report.Flow.Nodes = report.Flow.Nodes[:len(report.Flow.Nodes)-1]
			kept := report.Flow.Edges[:0]
			for _, edge := range report.Flow.Edges {
				if edge.From != nodeID && edge.To != nodeID {
					kept = append(kept, edge)
				}
			}
			report.Flow.Edges = kept
		} else if len(report.Flow.OpenHops) > 0 {
			report.Flow.OpenHops = report.Flow.OpenHops[:len(report.Flow.OpenHops)-1]
		} else {
			report.Flow = nil
		}
		if reportFits(*report, maxBytes) {
			return true
		}
	}
	return false
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
