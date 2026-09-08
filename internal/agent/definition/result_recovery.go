package definition

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

func canRecoverVerificationResult(ref agentapi.SchemaRef, err error) bool {
	return ref == (agentapi.SchemaRef{ID: "delegation.verification.result", Version: 1}) &&
		(errors.Is(err, execution.ErrReasoningTruncated) ||
			errors.Is(err, execution.ErrEmptyModelResponse) ||
			errors.Is(err, execution.ErrAnswerTruncated) ||
			errors.Is(err, execution.ErrModelCallBudgetExhausted))
}

func recoverVerificationResult(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	answer string,
) (json.RawMessage, error) {
	if schemas == nil {
		return nil, fmt.Errorf("verification recovery schema registry is required")
	}
	if strings.TrimSpace(answer) != "" {
		if output, err := validatedOutput(schemas, ref, answer); err == nil {
			return output, nil
		}
		if output, ok := recoverPartialVerification(schemas, ref, answer); ok {
			return output, nil
		}
	}
	fallback := json.RawMessage(`{"summary":"Verification output was truncated; no complete verdict was admitted.","verdicts":[],"uncertainties":["verification output was truncated before a complete result was produced"]}`)
	if err := schemas.Validate(ref, fallback); err != nil {
		return nil, fmt.Errorf("validate recovered verification result: %w", err)
	}
	return fallback, nil
}

type recoveredVerificationResult struct {
	Summary       string                         `json:"summary"`
	Verdicts      []recoveredVerificationVerdict `json:"verdicts"`
	Uncertainties []string                       `json:"uncertainties"`
}

type recoveredVerificationVerdict struct {
	ClaimIDs     []string `json:"claim_ids"`
	Decision     string   `json:"decision"`
	Rationale    string   `json:"rationale"`
	EvidenceRefs []string `json:"evidence_refs"`
}

func recoverPartialVerification(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	answer string,
) (json.RawMessage, bool) {
	repaired := llm.RepairJSON(answer)
	if !json.Valid([]byte(repaired)) {
		return nil, false
	}
	var partial recoveredVerificationResult
	if err := json.Unmarshal([]byte(repaired), &partial); err != nil {
		return nil, false
	}
	partial.Summary = truncateForSchema(strings.TrimSpace(partial.Summary), 1000)
	if partial.Summary == "" {
		partial.Summary = "Verification output was truncated."
	}
	partial.Verdicts = recoverVerificationVerdicts(partial.Verdicts)
	partial.Uncertainties = recoverVerificationUncertainties(partial.Uncertainties)
	output, err := json.Marshal(partial)
	if err != nil || schemas.Validate(ref, output) != nil {
		return nil, false
	}
	return output, true
}

func recoverVerificationVerdicts(
	verdicts []recoveredVerificationVerdict,
) []recoveredVerificationVerdict {
	valid := make([]recoveredVerificationVerdict, 0, len(verdicts))
	for _, verdict := range verdicts {
		if !validVerificationVerdict(&verdict) {
			continue
		}
		valid = append(valid, verdict)
		if len(valid) == 8 {
			break
		}
	}
	return valid
}

func validVerificationVerdict(verdict *recoveredVerificationVerdict) bool {
	if len(verdict.ClaimIDs) == 0 || strings.TrimSpace(verdict.Rationale) == "" {
		return false
	}
	switch verdict.Decision {
	case "supported", "contradicted", "distinct", "unresolved":
	default:
		return false
	}
	if len(verdict.ClaimIDs) > 20 {
		verdict.ClaimIDs = verdict.ClaimIDs[:20]
	}
	if len(verdict.EvidenceRefs) > 20 {
		verdict.EvidenceRefs = verdict.EvidenceRefs[:20]
	}
	verdict.Rationale = truncateForSchema(strings.TrimSpace(verdict.Rationale), 512)
	return verdict.Rationale != ""
}

func recoverVerificationUncertainties(uncertainties []string) []string {
	out := make([]string, 0, minInt(len(uncertainties), 4)+1)
	for _, uncertainty := range uncertainties {
		uncertainty = truncateForSchema(strings.TrimSpace(uncertainty), 512)
		if uncertainty != "" {
			out = append(out, uncertainty)
		}
		if len(out) == 4 {
			break
		}
	}
	if len(out) == 0 {
		out = []string{"verification output was truncated before a complete result was produced"}
	}
	return out
}

