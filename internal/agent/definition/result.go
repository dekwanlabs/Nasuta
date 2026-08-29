package definition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

func internalMessage(message agentapi.Message) llm.Message {
	compiled := llm.Message{
		Role: message.Role, Content: message.Content,
		ToolCallID: message.ToolCallID, Name: message.Name,
	}
	if len(message.ToolCalls) == 0 {
		return compiled
	}
	compiled.ToolCalls = make([]llm.ToolCall, 0, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		compiled.ToolCalls = append(compiled.ToolCalls, llm.ToolCall{
			ID: call.ID, Type: call.Type,
			Function: llm.ToolFunction{
				Name: call.Function.Name, Arguments: call.Function.Arguments,
			},
		})
	}
	return compiled
}

func publicMessages(messages []llm.Message) []agentapi.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]agentapi.Message, 0, len(messages))
	for _, message := range messages {
		compiled := agentapi.Message{
			Role: message.Role, Content: message.Content,
			ToolCallID: message.ToolCallID, Name: message.Name,
		}
		if len(message.ToolCalls) > 0 {
			compiled.ToolCalls = make([]agentapi.ToolCall, 0, len(message.ToolCalls))
			for _, call := range message.ToolCalls {
				compiled.ToolCalls = append(compiled.ToolCalls, agentapi.ToolCall{
					ID: call.ID, Type: call.Type,
					Function: agentapi.ToolFunction{
						Name: call.Function.Name, Arguments: call.Function.Arguments,
					},
				})
			}
		}
		out = append(out, compiled)
	}
	return out
}

func publicEvidence(evidence run.EvidenceMetrics) agentapi.EvidenceSummary {
	return agentapi.EvidenceSummary{
		Status: string(evidence.Status), ForcedConclusion: evidence.ForcedConclusion,
		ToolCallCount: evidence.ToolCallCount, ResultCount: evidence.ResultCount,
		ToolFailureCount:   evidence.ToolFailureCount,
		PartialResultCount: evidence.PartialResultCount,
		OmittedItemCount:   evidence.OmittedItemCount,
	}
}

func publicEvidenceConflicts(conflicts []evidence.Conflict) []agentapi.EvidenceConflict {
	if len(conflicts) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceConflict, len(conflicts))
	for index, conflict := range conflicts {
		out[index] = agentapi.EvidenceConflict{
			Identity: agentapi.EvidenceIdentity{
				SourceKind: conflict.Key.SourceKind,
				Target:     conflict.Key.Target,
				Section:    conflict.Key.Section,
				Version:    conflict.Key.Version,
				TimeRange:  conflict.Key.TimeRange,
			},
			Current:        evidence.CloneUnit(conflict.Current),
			Incoming:       evidence.CloneUnit(conflict.Incoming),
			CurrentOrigin:  conflict.CurrentOrigin,
			IncomingOrigin: conflict.IncomingOrigin,
		}
	}
	return out
}

func referencesFromRequest(blocks []agentapi.ContextBlock) []agentapi.Reference {
	count := 0
	for _, block := range blocks {
		count += len(block.References)
	}
	if count == 0 {
		return nil
	}
	references := make([]agentapi.Reference, 0, count)
	for _, block := range blocks {
		references = append(references, block.References...)
	}
	return references
}

func contextReferenceTypes(blocks []agentapi.ContextBlock) map[string]tool.ReferenceType {
	var index map[string]tool.ReferenceType
	for _, block := range blocks {
		for _, reference := range block.References {
			referenceType := tool.ReferenceType(reference.Type)
			switch referenceType {
			case tool.ReferenceRunbook, tool.ReferenceService, tool.ReferenceSymbol:
				if reference.Target == "" {
					continue
				}
				if index == nil {
					index = make(map[string]tool.ReferenceType)
				}
				index[reference.Target] = referenceType
			}
		}
	}
	return index
}

