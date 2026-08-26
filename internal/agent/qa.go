package agent

import (
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/platform/config"
)

type ConversationContext = execution.ConversationContext
type QA = agentqa.Service
type QADeps = agentqa.Deps
type QAModels = agentqa.Models
type QARequest = agentqa.Request
type AskResult = agentqa.AskResult
type InvestigationRequest = agentqa.InvestigationRequest
type InvestigationResult = agentqa.InvestigationResult
type InvestigationClaim = agentqa.InvestigationClaim
type InvestigationUnsupportedClaim = agentqa.InvestigationUnsupportedClaim
type InvestigationVerification = agentqa.InvestigationVerification
type InvestigationCitation = agentqa.InvestigationCitation
type InvestigationTerminal = agentqa.InvestigationTerminal
type InvestigationStatus = agentqa.InvestigationStatus
type InvestigationCompleteness = agentqa.InvestigationCompleteness
type InvestigationUsage = agentqa.InvestigationUsage
type InvestigationBudget = agentqa.InvestigationBudget
type InvestigationRunner = agentqa.InvestigationRunner
type InvestigationPlanner = agentqa.InvestigationPlanner
type InvestigationContinuationRunner = agentqa.InvestigationContinuationRunner
type InvestigationContinuationRequest = agentqa.InvestigationContinuationRequest
type InvestigationRoundSnapshot = agentqa.InvestigationRoundSnapshot
type QACoordinator = agentqa.Coordinator
type ParentRunReader = agentqa.ParentRunReader
type TaskContract = agentqa.TaskContract
type EntityRef = agentqa.EntityRef
type InvestigationGoal = agentqa.InvestigationGoal
type EvidenceGoal = agentqa.EvidenceGoal
type TaskEvidenceAssignment = agentqa.TaskEvidenceAssignment
type TaskContextRef = agentqa.TaskContextRef
type TaskContext = agentqa.TaskContext
type ConversationRef = agentqa.ConversationRef
type TaskTimeRange = agentqa.TaskTimeRange

// MergeInvestigationResults combines a partial answer with verified facts.
// The verifier's explicit empty goal lists are authoritative.
func MergeInvestigationResults(
	previous, current InvestigationResult,
) InvestigationResult {
	return agentqa.MergeRoundResult(previous, current)
}

const (
	InvestigationSucceeded = agentqa.InvestigationSucceeded
	InvestigationFailed    = agentqa.InvestigationFailed
	InvestigationCancelled = agentqa.InvestigationCancelled
	InvestigationTimedOut  = agentqa.InvestigationTimedOut

	InvestigationComplete    = agentqa.InvestigationComplete
	InvestigationPartial     = agentqa.InvestigationPartial
	InvestigationUnavailable = agentqa.InvestigationUnavailable
)

func NewQA(deps QADeps) *QA {
	return agentqa.New(deps)
}

func NewQACoordinator(
	investigation InvestigationRunner,
	scenarios ScenarioLifecycle,
	parentRuns ParentRunReader,
	sessions *memory.SessionStore,
) *QACoordinator {
	return agentqa.NewCoordinator(
		investigation,
		scenarios,
		parentRuns,
		sessions,
	)
}

func NewQAModels(settings *config.PlatformSettings) *QAModels {
	return agentqa.NewModels(settings)
}

func NewRunID() string {
	return agentqa.NewRunID()
}
