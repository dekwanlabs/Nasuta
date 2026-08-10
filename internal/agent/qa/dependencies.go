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
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/executiontrace"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	platformscope "github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

type ConversationContext = agentexecution.ConversationContext
type RunResult = agentexecution.RunResult
type Service = agenttools.Service
type SessionHistory = agentsession.SessionHistory
type HistoryCandidates = agentsession.HistoryCandidates
type CandidateDiscovery = agentsession.CandidateDiscovery
type DefinitionResolver = agentdefinition.DefinitionResolver
type ScenarioRunStart = agentdefinition.ScenarioRunStart
type ScenarioLifecycle = agentdefinition.ScenarioLifecycle
type ScenarioToolSet = agentdefinition.ScenarioToolSet
type ScenarioToolSource = agentdefinition.ScenarioToolSource
type Agent = agentexecution.Agent

type RunOutcome = agentrun.RunOutcome
type RunStepRecord = agentrun.StepRecord
type EvidenceStatus = agentrun.EvidenceStatus
type EvidenceMetrics = agentrun.EvidenceMetrics
type ExecutionEvent = agentrun.ExecutionEvent
type ExecutionEventEmitter = agentrun.ExecutionEventEmitter
type SessionStatusEvent = agentrun.SessionStatusEvent
type EventType = agentrun.EventType
type ToolPolicy = tool.Policy
type Tool = tool.Tool

const (
	RunStatusDone    = agentrun.RunStatusDone
	RunStatusFailed  = agentrun.RunStatusFailed
	RunStatusAborted = agentrun.RunStatusAborted

	EvidenceComplete    = agentrun.EvidenceComplete
	EvidencePartial     = agentrun.EvidencePartial
	EvidenceUnavailable = agentrun.EvidenceUnavailable

	EventExecutionRouted   = agentrun.EventExecutionRouted
	EventExecutionDegraded = agentrun.EventExecutionDegraded

	knowledgeReadScope  = platformscope.KnowledgeRead
	knowledgeWriteScope = platformscope.KnowledgeWrite
)

var ErrEmptyAnswer = agentrun.ErrEmptyAnswer

type preparationStepRecorder interface {
	RecordPreparationStep(context.Context, agentrun.StepRecord) error
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
