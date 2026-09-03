package qa

import (
	"context"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/config"
	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/memory"
	"github.com/dekwanlabs/nasuta/internal/platform/store/codegraph"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/tool"
)

// Deps bundles the services needed by the QA scenario.
type Deps struct {
	Tools           *ToolService
	Cfg             config.Config
	Platform        *config.PlatformSettings
	CodeGraphDB     *codegraph.DB
	History         SessionHistory
	Sessions        *memory.SessionStore
	Memory          *memory.MemoryStore
	Definitions     DefinitionResolver
	Agent           agentapi.DefinitionRef
	Runtime         agentapi.ManagedRuntime
	RuntimeTools    ScenarioToolSource
	Models          *Models
	PhaseEmitter    PhaseEmitter
	ExecutionEvents ExecutionEventEmitter
	WriteAvailable  bool
}

type SelectionResolver interface {
	ResolveFor(agentapi.DefinitionRef, string) (
		agentapi.Definition,
		agentapi.DefinitionSelection,
		error,
	)
}

type contextRetriever interface {
	RetrievePlan(context.Context, string, retrieval.QueryTerms, domain.EvidencePlan, domain.QueryPlan) (*retrieval.RetrievedContext, error)
	ContextBudget() int
}

// AskResult identifies the asynchronous run and its pre-retrieved context.
type AskResult struct {
	RunID   string
	Context *retrieval.RetrievedContext
}

// ContextBlock is trusted evidence prepared by an upper-layer scenario.
type ContextBlock struct {
	Source     string
	Title      string
	Content    string
	References []retrieval.Reference
	Evidence   []tool.EvidenceUnit
}

type PlannedToolCall struct {
	ToolID    tool.ToolID
	Arguments tool.Arguments
	Required  bool
}

type ToolPlan struct {
	Prefetch []PlannedToolCall
}

// Request is the stable use-case input for standard and scenario handlers.
type Request struct {
	Question         string
	Conversation     ConversationContext
	PreloadedContext []ContextBlock
	UserID           int64
	RolePrompt       string
	RunID            string
	EvidencePlan     *domain.EvidencePlan
	ToolPlan         ToolPlan
	WriteAuthorized  bool
	WriteRequested   bool
	Agent            agentapi.DefinitionRef
	ParentRunID      string
	WorkflowRunID    string
	WorkflowNodeID   string
}
