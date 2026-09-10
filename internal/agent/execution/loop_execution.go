package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
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

// answerLogRunes bounds the answer echoed to the run log. A schema-valid
// investigation.report does not fit in 4000 runes, so the old cap silently cut
// 25-62% of child answers and made post-hoc incident attribution impossible.
const answerLogRunes = 16000

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

func (state *compiledLoop) recordSeedEvidence(observer run.Observer) {
	if state.input.EvidenceContent == "" {
		return
	}
	state.stepSeq++
	_ = observer.OnStep(state.runCtx, state.runID, run.StepRecord{
		StepNo:     state.stepSeq,
		Kind:       run.StepKindRetrieval,
		Content:    state.input.EvidenceContent,
		TokenDelta: utf8.RuneCountInString(state.input.EvidenceContent),
		CreatedAt:  time.Now(),
	})
}

// mergeDelegatedFlows folds child FlowIRs into one server-owned FlowIR per
// subject. It is called both at the answer turn (so the deterministic renderer
// can replace model-owned diagrams) and again in finishLoop. It is idempotent
// and never downgrades an already-merged flow.
func (agent *Agent) mergeDelegatedFlows(state *compiledLoop) {
	if state == nil || len(state.delegatedFlows) == 0 {
		return
	}
	if state.result.Flows != nil {
		// Already merged (e.g. recovered from a checkpoint); do not re-merge.
		return
	}
	flows, err := delegation.MergeFlowIRsBySubject(state.delegatedFlows)
	if err != nil {
		log.WarnfCtx(state.ctx, "[agent] run %s flow merge failed: %v", state.runID, err)
		return
	}
	state.result.Flows = flows
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
	agent.classifyTerminalResult(state)
	agent.finalizeLoop(state)
}

// classifyTerminalResult derives the answer-complete, fallback-used, and
// termination-reason flags from loop facts so the public result never reports
// a fallback or partial answer as a clean success.
func (agent *Agent) classifyTerminalResult(state *compiledLoop) {
	if state == nil || state.result == nil {
		return
	}
	result := state.result

	// Completeness defaults to failed; the loops below upgrade it.
	result.Completeness = "failed"
	if result.Aborted {
		result.Completeness = "failed"
		if result.TerminationReason == "" {
			result.TerminationReason = "cancelled"
		}
		return
	}

	deadlineCause := context.DeadlineExceeded
	if result.Err != nil {
		result.TerminationReason = terminationReasonFor(result.Err, deadlineCause)
	}

	hasAnswer := strings.TrimSpace(result.Answer) != ""
	// A deterministic fallback is not a deliverable partial answer: it exists
	// only to keep the run from returning an empty body after the real
	// synthesis path failed (provider outage, deadline, budget). It is the
	// only fallback flavor that leaves Err nil (the cause is recorded on
	// TerminationReason instead), so we use that to distinguish it from a real
	// partial/fallback model answer that satisfied the output contract.
	deterministicFallback := hasAnswer &&
		result.ForcedConclusion &&
		result.FallbackUsed &&
		result.Err == nil

	switch {
	case state.answered && !result.ForcedConclusion && !result.FallbackUsed:
		result.AnswerComplete = true
		result.Completeness = "complete"
		if result.TerminationReason == "" {
			result.TerminationReason = "completed"
		}
	case hasAnswer && !deterministicFallback:
		result.AnswerComplete = false
		result.Completeness = "partial"
		if result.TerminationReason == "" {
			result.TerminationReason = "incomplete"
		}
	default:
		result.AnswerComplete = false
		result.Completeness = "failed"
		if result.TerminationReason == "" {
			result.TerminationReason = "failed"
		}
	}
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
	return agent.effectiveToolsForStep(state)
}

// effectiveToolsForStep applies phase-aware tool surface narrowing to the
// already-pruned definitions. Once every delegation the parent dispatched has
// settled, the parent must converge to synthesis instead of polling status or
// dispatching again, so the delegation tools and the status backfill query are
// removed from the offered surface.
func (agent *Agent) effectiveToolsForStep(state *compiledLoop) []llm.ToolDef {
	tools := state.tools
	if !agent.delegationSettled(state) {
		return tools
	}
	filtered := tools[:0]
	for _, def := range tools {
		name := def.Function.Name
		if name == string(delegation.DelegateToolID) ||
			name == string(delegation.DelegationStatusToolID) {
			continue
		}
		filtered = append(filtered, def)
	}
	return filtered
}