func joinedContextContent(blocks []agentapi.ContextBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	var content strings.Builder
	for _, block := range blocks {
		if content.Len() > 0 {
			content.WriteString("\n\n")
		}
		content.WriteString("## ")
		content.WriteString(block.Title)
		content.WriteString("\n")
		content.WriteString(block.Content)
	}
	return content.String()
}

func contextEvidenceUnits(blocks []agentapi.ContextBlock) []tool.EvidenceUnit {
	count := 0
	for _, block := range blocks {
		count += len(block.Evidence)
	}
	if count == 0 {
		return nil
	}
	units := make([]tool.EvidenceUnit, 0, count)
	for _, block := range blocks {
		for _, unit := range block.Evidence {
			unit.Sections = append([]string(nil), unit.Sections...)
			unit.Facets = append([]string(nil), unit.Facets...)
			units = append(units, unit)
		}
	}
	return units
}

func evidenceConflicts(
	blocks []agentapi.ContextBlock,
) []evidence.Conflict {
	count := 0
	for _, block := range blocks {
		count += len(block.EvidenceConflicts)
	}
	if count == 0 {
		return nil
	}
	conflicts := make([]evidence.Conflict, 0, count)
	for _, block := range blocks {
		for _, conflict := range block.EvidenceConflicts {
			conflicts = append(conflicts, evidence.Conflict{
				Key: evidence.Key{
					SourceKind: conflict.Identity.SourceKind,
					Target:     conflict.Identity.Target,
					Section:    conflict.Identity.Section,
					Version:    conflict.Identity.Version,
					TimeRange:  conflict.Identity.TimeRange,
				},
				Current:        evidence.CloneUnit(conflict.Current),
				Incoming:       evidence.CloneUnit(conflict.Incoming),
				CurrentOrigin:  conflict.CurrentOrigin,
				IncomingOrigin: conflict.IncomingOrigin,
			})
		}
	}
	return conflicts
}

func hashMessages(messages []llm.Message) string {
	raw, _ := json.Marshal(messages)
	return hashBytes(raw)
}

type outputRecoveryContext struct {
	AgentID      string
	Input        json.RawMessage
	Context      []agentapi.ContextBlock
	StrictOutput bool
}

