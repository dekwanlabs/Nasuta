package agent

import (
	agentexecution "github.com/dekwanlabs/nasuta/internal/agent/execution"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
)

type AgentConfig = agentexecution.AgentConfig
type ConversationContext = agentexecution.ConversationContext
type Agent = agentexecution.Agent
type RunResult = agentexecution.RunResult
type ToolExecutor = agentexecution.ToolExecutor
type ToolExecution = agentexecution.ToolExecution
type StreamPipe = agentexecution.StreamPipe
type StreamTiming = agentexecution.StreamTiming

var (
	ErrToolCallBudgetExhausted = agentexecution.ErrToolCallBudgetExhausted
	ErrAnswerContractViolation = agentexecution.ErrAnswerContractViolation
	ErrToolProtocolLeak        = agentexecution.ErrToolProtocolLeak
	ErrReasoningTruncated      = agentexecution.ErrReasoningTruncated
	ErrAnswerTruncated         = agentexecution.ErrAnswerTruncated
	ErrEmptyModelResponse      = agentexecution.ErrEmptyModelResponse
	ErrToolResultDelivery      = agentexecution.ErrToolResultDelivery
)

func NewAgent(
	client *llm.LLMClient,
	executor *ToolExecutor,
	config AgentConfig,
	observer Observer,
	controller Controller,
) *Agent {
	return agentexecution.NewAgent(client, executor, config, observer, controller)
}

func NewToolExecutor(registry *Registry) *ToolExecutor {
	return agentexecution.NewToolExecutor(registry)
}

func ClassifyResponseMode(question string) domain.ResponseMode {
	return agentexecution.ClassifyResponseMode(question)
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
		question,
		conversation,
		rc,
		plan,
		domainKnowledge,
		historyLimit,
	)
}

func replayableTailMessages(messages []llm.Message, limit int) []llm.Message {
	return agentexecution.ReplayableTailMessages(messages, limit)
}

func shouldShortCircuitMeta(question string) bool {
	return agentexecution.ShouldShortCircuitMeta(question)
}
