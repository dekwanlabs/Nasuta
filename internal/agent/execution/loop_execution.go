package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/delegation"
	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

// compiledLoop holds only the mutable execution state of one in-flight Run.
type compiledLoop struct {
	ctx          context.Context
	runCtx       context.Context
	loopCtx      context.Context
	runID        string
	input        Input
	toolSnapshot tool.Snapshot
	runStarted   time.Time

	answerContract             *exactAnswerContract
	messages                   []llm.Message
	tools                      []llm.ToolDef
	initialMessageCount        int
	answerToolSources          map[int]string
	result                     *RunResult
	stepSeq                    int
	answered                   bool
	seenTools                  map[string]bool
	evidenceLedger             *runEvidenceLedger
	remainingToolTokens        int
	webEvidence                webEvidenceState
	evidenceTurnExtended       bool
	stepLimit                  int
	toolBudgetExhausted        bool
	structuredLastStepReminded bool
	answerRecoveryPending      bool
	delegatedFlows             []agentapi.FlowIR
	dispatchedDelegations      []string
	settledDelegations         map[string]bool
	startStep                  int
}

func (agent *Agent) prepareLoop(
	ctx context.Context,
	runCtx context.Context,
	loopCtx context.Context,
	runID string,
	input Input,
	toolSnapshot tool.Snapshot,
	runStarted time.Time,
) *compiledLoop {
	maxSteps := agent.cfg.MaxSteps
	log.InfofCtx(ctx, "[agent] run %s start: %q (maxSteps=%d configured=%d timeout=%s reserve=%s)",
		runID, platform.TruncateForLog(input.Question, 10), maxSteps, agent.cfg.MaxSteps,
		agent.cfg.Timeout, agent.cfg.AnswerReserve)

	historyStarted := time.Now()
	messages, _ := runtrace.Invoke(
		ctx,
		historyCompileSpec,
		historyCompileInput{Messages: input.Messages},
		func(_ context.Context, input historyCompileInput) ([]llm.Message, error) {
			return append([]llm.Message(nil), input.Messages...), nil
		},
	)
	log.InfofCtx(ctx, "[agent] run %s request compiled in %s: messages=%d contextChars=%d",
		runID, time.Since(historyStarted), len(messages), contextChars(messages))

	tools := agent.prepareTools(ctx, runID, input, toolSnapshot)
	result := &RunResult{RunID: runID, OutputMode: input.OutputMode}
	if input.EvidenceSeeded {
		result.Evidence.ResultCount = 1
	}
	state := &compiledLoop{
		ctx:                 ctx,
		runCtx:              runCtx,
		loopCtx:             loopCtx,
		runID:               runID,
		input:               input,
		toolSnapshot:        toolSnapshot,
		runStarted:          runStarted,
		answerContract:      &exactAnswerContract{},
		messages:            messages,
		tools:               tools,
		initialMessageCount: len(messages),
		answerToolSources:   make(map[int]string),
		result:              result,
		seenTools:           map[string]bool{},
		evidenceLedger:      newRunEvidenceLedger(input.EvidenceUnits, input.EvidenceConflicts),
		settledDelegations:  map[string]bool{},
		remainingToolTokens: initialToolTokenBudget(agent, messages, tools),
		stepLimit:           maxSteps,
		startStep:           1,
	}
	state.recordSeedEvidence(agent.observer)
	return state
}

func (agent *Agent) prepareTools(
	ctx context.Context,
	runID string,
	input Input,
	toolSnapshot tool.Snapshot,
) []llm.ToolDef {
	tools := agent.executor.Definitions(toolSnapshot)
	if len(input.OfferedToolIDs) == 0 && !input.ToolPruningApplied {
		return tools
	}
	pruning, _ := runtrace.Invoke(
		ctx,
		toolPruningSpec,
		toolPruningInput{
			Tools: tools, Offered: input.OfferedToolIDs, Applied: input.ToolPruningApplied,
		},
		func(_ context.Context, input toolPruningInput) (toolPruningOutput, error) {
			effective := prunedDefinitions(input.Tools, input.Offered)
			fullEncoded, _ := json.Marshal(input.Tools)
			prunedEncoded, _ := json.Marshal(effective)
			return toolPruningOutput{
				Effective:    effective,
				FullTokens:   tooloutput.EstimateTokens(string(fullEncoded)),
				PrunedTokens: tooloutput.EstimateTokens(string(prunedEncoded)),
				RemovedIDs:   removedToolDefIDs(input.Tools, effective),
			}, nil
		},
	)
	log.InfofCtx(ctx, "[agent] run %s tool pruning: applied=%t offered=%d/%d tokens=%d->%d saved=%d removed=%v",
		runID, input.ToolPruningApplied, len(pruning.Effective), len(tools),
		pruning.FullTokens, pruning.PrunedTokens, pruning.FullTokens-pruning.PrunedTokens,
		pruning.RemovedIDs)
	if input.ToolPruningApplied {
		return pruning.Effective
	}
	return tools
}