func mapResult(
	runID string,
	result *execution.RunResult,
	runErr error,
	cancelCause error,
	usage agentapi.Usage,
	preRetrieved []agentapi.Reference,
	schemas *agentapi.SchemaRegistry,
	outputSchema agentapi.SchemaRef,
	recovery ...outputRecoveryContext,
) (agentapi.RunResult, run.Outcome) {
	if cancelCause != nil {
		outcome := execution.OutcomeFor(result, preRetrieved, cancelCause)
		outcome.Status = run.StatusAborted
		outcome.ErrorCode = "cancelled"
		outcome.DelegationAdoptions = unknownDelegationAdoptions(
			outcome.DelegationAdoptions,
			"parent_cancelled",
		)
		publicResult := publicTerminalEvidence(runID, result, outcome, usage)
		publicResult.Status = agentapi.RunCancelled
		publicResult.Error = &agentapi.RunError{
			Code: "cancelled", Message: cancelCause.Error(),
		}
		return publicResult, outcome
	}
	outcome := execution.OutcomeFor(result, preRetrieved, runErr)
	if errors.Is(outcome.Err, agentapi.ErrBudgetExceeded) {
		outcome.ErrorCode = "budget_exhausted"
	}
	if errors.Is(outcome.Err, execution.ErrToolCallBudgetExhausted) {
		outcome.ErrorCode = "tool_call_budget_exhausted"
	}
	if errors.Is(outcome.Err, errRunLimitExceeded) {
		outcome.ErrorCode = "run_limit_exceeded"
	}
	if outcome.Status != run.StatusDone &&
		len(recovery) > 0 &&
		!recovery[0].StrictOutput &&
		canRecoverVerificationResult(outputSchema, outcome.Err) {
		recovered, recoveryErr := recoverVerificationResult(
			schemas, outputSchema, outcome.Answer,
		)
		if recoveryErr == nil {
			modelErr := outcome.Err
			outcome.Status = run.StatusDone
			outcome.ErrorCode = ""
			outcome.Err = nil
			outcome.Answer = string(recovered)
			outcome.Evidence.ForcedConclusion = true
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s recovered truncated %s output for %s without another model call: %v",
				runID, outputSchema.ID, recovery[0].AgentID, modelErr,
			)
		}
	}
	if outcome.Status != run.StatusDone &&
		len(recovery) > 0 &&
		canRecoverInvestigationReportOutput(recovery[0], outputSchema, outcome.Err) {
		recovered, preserved, recoveryErr := recoverFailedInvestigationReport(
			schemas,
			outputSchema,
			recovery[0],
			outcome.Answer,
			outcome.Err,
		)
		if recoveryErr == nil {
			modelErr := outcome.Err
			outcome.Status = run.StatusDone
			outcome.ErrorCode = ""
			outcome.Err = nil
			outcome.Answer = string(recovered)
			outcome.Evidence.ForcedConclusion = true
			recoveryMode := "as an evidence-preserving partial report"
			if preserved {
				recoveryMode = "without discarding its findings"
			}
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s recovered model-output failure for %s from %s %s: %v",
				runID,
				outputSchema.ID,
				recovery[0].AgentID,
				recoveryMode,
				modelErr,
			)
		} else {
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s could not recover model-output failure for %s from %s: %v",
				runID,
				outputSchema.ID,
				recovery[0].AgentID,
				recoveryErr,
			)
		}
	}
	if outcome.Status != run.StatusDone &&
		len(recovery) > 0 &&
		!recovery[0].StrictOutput &&
		canRecoverInvestigationAnswer(outputSchema, outcome.Err) {
		recovered, recoveryErr := recoverInvestigationAnswer(
			schemas,
			outputSchema,
			recovery[0],
		)
		if recoveryErr == nil {
			modelErr := outcome.Err
			outcome.Status = run.StatusDone
			outcome.ErrorCode = ""
			outcome.Err = nil
			outcome.Answer = string(recovered)
			outcome.Evidence.ForcedConclusion = true
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s recovered unavailable %s output for %s from its verified input: %v",
				runID,
				outputSchema.ID,
				recovery[0].AgentID,
				modelErr,
			)
		} else {
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s could not recover unavailable %s output for %s: %v",
				runID,
				outputSchema.ID,
				recovery[0].AgentID,
				recoveryErr,
			)
		}
	}
	if outcome.Status != run.StatusDone {
		outcome.DelegationAdoptions = unknownDelegationAdoptions(
			outcome.DelegationAdoptions,
			"parent_run_failed",
		)
		runError := outcome.Err
		if runError == nil {
			runError = errors.New("definition run failed")
		}
		publicResult := publicTerminalEvidence(runID, result, outcome, usage)
		publicResult.Status = agentapi.RunFailed
		publicResult.Error = &agentapi.RunError{
			Code: outcome.ErrorCode, Message: runError.Error(),
			Retryable: retryableError(runError),
		}
		return publicResult, outcome
	}
	publicResult := publicTerminalEvidence(runID, result, outcome, usage)
	publicResult.Status = agentapi.RunSucceeded
	if result != nil && result.OutputMode == agentapi.RunOutputEvidenceWorker {
		if output := attachEvidenceWorkerStructuredOutput(
			schemas, outputSchema, outcome.Answer, recovery,
		); len(output) > 0 {
			publicResult.Output = output
		}
		return publicResult, outcome
	}
	publicResult.Text = outcome.Answer
	publicResult.References = append([]agentapi.Reference(nil), outcome.References...)
	publicResult.Messages = publicMessages(outcome.SessionMessages)
	output, err := validatedOutput(schemas, outputSchema, outcome.Answer)
	if err != nil && outputSchema == agentapi.InvestigationReportSchemaRef() && len(recovery) > 0 && !recovery[0].StrictOutput {
		if recovered, preserved, recoveryErr := recoverInvestigationReport(
			schemas, outputSchema, recovery[0], outcome.Answer, err,
		); recoveryErr == nil {
			validationErr := err
			output = recovered
			err = nil
			outcome.Answer = string(recovered)
			publicResult.Text = outcome.Answer
			if preserved {
				log.WarnfCtx(
					log.WithTraceID(context.Background(), runID),
					"[agent] run %s recovered invalid %s output for %s by deriving goal coverage: %v",
					runID, outputSchema.ID, recovery[0].AgentID, validationErr,
				)
			} else {
				log.WarnfCtx(
					log.WithTraceID(context.Background(), runID),
					"[agent] run %s recovered invalid %s output for %s as unavailable report: %v",
					runID, outputSchema.ID, recovery[0].AgentID, validationErr,
				)
			}
		}
	}
	if err != nil {
		outcome.Status = run.StatusFailed
		outcome.ErrorCode = "invalid_output"
		outcome.Err = err
		outcome.DelegationAdoptions = unknownDelegationAdoptions(
			outcome.DelegationAdoptions,
			"invalid_output",
		)
		publicResult.Status = agentapi.RunFailed
		publicResult.Text = ""
		publicResult.References = nil
		publicResult.Messages = nil
		publicResult.DelegationAdoptions = cloneDelegationAdoptions(
			outcome.DelegationAdoptions,
		)
		publicResult.Error = &agentapi.RunError{Code: "invalid_output", Message: err.Error()}
		return publicResult, outcome
	}
	publicResult.Output = output
	publicResult.Text = RenderPublicAnswer(output)
	outcome.Answer = publicResult.Text
	return publicResult, outcome
}

