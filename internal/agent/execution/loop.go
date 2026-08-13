package execution

import (
	"context"
	"errors"
	"time"

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
	FullInvestigation    bool
	EvidenceSeeded       bool
	PrunedToolIDs        map[tool.ToolID]struct{}
	PruneApplied         bool
}

func (config AgentConfig) withDefaults() AgentConfig {
	if config.ConclusionMaxTokens <= 0 {
		config.ConclusionMaxTokens = config.AnswerMaxTokens
	}
	return config
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

// NewAgent builds an Agent with optional observer/controller hooks.
func NewAgent(client *llm.LLMClient, executor *ToolExecutor, config AgentConfig, observer Observer, controller Controller) *Agent {
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
func (agent *Agent) SetOnFirstAnswerToken(fn func(runID string)) {
	agent.onFirstAnswerToken = fn
}

type RunResult struct {
	RunID             string
	Answer            string
	Steps             int
	Evidence          EvidenceMetrics
	EvidenceUnits     []tool.EvidenceUnit
	EvidenceConflicts []evidence.Conflict
	References        []tool.Reference
	ForcedConclusion  bool
	Aborted           bool
	Err               error
	SessionMessages   []llm.Message
}

// Input is a fully compiled request for the execution loop.
type Input struct {
	Question           string
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
	input := Input{
		Question:           question,
		Messages:           agent.buildAgentMessages(question, conversation, retrieved, plan),
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
func (agent *Agent) RunCompiled(
	ctx context.Context,
	runID string,
	input Input,
	toolSnapshot tool.Snapshot,
) (*RunResult, error) {
	runStarted := time.Now()
	runCtx, runCancel := context.WithTimeout(ctx, agent.cfg.Timeout)
	defer runCancel()
	loopCtx, loopCancel := context.WithTimeout(runCtx, agent.cfg.Timeout-agent.cfg.AnswerReserve)
	defer loopCancel()

	state := agent.prepareCompiledLoop(
		ctx,
		runCtx,
		loopCtx,
		runID,
		input,
		toolSnapshot,
		runStarted,
	)
	if err := agent.runTurns(state); err != nil {
		agent.finalizeCompiledLoop(state)
		return state.result, err
	}
	agent.finishCompiledLoop(state)
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
