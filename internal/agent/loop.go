package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/agent/tooloutput"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

type EvidenceStatus string

const (
	EvidenceNotRequired EvidenceStatus = "not_required"
	EvidenceComplete    EvidenceStatus = "complete"
	EvidencePartial     EvidenceStatus = "partial"
	EvidenceUnavailable EvidenceStatus = "unavailable"
)

var ErrToolCallBudgetExhausted = errors.New("agent tool call budget exhausted")

// EvidenceMetrics summarizes delivery coverage without re-reading persisted steps.
type EvidenceMetrics struct {
	Status             EvidenceStatus `json:"status"`
	ForcedConclusion   bool           `json:"forced_conclusion"`
	ToolCallCount      int            `json:"tool_call_count"`
	ResultCount        int            `json:"result_count"`
	ToolFailureCount   int            `json:"tool_failure_count"`
	PartialResultCount int            `json:"partial_result_count"`
	OmittedItemCount   int            `json:"omitted_item_count"`
}

func (metrics *EvidenceMetrics) finalize(direct bool) {
	switch {
	case metrics.ResultCount == 0 && metrics.ToolCallCount == 0 && direct:
		metrics.Status = EvidenceNotRequired
	case metrics.ResultCount == 0:
		metrics.Status = EvidenceUnavailable
	case metrics.ForcedConclusion || metrics.ToolFailureCount > 0 || metrics.PartialResultCount > 0:
		metrics.Status = EvidencePartial
	default:
		metrics.Status = EvidenceComplete
	}
}

// AgentConfig tunes the agent loop and answer generation limits.
type AgentConfig struct {
	MaxSteps            int
	MaxToolCalls        int64
	HistoryLimit        int
	Timeout             time.Duration
	AnswerReserve       time.Duration
	AnswerMaxTokens     int
	ConclusionMaxTokens int
	ContextWindow       int
	MaxContinueRounds   int
	DomainKnowledge     string
	ModelParameters     llm.ModelParameters
}

// ConversationContext carries recalled archived history and recent turns.
// RolePrompt is request-scoped RBAC identity; it is
// composed into the primary system prompt and is not conversation history.
type ConversationContext struct {
	SessionID            string
	RolePrompt           string
	RetrievedHistory     string
	HistoricalContext    string
	CompactedThroughTurn int
	Recent               []llm.Message
	RecentTurns          []memory.TurnMetadata
	SessionTitle         string
	Instructions         []llm.Message
	FullInvestigation    bool
	EvidenceSeeded       bool
	PrunedToolIDs        map[tool.ToolID]struct{} // non-nil => drop non-base tools from model-facing defs; nil => offer full set
	PruneApplied         bool                     // true => pruning is live (not dry-run) and reduces the offered set
}

func (c AgentConfig) withDefaults() AgentConfig {
	if c.ConclusionMaxTokens <= 0 {
		c.ConclusionMaxTokens = c.AnswerMaxTokens
	}
	return c
}

// Agent runs the think-tool-answer loop.
type Agent struct {
	llm                *llm.LLMClient
	executor           *ToolExecutor
	observer           Observer
	controller         Controller
	cfg                AgentConfig
	onFirstAnswerToken func(runID string)
}

type agentModelTurnInput struct {
	Step     int
	Messages []llm.Message
	Tools    []llm.ToolDef
	Stream   *StreamPipe
}

type agentModelTurnOutput struct {
	Result *llm.ChatStreamResult
	Timing StreamTiming
}

type forceConclusionInput struct {
	RunID          string
	Messages       []llm.Message
	AnswerContract *exactAnswerContract
}

type forceConclusionOutput struct {
	Result         *llm.ChatStreamResult
	Stream         *StreamPipe
	Timing         StreamTiming
	AttemptStarted time.Time
}

type historyCompileInput struct {
	Messages []llm.Message
}

var historyCompileSpec = executiontrace.Spec[historyCompileInput, []llm.Message]{
	Operation: "agent.history_compile",
	Node:      "history_compile",
	Input: func(input historyCompileInput) map[string]any {
		return map[string]any{"compiled_messages": len(input.Messages), "compiled_chars": messageChars(input.Messages)}
	},
	Output: func(_ historyCompileInput, output []llm.Message, _ error) map[string]any {
		return map[string]any{"messages": len(output), "context_chars": contextChars(output)}
	},
}

type toolPruningInput struct {
	Tools   []llm.ToolDef
	Offered map[tool.ToolID]struct{}
	Applied bool
}

type toolPruningOutput struct {
	Effective    []llm.ToolDef
	FullTokens   int
	PrunedTokens int
	RemovedIDs   []string
}

var toolPruningSpec = executiontrace.Spec[toolPruningInput, toolPruningOutput]{
	Operation: "agent.tool_pruning",
	Node:      "tool_pruning",
	Input: func(input toolPruningInput) map[string]any {
		return map[string]any{"applied": input.Applied}
	},
	Output: func(input toolPruningInput, output toolPruningOutput, _ error) map[string]any {
		return map[string]any{
			"offered": len(output.Effective), "total": len(input.Tools),
			"full_tokens": output.FullTokens, "pruned_tokens": output.PrunedTokens,
			"saved_tokens":     output.FullTokens - output.PrunedTokens,
			"removed_tool_ids": output.RemovedIDs,
		}
	},
}

type contextBudgetInput struct {
	Step     int
	Messages []llm.Message
	Tools    []llm.ToolDef
}

var contextBudgetSpec = executiontrace.Spec[contextBudgetInput, struct{}]{
	Operation: "agent.context_budget",
	Node:      "context_budget",
	Input: func(input contextBudgetInput) map[string]any {
		return map[string]any{"step": input.Step, "messages": len(input.Messages), "tools": len(input.Tools)}
	},
	Output: func(_ contextBudgetInput, _ struct{}, err error) map[string]any {
		return map[string]any{"error": err.Error()}
	},
	Record: func(_ struct{}, err error) bool { return err != nil },
}