// attachEvidenceWorkerStructuredOutput keeps evidence observations as the
// primary worker artifact, but promotes a schema-valid investigation.report
// when the model actually wrote one. Empty or invalid answers stay omitted so
// tool-only workers can still succeed.
func attachEvidenceWorkerStructuredOutput(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	answer string,
	recovery []outputRecoveryContext,
) json.RawMessage {
	if schemas == nil || ref != agentapi.InvestigationReportSchemaRef() {
		return nil
	}
	if strings.TrimSpace(answer) == "" {
		return nil
	}
	output, err := validatedOutput(schemas, ref, answer)
	if err == nil {
		return output
	}
	if len(recovery) == 0 {
		return nil
	}
	recovered, _, recoveryErr := recoverInvestigationReport(
		schemas, ref, recovery[0], answer, err,
	)
	if recoveryErr != nil {
		return nil
	}
	return recovered
}

func publicTerminalEvidence(
	runID string,
	result *execution.RunResult,
	outcome run.Outcome,
	usage agentapi.Usage,
) agentapi.RunResult {
	publicResult := agentapi.RunResult{
		RunID: runID, Usage: usage,
		Evidence: publicEvidence(outcome.Evidence),
		DelegationAdoptions: cloneDelegationAdoptions(
			outcome.DelegationAdoptions,
		),
	}
	if result == nil {
		return publicResult
	}
	publicResult.EvidenceUnits = evidence.CloneUnits(result.EvidenceUnits)
	publicResult.EvidenceObservations = cloneEvidenceObservations(result.EvidenceObservations)
	publicResult.EvidenceConflicts = publicEvidenceConflicts(result.EvidenceConflicts)
	return publicResult
}

func cloneEvidenceObservations(observations []agentapi.EvidenceObservation) []agentapi.EvidenceObservation {
	if len(observations) == 0 {
		return nil
	}
	out := make([]agentapi.EvidenceObservation, len(observations))
	for index, observation := range observations {
		observation.Facets = append([]string(nil), observation.Facets...)
		out[index] = observation
	}
	return out
}

