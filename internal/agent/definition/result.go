package definition

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/messages"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/log"
)

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
		return mapCancelledResult(runID, result, cancelCause, preRetrieved, usage)
	}
	outcome := execution.OutcomeFor(result, preRetrieved, runErr)
	applyOutcomeErrorCode(&outcome)

	// Recovery may still turn an invalid/truncated answer into a clean success,
	// so it must run before the terminal partial classification below.
	if outcome.Status != run.StatusDone && len(recovery) > 0 {
		attemptOutputRecovery(runID, &outcome, outputSchema, schemas, recovery[0])
	}

	if outcome.Status != run.StatusDone {
		return mapFailedResult(runID, result, outcome, usage)
	}

	// A usable-but-incomplete or fallback answer is terminal-partial, never a
	// clean success. The execution loop records this explicitly via
	// Completeness="partial" (and FallbackUsed=true on fallback paths), so we
	// classify on those signals rather than on the default-false
	// AnswerComplete field. Direct callers that construct a valid RunResult
	// without running the loop keep the historical succeeded behavior.
	// A deterministic fallback keeps Err nil while recording the cause on
	// TerminationReason. It is not a deliverable partial answer, so it must
	// remain failed rather than be upgraded through the partial path below.
	deterministicFallback := result != nil &&
		result.ForcedConclusion &&
		result.FallbackUsed &&
		result.Err == nil
	if result != nil &&
		result.OutputMode != agentapi.RunOutputEvidenceWorker &&
		!deterministicFallback &&
		(outcome.FallbackUsed || outcome.Completeness == "partial") {
		return mapPartialResult(runID, result, outcome, usage)
	}

	return mapSucceededResult(runID, result, outcome, usage, schemas, outputSchema, recovery)
}

func mapCancelledResult(
	runID string,
	result *execution.RunResult,
	cancelCause error,
	preRetrieved []agentapi.Reference,
	usage agentapi.Usage,
) (agentapi.RunResult, run.Outcome) {
	outcome := execution.OutcomeFor(result, preRetrieved, cancelCause)
	outcome.Status = run.StatusAborted
	outcome.ErrorCode = "cancelled"
	outcome.DelegationAdoptions = unknownDelegationAdoptions(
		outcome.DelegationAdoptions,
		"parent_cancelled",
	)
	publicResult := publicTerminalEvidence(runID, result, outcome, usage)
	publicResult.Status = agentapi.RunCancelled
	publicResult.AnswerComplete = false
	publicResult.FallbackUsed = false
	publicResult.Completeness = "failed"
	publicResult.TerminationReason = "cancelled"
	publicResult.Error = &agentapi.RunError{
		Code: "cancelled", Message: cancelCause.Error(),
	}
	return publicResult, outcome
}

func applyOutcomeErrorCode(outcome *run.Outcome) {
	if errors.Is(outcome.Err, agentapi.ErrBudgetExceeded) {
		outcome.ErrorCode = "budget_exhausted"
	}
	if errors.Is(outcome.Err, execution.ErrToolCallBudgetExhausted) {
		outcome.ErrorCode = "tool_call_budget_exhausted"
	}
	if errors.Is(outcome.Err, errRunLimitExceeded) {
		outcome.ErrorCode = "run_limit_exceeded"
	}
}

func attemptOutputRecovery(
	runID string,
	outcome *run.Outcome,
	outputSchema agentapi.SchemaRef,
	schemas *agentapi.SchemaRegistry,
	recovery outputRecoveryContext,
) {
	if !recovery.StrictOutput &&
		canRecoverVerificationResult(outputSchema, outcome.Err) {
		recovered, recoveryErr := recoverVerificationResult(
			schemas, outputSchema, outcome.Answer,
		)
		if recoveryErr == nil {
			applyRecoveredOutput(outcome, recovered)
		}
	}
	if outcome.Status != run.StatusDone &&
		canRecoverInvestigationReportOutput(recovery, outputSchema, outcome.Err) {
		recovered, preserved, recoveryErr := recoverFailedInvestigationReport(
			schemas,
			outputSchema,
			recovery,
			outcome.Answer,
			outcome.Err,
		)
		if recoveryErr == nil {
			recoveryMode := "as an evidence-preserving partial report"
			if preserved {
				recoveryMode = "without discarding its findings"
			}
			applyRecoveredOutput(outcome, recovered)
			_ = recoveryMode
		} else {
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s could not recover model-output failure for %s from %s: %v",
				runID,
				outputSchema.ID,
				recovery.AgentID,
				recoveryErr,
			)
		}
	}
	if outcome.Status != run.StatusDone &&
		!recovery.StrictOutput &&
		canRecoverInvestigationAnswer(outputSchema, outcome.Err) {
		recovered, recoveryErr := recoverInvestigationAnswer(
			schemas,
			outputSchema,
			recovery,
		)
		if recoveryErr == nil {
			applyRecoveredOutput(outcome, recovered)
		} else {
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s could not recover unavailable %s output for %s: %v",
				runID,
				outputSchema.ID,
				recovery.AgentID,
				recoveryErr,
			)
		}
	}
}

func applyRecoveredOutput(outcome *run.Outcome, recovered []byte) {
	outcome.Status = run.StatusDone
	outcome.ErrorCode = ""
	outcome.Err = nil
	outcome.Answer = string(recovered)
	outcome.Evidence.ForcedConclusion = true
	outcome.AnswerComplete = true
	outcome.FallbackUsed = false
	outcome.Completeness = "complete"
	outcome.TerminationReason = "recovered"
}

