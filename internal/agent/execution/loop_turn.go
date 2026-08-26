package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/tool"
)

type modelTurn struct {
	step     int
	result   *llm.ChatStreamResult
	stream   *StreamPipe
	started  time.Time
	duration time.Duration
	timing   StreamTiming
}

type toolTurnOutcome struct {
	producedEvidence bool
	webAttempted     bool
	webSucceeded     bool
}

func (agent *Agent) runTurns(state *compiledLoop) error {
	for step := 1; step <= state.stepLimit; step++ {
		if agent.controller != nil &&
			agent.handleControl(state.runCtx, state.runID, step, &state.messages, state.result) {
			break
		}
		if err := agent.ensureTurnBudget(state, step); err != nil {
			return err
		}

		turn, err := agent.callModelTurn(state, step)
		if err != nil {
			if !state.answerContract.Active() &&
				agent.preservePartialAnswer(
					state.runCtx,
					state.runID,
					&state.stepSeq,
					state.result,
					turn.result,
					turn.stream,
					turn.started,
					turn.duration,
				) {
				state.result.Err = fmt.Errorf("agent step %d: %w", step, err)
				log.WarnfCtx(state.ctx, "[agent] run %s preserving partial answer from interrupted step %d: %v",
					state.runID, step, err)
				break
			}
			if state.loopCtx.Err() != nil {
				log.InfofCtx(state.ctx, "[agent] run %s loop budget exhausted at step %d: %v",
					state.runID, step, state.loopCtx.Err())
				break
			}
			return fmt.Errorf("agent step %d: %w", step, err)
		}
		if agent.cfg.BudgetCheck != nil {
			if err := agent.cfg.BudgetCheck(); err != nil {
				return err
			}
		}
		if len(turn.result.ToolCalls) == 0 {
			agent.handleAnswerTurn(state, turn)
			break
		}

		if err := agent.recordThinkTurn(state, turn); err != nil {
			state.result.Err = err
			break
		}
		outcome := agent.executeToolTurn(state, turn.result.ToolCalls)
		if state.result.Err != nil {
			break
		}
		if state.toolBudgetExhausted {
			log.InfofCtx(
				state.ctx,
				"[agent] run %s tool-call budget exhausted; forcing conclusion with collected evidence",
				state.runID,
			)
			break
		}
		agent.advanceTurn(state, step, outcome)
		log.InfofCtx(state.ctx, "[agent] run %s context size after step %d: %d chars",
			state.runID, step, contextChars(state.messages))
	}
	return nil
}

func (agent *Agent) ensureTurnBudget(state *compiledLoop, step int) error {
	if agent.cfg.BudgetCheck != nil {
		if err := agent.cfg.BudgetCheck(); err != nil {
			return err
		}
	}
	if _, err := agent.compactAnswerContext(
		state, state.tools, fmt.Sprintf("model_step_%d", step),
	); err != nil {
		return err
	}
	_, err := runtrace.Invoke(
		state.ctx,
		contextBudgetSpec,
		contextBudgetInput{Step: step, Messages: state.messages, Tools: state.tools},
		func(_ context.Context, input contextBudgetInput) (struct{}, error) {
			return struct{}{}, agent.ensureInputBudget(input.Messages, input.Tools)
		},
	)
	return err
}

func (agent *Agent) callModelTurn(state *compiledLoop, step int) (modelTurn, error) {
	started := time.Now()
	stream := newStreamPipe(agent.observer, state.runID, step, started, agent.onFirstAnswerToken)
	callCtx := llm.WithUsagePhase(state.loopCtx, llm.PhaseAgentStep)
	output, err := runtrace.Invoke(
		callCtx,
		agentModelTurnSpec,
		agentModelTurnInput{
			Step: step, Messages: state.messages, Tools: state.tools, Stream: stream,
		},
		func(callCtx context.Context, input agentModelTurnInput) (agentModelTurnOutput, error) {
			result, callErr := agent.callModel(
				callCtx,
				input.Messages,
				input.Tools,
				input.Stream,
				agent.cfg.AnswerMaxTokens,
			)
			return agentModelTurnOutput{Result: result, Timing: input.Stream.Timings()}, callErr
		},
	)
	turn := modelTurn{
		step: step, result: output.Result, stream: stream, started: started,
		duration: time.Since(started), timing: output.Timing,
	}
	if err != nil {
		return turn, err
	}
	log.InfofCtx(state.ctx, "[agent] run %s model step %d timing: total=%s firstEvent=%s firstReasoning=%s firstContent=%s firstToolDelta=%s firstToolCall=%s",
		state.runID, step, turn.duration, turn.timing.FirstEvent, turn.timing.FirstReasoning,
		turn.timing.FirstContent, turn.timing.FirstToolDelta, turn.timing.FirstToolCall)
	state.result.Steps = step
	return turn, nil
}

