package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/log"
)

// forceConclusion asks the model to finish with the evidence already gathered.
func (agent *Agent) forceConclusion(ctx context.Context, runID string, messages []llm.Message, answerContract *exactAnswerContract, stepSeq *int, runStarted time.Time) (*llm.ChatStreamResult, error) {
	ctx = llm.WithUsagePhase(ctx, llm.PhaseForcedConclusion)
	input := &forceConclusionInput{RunID: runID, Messages: messages, AnswerContract: answerContract}
	conclusion, err := runtrace.Invoke(ctx, forceConclusionSpec, input, func(ctx context.Context, input *forceConclusionInput) (forceConclusionOutput, error) {
		input.Messages = append(input.Messages, llm.Message{
			Role:    "user",
			Content: forceConclusionInstruction,
		})
		attemptStarted := time.Now()
		stream := newStreamPipe(agent.observer, input.RunID, 0, attemptStarted, agent.onFirstAnswerToken)
		res, callErr := agent.generateWithContinue(ctx, input.Messages, agent.cfg.ConclusionMaxTokens, stream)
		if errors.Is(callErr, ErrReasoningTruncated) || errors.Is(callErr, ErrEmptyModelResponse) {
			log.WarnfCtx(ctx, "[agent] run %s force-conclusion produced no visible content, retrying with no-reasoning prompt: %v", input.RunID, callErr)
			input.Messages = append(input.Messages, llm.Message{
				Role:    "user",
				Content: forceConclusionNoReasoningInstruction,
			})
			attemptStarted = time.Now()
			stream = newStreamPipe(agent.observer, input.RunID, 0, attemptStarted, agent.onFirstAnswerToken)
			res, callErr = agent.generateWithContinue(ctx, input.Messages, agent.cfg.ConclusionMaxTokens, stream)
		}
		if callErr == nil && hasLeakedToolProtocol(res) {
			log.WarnfCtx(ctx, "[agent] run %s conclusion contained tool protocol; retrying without control markup", input.RunID)
			input.Messages = append(input.Messages, llm.Message{Role: "user", Content: protocolRepairInstruction})
			attemptStarted = time.Now()
			stream = newStreamPipe(agent.observer, input.RunID, 0, attemptStarted, agent.onFirstAnswerToken)
			res, callErr = agent.generateWithContinue(ctx, input.Messages, agent.cfg.ConclusionMaxTokens, stream)
			if callErr == nil && hasLeakedToolProtocol(res) {
				res = nil
				callErr = ErrToolProtocolLeak
			}
		}
		if callErr == nil {
			res, callErr = agent.enforceContract(ctx, input.Messages, res, input.AnswerContract, agent.cfg.ConclusionMaxTokens, stream)
		}
		return forceConclusionOutput{
			Result: res, Stream: stream, Timing: stream.Timings(), AttemptStarted: attemptStarted,
		}, callErr
	})
	res := conclusion.Result
	stream := conclusion.Stream
	timing := conclusion.Timing
	t0 := conclusion.AttemptStarted
	if timing.FirstContent > 0 {
		elapsed := timing.FirstContent.Milliseconds()
		if !runStarted.IsZero() {
			elapsed = t0.Sub(runStarted).Milliseconds() + timing.FirstContent.Milliseconds()
		}
		recordFirstAnswerToken(ctx, "force_conclusion", timing.FirstContent, time.Duration(elapsed)*time.Millisecond)
	}
	*stepSeq++
	validAnswer := !answerContract.Active() || res != nil && len(answerContract.Missing(res.Content)) == 0
	if hasDeliverableAnswer(res) && validAnswer && !errors.Is(err, ErrAnswerContractViolation) {
		stream.Publish(res.Content)
		agent.observer.OnStep(ctx, runID, StepRecord{
			StepNo:          *stepSeq,
			Kind:            StepKindAnswer,
			Content:         res.Content,
			TokenDelta:      utf8.RuneCountInString(res.Content),
			ReasoningTokens: res.ReasoningTokens,
			DurationMs:      int(time.Since(t0) / time.Millisecond),
			CreatedAt:       t0,
		})
	}
	return res, err
}

func recordFirstAnswerToken(ctx context.Context, step any, turnTTFT, runElapsed time.Duration) {
	if turnTTFT <= 0 {
		return
	}
	_, _ = runtrace.Invoke(
		ctx,
		firstTokenTraceSpec,
		firstTokenTraceInput{
			Step:         step,
			TurnTTFTMS:   turnTTFT.Milliseconds(),
			RunElapsedMS: runElapsed.Milliseconds(),
		},
		func(context.Context, firstTokenTraceInput) (struct{}, error) {
			return struct{}{}, nil
		},
	)
}

var ErrToolProtocolLeak = errors.New("final answer contained an unsupported tool protocol")