var agentModelTurnSpec = executiontrace.Spec[agentModelTurnInput, agentModelTurnOutput]{
	Operation: "agent.model_turn",
	Node:      "agent_model_turn",
	Input: func(input agentModelTurnInput) map[string]any {
		return map[string]any{"step": input.Step, "messages": len(input.Messages), "tools": len(input.Tools)}
	},
	Output: func(_ agentModelTurnInput, output agentModelTurnOutput, err error) map[string]any {
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		toolNames := make([]string, 0, len(output.Result.ToolCalls))
		for _, call := range output.Result.ToolCalls {
			toolNames = append(toolNames, call.Function.Name)
		}
		return map[string]any{
			"finish_reason": output.Result.FinishReason, "tool_calls": toolNames,
			"content_chars": len([]rune(output.Result.Content)), "reasoning_tokens": output.Result.ReasoningTokens,
			"first_event_ms": output.Timing.FirstEvent.Milliseconds(), "first_reasoning_ms": output.Timing.FirstReasoning.Milliseconds(),
			"first_content_ms": output.Timing.FirstContent.Milliseconds(), "first_tool_delta_ms": output.Timing.FirstToolDelta.Milliseconds(),
			"first_tool_call_ms": output.Timing.FirstToolCall.Milliseconds(),
		}
	},
}

var forceConclusionSpec = executiontrace.Spec[*forceConclusionInput, forceConclusionOutput]{
	Operation: "agent.force_conclusion",
	Node:      "force_conclusion",
	Input: func(input *forceConclusionInput) map[string]any {
		return map[string]any{"messages": len(input.Messages)}
	},
	Output: func(_ *forceConclusionInput, output forceConclusionOutput, err error) map[string]any {
		result := map[string]any{
			"first_event_ms": output.Timing.FirstEvent.Milliseconds(), "first_reasoning_ms": output.Timing.FirstReasoning.Milliseconds(),
			"first_content_ms": output.Timing.FirstContent.Milliseconds(),
		}
		if err != nil {
			result["error"] = err.Error()
		}
		if output.Result != nil {
			result["finish_reason"] = output.Result.FinishReason
			result["content_chars"] = len([]rune(output.Result.Content))
			result["reasoning_tokens"] = output.Result.ReasoningTokens
		}
		return result
	},
}

type firstAnswerTokenTraceInput struct {
	Step         any
	TurnTTFTMS   int64
	RunElapsedMS int64
}

var firstAnswerTokenTraceSpec = executiontrace.Spec[firstAnswerTokenTraceInput, struct{}]{
	Operation: "agent.first_answer_token",
	Node:      "first_answer_token",
	Output: func(input firstAnswerTokenTraceInput, _ struct{}, _ error) map[string]any {
		return map[string]any{
			"step":           input.Step,
			"turn_ttft_ms":   input.TurnTTFTMS,
			"run_elapsed_ms": input.RunElapsedMS,
		}
	},
}

// NewAgent builds an Agent with optional observer/controller hooks.
func NewAgent(llm *llm.LLMClient, executor *ToolExecutor, cfg AgentConfig, observer Observer, controller Controller) *Agent {
	if executor == nil {
		executor = NewToolExecutor(tool.NewRegistry())
	}
	if observer == nil {
		observer = NoopObserver()
	}
	return &Agent{
		llm:        llm,
		executor:   executor,
		observer:   observer,
		controller: controller,
		cfg:        cfg.withDefaults(),
	}
}

func (agent *Agent) MaxStepsFor(question string) int {
	return agent.cfg.MaxSteps
}

// MaxStepsForPlan leaves one extra turn for web research to fetch a selected page.
func (agent *Agent) MaxStepsForPlan(question string, plan domain.EvidencePlan) int {
	return agent.MaxStepsForContext(question, plan, false)
}

// MaxStepsForContext grants routed runtime investigations their configured budget.
func (agent *Agent) MaxStepsForContext(question string, plan domain.EvidencePlan, fullInvestigation bool) int {
	return agent.cfg.MaxSteps
}

// SetOnFirstAnswerToken installs a callback fired before the first answer token.
func (agent *Agent) SetOnFirstAnswerToken(fn func(runID string)) { agent.onFirstAnswerToken = fn }

type RunResult struct {
	RunID            string
	Answer           string // final text answer (concatenated tokens)
	Steps            int    // loop iterations taken
	Evidence         EvidenceMetrics
	References       []tool.Reference // canonical references from delivered, non-failed tool evidence
	ForcedConclusion bool
	Aborted          bool // true if aborted by user or timeout
	Err              error
	SessionMessages  []llm.Message
}

type loopInput struct {
	question           string
	messages           []llm.Message
	evidenceContent    string
	referenceTypes     map[string]tool.ReferenceType
	evidenceSeeded     bool
	direct             bool
	web                bool
	offeredToolIDs     map[tool.ToolID]struct{}
	toolPruningApplied bool
}

// RunWithPlan enforces one immutable retrieval/tool policy for the whole run.
func (agent *Agent) RunWithPlan(ctx context.Context, runID, question string, history []llm.Message, rc *retrieval.RetrievedContext, plan domain.EvidencePlan, allowWrite bool) (*RunResult, error) {
	return agent.RunWithContext(ctx, runID, question, ConversationContext{Recent: history}, rc, plan, allowWrite)
}

// RunWithContext runs without synchronous history summarization on the request path.
func (agent *Agent) RunWithContext(ctx context.Context, runID, question string, conversation ConversationContext, rc *retrieval.RetrievedContext, plan domain.EvidencePlan, allowWrite bool) (*RunResult, error) {
	policy := ToolPolicyForRun(allowWrite)
	return agent.RunWithSnapshot(ctx, runID, question, conversation, rc, plan, policy, agent.executor.Snapshot(policy))
}