func truncateForSchema(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

// canRecoverInvestigationReportOutput permits deterministic recovery for transient
// model-output failures even when the agent otherwise requires strict output.
func canRecoverInvestigationReportOutput(
	_ outputRecoveryContext,
	ref agentapi.SchemaRef,
	err error,
) bool {
	return canRecoverInvestigationReport(ref, err)
}

func shouldRecoverInvalidInvestigationOutput(recovery outputRecoveryContext, answer string) bool {
	if !recovery.StrictOutput {
		return true
	}
	return execution.LeakedToolProtocol(answer)
}

func canRecoverInvestigationReport(
	ref agentapi.SchemaRef,
	err error,
) bool {
	return ref == (agentapi.InvestigationReportSchemaRef()) && (errors.Is(err, execution.ErrReasoningTruncated) ||
		errors.Is(err, execution.ErrEmptyModelResponse) ||
		errors.Is(err, execution.ErrAnswerTruncated) ||
		errors.Is(err, execution.ErrModelCallBudgetExhausted) ||
		errors.Is(err, execution.ErrToolProtocolLeak))
}

func recoverFailedInvestigationReport(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	context outputRecoveryContext,
	answer string,
	modelErr error,
) (json.RawMessage, bool, error) {
	if strings.TrimSpace(answer) != "" {
		if output, err := validatedOutput(schemas, ref, answer); err == nil {
			return output, true, nil
		} else {
			return recoverInvestigationReport(
				schemas,
				ref,
				context,
				answer,
				err,
			)
		}
	}
	return recoverInvestigationReport(
		schemas,
		ref,
		context,
		answer,
		fmt.Errorf("model did not complete investigation report generation: %w", modelErr),
	)
}

func recoverInvestigationReport(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	context outputRecoveryContext,
	answer string,
	validationErr error,
) (json.RawMessage, bool, error) {
	if context.AgentID == "" || len(context.Input) == 0 {
		return nil, false, fmt.Errorf("investigation report recovery context is incomplete: %w", validationErr)
	}
	focus := map[string]string{
		"investigator.code":    "code",
		"investigator.runtime": "runtime",
		"investigator.docs":    "docs",
		"investigator.web":     "web",
		"investigator.memory":  "memory",
	}[context.AgentID]
	if focus == "" {
		return nil, false, fmt.Errorf("unsupported investigation agent %q: %w", context.AgentID, validationErr)
	}
	var contract struct {
		EvidenceGoals []struct {
			Facet string `json:"facet"`
		} `json:"evidence_goals"`
	}
	if err := json.Unmarshal(context.Input, &contract); err != nil {
		return nil, false, fmt.Errorf("decode investigation contract for recovery: %w", err)
	}
	required := make([]string, 0, len(contract.EvidenceGoals))
	seen := make(map[string]struct{}, len(contract.EvidenceGoals))
	for _, goal := range contract.EvidenceGoals {
		facet := strings.TrimSpace(goal.Facet)
		if facet == "" {
			continue
		}
		if _, ok := seen[facet]; ok {
			continue
		}
		seen[facet] = struct{}{}
		required = append(required, facet)
	}
	if repaired, ok := repairInvestigationGoalCoverage(answer, required); ok {
		repaired = normalizeOutputForSchema(ref, repaired)
		if err := schemas.Validate(ref, repaired); err == nil {
			return repaired, true, nil
		}
	}
	if report, ok := execution.BuildEvidencePreservingReport(
		context.EvidenceUnits, context.EvidenceObservations, required, focus,
	); ok {
		report = normalizeOutputForSchema(ref, report)
		if err := schemas.Validate(ref, report); err == nil {
			return report, true, nil
		}
	}
	fallback := map[string]any{
		"focus":    focus,
		"summary":  "Evidence collection completed, but the investigator could not produce a schema-valid report; no unsupported claim was accepted.",
		"findings": []any{},
		"gaps": []string{
			"Evidence collection completed, but report generation ended before a schema-valid investigation.report was produced.",
		},
		"covered_evidence_goals":    []string{},
		"unresolved_evidence_goals": required,
	}
	encoded, err := json.Marshal(fallback)
	if err != nil {
		return nil, false, fmt.Errorf("encode recovered investigation report: %w", err)
	}
	if err := schemas.Validate(ref, encoded); err != nil {
		return nil, false, fmt.Errorf("validate recovered investigation report: %w", err)
	}
	return encoded, false, nil
}

// repairInvestigationGoalCoverage restores only fields derivable from the task contract.
func repairInvestigationGoalCoverage(
	answer string,
	required []string,
) (json.RawMessage, bool) {
	report, ok := decodeInvestigationReport(answer)
	if !ok {
		return nil, false
	}
	if isTaskContractShape(report) {
		return nil, false
	}
	_, hasCovered := report["covered_evidence_goals"]
	_, hasUnresolved := report["unresolved_evidence_goals"]
	if hasCovered && hasUnresolved {
		return nil, false
	}
	coveredSet, ok := collectInvestigationCoveredGoals(report, required)
	if !ok {
		return nil, false
	}
	covered := make([]string, 0, len(coveredSet))
	unresolved := make([]string, 0, len(required)-len(coveredSet))
	for _, facet := range required {
		if _, ok := coveredSet[facet]; ok {
			covered = append(covered, facet)
		} else {
			unresolved = append(unresolved, facet)
		}
	}
	report["covered_evidence_goals"] = covered
	report["unresolved_evidence_goals"] = unresolved
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// taskContractShapeFields are the fields that identify a delegated
// investigation task contract. A report that carries any of them was produced
// by echoing the task input back, not by writing an investigation.report.
var taskContractShapeFields = []string{
	"objective",
	"capability",
	"delegation_id",
	"parent_run_id",
	"task_index",
	"parent_question_summary",
	"focus_facets",
	"evidence_refs",
	"output_kind",
}

// isTaskContractShape reports whether a decoded object is actually a task
// contract that leaked into the answer, rather than an investigation report.
func isTaskContractShape(report map[string]any) bool {
	if report == nil {
		return false
	}
	for _, field := range taskContractShapeFields {
		if _, exists := report[field]; exists {
			return true
		}
	}
	return false
}

// isEchoedTaskContractAnswer reports whether answer carries the shape of a
// delegated investigation task contract that was echoed back instead of a real
// investigation report. It is used to keep the recovery log accurate: such
// input is not a schema defect, so it must not be logged as an
// additionalProperties failure.
func isEchoedTaskContractAnswer(answer string) bool {
	report, ok := decodeInvestigationReport(answer)
	return ok && isTaskContractShape(report)
}

func decodeInvestigationReport(answer string) (map[string]any, bool) {
	raw, err := canonicalStructuredOutput(answer)
	if err != nil {
		repaired := llm.RepairJSON(answer)
		if repaired == strings.TrimSpace(answer) || !json.Valid([]byte(repaired)) {
			return nil, false
		}
		raw = json.RawMessage(repaired)
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, false
	}
	return report, true
}

func collectInvestigationCoveredGoals(
	report map[string]any,
	required []string,
) (map[string]struct{}, bool) {
	findings, ok := report["findings"].([]any)
	if !ok {
		return nil, false
	}
	requiredSet := make(map[string]struct{}, len(required))
	for _, facet := range required {
		requiredSet[facet] = struct{}{}
	}
	coveredSet := make(map[string]struct{}, len(required))
	for _, findingValue := range findings {
		finding, ok := findingValue.(map[string]any)
		if !ok {
			return nil, false
		}
		goalIDs, ok := finding["evidence_goal_ids"].([]any)
		if !ok {
			return nil, false
		}
		for _, goalValue := range goalIDs {
			goal, ok := goalValue.(string)
			if !ok {
				return nil, false
			}
			if _, requested := requiredSet[goal]; !requested {
				return nil, false
			}
			coveredSet[goal] = struct{}{}
		}
	}
	return coveredSet, true
}

func failedRun(runID, code string, err error) agentapi.RunResult {
	return agentapi.RunResult{
		RunID: runID, Status: agentapi.RunFailed,
		Error: &agentapi.RunError{Code: code, Message: err.Error()},
	}
}
