package qa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	agentdefinition "github.com/dekwanlabs/nasuta/internal/agent/definition"
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentrun "github.com/dekwanlabs/nasuta/internal/agent/run"
	agentsession "github.com/dekwanlabs/nasuta/internal/agent/session"
	agenttools "github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

type ConversationContext = agentexecution.ConversationContext
type RunResult = agentexecution.RunResult
type Service = agenttools.Service
type SessionHistory = agentsession.SessionHistory
type DefinitionResolver = agentdefinition.DefinitionResolver
type DefinitionRuntime = agentdefinition.DefinitionRuntime
type ScenarioRunStart = agentdefinition.ScenarioRunStart
type ScenarioRun = agentdefinition.ScenarioRun
type ScenarioLifecycle = agentdefinition.ScenarioLifecycle
type ScenarioToolSet = agentdefinition.ScenarioToolSet
type ScenarioToolSource = agentdefinition.ScenarioToolSource
type AgentConfig = agentexecution.AgentConfig
type ToolExecutor = agentexecution.ToolExecutor
type Agent = agentexecution.Agent
type Observer = agentexecution.Observer
type Controller = agentexecution.Controller

type RunOutcome = agentrun.RunOutcome
type RunStatus = agentrun.RunStatus
type EvidenceStatus = agentrun.EvidenceStatus
type EvidenceMetrics = agentrun.EvidenceMetrics
type ExecutionEvent = agentrun.ExecutionEvent
type ExecutionEventEmitter = agentrun.ExecutionEventEmitter
type EventType = agentrun.EventType
type ToolPolicy = tool.Policy
type Tool = tool.Tool
type Registry = tool.Registry
type RunStore = agentrun.RunStore
type RunRecord = agentrun.RunRecord
type RunUsageSummary = agentrun.RunUsageSummary
type RunTerminal = agentrun.RunTerminal
type SSEEvent = agentrun.SSEEvent
type StepKind = agentrun.StepKind
type StepRecord = agentrun.StepRecord
type StreamPipe = agentexecution.StreamPipe
type StreamTiming = agentexecution.StreamTiming

const (
	RunStatusRunning = agentrun.RunStatusRunning
	RunStatusPaused  = agentrun.RunStatusPaused
	RunStatusDone    = agentrun.RunStatusDone
	RunStatusFailed  = agentrun.RunStatusFailed
	RunStatusAborted = agentrun.RunStatusAborted
	RunKindAgent     = agentrun.RunKindAgent

	ToolKindRead  = tool.KindRead
	ToolKindWrite = tool.KindWrite

	EvidenceNotRequired = agentrun.EvidenceNotRequired
	EvidenceComplete    = agentrun.EvidenceComplete
	EvidencePartial     = agentrun.EvidencePartial
	EvidenceUnavailable = agentrun.EvidenceUnavailable

	StepKindThink      = agentrun.StepKindThink
	StepKindToolCall   = agentrun.StepKindToolCall
	StepKindToolResult = agentrun.StepKindToolResult
	StepKindAnswer     = agentrun.StepKindAnswer
	StepKindRetrieval  = agentrun.StepKindRetrieval

	EventAnswerDelta       = agentrun.EventAnswerDelta
	EventToolStarted       = agentrun.EventToolStarted
	EventToolFinished      = agentrun.EventToolFinished
	EventReasoningDelta    = agentrun.EventReasoningDelta
	EventTrace             = agentrun.EventTrace
	EventLLMCall           = agentrun.EventLLMCall
	EventExecutionRouted   = agentrun.EventExecutionRouted
	EventExecutionDegraded = agentrun.EventExecutionDegraded
	EventWorkflowStarted   = agentrun.EventWorkflowStarted
	EventAgentStarted      = agentrun.EventAgentStarted
	EventAgentCompleted    = agentrun.EventAgentCompleted
	EventEvidenceJoined    = agentrun.EventEvidenceJoined
	EventRunFinished       = agentrun.EventRunFinished

	knowledgeReadScope  = platformscope.KnowledgeRead
	knowledgeWriteScope = platformscope.KnowledgeWrite
)

var ErrEmptyAnswer = agentrun.ErrEmptyAnswer
var ErrRunNotActive = agentrun.ErrRunNotActive

func NewAgent(client *llm.LLMClient, executor *ToolExecutor, config AgentConfig, observer Observer, controller Controller) *Agent {
	return agentexecution.NewAgent(client, executor, config, observer, controller)
}

func NewToolExecutor(registry *Registry) *ToolExecutor {
	return agentexecution.NewToolExecutor(registry)
}

func NoopObserver() Observer {
	return agentexecution.NoopObserver()
}

func NewDefinitionRuntime(
	definitions DefinitionResolver,
	schemas *agentapi.SchemaRegistry,
	registry *tool.Registry,
	settings *config.PlatformSettings,
	runStore *agentrun.RunStore,
) (*DefinitionRuntime, error) {
	return agentdefinition.NewDefinitionRuntime(definitions, schemas, registry, settings, runStore)
}

func TerminalFromEvent(event SSEEvent) *RunTerminal {
	return agentrun.TerminalFromEvent(event)
}

func NewRegistry(svc *Service, cfg config.Config, sessions *memory.SessionStore, history SessionHistory) *Registry {
	return agenttools.NewRegistry(svc, cfg, sessions, history)
}


func toolPolicyForRun(allowWrite bool) ToolPolicy {
	return ToolPolicy{
		AllowRead:  true,
		AllowWrite: allowWrite,
	}
}

func beginExecutionTrace(ctx context.Context) (*executiontrace.Scope, bool) {
	inherited := executiontrace.FromContext(ctx)
	trace := executiontrace.Begin(ctx)
	return trace, trace != nil && inherited == nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func buildAgentMessages(
	question string,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	domainKnowledge string,
	historyLimit int,
) []llm.Message {
	return agentexecution.BuildAgentMessages(
		question, conversation, rc, plan, domainKnowledge, historyLimit,
	)
}

func replayableTailMessages(messages []llm.Message, limit int) []llm.Message {
	return agentexecution.ReplayableTailMessages(messages, limit)
}

func classifyResponseMode(question string) domain.ResponseMode {
	return agentexecution.ClassifyResponseMode(question)
}

func shouldShortCircuitMeta(question string) bool {
	return agentexecution.ShouldShortCircuitMeta(question)
}

func compressTurnDetail(turnNumber int, messages []llm.Message) (json.RawMessage, error) {
	return agentsession.CompressTurnDetail(turnNumber, messages)
}

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

func withSessionToolScope(
	ctx context.Context,
	conversation ConversationContext,
	userID int64,
) context.Context {
	return agentsession.WithToolScope(
		ctx,
		conversation.SessionID,
		conversation.CompactedThroughTurn,
		userID,
	)
}