// RunWithSnapshot keeps definitions and handlers fixed for the whole run.
func (agent *Agent) RunWithSnapshot(ctx context.Context, runID, question string, conversation ConversationContext, rc *retrieval.RetrievedContext, plan domain.EvidencePlan, policy ToolPolicy, toolSnapshot tool.Snapshot) (*RunResult, error) {
	return agent.runWithSnapshot(ctx, runID, question, conversation, rc, plan, policy, toolSnapshot)
}

func (agent *Agent) runWithSnapshot(ctx context.Context, runID, question string, conversation ConversationContext, rc *retrieval.RetrievedContext, plan domain.EvidencePlan, policy ToolPolicy, toolSnapshot tool.Snapshot) (*RunResult, error) {
	input := loopInput{
		question:           question,
		messages:           agent.buildAgentMessages(question, conversation, rc, plan),
		referenceTypes:     referenceTypeIndex(rc),
		evidenceSeeded:     conversation.EvidenceSeeded || rc != nil && rc.Text != "",
		direct:             plan.Direct(),
		web:                plan.Has(domain.Web),
		offeredToolIDs:     conversation.PrunedToolIDs,
		toolPruningApplied: conversation.PruneApplied,
	}
	if rc != nil {
		input.evidenceContent = rc.Text
	}
	return agent.runCompiled(ctx, runID, input, toolSnapshot)
}