func mapFailedResult(
	runID string,
	result *execution.RunResult,
	outcome run.Outcome,
	usage agentapi.Usage,
) (agentapi.RunResult, run.Outcome) {
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
	publicResult.AnswerComplete = false
	if publicResult.Completeness == "" || publicResult.Completeness == "complete" {
		publicResult.Completeness = "failed"
	}
	if publicResult.TerminationReason == "" {
		publicResult.TerminationReason = "failed"
	}
	publicResult.Error = &agentapi.RunError{
		Code: outcome.ErrorCode, Message: runError.Error(),
		Retryable: retryableError(runError),
	}
	return publicResult, outcome
}

func mapSucceededResult(
	runID string,
	result *execution.RunResult,
	outcome run.Outcome,
	usage agentapi.Usage,
	schemas *agentapi.SchemaRegistry,
	outputSchema agentapi.SchemaRef,
	recovery []outputRecoveryContext,
) (agentapi.RunResult, run.Outcome) {
	publicResult := publicTerminalEvidence(runID, result, outcome, usage)
	publicResult.Status = agentapi.RunSucceeded
	publicResult.AnswerComplete = true
	publicResult.FallbackUsed = false
	publicResult.Completeness = "complete"
	if publicResult.TerminationReason == "" {
		publicResult.TerminationReason = "completed"
	}
	if result != nil && result.OutputMode == agentapi.RunOutputEvidenceWorker {
		output, validationErr, recovered := attachEvidenceWorkerStructuredOutput(
			schemas, outputSchema, outcome.Answer, recovery,
		)
		if validationErr != nil {
			log.WarnfCtx(
				log.WithTraceID(context.Background(), runID),
				"[agent] run %s evidence-worker investigation.report answer_len=%d output_len=%d recovered=%t err=%v",
				runID, len(outcome.Answer), len(output), recovered, validationErr,
			)
		}
		if len(output) > 0 {
			publicResult.Output = output
		}
		return publicResult, outcome
	}
	publicResult.Text = outcome.Answer
	publicResult.References = append([]agentapi.Reference(nil), outcome.References...)
	publicResult.Messages = messages.Public(outcome.SessionMessages)
	output, err := validatedOutput(schemas, outputSchema, outcome.Answer)
	if err != nil && outputSchema == agentapi.InvestigationReportSchemaRef() && len(recovery) > 0 &&
		shouldRecoverInvalidInvestigationOutput(recovery[0], outcome.Answer) {
		if recovered, preserved, recoveryErr := recoverInvestigationReport(
			schemas, outputSchema, recovery[0], outcome.Answer, err,
		); recoveryErr == nil {
			validationErr := err
			echoedContract := isEchoedTaskContractAnswer(outcome.Answer)
			output = recovered
			err = nil
			outcome.Answer = string(recovered)
			publicResult.Text = outcome.Answer
			switch {
			case echoedContract:
				log.WarnfCtx(
					log.WithTraceID(context.Background(), runID),
					"[agent] run %s discarded echoed task-contract input for %s; replaced with a schema-valid investigation report",
					runID, recovery[0].AgentID,
				)
			case preserved:
				log.WarnfCtx(
					log.WithTraceID(context.Background(), runID),
					"[agent] run %s recovered invalid %s output for %s by deriving goal coverage: %v",
					runID, outputSchema.ID, recovery[0].AgentID, validationErr,
				)
			default:
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

// mapPartialResult renders a usable-but-incomplete result as a distinct
// terminal status instead of a clean success. It preserves the fallback and
// deadline/budget cause so downstream consumers and QA telemetry can tell
// "partial because deadline" apart from "failed because provider".
func mapPartialResult(
	runID string,
	result *execution.RunResult,
	outcome run.Outcome,
	usage agentapi.Usage,
) (agentapi.RunResult, run.Outcome) {
	publicResult := publicTerminalEvidence(runID, result, outcome, usage)
	publicResult.Status = agentapi.RunPartial
	publicResult.AnswerComplete = false
	publicResult.FallbackUsed = outcome.FallbackUsed
	publicResult.Completeness = "partial"
	if publicResult.TerminationReason == "" {
		publicResult.TerminationReason = "incomplete"
	}
	if outcome.Err != nil {
		publicResult.Error = &agentapi.RunError{
			Code: outcome.ErrorCode, Message: outcome.Err.Error(),
			Retryable: retryableError(outcome.Err),
		}
	}
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
) (json.RawMessage, error, bool) {
	if schemas == nil || ref != agentapi.InvestigationReportSchemaRef() {
		return nil, nil, false
	}
	if strings.TrimSpace(answer) == "" {
		return nil, nil, false
	}
	output, err := validatedOutput(schemas, ref, answer)
	if err == nil {
		return output, nil, false
	}
	if len(recovery) == 0 {
		return nil, err, false
	}
	recovered, _, recoveryErr := recoverInvestigationReport(
		schemas, ref, recovery[0], answer, err,
	)
	if recoveryErr != nil {
		return nil, err, false
	}
	return recovered, err, true
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
		AnswerComplete:    outcome.AnswerComplete,
		FallbackUsed:      outcome.FallbackUsed,
		Completeness:      outcome.Completeness,
		TerminationReason: outcome.TerminationReason,
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