func (state *compiledLoop) recordSeedEvidence(observer Observer) {
	if state.input.EvidenceContent == "" {
		return
	}
	state.stepSeq++
	_ = observer.OnStep(state.runCtx, state.runID, StepRecord{
		StepNo:     state.stepSeq,
		Kind:       StepKindRetrieval,
		Content:    state.input.EvidenceContent,
		TokenDelta: utf8.RuneCountInString(state.input.EvidenceContent),
		CreatedAt:  time.Now(),
	})
}

// mergeDelegatedFlows folds child FlowIRs into the server-owned flow once.
// It is called both at the answer turn (so the deterministic renderer can
// replace model-owned Mermaid) and again in finishLoop. It is idempotent and
// never downgrades an already-merged flow.
func (agent *Agent) mergeDelegatedFlows(state *compiledLoop) {
	if state == nil || len(state.delegatedFlows) == 0 {
		return
	}
	if state.result.Flow != nil {
		// Already merged (e.g. recovered from a checkpoint); do not re-merge.
		return
	}
	merged, err := delegation.MergeFlowIRs(state.delegatedFlows)
	if err != nil {
		log.WarnfCtx(state.ctx, "[agent] run %s flow merge failed: %v", state.runID, err)
		return
	}
	state.result.Flow = merged
}

func (agent *Agent) finishLoop(state *compiledLoop) {
	// Close out any delegation the parent dispatched but never awaited mid-loop
	// before the final answer is rendered, so the durable budget reservations
	// have settled by the time the caller releases the run lease.
	agent.awaitUnsettledDelegations(state)
	agent.mergeDelegatedFlows(state)
	if agent.shouldForceConclusion(state) {
		state.result.ForcedConclusion = true
		state.result.Evidence.ForcedConclusion = true
		log.InfofCtx(state.ctx, "[agent] run %s forcing conclusion (steps=%d)",
			state.runID, state.result.Steps)
		agent.concludeLoop(state)
	}
	agent.finalizeLoop(state)
}

func (agent *Agent) shouldForceConclusion(state *compiledLoop) bool {
	if state.answerRecoveryPending {
		return true
	}
	if state.answered || state.result.Aborted || state.result.Err != nil {
		return false
	}
	if state.input.OutputMode != agentapi.RunOutputEvidenceWorker {
		return true
	}
	// Structured investigators must emit investigation.report. Tool-only
	// evidence workers without a schema still stop after observations.
	return agent.cfg.StructuredOutput
}

func (agent *Agent) reservesLastStepForAnswer() bool {
	return agent.cfg.StructuredOutput
}

func (agent *Agent) toolsForStep(state *compiledLoop, step int) []llm.ToolDef {
	if agent.reservesLastStepForAnswer() && state.stepLimit > 1 && step >= state.stepLimit {
		return nil
	}
	return state.tools
}

func (agent *Agent) finalizeLoop(state *compiledLoop) {
	switch {
	case state.result.Aborted:
		state.result.DelegationAdoptions = state.answerContract.UnknownAdoptions(
			"parent_cancelled",
		)
	case state.result.Err != nil:
		state.result.DelegationAdoptions = state.answerContract.UnknownAdoptions(
			"parent_run_failed",
		)
	case state.answerContract != nil &&
		len(state.answerContract.delegationOrder) > 0 &&
		len(state.result.DelegationAdoptions) == 0:
		state.result.DelegationAdoptions = state.answerContract.UnknownAdoptions(
			"final_answer_unavailable",
		)
	}
	state.result.EvidenceUnits, state.result.EvidenceConflicts = state.evidenceLedger.snapshot()
	state.result.Evidence.Finalize(state.input.Direct)
	log.InfofCtx(state.ctx, "[agent] run %s end: steps=%d answerLen=%d aborted=%v err=%v",
		state.runID, state.result.Steps, len(state.result.Answer), state.result.Aborted, state.result.Err)
	if answer := strings.TrimSpace(state.result.Answer); answer != "" {
		log.InfofCtx(state.ctx, "[agent] run %s answer:\n%s",
			state.runID, platform.TruncateForLog(answer, 4000))
	}
}