func (agent *Agent) handleAnswerTurn(state *compiledLoop, turn modelTurn) {
	result := turn.result
	recordFirstAnswerToken(
		state.ctx,
		turn.step,
		turn.timing.FirstContent,
		turn.started.Sub(state.runStarted)+turn.timing.FirstContent,
	)
	continued, err := agent.continueIfNeeded(
		state.loopCtx,
		state.messages,
		result,
		agent.cfg.AnswerMaxTokens,
		turn.stream,
	)
	result = continued
	if err != nil && !state.answerContract.Active() &&
		agent.preservePartialAnswer(
			state.runCtx,
			state.runID,
			&state.stepSeq,
			state.result,
			result,
			turn.stream,
			turn.started,
			turn.duration,
		) {
		state.result.Err = err
		log.WarnfCtx(state.ctx, "[agent] run %s preserving partial final answer at step %d: %v",
			state.runID, turn.step, err)
		return
	}
	if errors.Is(err, ErrAnswerTruncated) &&
		state.answerContract.Active() &&
		validateAndStripContractPartial(result, state.answerContract) {
		state.result.DelegationAdoptions = state.answerContract.Adoptions()
		turn.stream.Publish(result.Content)
		state.result.Answer += result.Content
		state.stepSeq++
		_ = agent.observer.OnStep(state.runCtx, state.runID, StepRecord{
			StepNo:              state.stepSeq,
			Kind:                StepKindAnswer,
			Content:             result.Content,
			DelegationAdoptions: cloneDelegationAdoptions(state.result.DelegationAdoptions),
			TokenDelta:          utf8.RuneCountInString(result.Content),
			ReasoningTokens:     result.ReasoningTokens,
			DurationMs:          int(turn.duration / time.Millisecond),
			CreatedAt:           turn.started,
		})
		state.result.Err = err
		log.WarnfCtx(
			state.ctx,
			"[agent] run %s preserving contract-valid partial final answer at step %d: %v",
			state.runID,
			turn.step,
			err,
		)
		return
	}
	if errors.Is(err, ErrReasoningTruncated) || errors.Is(err, ErrEmptyModelResponse) {
		log.WarnfCtx(state.ctx, "[agent] run %s final-answer generation produced no visible content; forcing conclusion: %v",
			state.runID, err)
		return
	}
	if err == nil {
		result, err = agent.enforceContract(
			state.loopCtx,
			state.messages,
			result,
			state.answerContract,
			agent.cfg.AnswerMaxTokens,
			turn.stream,
		)
	}
	if err != nil && state.answerContract.Active() {
		state.result.Err = err
		log.ErrorfCtx(state.ctx, "[agent] run %s exact-answer validation failed at step %d: %v",
			state.runID, turn.step, err)
		return
	}

	state.result.DelegationAdoptions = state.answerContract.Adoptions()
	turn.stream.Publish(result.Content)
	state.result.Answer += result.Content
	state.stepSeq++
	_ = agent.observer.OnStep(state.runCtx, state.runID, StepRecord{
		StepNo:              state.stepSeq,
		Kind:                StepKindAnswer,
		Content:             result.Content,
		DelegationAdoptions: cloneDelegationAdoptions(state.result.DelegationAdoptions),
		TokenDelta:          utf8.RuneCountInString(result.Content),
		ReasoningTokens:     result.ReasoningTokens,
		DurationMs:          int(turn.duration / time.Millisecond),
		CreatedAt:           turn.started,
	})
	if err != nil {
		state.result.Err = err
		log.ErrorfCtx(state.ctx, "[agent] run %s final-answer truncated: %v", state.runID, err)
		return
	}
	state.answered = true
	log.InfofCtx(state.ctx, "[agent] run %s done at step %d (final answer)",
		state.runID, turn.step)
}

func (agent *Agent) recordThinkTurn(state *compiledLoop, turn modelTurn) error {
	reasoning := turn.result.Reasoning
	if reasoning == "" {
		reasoning = turn.result.Content
	}
	state.stepSeq++
	if err := agent.observer.OnStep(state.runCtx, state.runID, StepRecord{
		StepNo:              state.stepSeq,
		Kind:                StepKindThink,
		Content:             reasoning,
		PromptContent:       turn.result.Content,
		AuthoritativeSHA256: toolContentSHA256(reasoning),
		PromptSHA256:        toolContentSHA256(turn.result.Content),
		SizeBytes:           int64(len(reasoning)),
		TokenDelta:          utf8.RuneCountInString(reasoning),
		ReasoningTokens:     turn.result.ReasoningTokens,
		DurationMs:          int(turn.duration / time.Millisecond),
		CreatedAt:           turn.started,
	}); err != nil {
		return fmt.Errorf("persist tool reasoning at model step %d: %w", turn.step, err)
	}
	turn.stream.Discard()

	message := llm.Message{
		Role:      "assistant",
		Content:   turn.result.Content,
		ToolCalls: canonicalToolCalls(turn.result.ToolCalls),
	}
	state.messages = append(state.messages, message)
	state.result.SessionMessages = append(state.result.SessionMessages, message)
	return nil
}

