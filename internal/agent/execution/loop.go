package execution

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/evidence"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/prompts"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

var ErrToolCallBudgetExhausted = errors.New("agent tool call budget exhausted")

// Config tunes the agent loop and answer generation limits.
type Config struct {
	MaxSteps            int
	MaxToolCalls        int64
	HistoryLimit        int
	Timeout             time.Duration
	AnswerReserve       time.Duration
	AnswerMaxTokens     int
	ConclusionMaxTokens int
	// ConclusionRetryMaxTokens bounds the one-shot direct-answer retry after
	// reasoning truncation or an empty model response.
	ConclusionRetryMaxTokens int
	ContextWindow            int
	// MaxInputTokens is the cumulative provider-input budget for one Run.
	MaxInputTokens int64
	// MaxContextTokens is the ceiling for one provider request.
	MaxContextTokens int64
	// MaxToolResultBytes bounds the model-facing copy of one tool result.
	// The authoritative result remains available to observers and trace storage.
	MaxToolResultBytes int
	MaxContinueRounds  int
	StructuredOutput   bool
	DomainKnowledge    string
	ModelParameters    llm.ModelParameters
	// Token prices let the shared run ledger reserve a cost ceiling before
	// sending a provider request; zero keeps cost accounting unbounded.
	InputPriceMicrosPerMillionTokens  int64
	OutputPriceMicrosPerMillionTokens int64
	BudgetCheck                       func() error
	DisableLegacyAnswerRecovery       bool
	// Checkpoint is called after each completed logical turn.
	Checkpoint func(LogicalLoopCheckpoint) error
}

// ConversationContext carries recalled archived history and recent turns.
// RolePrompt is request-scoped RBAC identity; it is composed into the primary
// system prompt and is not conversation history.
type ConversationContext struct {
	SessionID            string
	RolePrompt           string
	RetrievedHistory     string
	HistoricalContext    string
	CompactedThroughTurn int
	Recent               []llm.Message
	RecentTurns          []memory.TurnMetadata
	RecentDialogue       []memory.RecentDialogueTurn
	SessionTitle         string
	Instructions         []llm.Message
	EvidenceSeeded       bool
	PrunedToolIDs        map[tool.ToolID]struct{}
	PruneApplied         bool
}

func (config Config) withDefaults() Config {
	if config.ConclusionMaxTokens <= 0 {
		config.ConclusionMaxTokens = config.AnswerMaxTokens
	}
	if config.ConclusionRetryMaxTokens <= 0 && config.ConclusionMaxTokens > 0 {
		config.ConclusionRetryMaxTokens = config.ConclusionMaxTokens / 4
		if config.ConclusionRetryMaxTokens <= 0 {
			config.ConclusionRetryMaxTokens = 1
		}
		if config.ConclusionRetryMaxTokens > 1024 {
			config.ConclusionRetryMaxTokens = 1024
		}
	}
	if config.ConclusionRetryMaxTokens > config.ConclusionMaxTokens {
		config.ConclusionRetryMaxTokens = config.ConclusionMaxTokens
	}
	return config
}

// Agent runs the think-tool-answer loop.
type Agent struct {
	llm                *llm.LLMClient
	executor           *ToolExecutor
	observer           Observer
	controller         Controller
	cfg                Config
	onFirstAnswerToken func(runID string)
}

// NewAgent builds an Agent with optional observer/controller hooks.
func NewAgent(client *llm.LLMClient, executor *ToolExecutor, config Config, observer Observer, controller Controller) *Agent {
	if executor == nil {
		executor = NewToolExecutor(tool.NewRegistry())
	}
	if observer == nil {
		observer = NoopObserver()
	}
	return &Agent{
		llm:        client,
		executor:   executor,
		observer:   observer,
		controller: controller,
		cfg:        config.withDefaults(),
	}
}

// SetOnFirstAnswerToken installs a callback fired before the first answer token.
func (agent *Agent) SetOnFirstAnswerToken(fn func(runID string)) {
	agent.onFirstAnswerToken = fn
}

type RunResult struct {
	RunID                string
	OutputMode           agentapi.RunOutputMode
	Answer               string
	Steps                int
	Evidence             EvidenceMetrics
	EvidenceUnits        []tool.EvidenceUnit
	EvidenceObservations []agentapi.EvidenceObservation
	EvidenceConflicts    []evidence.Conflict
	References           []tool.Reference
	DelegationAdoptions  []agentapi.DelegationAdoption
	Flow                 *agentapi.FlowIR
	ForcedConclusion     bool
	Aborted              bool
	Err                  error
	SessionMessages      []llm.Message
}