func cloneDelegationAdoptions(
	adoptions []agentapi.DelegationAdoption,
) []agentapi.DelegationAdoption {
	if len(adoptions) == 0 {
		return nil
	}
	cloned := make([]agentapi.DelegationAdoption, len(adoptions))
	for index, adoption := range adoptions {
		adoption.AdoptedReportIDs = append(
			[]string(nil),
			adoption.AdoptedReportIDs...,
		)
		cloned[index] = adoption
	}
	return cloned
}

func unknownDelegationAdoptions(
	adoptions []agentapi.DelegationAdoption,
	reason string,
) []agentapi.DelegationAdoption {
	unknown := cloneDelegationAdoptions(adoptions)
	for index := range unknown {
		unknown[index].AdoptedReportIDs = nil
		unknown[index].Status = agentapi.DelegationUnknown
		unknown[index].Reason = reason
	}
	return unknown
}

func retryableError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var classified interface{ Retryable() bool }
	return errors.As(err, &classified) && classified.Retryable()
}

func canonicalStructuredOutput(answer string) (json.RawMessage, error) {
	value := strings.TrimSpace(answer)
	if json.Valid([]byte(value)) {
		return json.RawMessage(value), nil
	}

	lines := strings.Split(value, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[len(lines)-1]) != "```" {
		return nil, errors.New("structured output must be JSON or one JSON fence")
	}
	opener := strings.ToLower(strings.TrimSpace(lines[0]))
	if opener != "```" && opener != "```json" {
		return nil, fmt.Errorf("unsupported structured output fence %q", opener)
	}
	for _, line := range lines[1 : len(lines)-1] {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			return nil, errors.New("structured output contains multiple fences")
		}
	}
	payload := strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	if !json.Valid([]byte(payload)) {
		return nil, errors.New("fenced structured output is not valid JSON")
	}
	return json.RawMessage(payload), nil
}

func validatedOutput(
	schemas *agentapi.SchemaRegistry,
	ref agentapi.SchemaRef,
	answer string,
) (json.RawMessage, error) {
	raw, rawErr := canonicalStructuredOutput(answer)
	if rawErr == nil {
		normalized := normalizeOutputForSchema(ref, raw)
		if err := schemas.Validate(ref, normalized); err == nil {
			return append(json.RawMessage(nil), normalized...), nil
		} else {
			rawErr = err
		}
	}
	// RepairJSON covers the common truncation/fence/trailing-comma defects
	// without altering valid string contents. Schema validation remains the
	// final arbiter, so a repair can never silently widen the contract.
	repaired := llm.RepairJSON(answer)
	if repaired != strings.TrimSpace(answer) && json.Valid([]byte(repaired)) {
		normalized := normalizeOutputForSchema(ref, json.RawMessage(repaired))
		if err := schemas.Validate(ref, normalized); err == nil {
			return append(json.RawMessage(nil), normalized...), nil
		} else {
			rawErr = err
		}
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("encode definition output: %w", err)
	}
	if err := schemas.Validate(ref, encoded); err == nil {
		return encoded, nil
	}
	return nil, fmt.Errorf(
		"definition output does not match schema %q version %d: %w",
		ref.ID, ref.Version, rawErr,
	)
}

func normalizeOutputForSchema(ref agentapi.SchemaRef, raw json.RawMessage) json.RawMessage {
	if ref != agentapi.InvestigationReportSchemaRef() {
		return raw
	}
	var report map[string]any
	if err := json.Unmarshal(raw, &report); err != nil {
		return raw
	}
	findings, ok := report["findings"].([]any)
	if !ok {
		return raw
	}
	changed := false
	for _, findingValue := range findings {
		finding, ok := findingValue.(map[string]any)
		if !ok {
			continue
		}
		evidenceItems, ok := finding["evidence"].([]any)
		if !ok {
			continue
		}
		for _, evidenceValue := range evidenceItems {
			item, ok := evidenceValue.(map[string]any)
			if !ok {
				continue
			}
			if identity, exists := item["identity"]; exists {
				if _, isString := identity.(string); isString {
					delete(item, "identity")
					changed = true
				}
			}
			if evidenceID, exists := item["evidence_id"]; exists {
				if value, isString := evidenceID.(string); !isString || !isValidEvidenceID(value) {
					delete(item, "evidence_id")
					changed = true
				}
			}
		}
	}
	if !changed {
		return raw
	}
	normalized, err := json.Marshal(report)
	if err != nil {
		return raw
	}
	return normalized
}