func (agent *Agent) runCompiled(ctx context.Context, runID string, input loopInput, toolSnapshot tool.Snapshot) (*RunResult, error) {
	runStarted := time.Now()
	runCtx, runCancel := context.WithTimeout(ctx, agent.cfg.Timeout)
	defer runCancel()

	loopCtx, loopCancel := context.WithTimeout(runCtx, agent.cfg.Timeout-agent.cfg.AnswerReserve)
	defer loopCancel()

	maxSteps := agent.cfg.MaxSteps
	log.InfofCtx(ctx, "[agent] run %s start: %q (maxSteps=%d configured=%d timeout=%s reserve=%s)",
		runID, platform.TruncateForLog(input.question, 10), maxSteps, agent.cfg.MaxSteps, agent.cfg.Timeout, agent.cfg.AnswerReserve)

	historyStarted := time.Now()
	answerContract := &exactAnswerContract{}
	messages, _ := executiontrace.Invoke(ctx, historyCompileSpec, historyCompileInput{Messages: input.messages}, func(_ context.Context, input historyCompileInput) ([]llm.Message, error) {
		return append([]llm.Message(nil), input.Messages...), nil
	})
	historyDuration := time.Since(historyStarted)
	log.InfofCtx(ctx, "[agent] run %s request compiled in %s: messages=%d contextChars=%d",
		runID, historyDuration, len(messages), contextChars(messages))
	tools := agent.executor.Definitions(toolSnapshot)
	if len(input.offeredToolIDs) > 0 {
		// Dry-run until PruneApplied flips: always measure the would-be saving,
		// but only shrink the offered set when pruning is live. Execution keeps
		// the full snapshot so a pruned tool is still callable if replayed.
		pruning, _ := executiontrace.Invoke(ctx, toolPruningSpec, toolPruningInput{
			Tools: tools, Offered: input.offeredToolIDs, Applied: input.toolPruningApplied,
		}, func(_ context.Context, input toolPruningInput) (toolPruningOutput, error) {
			effective := prunedDefinitions(input.Tools, input.Offered)
			fullEncoded, _ := json.Marshal(input.Tools)
			prunedEncoded, _ := json.Marshal(effective)
			return toolPruningOutput{
				Effective:  effective,
				FullTokens: tooloutput.EstimateTokens(string(fullEncoded)), PrunedTokens: tooloutput.EstimateTokens(string(prunedEncoded)),
				RemovedIDs: removedToolDefIDs(input.Tools, effective),
			}, nil
		})
		log.InfofCtx(ctx, "[agent] run %s tool pruning: applied=%t offered=%d/%d tokens=%d->%d saved=%d removed=%v",
			runID, input.toolPruningApplied, len(pruning.Effective), len(tools), pruning.FullTokens, pruning.PrunedTokens,
			pruning.FullTokens-pruning.PrunedTokens, pruning.RemovedIDs)
		if input.toolPruningApplied {
			tools = pruning.Effective
		}
	}

	result := &RunResult{RunID: runID}
	if input.evidenceSeeded {
		result.Evidence.ResultCount = 1
	}
	stepSeq := 0
	answered := false
	seenTools := map[string]bool{}
	referenceTypes := input.referenceTypes
	var webEvidence webEvidenceState
	evidenceTurnExtended := false

	if input.evidenceContent != "" {
		stepSeq++
		agent.observer.OnStep(runCtx, runID, StepRecord{
			StepNo:     stepSeq,
			Kind:       StepKindRetrieval,
			Content:    input.evidenceContent,
			TokenDelta: utf8.RuneCountInString(input.evidenceContent),
			CreatedAt:  time.Now(),
		})
	}

	stepLimit := maxSteps
	for step := 1; step <= stepLimit; step++ {
		if agent.controller != nil {
			if stopped := agent.handleControl(runCtx, runID, step, &messages, result); stopped {
				break
			}
		}

		stepp := step
		_, budgetErr := executiontrace.Invoke(ctx, contextBudgetSpec, contextBudgetInput{
			Step: stepp, Messages: messages, Tools: tools,
		}, func(_ context.Context, input contextBudgetInput) (struct{}, error) {
			return struct{}{}, agent.ensureInputBudget(input.Messages, input.Tools)
		})
		if budgetErr != nil {
			return result, budgetErr
		}
		t0 := time.Now()
		h := newStreamPipe(agent.observer, runID, stepp, t0, agent.onFirstAnswerToken)

		callCtx := llm.WithUsagePhase(loopCtx, llm.PhaseAgentStep)
		turn, err := executiontrace.Invoke(callCtx, agentModelTurnSpec, agentModelTurnInput{
			Step: stepp, Messages: messages, Tools: tools, Stream: h,
		}, func(callCtx context.Context, input agentModelTurnInput) (agentModelTurnOutput, error) {
			chatResult, callErr := agent.llm.ChatWithToolsMaxWithParameters(
				callCtx, input.Messages, input.Tools, input.Stream, agent.cfg.AnswerMaxTokens,
				agent.cfg.ModelParameters,
			)
			return agentModelTurnOutput{Result: chatResult, Timing: input.Stream.Timings()}, callErr
		})
		chatResult := turn.Result
		duration := time.Since(t0)
		timing := turn.Timing
		if err != nil {
			if !answerContract.Active() && agent.preserveInterruptedAnswer(runCtx, runID, &stepSeq, result, chatResult, h, t0, duration) {
				result.Err = fmt.Errorf("agent step %d: %w", stepp, err)
				log.WarnfCtx(ctx, "[agent] run %s preserving partial answer from interrupted step %d: %v", runID, stepp, err)
				break
			}
			if loopCtx.Err() != nil {
				log.InfofCtx(ctx, "[agent] run %s loop budget exhausted at step %d: %v", runID, stepp, loopCtx.Err())
				break
			}
			return result, fmt.Errorf("agent step %d: %w", stepp, err)
		}
		log.InfofCtx(ctx, "[agent] run %s model step %d timing: total=%s firstEvent=%s firstReasoning=%s firstContent=%s firstToolDelta=%s firstToolCall=%s",
			runID, stepp, duration, timing.FirstEvent, timing.FirstReasoning, timing.FirstContent, timing.FirstToolDelta, timing.FirstToolCall)
		result.Steps = stepp

		if len(chatResult.ToolCalls) == 0 {
			recordFirstAnswerToken(ctx, stepp, timing.FirstContent, t0.Sub(runStarted)+timing.FirstContent)
			cont, err := agent.continueIfNeeded(loopCtx, messages, chatResult, agent.cfg.AnswerMaxTokens, h)
			chatResult = cont
			if err != nil && !answerContract.Active() && agent.preserveInterruptedAnswer(runCtx, runID, &stepSeq, result, chatResult, h, t0, duration) {
				result.Err = err
				log.WarnfCtx(ctx, "[agent] run %s preserving partial final answer at step %d: %v", runID, stepp, err)
				break
			}
			if errors.Is(err, ErrReasoningTruncated) || errors.Is(err, ErrEmptyModelResponse) {
				log.WarnfCtx(ctx, "[agent] run %s final-answer generation produced no visible content; forcing conclusion: %v", runID, err)
				break
			}
			if err == nil {
				chatResult, err = agent.validateOrRepairAnswer(loopCtx, messages, chatResult, answerContract, agent.cfg.AnswerMaxTokens, h)
			}
			if err != nil && answerContract.Active() {
				result.Err = err
				log.ErrorfCtx(ctx, "[agent] run %s exact-answer validation failed at step %d: %v", runID, stepp, err)
				break
			}
			h.Publish(chatResult.Content)
			result.Answer += chatResult.Content
			stepSeq++
			agent.observer.OnStep(runCtx, runID, StepRecord{
				StepNo:          stepSeq,
				Kind:            StepKindAnswer,
				Content:         chatResult.Content,
				TokenDelta:      utf8.RuneCountInString(chatResult.Content),
				ReasoningTokens: chatResult.ReasoningTokens,
				DurationMs:      int(duration / time.Millisecond),
				CreatedAt:       t0,
			})
			if err != nil {
				result.Err = err
				log.ErrorfCtx(ctx, "[agent] run %s final-answer truncated: %v", runID, err)
			} else {
				answered = true
				log.InfofCtx(ctx, "[agent] run %s done at step %d (final answer)", runID, step)
			}
			break
		}

		stepSeq++
		agent.observer.OnStep(runCtx, runID, StepRecord{
			StepNo:          stepSeq,
			Kind:            StepKindThink,
			Content:         chatResult.Content,
			TokenDelta:      utf8.RuneCountInString(chatResult.Content),
			ReasoningTokens: chatResult.ReasoningTokens,
			DurationMs:      int(duration / time.Millisecond),
			CreatedAt:       t0,
		})

		// StreamPipe already stopped forwarding tokens via OnToolCallDelta.
		h.Discard()

		assistantToolMessage := llm.Message{
			Role:      "assistant",
			Content:   chatResult.Content,
			ToolCalls: canonicalSessionToolCalls(chatResult.ToolCalls),
		}
		messages = append(messages, assistantToolMessage)
		result.SessionMessages = append(result.SessionMessages, assistantToolMessage)

		webAttempted, webSucceeded := false, false
		turnProducedEvidence := false
		for _, call := range chatResult.ToolCalls {
			if agent.cfg.MaxToolCalls > 0 &&
				int64(result.Evidence.ToolCallCount) >= agent.cfg.MaxToolCalls {
				result.Err = fmt.Errorf(
					"%w: maximum %d calls",
					ErrToolCallBudgetExhausted,
					agent.cfg.MaxToolCalls,
				)
				break
			}
			result.Evidence.ToolCallCount++
			stepSeq++
			agent.observer.OnStep(runCtx, runID, StepRecord{
				StepNo:    stepSeq,
				Kind:      StepKindToolCall,
				Tool:      call.Function.Name,
				Args:      call.Function.Arguments,
				CreatedAt: time.Now(),
			})
			execution := agent.executor.Execute(loopCtx, toolSnapshot, call, referenceTypes, seenTools)
			execution = agent.prepareToolDelivery(runID, messages, tools, call, execution)
			if execution.Failed {
				result.Evidence.ToolFailureCount++
			} else if execution.Evidence {
				result.Evidence.ResultCount++
			}
			turnProducedEvidence = turnProducedEvidence || execution.Evidence
			acceptedWebEvidence := false
			if !execution.Failed {
				acceptedWebEvidence = webEvidence.Observe(call, execution.AuthoritativeContent)
			}
			if !execution.Failed && execution.Evidence && execution.DeliveryError == "" {
				mergeToolReferences(&result.References, execution.References)
			}
			if isWebEvidenceTool(call.Function.Name) {
				webAttempted = true
				webSucceeded = webSucceeded || acceptedWebEvidence
			}
			if execution.Coverage.Partial {
				result.Evidence.PartialResultCount++
			}
			result.Evidence.OmittedItemCount += execution.Coverage.OmittedItems
			stepSeq++
			toolResultStep := newToolResultStep(runID, stepSeq, call, execution)
			if err := agent.observer.OnStep(runCtx, runID, toolResultStep); err != nil {
				result.Err = fmt.Errorf("persist tool result trace %q: %w", toolResultStep.TraceID, err)
				break
			}
			log.InfofCtx(ctx, "[agent] tool result trace persisted: trace_id=%s tool_call_id=%s tool=%s bytes=%d sha256=%s artifact_id=%s failed=%v",
				toolResultStep.TraceID, call.ID, call.Function.Name, toolResultStep.SizeBytes,
				toolResultStep.AuthoritativeSHA256, execution.ArtifactID, execution.Failed)
			if execution.DeliveryError != "" {
				log.WarnfCtx(ctx, "[agent] tool %s delivery failed: trace_id=%s reason=%s bytes=%d",
					call.Function.Name, toolResultStep.TraceID, execution.DeliveryError, toolResultStep.SizeBytes)
			}
			messages = append(messages, toolMessage(call.ID, call.Function.Name, execution.PromptContent))
			result.SessionMessages = append(result.SessionMessages,
				toolMessage(call.ID, call.Function.Name, execution.PromptContent))
			if !execution.Failed {
				for _, notice := range execution.Notices {
					messages = append(messages, llm.Message{
						Role: "system",
						Content: prompts.MustRender(prompts.AgentQAToolDeliveryNotice, struct {
							Notice string
						}{Notice: notice}),
					})
				}
				if _, ok := answerContractMessage(execution.AnswerContract); ok {
					answerContract.Add(execution.AnswerContract)
					contractMessage, _ := answerContractMessage(tool.AnswerContract{RequiredLiterals: answerContract.required})
					messages = append(withoutAnswerContractMessages(messages), contractMessage)
				}
			}
		}
		if result.Err != nil {
			break
		}
		if extended := extendEvidenceStepLimit(step, stepLimit, agent.cfg.MaxSteps, turnProducedEvidence, evidenceTurnExtended); extended > stepLimit {
			stepLimit = extended
			evidenceTurnExtended = true
			log.InfofCtx(ctx, "[agent] run %s extending after boundary evidence (newLimit=%d configured=%d)", runID, stepLimit, agent.cfg.MaxSteps)
		}
		if input.web {
			if hint := webEvidence.ConvergenceHint(); hint != "" {
				messages = append(messages, llm.Message{Role: "system", Content: hint})
			}
			if extended := extendWebStepLimit(step, stepLimit, agent.cfg.MaxSteps, webAttempted, webSucceeded); extended > stepLimit {
				stepLimit = extended
				log.InfofCtx(ctx, "[agent] run %s extending web research after unusable evidence (newLimit=%d configured=%d)", runID, stepLimit, agent.cfg.MaxSteps)
			}
		}

		log.InfofCtx(ctx, "[agent] run %s context size after step %d: %d chars", runID, stepp, contextChars(messages))
	}

	if !answered && !result.Aborted && result.Err == nil {
		result.ForcedConclusion = true
		result.Evidence.ForcedConclusion = true
		log.InfofCtx(ctx, "[agent] run %s forcing conclusion (steps=%d)", runID, result.Steps)
		final, ferr := agent.forceConclusion(runCtx, runID, messages, answerContract, &stepSeq, runStarted)
		if ferr != nil {
			validPartial := !answerContract.Active() || final != nil && len(answerContract.Missing(final.Content)) == 0
			if hasDeliverableAnswer(final) && validPartial && !errors.Is(ferr, ErrAnswerContractViolation) {
				result.Answer += final.Content
				result.Err = ferr
				log.WarnfCtx(ctx, "[agent] run %s preserving partial force-conclusion answer: %v", runID, ferr)
			} else {
				result.Err = ferr
				log.ErrorfCtx(ctx, "[agent] run %s force-conclusion error: %v", runID, ferr)
			}
		} else if final != nil {
			result.Answer += final.Content
		}
	}
	result.Evidence.finalize(input.direct)

	log.InfofCtx(ctx, "[agent] run %s end: steps=%d answerLen=%d aborted=%v err=%v",
		runID, result.Steps, len(result.Answer), result.Aborted, result.Err)
	return result, nil
}

