package agent

import (
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/platform/config"
)

type ConversationContext = execution.ConversationContext
type QA = agentqa.QA
type QADeps = agentqa.QADeps
type QAModels = agentqa.QAModels
type QARequest = agentqa.QARequest
type AskResult = agentqa.AskResult
type InvestigationRequest = agentqa.InvestigationRequest
type InvestigationResult = agentqa.InvestigationResult
type InvestigationTerminal = agentqa.InvestigationTerminal
type InvestigationStatus = agentqa.InvestigationStatus
type InvestigationUsage = agentqa.InvestigationUsage
type InvestigationRunner = agentqa.InvestigationRunner
type InvestigationCoordinator = agentqa.InvestigationCoordinator
type ParentRunReader = agentqa.ParentRunReader
type TaskContract = agentqa.TaskContract
type EntityRef = agentqa.EntityRef
type EvidenceGoal = agentqa.EvidenceGoal
type TaskContext = agentqa.TaskContext
type ConversationRef = agentqa.ConversationRef
type TaskTimeRange = agentqa.TaskTimeRange

const (
	InvestigationSucceeded = agentqa.InvestigationSucceeded
	InvestigationFailed    = agentqa.InvestigationFailed
	InvestigationCancelled = agentqa.InvestigationCancelled
	InvestigationTimedOut  = agentqa.InvestigationTimedOut
)

func NewQA(deps QADeps) *QA {
	return agentqa.NewQA(deps)
}

func NewInvestigationCoordinator(
	investigation InvestigationRunner,
	scenarios ScenarioLifecycle,
	parentRuns ParentRunReader,
	sessions *memory.SessionStore,
) *InvestigationCoordinator {
	return agentqa.NewInvestigationCoordinator(
		investigation,
		scenarios,
		parentRuns,
		sessions,
	)
}

func NewQAModels(settings *config.PlatformSettings) *QAModels {
	return agentqa.NewQAModels(settings)
}

func NewRunID() string {
	return agentqa.NewRunID()
}