func (agent *Agent) executeToolTurn(state *compiledLoop, calls []llm.ToolCall) toolTurnOutcome {
	var outcome toolTurnOutcome
	var notices []string
	for index, call := range calls {
		if agent.cfg.MaxToolCalls > 0 &&
			int64(state.result.Evidence.ToolCallCount) >= agent.cfg.MaxToolCalls {
			agent.appendBudgetSkippedToolResults(state, calls[index:])
			state.toolBudgetExhausted = true
			break
		}
		state.result.Evidence.ToolCallCount++
		state.stepSeq++
		if err := agent.observer.OnStep(state.runCtx, state.runID, StepRecord{
			StepNo:     state.stepSeq,
			Kind:       StepKindToolCall,
			ToolCallID: call.ID,
			Tool:       call.Function.Name,
			Args:       call.Function.Arguments,
			CreatedAt:  time.Now(),
		}); err != nil {
			state.result.Err = fmt.Errorf("persist tool call %q: %w", call.ID, err)
			return outcome
		}

		executionCall, admission := agent.admitToolCall(state, call)
		var execution ToolExecution
		switch admission.Action {
		case toolAdmissionAlreadyAvailable, toolAdmissionDenyBudget:
			execution = toolAdmissionExecution(admission)
		default:
			execution = agent.executor.ExecuteLimited(
				state.loopCtx,
				state.toolSnapshot,
				executionCall,
				state.input.ReferenceTypes,
				state.seenTools,
				state.runID,
				agent.cfg.MaxToolResultBytes,
			)
			if !execution.Failed {
				conflicts := state.evidenceLedger.add(execution.EvidenceUnits, "tool")
				if len(conflicts) > 0 {
					notices, err := marshalConflictNotices(conflicts)
					if err != nil {
						state.result.Err = fmt.Errorf(
							"prepare evidence conflict notice for tool %q: %w",
							executionCall.Function.Name,
							err,
						)
						return outcome
					}
					execution.Notices = append(execution.Notices, notices...)
					execution.Coverage.Complete = false
					execution.Coverage.Partial = true
				}
			}
		}
		if state.input.OutputMode == agentapi.RunOutputEvidenceWorker &&
			!execution.Failed && execution.Evidence {
			appendEvidenceObservations(&state.result.EvidenceObservations, execution, executionCall.Function.Name)
		}
		execution = agent.prepareDelivery(
			state.runID,
			state.messages,
			notices,
			state.tools,
			executionCall,
			state.answerContract,
			execution,
		)
		if execution.Failed {
			state.result.Evidence.ToolFailureCount++
		} else if execution.Evidence {
			state.result.Evidence.ResultCount++
		}
		outcome.producedEvidence = outcome.producedEvidence || execution.Evidence

		acceptedWebEvidence := false
		if !execution.Failed {
			acceptedWebEvidence = state.webEvidence.Observe(executionCall, execution.AuthoritativeContent)
		}
		if !execution.Failed && execution.Evidence && execution.DeliveryError == "" {
			mergeToolReferences(&state.result.References, execution.References)
		}
		if isWebEvidenceTool(executionCall.Function.Name) {
			outcome.webAttempted = true
			outcome.webSucceeded = outcome.webSucceeded || acceptedWebEvidence
		}
		if execution.Coverage.Partial {
			state.result.Evidence.PartialResultCount++
		}
		state.result.Evidence.OmittedItemCount += execution.Coverage.OmittedItems

		state.stepSeq++
		toolResultStep := newToolResultStep(state.runID, state.stepSeq, executionCall, execution)
		if err := agent.observer.OnStep(state.runCtx, state.runID, toolResultStep); err != nil {
			state.result.Err = fmt.Errorf("persist tool result trace %q: %w", toolResultStep.TraceID, err)
			return outcome
		}
		log.InfofCtx(state.ctx, "[agent] tool result trace persisted: trace_id=%s tool_call_id=%s tool=%s bytes=%d sha256=%s artifact_id=%s failed=%v",
			toolResultStep.TraceID, executionCall.ID, executionCall.Function.Name, toolResultStep.SizeBytes,
			toolResultStep.AuthoritativeSHA256, execution.ArtifactID, execution.Failed)
		if execution.DeliveryError != "" {
			log.WarnfCtx(state.ctx, "[agent] tool %s delivery failed: trace_id=%s reason=%s bytes=%d",
				executionCall.Function.Name, toolResultStep.TraceID, execution.DeliveryError, toolResultStep.SizeBytes)
		}

		consumeToolTokens(state, execution.PromptContent)
		message := toolMessage(executionCall.ID, executionCall.Function.Name, execution.PromptContent)
		state.messages = append(state.messages, message)
		state.result.SessionMessages = append(state.result.SessionMessages, message)
		if execution.Failed {
			continue
		}
		notices = append(notices, execution.Notices...)
		state.answerContract.Add(execution.AnswerContract)
	}
	if agent.cfg.MaxToolCalls > 0 &&
		int64(state.result.Evidence.ToolCallCount) >= agent.cfg.MaxToolCalls {
		state.toolBudgetExhausted = true
		notices = append(
			notices,
			fmt.Sprintf(
				"Tool-call budget exhausted after %d calls. Do not request more tools; summarize the evidence already available.",
				agent.cfg.MaxToolCalls,
			),
		)
	}
	// Provider protocols forbid non-tool messages inside a parallel result group.
	state.messages = appendToolTurnPostlude(state.messages, notices, state.answerContract)
	return outcome
}