// forceConclusion asks the model to finish with the evidence already gathered.
func (agent *Agent) forceConclusion(ctx context.Context, runID string, messages []llm.Message, answerContract *exactAnswerContract, stepSeq *int, runStarted time.Time) (*llm.ChatStreamResult, error) {
	ctx = llm.WithUsagePhase(ctx, llm.PhaseForcedConclusion)
	input := &forceConclusionInput{RunID: runID, Messages: messages, AnswerContract: answerContract}
	conclusion, err := executiontrace.Invoke(ctx, forceConclusionSpec, input, func(ctx context.Context, input *forceConclusionInput) (forceConclusionOutput, error) {
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
			res, callErr = agent.validateOrRepairAnswer(ctx, input.Messages, res, input.AnswerContract, agent.cfg.ConclusionMaxTokens, stream)
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
	_, _ = executiontrace.Invoke(
		ctx,
		firstAnswerTokenTraceSpec,
		firstAnswerTokenTraceInput{
			Step:         step,
			TurnTTFTMS:   turnTTFT.Milliseconds(),
			RunElapsedMS: runElapsed.Milliseconds(),
		},
		func(context.Context, firstAnswerTokenTraceInput) (struct{}, error) {
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

func (agent *Agent) preserveInterruptedAnswer(ctx context.Context, runID string, stepSeq *int, result *RunResult, res *llm.ChatStreamResult, stream *StreamPipe, started time.Time, duration time.Duration) bool {
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

// handleControl drains all pending control signals for the run and returns true
// when the loop must stop (user abort or context-exhausted pause).
func (agent *Agent) handleControl(ctx context.Context, runID string, step int, messages *[]llm.Message, result *RunResult) bool {
	for {
		sig := agent.controller.Poll(runID)
		switch sig.Kind {
		default:
			return false
		case CtrlAbort:
			result.Aborted = true
			log.InfofCtx(ctx, "[agent] run %s aborted by user at step %d", runID, step)
			return true
		case CtrlPause:
			log.InfofCtx(ctx, "[agent] run %s paused at step %d", runID, step)
			if err := agent.controller.WaitResume(ctx, runID); err != nil {
				result.Aborted = true
				log.InfofCtx(ctx, "[agent] run %s pause ended with cancel: %v", runID, err)
				return true
			}
			log.InfofCtx(ctx, "[agent] run %s resumed", runID)

		case CtrlNudge:
			if sig.Message != "" {
				*messages = append(*messages, llm.Message{
					Role: "user",
					Content: prompts.MustRender(prompts.AgentQAMidRunAddition, struct {
						Message string
					}{Message: sig.Message}),
				})
				log.InfofCtx(ctx, "[agent] run %s nudged at step %d: %q", runID, step, platform.TruncateForLog(sig.Message, 8))
			}

		}
	}
}

func (agent *Agent) buildAgentMessages(question string, conversation ConversationContext, rc *retrieval.RetrievedContext, plan domain.EvidencePlan) []llm.Message {
	return buildAgentMessages(question, conversation, rc, plan, agent.cfg.DomainKnowledge, agent.cfg.HistoryLimit)
}

func buildAgentMessages(question string, conversation ConversationContext, rc *retrieval.RetrievedContext, plan domain.EvidencePlan, domainKnowledge string, historyLimit int) []llm.Message {
	mode := ClassifyResponseMode(question)
	hint := "\n\n---\n" + prompts.MustRender(prompts.AgentQAResponseMode, struct {
		Mode string
	}{Mode: string(mode)})
	sysPrompt := composeSystemPrompt(agentPromptForPlan(plan), conversation.RolePrompt) + hint
	if dk := strings.TrimSpace(domainKnowledge); dk != "" && plan.Has(domain.Internal) {
		sysPrompt += "\n\n## Domain Knowledge\n" + dk
	}
	msgs := []llm.Message{{Role: "system", Content: sysPrompt}}
	msgs = append(msgs, llm.Message{Role: "system", Content: evidencePlanInstruction(plan)})
	msgs = append(msgs, conversation.Instructions...)

	if conversation.RetrievedHistory != "" {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: prompts.MustRender(prompts.AgentQARetrievedHistory, struct {
				History string
			}{History: conversation.RetrievedHistory}),
		})
	}
	if conversation.HistoricalContext != "" {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: prompts.MustRender(prompts.AgentQAHistoricalContext, struct {
				Context string
			}{Context: conversation.HistoricalContext}),
		})
	}
	recent := withoutAnswerContractMessages(conversation.Recent)
	msgs = append(msgs, replayableTailMessages(recent, historyLimit)...)

	if rc != nil && rc.Text != "" {
		msgs = append(msgs, llm.Message{
			Role: "system",
			Content: prompts.MustRender(prompts.AgentQAPreRetrievedEvidence, struct {
				HitCount int
				Evidence string
			}{HitCount: rc.HitCount, Evidence: rc.Text}),
		})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: question})
	return msgs
}

func agentPromptForPlan(plan domain.EvidencePlan) string {
	if plan.Direct() || plan.Sources == domain.Memory {
		return directAgentSystemPrompt
	}
	if plan.Sources == domain.Web {
		return webAgentSystemPrompt
	}
	return agentSystemPrompt
}

func evidencePlanInstruction(plan domain.EvidencePlan) string {
	return prompts.MustRender(prompts.AgentQAEvidencePlan, struct {
		Direct bool
		Plan   string
	}{Direct: plan.Direct(), Plan: plan.String()})
}

var directAgentSystemPrompt = promptWithRolePlaceholder(prompts.AgentQADirect)

// replayableTailMessages keeps only provider-valid tool call/result groups.
func replayableTailMessages(msgs []llm.Message, n int) []llm.Message {
	start := 0
	if n > 0 && len(msgs) > n {
		start = len(msgs) - n
		if msgs[start].Role == "tool" {
			for start > 0 && msgs[start-1].Role == "tool" {
				start--
			}
			if start > 0 && len(msgs[start-1].ToolCalls) > 0 {
				start--
			}
		}
		if start > 0 && len(msgs[start].ToolCalls) > 0 && msgs[start-1].Role == "user" {
			start--
		}
	}
	tail := msgs[start:]
	out := make([]llm.Message, 0, len(tail))
	for i := 0; i < len(tail); {
		message := tail[i]
		if message.Role == "tool" {
			i++
			continue
		}
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			out = append(out, message)
			i++
			continue
		}
		j := i + 1
		for j < len(tail) && tail[j].Role == "tool" {
			j++
		}
		if completeToolResultGroup(message.ToolCalls, tail[i+1:j]) {
			out = append(out, tail[i:j]...)
		}
		i = j
	}
	return out
}