// Input is a fully compiled request for the execution loop.
type Input struct {
	// OriginalRequest is persisted with logical checkpoints so a recovery worker
	// can rebuild the immutable definition/tool contract without guessing from
	// provider-facing messages.
	OriginalRequest    *agentapi.RunRequest
	Question           string
	OutputMode         agentapi.RunOutputMode
	OutputContract     agentapi.RunOutputContract
	Messages           []llm.Message
	EvidenceContent    string
	EvidenceUnits      []tool.EvidenceUnit
	EvidenceConflicts  []evidence.Conflict
	ReferenceTypes     map[string]tool.ReferenceType
	EvidenceSeeded     bool
	Direct             bool
	Web                bool
	OfferedToolIDs     map[tool.ToolID]struct{}
	ToolPruningApplied bool
}

// RunWithPlan enforces one immutable retrieval/tool policy for the whole run.
func (agent *Agent) RunWithPlan(
	ctx context.Context,
	runID string,
	question string,
	history []llm.Message,
	retrieved *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	allowWrite bool,
) (*RunResult, error) {
	return agent.RunWithContext(
		ctx,
		runID,
		question,
		ConversationContext{Recent: history},
		retrieved,
		plan,
		allowWrite,
	)
}

// RunWithContext runs without synchronous history summarization on the request path.
func (agent *Agent) RunWithContext(
	ctx context.Context,
	runID string,
	question string,
	conversation ConversationContext,
	retrieved *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	allowWrite bool,
) (*RunResult, error) {
	policy := ToolPolicyForRun(allowWrite)
	return agent.RunWithSnapshot(
		ctx,
		runID,
		question,
		conversation,
		retrieved,
		plan,
		policy,
		agent.executor.Snapshot(policy),
	)
}

// RunWithSnapshot keeps definitions and handlers fixed for the whole run.
func (agent *Agent) RunWithSnapshot(
	ctx context.Context,
	runID string,
	question string,
	conversation ConversationContext,
	retrieved *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	policy ToolPolicy,
	toolSnapshot tool.Snapshot,
) (*RunResult, error) {
	return agent.runWithSnapshot(
		ctx,
		runID,
		question,
		conversation,
		retrieved,
		plan,
		policy,
		toolSnapshot,
	)
}

func (agent *Agent) runWithSnapshot(
	ctx context.Context,
	runID string,
	question string,
	conversation ConversationContext,
	retrieved *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	policy ToolPolicy,
	toolSnapshot tool.Snapshot,
) (*RunResult, error) {
	query := domain.QueryPlan{}
	if retrieved != nil {
		query = retrieved.Query
	}
	if query.Kind == "" {
		query = domain.ResolveQueryPlan(question, nil, nil).Plan
	}
	input := Input{
		Question:           question,
		Messages:           agent.buildMessages(question, query, conversation, retrieved, plan),
		ReferenceTypes:     referenceTypeIndex(retrieved),
		EvidenceSeeded:     conversation.EvidenceSeeded || retrieved != nil && retrieved.Text != "",
		Direct:             plan.Direct(),
		Web:                plan.Has(domain.Web),
		OfferedToolIDs:     conversation.PrunedToolIDs,
		ToolPruningApplied: conversation.PruneApplied,
	}
	if retrieved != nil {
		input.EvidenceContent = retrieved.Text
		input.EvidenceUnits = cloneEvidenceUnits(retrieved.EvidenceUnits)
		input.EvidenceConflicts = evidence.CloneConflicts(retrieved.EvidenceConflicts)
	}
	return agent.RunCompiled(ctx, runID, input, toolSnapshot)
}