func hasLeakedToolProtocol(res *llm.ChatStreamResult) bool {
	if res == nil {
		return false
	}
	content := strings.ToLower(strings.ReplaceAll(res.Content, "｜", "|"))
	return strings.Contains(content, "dsml") &&
		(strings.Contains(content, "tool_calls") || strings.Contains(content, "invoke name="))
}

func hasDeliverableAnswer(res *llm.ChatStreamResult) bool {
	return res != nil && strings.TrimSpace(res.Content) != "" && len(res.ToolCalls) == 0 && !hasLeakedToolProtocol(res)
}

func (agent *Agent) preservePartialAnswer(ctx context.Context, runID string, stepSeq *int, result *RunResult, res *llm.ChatStreamResult, stream *StreamPipe, started time.Time, duration time.Duration) bool {
	if stream.HasToolCallDelta() || !hasDeliverableAnswer(res) {
		return false
	}
	result.Answer += res.Content
	stream.Publish(res.Content)
	*stepSeq++
	agent.observer.OnStep(ctx, runID, StepRecord{
		StepNo:          *stepSeq,
		Kind:            StepKindAnswer,
		Content:         res.Content,
		TokenDelta:      utf8.RuneCountInString(res.Content),
		ReasoningTokens: res.ReasoningTokens,
		DurationMs:      int(duration / time.Millisecond),
		CreatedAt:       started,
	})
	return true
}

func (agent *Agent) generateWithContinue(ctx context.Context, messages []llm.Message, maxTokens int, h llm.StreamHandler) (*llm.ChatStreamResult, error) {
	if err := agent.ensureInputBudget(messages, nil); err != nil {
		return nil, err
	}
	res, err := agent.llm.ChatWithToolsMaxWithParameters(
		ctx, messages, nil, h, maxTokens, agent.cfg.ModelParameters,
	)
	if err != nil {
		return res, err
	}
	res, cerr := agent.continueIfNeeded(ctx, messages, res, maxTokens, h)
	return res, cerr
}

// ErrReasoningTruncated means the model used the full token budget before
// emitting any visible content.
var ErrReasoningTruncated = errors.New("turn truncated during reasoning: max_tokens exhausted before any visible content; retry with a larger budget")

// ErrAnswerTruncated means continuation rounds ended before the visible answer completed.
var ErrAnswerTruncated = errors.New("answer remained truncated after continuation limit")

// ErrEmptyModelResponse means the provider ended normally without producing an answer.
var ErrEmptyModelResponse = errors.New("model returned no visible content")

// continueIfNeeded retries a length-truncated answer with continuation prompts.
func (agent *Agent) continueIfNeeded(ctx context.Context, messages []llm.Message, res *llm.ChatStreamResult, maxTokens int, h llm.StreamHandler) (*llm.ChatStreamResult, error) {
	if res.Content == "" {
		log.WarnfCtx(ctx, "[agent] empty visible content: %d reasoning tokens, finish_reason=%s",
			res.ReasoningTokens, res.FinishReason)
		if res.FinishReason == llm.FinishLength {
			return res, ErrReasoningTruncated
		}
		return res, ErrEmptyModelResponse
	}

	rounds := 0
	for res.FinishReason == "length" && rounds < agent.cfg.MaxContinueRounds {
		rounds++
		log.WarnfCtx(ctx, "[agent] answer truncated by max_tokens, continuing (round %d/%d)", rounds, agent.cfg.MaxContinueRounds)
		msgs := append(append([]llm.Message{}, messages...),
			llm.Message{Role: "assistant", Content: res.Content},
			llm.Message{Role: "user", Content: continuationInstruction},
		)
		continuationCtx := llm.WithUsagePhase(ctx, llm.PhaseContinuation)
		if err := agent.ensureInputBudget(msgs, nil); err != nil {
			return nil, err
		}
		cont, err := agent.llm.ChatWithToolsMaxWithParameters(
			continuationCtx, msgs, nil, h, maxTokens, agent.cfg.ModelParameters,
		)
		if err != nil {
			log.ErrorfCtx(ctx, "[agent] continuation round %d failed: %v", rounds, err)
			return res, fmt.Errorf("continuation round %d: %w", rounds, err)
		}
		res.Content += cont.Content
		res.ReasoningTokens += cont.ReasoningTokens
		res.Usage = res.Usage.Add(cont.Usage)
		res.FinishReason = cont.FinishReason
	}
	if res.FinishReason == "length" {
		log.WarnfCtx(ctx, "[agent] answer still truncated after %d continuation rounds", agent.cfg.MaxContinueRounds)
		return res, ErrAnswerTruncated
	}
	return res, nil
}

var (
	forceConclusionInstruction            = prompts.Text(prompts.AgentQAForceConclusion)
	forceConclusionNoReasoningInstruction = prompts.Text(prompts.AgentQAForceConclusionNoThink)
	protocolRepairInstruction             = prompts.Text(prompts.AgentQAProtocolRepair)
	continuationInstruction               = prompts.Text(prompts.AgentQAContinuation)
)