// installDeterministicConclusion writes a guaranteed non-empty answer when the
// normal force-conclusion path fails, so a slow child can never leave the run
// with answerLen=0. It derives a conservative prose answer from the evidence
// already observed, then passes it through the exact-answer contract so
// adoption metadata is consumed server-side instead of leaking as unknown.
func (agent *Agent) installDeterministicConclusion(state *compiledLoop, cause error) bool {
	if state == nil || state.result == nil {
		return false
	}
	answer := deterministicConclusionProse(state)
	if strings.TrimSpace(answer) == "" {
		return false
	}
	if state.answerContract != nil && state.answerContract.Active() {
		withMetadata, err := state.answerContract.appendConservativeFallbackMetadata(answer)
		if err != nil {
			log.WarnfCtx(state.ctx, "[agent] run %s deterministic conclusion contract metadata failed: %v", state.runID, err)
			return false
		}
		visible, violations := state.answerContract.ValidateAndStrip(withMetadata)
		if len(violations) > 0 {
			log.WarnfCtx(state.ctx, "[agent] run %s deterministic conclusion contract rejected: %v", state.runID, violations)
			return false
		}
		answer = visible
		state.result.DelegationAdoptions = state.answerContract.Adoptions()
	}
	state.result.Answer = answer
	state.result.Err = nil
	state.result.ForcedConclusion = true
	state.result.Evidence.ForcedConclusion = true
	if cause != nil {
		log.WarnfCtx(state.ctx, "[agent] run %s installed deterministic conclusion after %v", state.runID, cause)
	}
	return true
}

// deterministicConclusionProse renders a conservative, evidence-derived answer
// without a model call. It is the final deterministic backstop before a run
// would otherwise return an empty answer.
func deterministicConclusionProse(state *compiledLoop) string {
	var parts []string
	if question := strings.TrimSpace(state.input.Question); question != "" {
		parts = append(parts, "问题："+question)
	}
	parts = append(parts, "由于时间或模型调用不可用，本次未完成完整分析。")
	resultCount := state.result.Evidence.ResultCount
	partialCount := state.result.Evidence.PartialResultCount
	if resultCount > 0 || partialCount > 0 {
		parts = append(parts, fmt.Sprintf(
			"已收集 %d 条完整结果、%d 条部分结果，但尚未整合成最终结论。",
			resultCount, partialCount,
		))
	}
	if state.result.Evidence.ToolFailureCount > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d 次工具调用失败。", state.result.Evidence.ToolFailureCount,
		))
	}
	parts = append(parts, "以下为当前已确认的信息，仍需进一步核实后才能给出确定性结论。")
	return strings.Join(parts, "\n")
}

func isRecoverableConclusionError(err error) bool {
	return errors.Is(err, ErrModelCallBudgetExhausted) ||
		errors.Is(err, ErrReasoningTruncated) ||
		errors.Is(err, ErrEmptyModelResponse) ||
		errors.Is(err, ErrAnswerTruncated)
}

func (agent *Agent) concludeLoop(state *compiledLoop) {
	if _, err := agent.compactAnswerContext(state, nil, "forced_conclusion"); err != nil {
		state.result.Err = fmt.Errorf("compact context before forced conclusion: %w", err)
		log.ErrorfCtx(state.ctx, "[agent] run %s final-answer context compaction failed: %v",
			state.runID, err)
		return
	}
	if agent.cfg.BudgetCheck != nil {
		if err := agent.cfg.BudgetCheck(); err != nil {
			state.result.Err = err
			return
		}
	}
	final, err := agent.forceConclusion(
		state.runCtx,
		state.runID,
		state.messages,
		state.answerContract,
		&state.stepSeq,
		state.runStarted,
	)
	if err != nil {
		validPartial := final != nil &&
			state.answerContract.Satisfied(final.Content)
		if hasDeliverableAnswer(final) && validPartial && !errors.Is(err, ErrAnswerContractViolation) {
			state.result.Answer += final.Content
			state.result.DelegationAdoptions = state.answerContract.Adoptions()
			state.result.Err = err
			log.WarnfCtx(state.ctx, "[agent] run %s preserving partial force-conclusion answer: %v",
				state.runID, err)
		} else if agent.installDeterministicConclusion(state, err) {
			return
		} else {
			state.result.Err = err
			if isRecoverableConclusionError(err) {
				log.WarnfCtx(state.ctx, "[agent] run %s force-conclusion unavailable; recovery may use evidence or a deterministic renderer: %v", state.runID, err)
			} else {
				log.ErrorfCtx(state.ctx, "[agent] run %s force-conclusion error: %v", state.runID, err)
			}
		}
	} else if final != nil {
		state.result.Answer += final.Content
		state.result.DelegationAdoptions = state.answerContract.Adoptions()
	}
}