// RunCompiled executes a request whose messages and evidence are already assembled.
// RunCompiledFromCheckpoint resumes a parent logical loop from its last
// durable boundary. The checkpoint contains only server-owned execution state;
// the caller must provide the immutable tool snapshot captured for the run.
func (agent *Agent) RunCompiledFromCheckpoint(
	ctx context.Context,
	runID string,
	checkpoint LogicalLoopCheckpoint,
	toolSnapshot tool.Snapshot,
) (*RunResult, error) {
	stateData, err := UnmarshalLogicalLoopState(checkpoint.State)
	if err != nil {
		return nil, fmt.Errorf("decode logical loop checkpoint: %w", err)
	}
	if strings.TrimSpace(runID) == "" {
		runID = stateData.Input.Question
	}
	runStarted := time.Now()
	runTimeout := agent.cfg.Timeout
	if runTimeout <= 0 {
		runTimeout = 5 * time.Minute
	}
	runCtx, runCancel := context.WithTimeout(ctx, runTimeout)
	defer runCancel()
	loopTimeout := runTimeout - agent.cfg.AnswerReserve
	if loopTimeout <= 0 {
		loopTimeout = runTimeout
	}
	loopCtx, loopCancel := context.WithTimeout(runCtx, loopTimeout)
	defer loopCancel()
	state := agent.prepareLoop(ctx, runCtx, loopCtx, runID, stateData.Input, toolSnapshot, runStarted)
	state.messages = append([]llm.Message(nil), stateData.Messages...)
	state.stepSeq = stateData.StepSeq
	if state.stepSeq == 0 {
		state.stepSeq = stateData.StepNo
	}
	state.result.Answer = stateData.Answer
	state.result.References = append([]tool.Reference(nil), stateData.References...)
	state.result.Flow = cloneExecutionFlow(stateData.Flow)
	state.result.DelegationAdoptions = cloneDelegationAdoptions(stateData.DelegationAdoptions)
	state.delegatedFlows = cloneExecutionFlows(stateData.DelegatedFlows)
	state.result.Steps = stateData.StepNo
	state.answered = stateData.Answered
	state.toolBudgetExhausted = stateData.ToolBudgetExhausted
	state.startStep = stateData.StepNo + 1
	if state.startStep < 1 {
		state.startStep = 1
	}
	state.evidenceLedger = newRunEvidenceLedger(stateData.EvidenceUnits, stateData.EvidenceConflicts)
	state.answerContract = &exactAnswerContract{}
	state.answerContract.Add(stateData.AnswerContract)
	state.answerContract.restoreEvaluated(stateData.EvaluatedAdoptions)
	if stateData.Answered || strings.EqualFold(checkpoint.Phase, "completed") {
		agent.finalizeLoop(state)
		return state.result, nil
	}
	if err := agent.runTurns(state); err != nil {
		state.result.Err = err
		state.result.DelegationAdoptions = state.answerContract.UnknownAdoptions("parent_run_failed")
		agent.finalizeLoop(state)
		return state.result, err
	}
	agent.finishLoop(state)
	if err := agent.checkpointState(state, "completed", state.result.Steps); err != nil {
		state.result.Err = err
		return state.result, err
	}
	return state.result, nil
}

func (agent *Agent) RunCompiled(
	ctx context.Context,
	runID string,
	input Input,
	toolSnapshot tool.Snapshot,
) (*RunResult, error) {
	return agent.runCompiled(ctx, runID, input, nil, toolSnapshot)
}

// RunCompiledWithRequest is the durable variant of RunCompiled. The original
// request is carried into every logical checkpoint so recovery can rebuild the
// exact definition, permissions, policy and tool scope rather than infer them
// from provider-facing messages.
func (agent *Agent) RunCompiledWithRequest(
	ctx context.Context,
	runID string,
	input Input,
	request *agentapi.RunRequest,
	toolSnapshot tool.Snapshot,
) (*RunResult, error) {
	return agent.runCompiled(ctx, runID, input, request, toolSnapshot)
}

func (agent *Agent) runCompiled(
	ctx context.Context,
	runID string,
	input Input,
	request *agentapi.RunRequest,
	toolSnapshot tool.Snapshot,
) (*RunResult, error) {
	runStarted := time.Now()
	runTimeout := agent.cfg.Timeout
	if runTimeout <= 0 {
		runTimeout = 5 * time.Minute
	}
	runCtx, runCancel := context.WithTimeout(ctx, runTimeout)
	defer runCancel()
	loopTimeout := runTimeout - agent.cfg.AnswerReserve
	if loopTimeout <= 0 {
		loopTimeout = runTimeout
	}
	loopCtx, loopCancel := context.WithTimeout(runCtx, loopTimeout)
	defer loopCancel()

	if request != nil {
		input.OriginalRequest = request
	}
	state := agent.prepareLoop(
		ctx,
		runCtx,
		loopCtx,
		runID,
		input,
		toolSnapshot,
		runStarted,
	)
	if err := agent.runTurns(state); err != nil {
		// runTurns returns control-flow failures separately from result.Err.
		// Copy it before finalization so terminal logs and evidence snapshots
		// describe the actual failure that stopped the loop.
		state.result.Err = err
		state.result.DelegationAdoptions = state.answerContract.UnknownAdoptions(
			"parent_run_failed",
		)
		agent.finalizeLoop(state)
		return state.result, err
	}
	agent.finishLoop(state)
	if err := agent.checkpointState(state, "completed", state.result.Steps); err != nil {
		state.result.Err = err
		return state.result, err
	}
	return state.result, nil
}

// handleControl drains pending signals and reports whether the loop must stop.
func (agent *Agent) handleControl(
	ctx context.Context,
	runID string,
	step int,
	messages *[]llm.Message,
	result *RunResult,
) bool {
	for {
		signal := agent.controller.Poll(runID)
		switch signal.Kind {
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
			if signal.Message == "" {
				continue
			}
			*messages = append(*messages, llm.Message{
				Role: "user",
				Content: prompts.MustRender(prompts.AgentQAMidRunAddition, struct {
					Message string
				}{Message: signal.Message}),
			})
			log.InfofCtx(ctx, "[agent] run %s nudged at step %d: %q",
				runID, step, platform.TruncateForLog(signal.Message, 8))
		}
	}
}