func completeToolResultGroup(calls []llm.ToolCall, results []llm.Message) bool {
	if len(calls) != len(results) {
		return false
	}
	expected := make(map[string]string, len(calls))
	for _, call := range calls {
		if call.ID == "" || call.Function.Name == "" {
			return false
		}
		expected[call.ID] = call.Function.Name
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		name, ok := expected[result.ToolCallID]
		if !ok || name != result.Name {
			return false
		}
		if _, duplicate := seen[result.ToolCallID]; duplicate {
			return false
		}
		seen[result.ToolCallID] = struct{}{}
	}
	return len(seen) == len(expected)
}

// contextChars sums visible content length for context-size logging.
func contextChars(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += len([]rune(m.Content))
	}
	return total
}

func messageChars(msgs []llm.Message) int {
	total := 0
	for _, message := range msgs {
		total += len([]rune(message.Content))
	}
	return total
}

func (agent *Agent) ensureInputBudget(messages []llm.Message, tools []llm.ToolDef) error {
	window := agent.cfg.ContextWindow
	if window <= 0 {
		return nil
	}
	inputTokens := estimateMessagesTokens(messages)
	if len(tools) > 0 {
		encoded, err := json.Marshal(tools)
		if err != nil {
			return fmt.Errorf("encode tool definitions for context budget: %w", err)
		}
		inputTokens += tooloutput.EstimateTokens(string(encoded))
	}
	outputReserve := max(agent.cfg.AnswerMaxTokens, agent.cfg.ConclusionMaxTokens)
	safety := max(window/20, 1024)
	if inputTokens+outputReserve+safety > window {
		return fmt.Errorf(
			"QA context exceeds configured window before provider call: input=%d output_reserve=%d safety=%d window=%d; shorten the question or attachments",
			inputTokens, outputReserve, safety, window,
		)
	}
	return nil
}