// delegationSettled reports whether the parent dispatched at least one
// delegation and every dispatched batch has reached a durable settlement.
func (agent *Agent) delegationSettled(state *compiledLoop) bool {
	if len(state.dispatchedDelegations) == 0 {
		return false
	}
	for _, id := range state.dispatchedDelegations {
		if !state.settledDelegations[id] {
			return false
		}
	}
	return true
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
			state.runID, platform.TruncateForLog(answer, answerLogRunes))
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
	answer := ""
	if agent != nil && agent.cfg.StructuredOutput {
		answer = structuredConclusionFallback(state)
	} else {
		answer = deterministicConclusionProse(state)
	}
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
	state.result.ForcedConclusion = true
	state.result.Evidence.ForcedConclusion = true
	state.result.FallbackUsed = true
	state.result.AnswerComplete = false
	if cause != nil {
		state.result.TerminationReason = terminationReasonFor(cause, context.DeadlineExceeded)
		log.WarnfCtx(state.ctx, "[agent] run %s installed deterministic conclusion after %v (termination_reason=%s)",
			state.runID, cause, state.result.TerminationReason)
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

// structuredConclusionFallback renders a schema-valid investigation.report when a
// structured investigator exhausted its completion budget before emitting any
// visible JSON. It never echoes the task input, so recovery cannot mistake the
// task contract for a report.
func structuredConclusionFallback(state *compiledLoop) string {
	focus := structuredConclusionFocus(state)
	units, _ := state.evidenceLedger.snapshot()
	if report, ok := BuildEvidencePreservingReport(
		units, state.result.EvidenceObservations, nil, focus,
	); ok {
		return string(report)
	}
	fallback := map[string]any{
		"focus":    focus,
		"summary":  "Evidence collection completed, but the final report could not be generated; no unverified conclusion was accepted.",
		"findings": []any{},
		"gaps": []string{
			"Evidence collection completed, but report generation ended before a schema-valid investigation.report was produced.",
		},
		"covered_evidence_goals":    []string{},
		"unresolved_evidence_goals": []string{},
	}
	encoded, err := json.Marshal(fallback)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// structuredConclusionFocus maps an investigator agent ID to the
// investigation.report focus enum. It falls back to "docs" when the agent
// identity is unavailable (for example in synthetic loop states).
func structuredConclusionFocus(state *compiledLoop) string {
	if state != nil && state.input.OriginalRequest != nil {
		switch state.input.OriginalRequest.Agent.ID {
		case "investigator.code":
			return "code"
		case "investigator.runtime":
			return "runtime"
		case "investigator.docs":
			return "docs"
		case "investigator.web":
			return "web"
		case "investigator.memory":
			return "memory"
		}
	}
	return "docs"
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
			state.result.FallbackUsed = true
			state.result.AnswerComplete = false
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
	for index, task := range dispatch.Tasks {
		if task.Report == nil || task.Report.Flow == nil {
			continue
		}
		flow := cloneExecutionFlow(task.Report.Flow)
		flow.Order = index + 1
		state.delegatedFlows = append(state.delegatedFlows, *flow)
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
		state.loopCtx, delegationID, agent.settlementDeadline(state),
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
	encoded, err := json.Marshal(delegationHandoffProjection(dispatch))
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

// delegationHandoffProjection bounds the server-side "children finished"
// notice to the fields the parent needs to synthesize an answer. Full reports,
// findings and flow edges stay in durable artifacts; only IDs, summaries,
// top-level claims and unresolved gaps are copied into provider messages.
func delegationHandoffProjection(dispatch agentapi.DelegationDispatchResult) agentapi.DelegationDispatchResult {
	if len(dispatch.Tasks) == 0 {
		return dispatch
	}
	tasks := make([]agentapi.DelegationTaskStatus, 0, len(dispatch.Tasks))
	for _, task := range dispatch.Tasks {
		tasks = append(tasks, delegationTaskProjection(task))
	}
	dispatch.Tasks = tasks
	return dispatch
}

func delegationTaskProjection(task agentapi.DelegationTaskStatus) agentapi.DelegationTaskStatus {
	if task.Report == nil {
		return task
	}
	report := task.Report
	projected := agentapi.DelegationTaskStatus{
		TaskID:  task.TaskID,
		Subject: task.Subject,
		Status:  task.Status,
		Report: &agentapi.DelegationReport{
			ReportID:      report.ReportID,
			Capability:    report.Capability,
			Status:        report.Status,
			Completeness:  report.Completeness,
			Summary:       report.Summary,
			Uncertainties: report.Uncertainties,
			Error:         report.Error,
		},
	}
	if report.Flow != nil {
		projected.Report.Flow = &agentapi.FlowIR{
			Subject:       report.Flow.Subject,
			Status:        report.Flow.Status,
			Uncertainties: report.Flow.Uncertainties,
			Confidence:    report.Flow.Confidence,
			Nodes:         make([]agentapi.FlowNode, 0, len(report.Flow.Nodes)),
			Edges:         make([]agentapi.FlowEdge, 0, len(report.Flow.Edges)),
		}
		for _, node := range report.Flow.Nodes {
			projected.Report.Flow.Nodes = append(projected.Report.Flow.Nodes, agentapi.FlowNode{
				ID:    node.ID,
				Label: node.Label,
				Kind:  node.Kind,
			})
		}
		// Only verified hops are safe to hand the parent as facts; inferred or
		// unresolved hops are summarized in uncertainties rather than copied.
		for _, edge := range report.Flow.Edges {
			if edge.EvidenceState != "verified" {
				continue
			}
			projected.Report.Flow.Edges = append(projected.Report.Flow.Edges, agentapi.FlowEdge{
				From:          edge.From,
				To:            edge.To,
				Protocol:      edge.Protocol,
				SyncMode:      edge.SyncMode,
				EvidenceRefs:  edge.EvidenceRefs,
				EvidenceState: edge.EvidenceState,
			})
		}
	}
	return projected
}

// settlementDeadline computes the latest wall-clock instant the parent may
// block on a child batch before it must enter synthesis. It prefers the loop
// deadline (run deadline minus answer reserve) so a slow child can never eat
// into the final answer window, and it never extends beyond the outer request
// deadline.
func (agent *Agent) settlementDeadline(state *compiledLoop) time.Time {
	if state == nil {
		return time.Time{}
	}
	var deadline time.Time
	consider := func(candidate time.Time) {
		if candidate.IsZero() {
			return
		}
		if deadline.IsZero() || candidate.Before(deadline) {
			deadline = candidate
		}
	}
	for _, ctx := range []context.Context{state.loopCtx, state.runCtx, state.ctx} {
		if ctx == nil {
			continue
		}
		if candidate, ok := ctx.Deadline(); ok {
			consider(candidate)
		}
	}
	return deadline
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
			state.loopCtx, delegationID, agent.settlementDeadline(state),
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
