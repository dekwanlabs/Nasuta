package agent

import (
	"github.com/dekwanlabs/nasuta/internal/agent/execution"
	agentqa "github.com/dekwanlabs/nasuta/internal/agent/qa"
	"github.com/dekwanlabs/nasuta/platform/config"
)

type ConversationContext = execution.ConversationContext
type QA = agentqa.Service
type QADeps = agentqa.Deps
type QAModels = agentqa.Models
type QARequest = agentqa.Request
type AskResult = agentqa.AskResult

func NewQA(deps QADeps) *QA {
	return agentqa.New(deps)
}

func NewQAModels(settings *config.PlatformSettings) *QAModels {
	return agentqa.NewModels(settings)
}

func NewRunID() string {
	return agentqa.NewRunID()
}
