package qa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/agent/definition"
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/agent/run"
	"github.com/dekwanlabs/nasuta/internal/agent/session"
	"github.com/dekwanlabs/nasuta/internal/agent/tools"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/runtrace"
	"github.com/dekwanlabs/nasuta/internal/scope"
	"github.com/dekwanlabs/nasuta/tool"
)

type ConversationContext = execution.ConversationContext
type RunResult = execution.RunResult
type ToolService = tools.Service
type SessionHistory = session.History
type HistoryCandidates = session.HistoryCandidates
type CandidateDiscovery = session.CandidateDiscovery
type DefinitionResolver = definition.Resolver
type ScenarioToolSet = definition.ScenarioToolSet
type ScenarioToolSource = definition.ScenarioToolSource
type Agent = execution.Agent

type RunOutcome = run.Outcome
type RunStepRecord = run.StepRecord
type EvidenceStatus = run.EvidenceStatus
type EvidenceMetrics = run.EvidenceMetrics
type ExecutionEvent = run.ExecutionEvent
type ExecutionEventEmitter = run.ExecutionEventEmitter
type SessionStatusEvent = run.SessionStatusEvent
type ContextUsageEvent = run.ContextUsageEvent
type EventType = run.EventType
type ToolPolicy = tool.Policy
type Tool = tool.Tool

const (
	RunStatusDone    = run.StatusDone
	RunStatusFailed  = run.StatusFailed
	RunStatusAborted = run.StatusAborted

	EvidenceComplete    = run.EvidenceComplete
	EvidencePartial     = run.EvidencePartial
	EvidenceUnavailable = run.EvidenceUnavailable

	EventExecutionRouted   = run.EventExecutionRouted
	EventExecutionDegraded = run.EventExecutionDegraded

	knowledgeReadScope  = scope.KnowledgeRead
	knowledgeWriteScope = scope.KnowledgeWrite
)

var ErrEmptyAnswer = run.ErrEmptyAnswer

type sessionTurnStore interface {
	EnsureSession(string, int64, string) error
	AppendTurn(string, string, int64, []llm.Message) (int, error)
}

type preparationStepRecorder interface {
	RecordStep(context.Context, run.StepRecord) error
}

type evidenceLedgerRecorder interface {
	RecordEvidence(context.Context, []tool.EvidenceUnit) error
}

func recordEvidenceLedger(
	ctx context.Context,
	owner any,
	units []tool.EvidenceUnit,
) error {
	recorder, ok := owner.(evidenceLedgerRecorder)
	if !ok || len(units) == 0 {
		return nil
	}
	return recorder.RecordEvidence(ctx, units)
}

func toolPolicyForRun(allowWrite bool) ToolPolicy {
	return ToolPolicy{
		AllowRead:  true,
		AllowWrite: allowWrite,
	}
}

func beginExecutionTrace(ctx context.Context) (*runtrace.Scope, bool) {
	inherited := runtrace.FromContext(ctx)
	trace := runtrace.Begin(ctx)
	return trace, trace != nil && inherited == nil
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func buildAgentMessages(
	question string,
	query domain.QueryPlan,
	conversation ConversationContext,
	rc *retrieval.RetrievedContext,
	plan domain.EvidencePlan,
	domainKnowledge string,
	historyLimit int,
) []llm.Message {
	return execution.BuildMessages(
		question, query, conversation, rc, plan, domainKnowledge, historyLimit,
	)
}

func replayableTailMessages(messages []llm.Message, limit int) []llm.Message {
	return execution.ReplayableTailMessages(messages, limit)
}

func shouldShortCircuitMeta(question string) bool {
	return execution.ShouldShortCircuitMeta(question)
}

func compressTurnDetail(turnNumber int, messages []llm.Message) (json.RawMessage, error) {
	return session.CompressDetail(turnNumber, messages)
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
	return session.WithToolScope(
		ctx,
		conversation.SessionID,
		conversation.CompactedThroughTurn,
		userID,
	)
}