// mergeDelegationDispatchFlows folds child FlowIRs present in a dispatch
// projection into the server-owned flow ledger. The actual merge stays in
// mergeDelegatedFlows; this only collects the child flows without downgrading
// an already-merged result.
func (agent *Agent) mergeDelegationDispatchFlows(state *compiledLoop, dispatch agentapi.DelegationDispatchResult) {
	if state == nil {
		return
	}
	for _, task := range dispatch.Tasks {
		if task.Report == nil || task.Report.Flow == nil {
			continue
		}
		state.delegatedFlows = append(state.delegatedFlows, *cloneExecutionFlow(task.Report.Flow))
	}
}

// parseDelegationDispatch decodes the authoritative delegate_investigation
// result into its streaming projection shape.
func parseDelegationDispatch(content string) (agentapi.DelegationDispatchResult, bool) {
	var dispatch agentapi.DelegationDispatchResult
	if strings.TrimSpace(content) == "" {
		return dispatch, false
	}
	if err := json.Unmarshal([]byte(content), &dispatch); err != nil {
		return dispatch, false
	}
	return dispatch, true
}

// awaitDelegationSettlement blocks until one dispatched delegation settles (or
// the parent answer window closes) and backfills the finished reports into the
// parent's answer contract and flow ledger. The second return reports whether
// every admitted child actually settled, so a timed-out await is not mistaken
// for a complete batch.
func (agent *Agent) awaitDelegationSettlement(
	state *compiledLoop,
	delegationID string,
) (agentapi.DelegationDispatchResult, bool) {
	if agent.cfg.DelegationAwaiter == nil || strings.TrimSpace(delegationID) == "" {
		return agentapi.DelegationDispatchResult{}, false
	}
	dispatch, err := agent.cfg.DelegationAwaiter.AwaitSettlement(
		state.loopCtx, delegationID, time.Time{},
	)
	if err != nil {
		log.WarnfCtx(state.ctx, "[agent] run %s await delegation %s failed: %v",
			state.runID, delegationID, err)
		return agentapi.DelegationDispatchResult{}, false
	}
	agent.mergeDelegationDispatchFlows(state, dispatch)
	state.answerContract.Add(delegation.DelegationAdoptionContract(dispatch))
	if dispatchStillRunning(dispatch) {
		return dispatch, false
	}
	if state.settledDelegations == nil {
		state.settledDelegations = make(map[string]bool)
	}
	state.settledDelegations[delegationID] = true
	return dispatch, true
}

// dispatchStillRunning reports whether any admitted child has not yet settled.
func dispatchStillRunning(dispatch agentapi.DelegationDispatchResult) bool {
	for _, task := range dispatch.Tasks {
		if task.Status == agentapi.DelegationRunning {
			return true
		}
	}
	return false
}

// settledDelegationNotice renders the server-side "children finished" notice
// that hands the model the backfilled reports instead of delegation_status.
func settledDelegationNotice(dispatch agentapi.DelegationDispatchResult) llm.Message {
	encoded, err := json.Marshal(dispatch)
	if err != nil {
		encoded = []byte(`{"status":"unavailable"}`)
	}
	return llm.Message{
		Role: "system",
		Content: prompts.MustRender(prompts.AgentQADelegationSettled, struct {
			Dispatch string
		}{Dispatch: string(encoded)}),
	}
}

// awaitUnsettledDelegations is the finish-loop backstop: it waits for any
// delegation the parent dispatched but never awaited mid-loop, so the durable
// budget reservations have closed before the caller releases the run lease.
func (agent *Agent) awaitUnsettledDelegations(state *compiledLoop) {
	if agent.cfg.DelegationAwaiter == nil || state == nil {
		return
	}
	for _, delegationID := range state.dispatchedDelegations {
		if state.settledDelegations[delegationID] {
			continue
		}
		dispatch, err := agent.cfg.DelegationAwaiter.AwaitSettlement(
			state.runCtx, delegationID, time.Time{},
		)
		if err != nil {
			log.WarnfCtx(state.ctx, "[agent] run %s finish await delegation %s failed: %v",
				state.runID, delegationID, err)
			continue
		}
		agent.mergeDelegationDispatchFlows(state, dispatch)
		state.answerContract.Add(delegation.DelegationAdoptionContract(dispatch))
		if !dispatchStillRunning(dispatch) {
			if state.settledDelegations == nil {
				state.settledDelegations = make(map[string]bool)
			}
			state.settledDelegations[delegationID] = true
		}
	}
}
