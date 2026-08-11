package agent

import (
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
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
type InvestigationUsage = agentqa.InvestigationUsage
type InvestigationRunner = agentqa.InvestigationRunner

func NewQA(deps QADeps) *QA {
	return agentqa.NewQA(deps)
}

func NewQAModels(settings *config.PlatformSettings) *QAModels {
	return agentqa.NewQAModels(settings)
}

func NewRunID() string {
	return agentqa.NewRunID()
}