// runeSafeTruncate truncates to max runes safely.
func runeSafeTruncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

var (
	agentToolPrompt      = prompts.Text(prompts.AgentQAToolPolicy)
	agentSystemPrompt    = systemPrompt + "\n\n" + agentToolPrompt
	webAgentSystemPrompt = promptWithRolePlaceholder(prompts.AgentQAWeb)
)

// ToolExecutor adapts tools.Registry to the agent loop.
type ToolExecutor struct {
	registry *Registry
	runtime  *tool.Executor
}

// ToolExecution separates authoritative evidence from the exact model payload.
type ToolExecution struct {
	AuthoritativeContent string
	PromptContent        string
	Notices              []string
	References           []tool.Reference
	Evidence             bool
	Failed               bool
	Coverage             tool.EvidenceCoverage
	AnswerContract       tool.AnswerContract
	DeliveryError        string
	ArtifactID           string
	DurationMs           int
}

// NewToolExecutor wraps a registry with a default per-tool timeout.
func NewToolExecutor(registry *Registry) *ToolExecutor {
	return &ToolExecutor{registry: registry, runtime: tool.NewExecutor(15 * time.Second)}
}

// Snapshot pins definitions and handlers before the model sees any tool.
func (te *ToolExecutor) Snapshot(policy ToolPolicy) tool.Snapshot {
	return te.registry.Snapshot(policy)
}

// Definitions returns model schemas from one immutable snapshot.
func (te *ToolExecutor) Definitions(snapshot tool.Snapshot) []llm.ToolDef {
	all := snapshot.Tools()
	defs := make([]llm.ToolDef, 0, len(all))
	for _, candidate := range all {
		defs = append(defs, llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        string(candidate.ID),
				Description: candidate.Description,
				Parameters:  candidate.InputSchema,
			},
		})
	}
	return defs
}

// DefinitionsFor snapshots current tools for one-shot callers.
func (te *ToolExecutor) DefinitionsFor(policy ToolPolicy) []llm.ToolDef {
	return te.Definitions(te.Snapshot(policy))
}

// Execute runs against the same snapshot used to publish model definitions.
func (te *ToolExecutor) Execute(ctx context.Context, snapshot tool.Snapshot, call llm.ToolCall, referenceTypes map[string]tool.ReferenceType, seen map[string]bool) ToolExecution {
	name := call.Function.Name
	args, err := parseArgs(ctx, call.Function.Arguments)
	if err != nil {
		result := fmt.Sprintf("error: %v", err)
		return ToolExecution{AuthoritativeContent: result, PromptContent: result, Failed: true}
	}
	arguments := args

	candidate, ok := snapshot.Get(tool.ToolID(name))
	if !ok {
		result := fmt.Sprintf("error: unknown tool %q", name)
		return ToolExecution{AuthoritativeContent: result, PromptContent: result, Failed: true}
	}
	if mismatch := referenceMismatch(snapshot, candidate, arguments, referenceTypes); mismatch != "" {
		return ToolExecution{AuthoritativeContent: mismatch, PromptContent: mismatch, Failed: true}
	}

	fp := ""
	if seen != nil {
		fp = toolFingerprint(name, args)
		if seen[fp] {
			log.InfofCtx(ctx, "[agent] tool %s deduped (repeat call — returning placeholder)", name)
			result := "(already searched with the same arguments; see previous result above)"
			return ToolExecution{AuthoritativeContent: result, PromptContent: result}
		}
	}

	t0 := time.Now()
	toolResult, err := te.runtime.Execute(ctx, snapshot, tool.ToolID(name), arguments)
	duration := time.Since(t0)
	if err != nil {
		result := fmt.Sprintf("error: %v", err)
		log.InfofCtx(ctx, "[agent] tool %s error after %s: args=%s err=%v", name, duration, platform.TruncateForLog(argSummary(args), 400), err)
		return ToolExecution{AuthoritativeContent: result, PromptContent: result, Failed: true, DurationMs: int(duration / time.Millisecond)}
	}
	result := toolResult.Content
	if seen != nil {
		seen[fp] = true
	}

	log.InfofCtx(ctx, "[agent] tool %s ok in %s (%d chars): args=%s result=%s", name, duration, len(result),
		platform.TruncateForLog(argSummary(args), 600), platform.TruncateForLog(result, 1200))
	execution := ToolExecution{
		AuthoritativeContent: result,
		PromptContent:        result,
		References:           cloneReferences(toolResult.References),
		Evidence:             true,
		Coverage:             toolResult.Coverage,
		AnswerContract:       toolResult.AnswerContract,
		DurationMs:           int(duration / time.Millisecond),
	}
	return execution
}

func cloneReferences(refs []tool.Reference) []tool.Reference {
	if len(refs) == 0 {
		return nil
	}
	out := make([]tool.Reference, len(refs))
	copy(out, refs)
	return out
}

