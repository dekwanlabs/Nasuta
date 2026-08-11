package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/llm"
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

	answerContract       *exactAnswerContract
	messages             []llm.Message
	tools                []llm.ToolDef
	initialMessageCount  int
	answerToolSources    map[int]string
	result               *RunResult
	stepSeq              int
	answered             bool
	seenTools            map[string]bool
	evidenceLedger       *runEvidenceLedger
	remainingToolTokens  int
	webEvidence          webEvidenceState
	evidenceTurnExtended bool
	stepLimit            int
}

func (agent *Agent) prepareCompiledLoop(
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

	tools := agent.prepareToolDefinitions(ctx, runID, input, toolSnapshot)
	result := &RunResult{RunID: runID}
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
		evidenceLedger:      newRunEvidenceLedger(input.EvidenceUnits),
		remainingToolTokens: initialToolTokenBudget(agent, messages, tools),
		stepLimit:           maxSteps,
	}
	state.recordSeedEvidence(agent.observer)
	return state
}

func (agent *Agent) prepareToolDefinitions(
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

func (agent *Agent) finishCompiledLoop(state *compiledLoop) {
	if !state.answered && !state.result.Aborted && state.result.Err == nil {
		state.result.ForcedConclusion = true
		state.result.Evidence.ForcedConclusion = true
		log.InfofCtx(state.ctx, "[agent] run %s forcing conclusion (steps=%d)",
			state.runID, state.result.Steps)
		agent.concludeCompiledLoop(state)
	}
	state.result.Evidence.Finalize(state.input.Direct)
	log.InfofCtx(state.ctx, "[agent] run %s end: steps=%d answerLen=%d aborted=%v err=%v",
		state.runID, state.result.Steps, len(state.result.Answer), state.result.Aborted, state.result.Err)
}

func (agent *Agent) concludeCompiledLoop(state *compiledLoop) {
	if _, err := agent.compactRunContextBeforeAnswer(state, nil, "forced_conclusion"); err != nil {
		state.result.Err = fmt.Errorf("compact context before forced conclusion: %w", err)
		log.ErrorfCtx(state.ctx, "[agent] run %s final-answer context compaction failed: %v",
			state.runID, err)
		return
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
		validPartial := !state.answerContract.Active() ||
			final != nil && len(state.answerContract.Missing(final.Content)) == 0
		if hasDeliverableAnswer(final) && validPartial && !errors.Is(err, ErrAnswerContractViolation) {
			state.result.Answer += final.Content
			state.result.Err = err
			log.WarnfCtx(state.ctx, "[agent] run %s preserving partial force-conclusion answer: %v",
				state.runID, err)
		} else {
			state.result.Err = err
			log.ErrorfCtx(state.ctx, "[agent] run %s force-conclusion error: %v", state.runID, err)
		}
	} else if final != nil {
		state.result.Answer += final.Content
	}
}