func isValidEvidenceID(value string) bool {
	if len(value) != len("ev_")+64 || !strings.HasPrefix(value, "ev_") {
		return false
	}
	for _, char := range value[len("ev_"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

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
	validVerdicts := make([]recoveredVerificationVerdict, 0, len(partial.Verdicts))
	for _, verdict := range partial.Verdicts {
		if len(verdict.ClaimIDs) == 0 || strings.TrimSpace(verdict.Rationale) == "" {
			continue
		}
		switch verdict.Decision {
		case "supported", "contradicted", "distinct", "unresolved":
		default:
			continue
		}
		if len(verdict.ClaimIDs) > 20 {
			verdict.ClaimIDs = verdict.ClaimIDs[:20]
		}
		if len(verdict.EvidenceRefs) > 20 {
			verdict.EvidenceRefs = verdict.EvidenceRefs[:20]
		}
		verdict.Rationale = truncateForSchema(strings.TrimSpace(verdict.Rationale), 512)
		if verdict.Rationale == "" {
			continue
		}
		validVerdicts = append(validVerdicts, verdict)
		if len(validVerdicts) == 8 {
			break
		}
	}
	partial.Verdicts = validVerdicts
	uncertainties := make([]string, 0, minInt(len(partial.Uncertainties), 4)+1)
	for _, uncertainty := range partial.Uncertainties {
		uncertainty = truncateForSchema(strings.TrimSpace(uncertainty), 512)
		if uncertainty != "" {
			uncertainties = append(uncertainties, uncertainty)
		}
		if len(uncertainties) == 4 {
			break
		}
	}
	if len(uncertainties) == 0 {
		uncertainties = []string{"verification output was truncated before a complete result was produced"}
	}
	partial.Uncertainties = uncertainties
	output, err := json.Marshal(partial)
	if err != nil || schemas.Validate(ref, output) != nil {
		return nil, false
	}
	return output, true
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
	context outputRecoveryContext,
	ref agentapi.SchemaRef,
	err error,
) bool {
	if !context.StrictOutput {
		return canRecoverInvestigationReport(ref, err)
	}
	return ref == agentapi.InvestigationReportSchemaRef() &&
		(errors.Is(err, execution.ErrReasoningTruncated) ||
			errors.Is(err, execution.ErrEmptyModelResponse) ||
			errors.Is(err, execution.ErrAnswerTruncated) ||
			errors.Is(err, execution.ErrModelCallBudgetExhausted))
}

func canRecoverInvestigationReport(
	ref agentapi.SchemaRef,
	err error,
) bool {
	return ref == (agentapi.InvestigationReportSchemaRef()) && (errors.Is(err, execution.ErrReasoningTruncated) ||
		errors.Is(err, execution.ErrEmptyModelResponse) ||
		errors.Is(err, execution.ErrAnswerTruncated) ||
		errors.Is(err, execution.ErrModelCallBudgetExhausted))
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
	_, hasCovered := report["covered_evidence_goals"]
	_, hasUnresolved := report["unresolved_evidence_goals"]
	if hasCovered && hasUnresolved {
		return nil, false
	}
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

func failedRun(runID, code string, err error) agentapi.RunResult {
	return agentapi.RunResult{
		RunID: runID, Status: agentapi.RunFailed,
		Error: &agentapi.RunError{Code: code, Message: err.Error()},
	}
}