// mergeToolReferences dedups references by (type, target) so repeated tool hits
// never inflate the final reference set.
func mergeToolReferences(dst *[]tool.Reference, refs []tool.Reference) {
	if len(refs) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(*dst)+len(refs))
	for _, existing := range *dst {
		seen[string(existing.Type)+"\x00"+existing.Target] = struct{}{}
	}
	for _, ref := range refs {
		key := string(ref.Type) + "\x00" + ref.Target
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		*dst = append(*dst, ref)
	}
}

func referenceTypeIndex(context *retrieval.RetrievedContext) map[string]tool.ReferenceType {
	if context == nil || len(context.References) == 0 {
		return nil
	}
	index := make(map[string]tool.ReferenceType, len(context.References))
	for _, reference := range context.References {
		referenceType := tool.ReferenceType(reference.Type)
		switch referenceType {
		case tool.ReferenceRunbook, tool.ReferenceService, tool.ReferenceSymbol:
			if reference.Target != "" {
				index[reference.Target] = referenceType
			}
		}
	}
	return index
}

func referenceMismatch(snapshot tool.Snapshot, candidate tool.Tool, args tool.Arguments, references map[string]tool.ReferenceType) string {
	if len(references) == 0 || len(candidate.ReferenceInputs) == 0 {
		return ""
	}
	for _, input := range candidate.ReferenceInputs {
		value := args.String(input.Argument)
		if value == "" {
			continue
		}
		for entity, actualType := range references {
			if !containsReferenceToken(value, entity) || acceptsReference(input.Accepts, actualType) {
				continue
			}
			candidates := snapshot.CandidateTools(actualType)
			candidateNames := make([]string, len(candidates))
			for i, id := range candidates {
				candidateNames[i] = string(id)
			}
			content, _ := json.Marshal(map[string]any{
				"code": "entity_type_mismatch", "entity": entity,
				"actualType": actualType, "tool": candidate.ID, "candidateTools": candidateNames,
			})
			return string(content)
		}
	}
	return ""
}

func acceptsReference(accepted []tool.ReferenceType, actual tool.ReferenceType) bool {
	for _, candidate := range accepted {
		if candidate == actual {
			return true
		}
	}
	return false
}

func containsReferenceToken(value, entity string) bool {
	for offset := 0; ; {
		position := strings.Index(value[offset:], entity)
		if position < 0 {
			return false
		}
		start := offset + position
		end := start + len(entity)
		if referenceBoundary(value, start-1) && referenceBoundary(value, end) {
			return true
		}
		offset = end
	}
}

func referenceBoundary(value string, index int) bool {
	if index < 0 || index >= len(value) {
		return true
	}
	b := value[index]
	return !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_' || b == '-')
}

func isWebEvidenceTool(name string) bool {
	return name == "web_search" || name == "web_fetch"
}

func extendWebStepLimit(step, current, configured int, attempted, succeeded bool) int {
	if step != current || !attempted || succeeded || current >= configured {
		return current
	}
	return current + 1
}

func extendEvidenceStepLimit(step, current, configured int, produced, alreadyExtended bool) int {
	if step != current || !produced || alreadyExtended || current >= configured {
		return current
	}
	return current + 1
}

const sessionToolArgumentLimit = 8_000

func canonicalSessionToolCalls(calls []llm.ToolCall) []llm.ToolCall {
	out := make([]llm.ToolCall, len(calls))
	for i, call := range calls {
		out[i] = call
		if len(call.Function.Arguments) > sessionToolArgumentLimit {
			out[i].Function.Arguments = `{"_nasuta_omitted":"arguments exceeded session limit"}`
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args == nil {
			// Preserve malformed model output for audit instead of making it look valid.
			continue
		}
		canonical, err := json.Marshal(args)
		if err != nil {
			continue
		}
		out[i].Function.Arguments = string(canonical)
	}
	return out
}

// ExecuteArguments is the non-LLM entry used by trusted prefetch plans.
func (te *ToolExecutor) ExecuteArguments(ctx context.Context, snapshot tool.Snapshot, id tool.ToolID, args tool.Arguments) (tool.Result, error) {
	return te.runtime.Execute(ctx, snapshot, id, args)
}

// ExecuteWithPolicy snapshots current tools for one-shot callers.
func (te *ToolExecutor) ExecuteWithPolicy(ctx context.Context, policy ToolPolicy, call llm.ToolCall, seen map[string]bool) ToolExecution {
	return te.Execute(ctx, te.Snapshot(policy), call, nil, seen)
}

// toolFingerprint builds a stable dedup key (name + canonical JSON args).
func toolFingerprint(name string, args map[string]any) string {
	canonical, err := json.Marshal(args)
	if err != nil {
		canonical = []byte(fmt.Sprintf("%v", args))
	}
	return name + "|" + string(canonical)
}

func toolMessage(toolCallID, name, content string) llm.Message {
	return llm.Message{
		Role:       "tool",
		ToolCallID: toolCallID,
		Name:       name,
		Content:    content,
	}
}

// parseArgs decodes a tool call's JSON arguments.
func parseArgs(ctx context.Context, arguments string) (tool.Arguments, error) {
	args := tool.Arguments{}
	s := strings.TrimSpace(arguments)
	if s == "" {
		return args, nil
	}
	if err := json.Unmarshal([]byte(s), &args); err != nil {
		log.InfofCtx(ctx, "[agent] malformed tool args %q: %v", arguments, err)
		return nil, fmt.Errorf("invalid tool arguments: %w", err)
	}
	if args == nil {
		return nil, fmt.Errorf("invalid tool arguments: expected a JSON object")
	}
	return args, nil
}

// argSummary renders tool args as a compact "k=v,..." log string.
func argSummary(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	var sb strings.Builder
	first := true
	for k, v := range args {
		if !first {
			sb.WriteString(", ")
		}
		first = false
		sb.WriteString(k)
		sb.WriteString("=")
		b, err := json.Marshal(v)
		if err != nil {
			fmt.Fprintf(&sb, "%v", v)
		} else {
			sb.Write(b)
		}
	}
	return sb.String()
}