const (
	maxEvidenceObservationTokens = 256
	maxEvidenceObservations      = 128
)

func appendEvidenceObservations(
	observations *[]agentapi.EvidenceObservation,
	execution ToolExecution,
	toolName string,
) {
	if observations == nil || len(*observations) >= maxEvidenceObservations {
		return
	}
	summary := tooloutput.TruncateContent(strings.TrimSpace(execution.AuthoritativeContent), maxEvidenceObservationTokens)
	if summary == "" {
		return
	}
	if len(execution.EvidenceUnits) == 0 {
		*observations = append(*observations, agentapi.EvidenceObservation{
			SourceKind: "tool", Target: toolName, Summary: summary,
		})
		return
	}
	for _, unit := range execution.EvidenceUnits {
		if len(*observations) >= maxEvidenceObservations {
			return
		}
		section := ""
		if len(unit.Sections) > 0 {
			section = unit.Sections[0]
		}
		*observations = append(*observations, agentapi.EvidenceObservation{
			SourceKind:    unit.SourceKind,
			Target:        unit.Target,
			Section:       section,
			Summary:       summary,
			ContentHash:   unit.ContentHash,
			Facets:        append([]string(nil), unit.Facets...),
			TrustTier:     unit.TrustTier,
			EvidenceClass: unit.EvidenceClass,
			Version:       unit.Version,
			TimeRange:     unit.TimeRange,
		})
	}
}

// appendBudgetSkippedToolResults closes every omitted call in a parallel tool
// group so the next provider request still satisfies tool-call protocol.
func (agent *Agent) appendBudgetSkippedToolResults(
	state *compiledLoop,
	calls []llm.ToolCall,
) {
	const content = `{"error":"tool_call_budget_exhausted","message":"Tool call skipped because the run tool-call budget is exhausted."}`
	for _, call := range calls {
		message := toolMessage(call.ID, call.Function.Name, content)
		state.messages = append(state.messages, message)
		state.result.SessionMessages = append(state.result.SessionMessages, message)
	}
}

func appendToolTurnPostlude(messages []llm.Message, notices []string, contract *exactAnswerContract) []llm.Message {
	postlude := make([]llm.Message, 0, len(notices)+1)
	for _, notice := range notices {
		postlude = append(postlude, deliveryNoticeMessage(notice))
	}
	contractMessage, ok := combinedContractMessage(contract, tool.AnswerContract{})
	if !ok {
		return append(messages, postlude...)
	}
	messages = withoutContractMessages(messages)
	messages = append(messages, postlude...)
	return append(messages, contractMessage)
}

func (agent *Agent) advanceTurn(state *compiledLoop, step int, outcome toolTurnOutcome) {
	if extended := extendEvidenceStepLimit(
		step,
		state.stepLimit,
		agent.cfg.MaxSteps,
		outcome.producedEvidence,
		state.evidenceTurnExtended,
	); extended > state.stepLimit {
		state.stepLimit = extended
		state.evidenceTurnExtended = true
		log.InfofCtx(state.ctx, "[agent] run %s extending after boundary evidence (newLimit=%d configured=%d)",
			state.runID, state.stepLimit, agent.cfg.MaxSteps)
	}
	if !state.input.Web {
		return
	}
	if hint := state.webEvidence.ConvergenceHint(); hint != "" {
		state.messages = append(state.messages, llm.Message{Role: "system", Content: hint})
	}
	if extended := extendWebStepLimit(
		step,
		state.stepLimit,
		agent.cfg.MaxSteps,
		outcome.webAttempted,
		outcome.webSucceeded,
	); extended > state.stepLimit {
		state.stepLimit = extended
		log.InfofCtx(state.ctx, "[agent] run %s extending web research after unusable evidence (newLimit=%d configured=%d)",
			state.runID, state.stepLimit, agent.cfg.MaxSteps)
	}
}
